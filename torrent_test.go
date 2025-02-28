package torrent

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/missinggo/v2"
	"github.com/anacrolix/missinggo/v2/bitmap"
	"github.com/bradfitz/iter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/storage"
)

func r(i, b, l pp.Integer) Request {
	return Request{i, ChunkSpec{b, l}}
}

// Check the given request is correct for various torrent offsets.
func TestTorrentRequest(t *testing.T) {
	const s = 472183431 // Length of torrent.
	for _, _case := range []struct {
		off int64   // An offset into the torrent.
		req Request // The expected request. The zero value means !ok.
	}{
		// Invalid offset.
		{-1, Request{}},
		{0, r(0, 0, 16384)},
		// One before the end of a piece.
		{1<<18 - 1, r(0, 1<<18-16384, 16384)},
		// Offset beyond torrent length.
		{472 * 1 << 20, Request{}},
		// One before the end of the torrent. Complicates the chunk length.
		{s - 1, r((s-1)/(1<<18), (s-1)%(1<<18)/(16384)*(16384), 12935)},
		{1, r(0, 0, 16384)},
		// One before end of chunk.
		{16383, r(0, 0, 16384)},
		// Second chunk.
		{16384, r(0, 16384, 16384)},
	} {
		req, ok := torrentOffsetRequest(472183431, 1<<18, 16384, _case.off)
		if (_case.req == Request{}) == ok {
			t.Fatalf("expected %v, got %v", _case.req, req)
		}
		if req != _case.req {
			t.Fatalf("expected %v, got %v", _case.req, req)
		}
	}
}

func TestAppendToCopySlice(t *testing.T) {
	orig := []int{1, 2, 3}
	dupe := append([]int{}, orig...)
	dupe[0] = 4
	if orig[0] != 1 {
		t.FailNow()
	}
}

func TestTorrentString(t *testing.T) {
	tor := &Torrent{}
	s := tor.InfoHash().HexString()
	if s != "0000000000000000000000000000000000000000" {
		t.FailNow()
	}
}

// This benchmark is from the observation that a lot of overlapping Readers on
// a large torrent with small pieces had a lot of overhead in recalculating
// piece priorities everytime a reader (possibly in another Torrent) changed.
func BenchmarkUpdatePiecePriorities(b *testing.B) {
	const (
		numPieces   = 13410
		pieceLength = 256 << 10
	)
	cl := &Client{config: TestingConfig(b)}
	cl.initLogger()
	t := cl.newTorrent(metainfo.Hash{}, nil)
	require.NoError(b, t.setInfo(&metainfo.Info{
		Pieces:      make([]byte, metainfo.HashSize*numPieces),
		PieceLength: pieceLength,
		Length:      pieceLength * numPieces,
	}))
	assert.EqualValues(b, 13410, t.numPieces())
	for range iter.N(7) {
		r := t.NewReader()
		r.SetReadahead(32 << 20)
		r.Seek(3500000, io.SeekStart)
	}
	assert.Len(b, t.readers, 7)
	for i := 0; i < t.numPieces(); i += 3 {
		t._completedPieces.Add(bitmap.BitIndex(i))
	}
	t.DownloadPieces(0, t.numPieces())
	for range iter.N(b.N) {
		t.updateAllPiecePriorities()
	}
}

// Check that a torrent containing zero-length file(s) will start, and that
// they're created in the filesystem. The client storage is assumed to be
// file-based on the native filesystem based.
func testEmptyFilesAndZeroPieceLength(t *testing.T, cfg *ClientConfig) {
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	ib, err := bencode.Marshal(metainfo.Info{
		Name:        "empty",
		Length:      0,
		PieceLength: 0,
	})
	require.NoError(t, err)
	fp := filepath.Join(cfg.DataDir, "empty")
	os.Remove(fp)
	assert.False(t, missinggo.FilePathExists(fp))
	tt, err := cl.AddTorrent(&metainfo.MetaInfo{
		InfoBytes: ib,
	})
	require.NoError(t, err)
	defer tt.Drop()
	tt.DownloadAll()
	require.True(t, cl.WaitAll())
	assert.True(t, missinggo.FilePathExists(fp))
}

func TestEmptyFilesAndZeroPieceLengthWithFileStorage(t *testing.T) {
	cfg := TestingConfig(t)
	ci := storage.NewFile(cfg.DataDir)
	defer ci.Close()
	cfg.DefaultStorage = ci
	testEmptyFilesAndZeroPieceLength(t, cfg)
}

func TestPieceHashFailed(t *testing.T) {
	mi := testutil.GreetingMetaInfo()
	cl := new(Client)
	cl.config = TestingConfig(t)
	cl.initLogger()
	tt := cl.newTorrent(mi.HashInfoBytes(), badStorage{})
	tt.setChunkSize(2)
	require.NoError(t, tt.setInfoBytes(mi.InfoBytes))
	tt.cl.lock()
	tt.pieces[1]._dirtyChunks.AddRange(0, 3)
	require.True(t, tt.pieceAllDirty(1))
	tt.pieceHashed(1, false, nil)
	// Dirty chunks should be cleared so we can try again.
	require.False(t, tt.pieceAllDirty(1))
	tt.cl.unlock()
}

// Check the behaviour of Torrent.Metainfo when metadata is not completed.
func TestTorrentMetainfoIncompleteMetadata(t *testing.T) {
	cfg := TestingConfig(t)
	cfg.Debug = true
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()

	mi := testutil.GreetingMetaInfo()
	ih := mi.HashInfoBytes()

	tt, _ := cl.AddTorrentInfoHash(ih)
	assert.Nil(t, tt.Metainfo().InfoBytes)
	assert.False(t, tt.haveAllMetadataPieces())

	nc, err := net.Dial("tcp", fmt.Sprintf(":%d", cl.LocalPort()))
	require.NoError(t, err)
	defer nc.Close()

	var pex PeerExtensionBits
	pex.SetBit(pp.ExtensionBitExtended, true)
	hr, err := pp.Handshake(nc, &ih, [20]byte{}, pex)
	require.NoError(t, err)
	assert.True(t, hr.PeerExtensionBits.GetBit(pp.ExtensionBitExtended))
	assert.EqualValues(t, cl.PeerID(), hr.PeerID)
	assert.EqualValues(t, ih, hr.Hash)

	assert.EqualValues(t, 0, tt.metadataSize())

	func() {
		cl.lock()
		defer cl.unlock()
		go func() {
			_, err = nc.Write(pp.Message{
				Type:       pp.Extended,
				ExtendedID: pp.HandshakeExtendedID,
				ExtendedPayload: func() []byte {
					d := map[string]interface{}{
						"metadata_size": len(mi.InfoBytes),
					}
					b, err := bencode.Marshal(d)
					if err != nil {
						panic(err)
					}
					return b
				}(),
			}.MustMarshalBinary())
			require.NoError(t, err)
		}()
		tt.metadataChanged.Wait()
	}()
	assert.Equal(t, make([]byte, len(mi.InfoBytes)), tt.metadataBytes)
	assert.False(t, tt.haveAllMetadataPieces())
	assert.Nil(t, tt.Metainfo().InfoBytes)
}
