package torrent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/iter"
	"github.com/anacrolix/missinggo/v2/bitmap"
	"github.com/anacrolix/missinggo/v2/prioritybitmap"
	"github.com/anacrolix/multiless"

	"github.com/anacrolix/chansync"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mse"
	pp "github.com/anacrolix/torrent/peer_protocol"
	request_strategy "github.com/anacrolix/torrent/request-strategy"
)

type PeerSource string

const (
	PeerSourceTracker         = "Tr"
	PeerSourceIncoming        = "I"
	PeerSourceDhtGetPeers     = "Hg" // Peers we found by searching a DHT.
	PeerSourceDhtAnnouncePeer = "Ha" // Peers that were announced to us by a DHT.
	PeerSourcePex             = "X"
	// The peer was given directly, such as through a magnet link.
	PeerSourceDirect = "M"
)

type peerRequestState struct {
	data []byte
}

type PeerRemoteAddr interface {
	String() string
}

// Since we have to store all the requests in memory, we can't reasonably exceed what would be
// indexable with the memory space available.
type (
	maxRequests  = int
	requestState = request_strategy.PeerNextRequestState
)

type Peer struct {
	// First to ensure 64-bit alignment for atomics. See #262.
	_stats ConnStats

	t *Torrent

	peerImpl
	callbacks *Callbacks

	outgoing   bool
	Network    string
	RemoteAddr PeerRemoteAddr
	// True if the connection is operating over MSE obfuscation.
	headerEncrypted bool
	cryptoMethod    mse.CryptoMethod
	Discovery       PeerSource
	trusted         bool
	closed          chansync.SetOnce
	// Set true after we've added our ConnStats generated during handshake to
	// other ConnStat instances as determined when the *Torrent became known.
	reconciledHandshakeStats bool

	lastMessageReceived     time.Time
	completedHandshake      time.Time
	lastUsefulChunkReceived time.Time
	lastChunkSent           time.Time

	// Stuff controlled by the local peer.
	nextRequestState     requestState
	actualRequestState   requestState
	lastBecameInterested time.Time
	priorInterest        time.Duration

	lastStartedExpectingToReceiveChunks time.Time
	cumulativeExpectedToReceiveChunks   time.Duration
	_chunksReceivedWhileExpecting       int64

	choking                                bool
	piecesReceivedSinceLastRequestUpdate   maxRequests
	maxPiecesReceivedBetweenRequestUpdates maxRequests
	// Chunks that we might reasonably expect to receive from the peer. Due to
	// latency, buffering, and implementation differences, we may receive
	// chunks that are no longer in the set of requests actually want.
	validReceiveChunks map[Request]int
	// Indexed by metadata piece, set to true if posted and pending a
	// response.
	metadataRequests []bool
	sentHaves        bitmap.Bitmap

	// Stuff controlled by the remote peer.
	peerInterested        bool
	peerChoking           bool
	peerRequests          map[Request]*peerRequestState
	PeerPrefersEncryption bool // as indicated by 'e' field in extension handshake
	PeerListenPort        int
	// The pieces the peer has claimed to have.
	_peerPieces bitmap.Bitmap
	// The peer has everything. This can occur due to a special message, when
	// we may not even know the number of pieces in the torrent yet.
	peerSentHaveAll bool
	// The highest possible number of pieces the torrent could have based on
	// communication with the peer. Generally only useful until we have the
	// torrent info.
	peerMinPieces pieceIndex
	// Pieces we've accepted chunks for from the peer.
	peerTouchedPieces map[pieceIndex]struct{}
	peerAllowedFast   bitmap.Bitmap

	PeerMaxRequests  maxRequests // Maximum pending requests the peer allows.
	PeerExtensionIDs map[pp.ExtensionName]pp.ExtensionNumber
	PeerClientName   string

	pieceInclination   []int
	_pieceRequestOrder prioritybitmap.PriorityBitmap

	logger log.Logger
}

// Maintains the state of a BitTorrent-protocol based connection with a peer.
type PeerConn struct {
	Peer

	// A string that should identify the PeerConn's net.Conn endpoints. The net.Conn could
	// be wrapping WebRTC, uTP, or TCP etc. Used in writing the conn status for peers.
	connString string

	// See BEP 3 etc.
	PeerID             PeerID
	PeerExtensionBytes pp.PeerExtensionBits

	// The actual Conn, used for closing, and setting socket options.
	conn net.Conn
	// The Reader and Writer for this Conn, with hooks installed for stats,
	// limiting, deadlines etc.
	w io.Writer
	r io.Reader

	messageWriter peerConnMsgWriter

	uploadTimer *time.Timer
	pex         pexConnState
}

func (cn *PeerConn) connStatusString() string {
	return fmt.Sprintf("%+-55q %s %s", cn.PeerID, cn.PeerExtensionBytes, cn.connString)
}

func (cn *Peer) updateExpectingChunks() {
	if cn.expectingChunks() {
		if cn.lastStartedExpectingToReceiveChunks.IsZero() {
			cn.lastStartedExpectingToReceiveChunks = time.Now()
		}
	} else {
		if !cn.lastStartedExpectingToReceiveChunks.IsZero() {
			cn.cumulativeExpectedToReceiveChunks += time.Since(cn.lastStartedExpectingToReceiveChunks)
			cn.lastStartedExpectingToReceiveChunks = time.Time{}
		}
	}
}

func (cn *Peer) expectingChunks() bool {
	if len(cn.actualRequestState.Requests) == 0 {
		return false
	}
	if !cn.actualRequestState.Interested {
		return false
	}
	for r := range cn.actualRequestState.Requests {
		if !cn.remoteChokingPiece(r.Index.Int()) {
			return true
		}
	}
	return false
}

func (cn *Peer) remoteChokingPiece(piece pieceIndex) bool {
	return cn.peerChoking && !cn.peerAllowedFast.Contains(bitmap.BitIndex(piece))
}

// Returns true if the connection is over IPv6.
func (cn *PeerConn) ipv6() bool {
	ip := cn.remoteIp()
	if ip.To4() != nil {
		return false
	}
	return len(ip) == net.IPv6len
}

// Returns true the if the dialer/initiator has the lower client peer ID. TODO: Find the
// specification for this.
func (cn *PeerConn) isPreferredDirection() bool {
	return bytes.Compare(cn.t.cl.peerID[:], cn.PeerID[:]) < 0 == cn.outgoing
}

// Returns whether the left connection should be preferred over the right one,
// considering only their networking properties. If ok is false, we can't
// decide.
func (l *PeerConn) hasPreferredNetworkOver(r *PeerConn) (left, ok bool) {
	var ml multiLess
	ml.NextBool(l.isPreferredDirection(), r.isPreferredDirection())
	ml.NextBool(!l.utp(), !r.utp())
	ml.NextBool(l.ipv6(), r.ipv6())
	return ml.FinalOk()
}

func (cn *Peer) cumInterest() time.Duration {
	ret := cn.priorInterest
	if cn.actualRequestState.Interested {
		ret += time.Since(cn.lastBecameInterested)
	}
	return ret
}

func (cn *Peer) peerHasAllPieces() (all bool, known bool) {
	if cn.peerSentHaveAll {
		return true, true
	}
	if !cn.t.haveInfo() {
		return false, false
	}
	return bitmap.Flip(cn._peerPieces, 0, bitmap.BitRange(cn.t.numPieces())).IsEmpty(), true
}

func (cn *PeerConn) locker() *lockWithDeferreds {
	return cn.t.cl.locker()
}

func (cn *Peer) supportsExtension(ext pp.ExtensionName) bool {
	_, ok := cn.PeerExtensionIDs[ext]
	return ok
}

// The best guess at number of pieces in the torrent for this peer.
func (cn *Peer) bestPeerNumPieces() pieceIndex {
	if cn.t.haveInfo() {
		return cn.t.numPieces()
	}
	return cn.peerMinPieces
}

func (cn *Peer) completedString() string {
	have := pieceIndex(cn._peerPieces.Len())
	if cn.peerSentHaveAll {
		have = cn.bestPeerNumPieces()
	}
	return fmt.Sprintf("%d/%d", have, cn.bestPeerNumPieces())
}

func (cn *PeerConn) onGotInfo(info *metainfo.Info) {
	cn.setNumPieces(info.NumPieces())
}

// Correct the PeerPieces slice length. Return false if the existing slice is invalid, such as by
// receiving badly sized BITFIELD, or invalid HAVE messages.
func (cn *PeerConn) setNumPieces(num pieceIndex) {
	cn._peerPieces.RemoveRange(bitmap.BitRange(num), bitmap.ToEnd)
	cn.peerPiecesChanged()
}

func eventAgeString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%.2fs ago", time.Since(t).Seconds())
}

func (cn *PeerConn) connectionFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.cryptoMethod == mse.CryptoMethodRC4 {
		c('E')
	} else if cn.headerEncrypted {
		c('e')
	}
	ret += string(cn.Discovery)
	if cn.utp() {
		c('U')
	}
	return
}

func (cn *PeerConn) utp() bool {
	return parseNetworkString(cn.Network).Udp
}

// Inspired by https://github.com/transmission/transmission/wiki/Peer-Status-Text.
func (cn *Peer) statusFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.actualRequestState.Interested {
		c('i')
	}
	if cn.choking {
		c('c')
	}
	c('-')
	ret += cn.connectionFlags()
	c('-')
	if cn.peerInterested {
		c('i')
	}
	if cn.peerChoking {
		c('c')
	}
	return
}

func (cn *Peer) downloadRate() float64 {
	num := cn._stats.BytesReadUsefulData.Int64()
	if num == 0 {
		return 0
	}
	return float64(num) / cn.totalExpectingTime().Seconds()
}

func (cn *Peer) numRequestsByPiece() (ret map[pieceIndex]int) {
	ret = make(map[pieceIndex]int)
	for r := range cn.actualRequestState.Requests {
		ret[pieceIndex(r.Index)]++
	}
	return
}

func (cn *Peer) writeStatus(w io.Writer, t *Torrent) {
	// \t isn't preserved in <pre> blocks?
	if cn.closed.IsSet() {
		fmt.Fprint(w, "CLOSED: ")
	}
	fmt.Fprintln(w, cn.connStatusString())
	prio, err := cn.peerPriority()
	prioStr := fmt.Sprintf("%08x", prio)
	if err != nil {
		prioStr += ": " + err.Error()
	}
	fmt.Fprintf(w, "    bep40-prio: %v\n", prioStr)
	fmt.Fprintf(w, "    last msg: %s, connected: %s, last helpful: %s, itime: %s, etime: %s\n",
		eventAgeString(cn.lastMessageReceived),
		eventAgeString(cn.completedHandshake),
		eventAgeString(cn.lastHelpful()),
		cn.cumInterest(),
		cn.totalExpectingTime(),
	)
	fmt.Fprintf(w,
		"    %s completed, %d pieces touched, good chunks: %v/%v-%v reqq: %d/(%d/%d)-%d/%d, flags: %s, dr: %.1f KiB/s\n",
		cn.completedString(),
		len(cn.peerTouchedPieces),
		&cn._stats.ChunksReadUseful,
		&cn._stats.ChunksRead,
		&cn._stats.ChunksWritten,
		cn.numLocalRequests(),
		cn.nominalMaxRequests(),
		cn.PeerMaxRequests,
		len(cn.peerRequests),
		localClientReqq,
		cn.statusFlags(),
		cn.downloadRate()/(1<<10),
	)
	fmt.Fprintf(w, "    requested pieces:")
	type pieceNumRequestsType struct {
		piece       pieceIndex
		numRequests int
	}
	var pieceNumRequests []pieceNumRequestsType
	for piece, count := range cn.numRequestsByPiece() {
		pieceNumRequests = append(pieceNumRequests, pieceNumRequestsType{piece, count})
	}
	sort.Slice(pieceNumRequests, func(i, j int) bool {
		return pieceNumRequests[i].piece < pieceNumRequests[j].piece
	})
	for _, elem := range pieceNumRequests {
		fmt.Fprintf(w, " %v(%v)", elem.piece, elem.numRequests)
	}
	fmt.Fprintf(w, "\n")
}

func (p *Peer) close() {
	if !p.closed.Set() {
		return
	}
	p.discardPieceInclination()
	p._pieceRequestOrder.Clear()
	p.peerImpl.onClose()
	if p.t != nil {
		p.t.decPeerPieceAvailability(p)
	}
	for _, f := range p.callbacks.PeerClosed {
		f(p)
	}
}

func (cn *PeerConn) onClose() {
	if cn.pex.IsEnabled() {
		cn.pex.Close()
	}
	cn.tickleWriter()
	if cn.conn != nil {
		cn.conn.Close()
	}
	if cb := cn.callbacks.PeerConnClosed; cb != nil {
		cb(cn)
	}
}

func (cn *Peer) peerHasPiece(piece pieceIndex) bool {
	return cn.peerSentHaveAll || cn._peerPieces.Contains(bitmap.BitIndex(piece))
}

// 64KiB, but temporarily less to work around an issue with WebRTC. TODO: Update when
// https://github.com/pion/datachannel/issues/59 is fixed.
const writeBufferHighWaterLen = 1 << 15

// Writes a message into the write buffer. Returns whether it's okay to keep writing. Writing is
// done asynchronously, so it may be that we're not able to honour backpressure from this method.
func (cn *PeerConn) write(msg pp.Message) bool {
	torrent.Add(fmt.Sprintf("messages written of type %s", msg.Type.String()), 1)
	// We don't need to track bytes here because the connection's Writer has that behaviour injected
	// (although there's some delay between us buffering the message, and the connection writer
	// flushing it out.).
	notFull := cn.messageWriter.write(msg)
	// Last I checked only Piece messages affect stats, and we don't write those.
	cn.wroteMsg(&msg)
	cn.tickleWriter()
	return notFull
}

func (cn *PeerConn) requestMetadataPiece(index int) {
	eID := cn.PeerExtensionIDs[pp.ExtensionNameMetadata]
	if eID == pp.ExtensionDeleteNumber {
		return
	}
	if index < len(cn.metadataRequests) && cn.metadataRequests[index] {
		return
	}
	cn.logger.WithDefaultLevel(log.Debug).Printf("requesting metadata piece %d", index)
	cn.write(pp.MetadataExtensionRequestMsg(eID, index))
	for index >= len(cn.metadataRequests) {
		cn.metadataRequests = append(cn.metadataRequests, false)
	}
	cn.metadataRequests[index] = true
}

func (cn *PeerConn) requestedMetadataPiece(index int) bool {
	return index < len(cn.metadataRequests) && cn.metadataRequests[index]
}

// The actual value to use as the maximum outbound requests.
func (cn *Peer) nominalMaxRequests() (ret maxRequests) {
	return int(clamp(1, 2*int64(cn.maxPiecesReceivedBetweenRequestUpdates), int64(cn.PeerMaxRequests)))
}

func (cn *Peer) totalExpectingTime() (ret time.Duration) {
	ret = cn.cumulativeExpectedToReceiveChunks
	if !cn.lastStartedExpectingToReceiveChunks.IsZero() {
		ret += time.Since(cn.lastStartedExpectingToReceiveChunks)
	}
	return

}

func (cn *PeerConn) onPeerSentCancel(r Request) {
	if _, ok := cn.peerRequests[r]; !ok {
		torrent.Add("unexpected cancels received", 1)
		return
	}
	if cn.fastEnabled() {
		cn.reject(r)
	} else {
		delete(cn.peerRequests, r)
	}
}

func (cn *PeerConn) choke(msg messageWriter) (more bool) {
	if cn.choking {
		return true
	}
	cn.choking = true
	more = msg(pp.Message{
		Type: pp.Choke,
	})
	if cn.fastEnabled() {
		for r := range cn.peerRequests {
			// TODO: Don't reject pieces in allowed fast set.
			cn.reject(r)
		}
	} else {
		cn.peerRequests = nil
	}
	return
}

func (cn *PeerConn) unchoke(msg func(pp.Message) bool) bool {
	if !cn.choking {
		return true
	}
	cn.choking = false
	return msg(pp.Message{
		Type: pp.Unchoke,
	})
}

func (cn *Peer) setInterested(interested bool) bool {
	if cn.actualRequestState.Interested == interested {
		return true
	}
	cn.actualRequestState.Interested = interested
	if interested {
		cn.lastBecameInterested = time.Now()
	} else if !cn.lastBecameInterested.IsZero() {
		cn.priorInterest += time.Since(cn.lastBecameInterested)
	}
	cn.updateExpectingChunks()
	// log.Printf("%p: setting interest: %v", cn, interested)
	return cn.writeInterested(interested)
}

func (pc *PeerConn) writeInterested(interested bool) bool {
	return pc.write(pp.Message{
		Type: func() pp.MessageType {
			if interested {
				return pp.Interested
			} else {
				return pp.NotInterested
			}
		}(),
	})
}

// The function takes a message to be sent, and returns true if more messages
// are okay.
type messageWriter func(pp.Message) bool

func (cn *Peer) shouldRequest(r Request) error {
	if !cn.peerHasPiece(pieceIndex(r.Index)) {
		return errors.New("requesting piece peer doesn't have")
	}
	if !cn.t.peerIsActive(cn) {
		panic("requesting but not in active conns")
	}
	if cn.closed.IsSet() {
		panic("requesting when connection is closed")
	}
	if cn.t.hashingPiece(pieceIndex(r.Index)) {
		panic("piece is being hashed")
	}
	if cn.t.pieceQueuedForHash(pieceIndex(r.Index)) {
		panic("piece is queued for hash")
	}
	return nil
}

func (cn *Peer) request(r Request) (more bool, err error) {
	if err := cn.shouldRequest(r); err != nil {
		panic(err)
	}
	if _, ok := cn.actualRequestState.Requests[r]; ok {
		return true, nil
	}
	if cn.numLocalRequests() >= cn.nominalMaxRequests() {
		return true, errors.New("too many outstanding requests")
	}
	if cn.actualRequestState.Requests == nil {
		cn.actualRequestState.Requests = make(map[Request]struct{})
	}
	cn.actualRequestState.Requests[r] = struct{}{}
	if cn.validReceiveChunks == nil {
		cn.validReceiveChunks = make(map[Request]int)
	}
	cn.validReceiveChunks[r]++
	cn.t.pendingRequests[r]++
	cn.updateExpectingChunks()
	for _, f := range cn.callbacks.SentRequest {
		f(PeerRequestEvent{cn, r})
	}
	return cn.peerImpl._request(r), nil
}

func (me *PeerConn) _request(r Request) bool {
	return me.write(pp.Message{
		Type:   pp.Request,
		Index:  r.Index,
		Begin:  r.Begin,
		Length: r.Length,
	})
}

func (me *Peer) cancel(r Request) bool {
	if me.deleteRequest(r) {
		return me.peerImpl._cancel(r)
	}
	return true
}

func (me *PeerConn) _cancel(r Request) bool {
	return me.write(makeCancelMessage(r))
}

func (cn *PeerConn) fillWriteBuffer() {
	if !cn.applyNextRequestState() {
		return
	}
	if cn.pex.IsEnabled() {
		if flow := cn.pex.Share(cn.write); !flow {
			return
		}
	}
	cn.upload(cn.write)
}

func (cn *PeerConn) have(piece pieceIndex) {
	if cn.sentHaves.Get(bitmap.BitIndex(piece)) {
		return
	}
	cn.write(pp.Message{
		Type:  pp.Have,
		Index: pp.Integer(piece),
	})
	cn.sentHaves.Add(bitmap.BitIndex(piece))
}

func (cn *PeerConn) postBitfield() {
	if cn.sentHaves.Len() != 0 {
		panic("bitfield must be first have-related message sent")
	}
	if !cn.t.haveAnyPieces() {
		return
	}
	cn.write(pp.Message{
		Type:     pp.Bitfield,
		Bitfield: cn.t.bitfield(),
	})
	cn.sentHaves = bitmap.Bitmap{cn.t._completedPieces.Clone()}
}

func (cn *PeerConn) updateRequests() {
	cn.t.cl.tickleRequester()
}

// Emits the indices in the Bitmaps bms in order, never repeating any index.
// skip is mutated during execution, and its initial values will never be
// emitted.
func iterBitmapsDistinct(skip *bitmap.Bitmap, bms ...bitmap.Bitmap) iter.Func {
	return func(cb iter.Callback) {
		for _, bm := range bms {
			if !iter.All(
				func(_i interface{}) bool {
					i := _i.(int)
					if skip.Contains(bitmap.BitIndex(i)) {
						return true
					}
					skip.Add(bitmap.BitIndex(i))
					return cb(i)
				},
				bm.Iter,
			) {
				return
			}
		}
	}
}

// check callers updaterequests
func (cn *Peer) stopRequestingPiece(piece pieceIndex) bool {
	return cn._pieceRequestOrder.Remove(piece)
}

// This is distinct from Torrent piece priority, which is the user's
// preference. Connection piece priority is specific to a connection and is
// used to pseudorandomly avoid connections always requesting the same pieces
// and thus wasting effort.
func (cn *Peer) updatePiecePriority(piece pieceIndex) bool {
	tpp := cn.t.piecePriority(piece)
	if !cn.peerHasPiece(piece) {
		tpp = PiecePriorityNone
	}
	if tpp == PiecePriorityNone {
		return cn.stopRequestingPiece(piece)
	}
	prio := cn.getPieceInclination()[piece]
	return cn._pieceRequestOrder.Set(piece, prio)
}

func (cn *Peer) getPieceInclination() []int {
	if cn.pieceInclination == nil {
		cn.pieceInclination = cn.t.getConnPieceInclination()
	}
	return cn.pieceInclination
}

func (cn *Peer) discardPieceInclination() {
	if cn.pieceInclination == nil {
		return
	}
	cn.t.putPieceInclination(cn.pieceInclination)
	cn.pieceInclination = nil
}

func (cn *Peer) peerPiecesChanged() {
	if cn.t.haveInfo() {
		prioritiesChanged := false
		for i := pieceIndex(0); i < cn.t.numPieces(); i++ {
			if cn.updatePiecePriority(i) {
				prioritiesChanged = true
			}
		}
		if prioritiesChanged {
			cn.updateRequests()
		}
	}
	cn.t.maybeDropMutuallyCompletePeer(cn)
}

func (cn *PeerConn) raisePeerMinPieces(newMin pieceIndex) {
	if newMin > cn.peerMinPieces {
		cn.peerMinPieces = newMin
	}
}

func (cn *PeerConn) peerSentHave(piece pieceIndex) error {
	if cn.t.haveInfo() && piece >= cn.t.numPieces() || piece < 0 {
		return errors.New("invalid piece")
	}
	if cn.peerHasPiece(piece) {
		return nil
	}
	cn.raisePeerMinPieces(piece + 1)
	if !cn.peerHasPiece(piece) {
		cn.t.incPieceAvailability(piece)
	}
	cn._peerPieces.Set(bitmap.BitIndex(piece), true)
	cn.t.maybeDropMutuallyCompletePeer(&cn.Peer)
	if cn.updatePiecePriority(piece) {
		cn.updateRequests()
	}
	return nil
}

func (cn *PeerConn) peerSentBitfield(bf []bool) error {
	if len(bf)%8 != 0 {
		panic("expected bitfield length divisible by 8")
	}
	// We know that the last byte means that at most the last 7 bits are wasted.
	cn.raisePeerMinPieces(pieceIndex(len(bf) - 7))
	if cn.t.haveInfo() && len(bf) > int(cn.t.numPieces()) {
		// Ignore known excess pieces.
		bf = bf[:cn.t.numPieces()]
	}
	pp := cn.newPeerPieces()
	cn.peerSentHaveAll = false
	for i, have := range bf {
		if have {
			cn.raisePeerMinPieces(pieceIndex(i) + 1)
			if !pp.Contains(bitmap.BitIndex(i)) {
				cn.t.incPieceAvailability(i)
			}
		} else {
			if pp.Contains(bitmap.BitIndex(i)) {
				cn.t.decPieceAvailability(i)
			}
		}
		cn._peerPieces.Set(bitmap.BitIndex(i), have)
	}
	cn.peerPiecesChanged()
	return nil
}

func (cn *Peer) onPeerHasAllPieces() {
	t := cn.t
	if t.haveInfo() {
		pp := cn.newPeerPieces()
		for i := range iter.N(t.numPieces()) {
			if !pp.Contains(bitmap.BitIndex(i)) {
				t.incPieceAvailability(i)
			}
		}
	}
	cn.peerSentHaveAll = true
	cn._peerPieces.Clear()
	cn.peerPiecesChanged()
}

func (cn *PeerConn) onPeerSentHaveAll() error {
	cn.onPeerHasAllPieces()
	return nil
}

func (cn *PeerConn) peerSentHaveNone() error {
	cn.t.decPeerPieceAvailability(&cn.Peer)
	cn._peerPieces.Clear()
	cn.peerSentHaveAll = false
	cn.peerPiecesChanged()
	return nil
}

func (c *PeerConn) requestPendingMetadata() {
	if c.t.haveInfo() {
		return
	}
	if c.PeerExtensionIDs[pp.ExtensionNameMetadata] == 0 {
		// Peer doesn't support this.
		return
	}
	// Request metadata pieces that we don't have in a random order.
	var pending []int
	for index := 0; index < c.t.metadataPieceCount(); index++ {
		if !c.t.haveMetadataPiece(index) && !c.requestedMetadataPiece(index) {
			pending = append(pending, index)
		}
	}
	rand.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	for _, i := range pending {
		c.requestMetadataPiece(i)
	}
}

func (cn *PeerConn) wroteMsg(msg *pp.Message) {
	torrent.Add(fmt.Sprintf("messages written of type %s", msg.Type.String()), 1)
	if msg.Type == pp.Extended {
		for name, id := range cn.PeerExtensionIDs {
			if id != msg.ExtendedID {
				continue
			}
			torrent.Add(fmt.Sprintf("Extended messages written for protocol %q", name), 1)
		}
	}
	cn.allStats(func(cs *ConnStats) { cs.wroteMsg(msg) })
}

// After handshake, we know what Torrent and Client stats to include for a
// connection.
func (cn *Peer) postHandshakeStats(f func(*ConnStats)) {
	t := cn.t
	f(&t.stats)
	f(&t.cl.stats)
}

// All ConnStats that include this connection. Some objects are not known
// until the handshake is complete, after which it's expected to reconcile the
// differences.
func (cn *Peer) allStats(f func(*ConnStats)) {
	f(&cn._stats)
	if cn.reconciledHandshakeStats {
		cn.postHandshakeStats(f)
	}
}

func (cn *PeerConn) wroteBytes(n int64) {
	cn.allStats(add(n, func(cs *ConnStats) *Count { return &cs.BytesWritten }))
}

func (cn *PeerConn) readBytes(n int64) {
	cn.allStats(add(n, func(cs *ConnStats) *Count { return &cs.BytesRead }))
}

// Returns whether the connection could be useful to us. We're seeding and
// they want data, we don't have metainfo and they can provide it, etc.
func (c *Peer) useful() bool {
	t := c.t
	if c.closed.IsSet() {
		return false
	}
	if !t.haveInfo() {
		return c.supportsExtension("ut_metadata")
	}
	if t.seeding() && c.peerInterested {
		return true
	}
	if c.peerHasWantedPieces() {
		return true
	}
	return false
}

func (c *Peer) lastHelpful() (ret time.Time) {
	ret = c.lastUsefulChunkReceived
	if c.t.seeding() && c.lastChunkSent.After(ret) {
		ret = c.lastChunkSent
	}
	return
}

func (c *PeerConn) fastEnabled() bool {
	return c.PeerExtensionBytes.SupportsFast() && c.t.cl.config.Extensions.SupportsFast()
}

func (c *PeerConn) reject(r Request) {
	if !c.fastEnabled() {
		panic("fast not enabled")
	}
	c.write(r.ToMsg(pp.Reject))
	delete(c.peerRequests, r)
}

func (c *PeerConn) onReadRequest(r Request) error {
	requestedChunkLengths.Add(strconv.FormatUint(r.Length.Uint64(), 10), 1)
	if _, ok := c.peerRequests[r]; ok {
		torrent.Add("duplicate requests received", 1)
		return nil
	}
	if c.choking {
		torrent.Add("requests received while choking", 1)
		if c.fastEnabled() {
			torrent.Add("requests rejected while choking", 1)
			c.reject(r)
		}
		return nil
	}
	// TODO: What if they've already requested this?
	if len(c.peerRequests) >= localClientReqq {
		torrent.Add("requests received while queue full", 1)
		if c.fastEnabled() {
			c.reject(r)
		}
		// BEP 6 says we may close here if we choose.
		return nil
	}
	if !c.t.havePiece(pieceIndex(r.Index)) {
		// This isn't necessarily them screwing up. We can drop pieces
		// from our storage, and can't communicate this to peers
		// except by reconnecting.
		requestsReceivedForMissingPieces.Add(1)
		return fmt.Errorf("peer requested piece we don't have: %v", r.Index.Int())
	}
	// Check this after we know we have the piece, so that the piece length will be known.
	if r.Begin+r.Length > c.t.pieceLength(pieceIndex(r.Index)) {
		torrent.Add("bad requests received", 1)
		return errors.New("bad Request")
	}
	if c.peerRequests == nil {
		c.peerRequests = make(map[Request]*peerRequestState, localClientReqq)
	}
	value := &peerRequestState{}
	c.peerRequests[r] = value
	go c.peerRequestDataReader(r, value)
	//c.tickleWriter()
	return nil
}

func (c *PeerConn) peerRequestDataReader(r Request, prs *peerRequestState) {
	b, err := readPeerRequestData(r, c)
	c.locker().Lock()
	defer c.locker().Unlock()
	if err != nil {
		c.peerRequestDataReadFailed(err, r)
	} else {
		if b == nil {
			panic("data must be non-nil to trigger send")
		}
		prs.data = b
		c.tickleWriter()
	}
}

// If this is maintained correctly, we might be able to support optional synchronous reading for
// chunk sending, the way it used to work.
func (c *PeerConn) peerRequestDataReadFailed(err error, r Request) {
	c.logger.WithDefaultLevel(log.Warning).Printf("error reading chunk for peer Request %v: %v", r, err)
	i := pieceIndex(r.Index)
	if c.t.pieceComplete(i) {
		// There used to be more code here that just duplicated the following break. Piece
		// completions are currently cached, so I'm not sure how helpful this update is, except to
		// pull any completion changes pushed to the storage backend in failed reads that got us
		// here.
		c.t.updatePieceCompletion(i)
	}
	// If we failed to send a chunk, choke the peer to ensure they flush all their requests. We've
	// probably dropped a piece from storage, but there's no way to communicate this to the peer. If
	// they ask for it again, we'll kick them to allow us to send them an updated bitfield on the
	// next connect. TODO: Support rejecting here too.
	if c.choking {
		c.logger.WithDefaultLevel(log.Warning).Printf("already choking peer, requests might not be rejected correctly")
	}
	c.choke(c.write)
}

func readPeerRequestData(r Request, c *PeerConn) ([]byte, error) {
	b := make([]byte, r.Length)
	p := c.t.info.Piece(int(r.Index))
	n, err := c.t.readAt(b, p.Offset()+int64(r.Begin))
	if n == len(b) {
		if err == io.EOF {
			err = nil
		}
	} else {
		if err == nil {
			panic("expected error")
		}
	}
	return b, err
}

func runSafeExtraneous(f func()) {
	if true {
		go f()
	} else {
		f()
	}
}

// Processes incoming BitTorrent wire-protocol messages. The client lock is held upon entry and
// exit. Returning will end the connection.
func (c *PeerConn) mainReadLoop() (err error) {
	defer func() {
		if err != nil {
			torrent.Add("connection.mainReadLoop returned with error", 1)
		} else {
			torrent.Add("connection.mainReadLoop returned with no error", 1)
		}
	}()
	t := c.t
	cl := t.cl

	decoder := pp.Decoder{
		R:         bufio.NewReaderSize(c.r, 1<<17),
		MaxLength: 256 * 1024,
		Pool:      t.chunkPool,
	}
	for {
		var msg pp.Message
		func() {
			cl.unlock()
			defer cl.lock()
			err = decoder.Decode(&msg)
		}()
		if cb := c.callbacks.ReadMessage; cb != nil && err == nil {
			cb(c, &msg)
		}
		if t.closed.IsSet() || c.closed.IsSet() {
			return nil
		}
		if err != nil {
			return err
		}
		c.lastMessageReceived = time.Now()
		if msg.Keepalive {
			receivedKeepalives.Add(1)
			continue
		}
		messageTypesReceived.Add(msg.Type.String(), 1)
		if msg.Type.FastExtension() && !c.fastEnabled() {
			runSafeExtraneous(func() { torrent.Add("fast messages received when extension is disabled", 1) })
			return fmt.Errorf("received fast extension message (type=%v) but extension is disabled", msg.Type)
		}
		switch msg.Type {
		case pp.Choke:
			c.peerChoking = true
			if !c.fastEnabled() {
				c.deleteAllRequests()
			}
			// We can then reset our interest.
			c.updateRequests()
			c.updateExpectingChunks()
		case pp.Unchoke:
			c.peerChoking = false
			c.tickleWriter()
			c.updateExpectingChunks()
		case pp.Interested:
			c.peerInterested = true
			c.tickleWriter()
		case pp.NotInterested:
			c.peerInterested = false
			// We don't clear their requests since it isn't clear in the spec.
			// We'll probably choke them for this, which will clear them if
			// appropriate, and is clearly specified.
		case pp.Have:
			err = c.peerSentHave(pieceIndex(msg.Index))
		case pp.Bitfield:
			err = c.peerSentBitfield(msg.Bitfield)
		case pp.Request:
			r := newRequestFromMessage(&msg)
			err = c.onReadRequest(r)
		case pp.Piece:
			c.doChunkReadStats(int64(len(msg.Piece)))
			err = c.receiveChunk(&msg)
			if len(msg.Piece) == int(t.chunkSize) {
				t.chunkPool.Put(&msg.Piece)
			}
			if err != nil {
				err = fmt.Errorf("receiving chunk: %s", err)
			}
		case pp.Cancel:
			req := newRequestFromMessage(&msg)
			c.onPeerSentCancel(req)
		case pp.Port:
			ipa, ok := tryIpPortFromNetAddr(c.RemoteAddr)
			if !ok {
				break
			}
			pingAddr := net.UDPAddr{
				IP:   ipa.IP,
				Port: ipa.Port,
			}
			if msg.Port != 0 {
				pingAddr.Port = int(msg.Port)
			}
			cl.eachDhtServer(func(s DhtServer) {
				go s.Ping(&pingAddr)
			})
		case pp.Suggest:
			torrent.Add("suggests received", 1)
			log.Fmsg("peer suggested piece %d", msg.Index).AddValues(c, msg.Index).SetLevel(log.Debug).Log(c.t.logger)
			c.updateRequests()
		case pp.HaveAll:
			err = c.onPeerSentHaveAll()
		case pp.HaveNone:
			err = c.peerSentHaveNone()
		case pp.Reject:
			c.remoteRejectedRequest(newRequestFromMessage(&msg))
		case pp.AllowedFast:
			torrent.Add("allowed fasts received", 1)
			log.Fmsg("peer allowed fast: %d", msg.Index).AddValues(c).SetLevel(log.Debug).Log(c.t.logger)
			c.peerAllowedFast.Add(bitmap.BitIndex(msg.Index))
			c.updateRequests()
		case pp.Extended:
			err = c.onReadExtendedMsg(msg.ExtendedID, msg.ExtendedPayload)
		default:
			err = fmt.Errorf("received unknown message type: %#v", msg.Type)
		}
		if err != nil {
			return err
		}
	}
}

func (c *Peer) remoteRejectedRequest(r Request) {
	if c.deleteRequest(r) {
		c.decExpectedChunkReceive(r)
	}
}

func (c *Peer) decExpectedChunkReceive(r Request) {
	count := c.validReceiveChunks[r]
	if count == 1 {
		delete(c.validReceiveChunks, r)
	} else if count > 1 {
		c.validReceiveChunks[r] = count - 1
	} else {
		panic(r)
	}
}

func (c *PeerConn) onReadExtendedMsg(id pp.ExtensionNumber, payload []byte) (err error) {
	defer func() {
		// TODO: Should we still do this?
		if err != nil {
			// These clients use their own extension IDs for outgoing message
			// types, which is incorrect.
			if bytes.HasPrefix(c.PeerID[:], []byte("-SD0100-")) || strings.HasPrefix(string(c.PeerID[:]), "-XL0012-") {
				err = nil
			}
		}
	}()
	t := c.t
	cl := t.cl
	switch id {
	case pp.HandshakeExtendedID:
		var d pp.ExtendedHandshakeMessage
		if err := bencode.Unmarshal(payload, &d); err != nil {
			c.logger.Printf("error parsing extended handshake message %q: %s", payload, err)
			return fmt.Errorf("unmarshalling extended handshake payload: %w", err)
		}
		if cb := c.callbacks.ReadExtendedHandshake; cb != nil {
			cb(c, &d)
		}
		//c.logger.WithDefaultLevel(log.Debug).Printf("received extended handshake message:\n%s", spew.Sdump(d))
		if d.Reqq != 0 {
			c.PeerMaxRequests = d.Reqq
		}
		c.PeerClientName = d.V
		if c.PeerExtensionIDs == nil {
			c.PeerExtensionIDs = make(map[pp.ExtensionName]pp.ExtensionNumber, len(d.M))
		}
		c.PeerListenPort = d.Port
		c.PeerPrefersEncryption = d.Encryption
		for name, id := range d.M {
			if _, ok := c.PeerExtensionIDs[name]; !ok {
				peersSupportingExtension.Add(string(name), 1)
			}
			c.PeerExtensionIDs[name] = id
		}
		if d.MetadataSize != 0 {
			if err = t.setMetadataSize(d.MetadataSize); err != nil {
				return fmt.Errorf("setting metadata size to %d: %w", d.MetadataSize, err)
			}
		}
		c.requestPendingMetadata()
		if !t.cl.config.DisablePEX {
			t.pex.Add(c) // we learnt enough now
			c.pex.Init(c)
		}
		return nil
	case metadataExtendedId:
		err := cl.gotMetadataExtensionMsg(payload, t, c)
		if err != nil {
			return fmt.Errorf("handling metadata extension message: %w", err)
		}
		return nil
	case pexExtendedId:
		if !c.pex.IsEnabled() {
			return nil // or hang-up maybe?
		}
		return c.pex.Recv(payload)
	default:
		return fmt.Errorf("unexpected extended message ID: %v", id)
	}
}

// Set both the Reader and Writer for the connection from a single ReadWriter.
func (cn *PeerConn) setRW(rw io.ReadWriter) {
	cn.r = rw
	cn.w = rw
}

// Returns the Reader and Writer as a combined ReadWriter.
func (cn *PeerConn) rw() io.ReadWriter {
	return struct {
		io.Reader
		io.Writer
	}{cn.r, cn.w}
}

func (c *Peer) doChunkReadStats(size int64) {
	c.allStats(func(cs *ConnStats) { cs.receivedChunk(size) })
}

// Handle a received chunk from a peer.
func (c *Peer) receiveChunk(msg *pp.Message) error {
	chunksReceived.Add("total", 1)

	req := newRequestFromMessage(msg)

	if c.peerChoking {
		chunksReceived.Add("while choked", 1)
	}

	if c.validReceiveChunks[req] <= 0 {
		chunksReceived.Add("unexpected", 1)
		return errors.New("received unexpected chunk")
	}
	c.decExpectedChunkReceive(req)

	if c.peerChoking && c.peerAllowedFast.Get(bitmap.BitIndex(req.Index)) {
		chunksReceived.Add("due to allowed fast", 1)
	}

	// The request needs to be deleted immediately to prevent cancels occurring asynchronously when
	// have actually already received the piece, while we have the Client unlocked to write the data
	// out.
	deletedRequest := false
	{
		if _, ok := c.actualRequestState.Requests[req]; ok {
			for _, f := range c.callbacks.ReceivedRequested {
				f(PeerMessageEvent{c, msg})
			}
		}
		// Request has been satisfied.
		if c.deleteRequest(req) {
			deletedRequest = true
			if !c.peerChoking {
				c._chunksReceivedWhileExpecting++
			}
		} else {
			chunksReceived.Add("unwanted", 1)
		}
	}

	t := c.t
	cl := t.cl

	// Do we actually want this chunk?
	if t.haveChunk(req) {
		chunksReceived.Add("wasted", 1)
		c.allStats(add(1, func(cs *ConnStats) *Count { return &cs.ChunksReadWasted }))
		return nil
	}

	piece := &t.pieces[req.Index]

	c.allStats(add(1, func(cs *ConnStats) *Count { return &cs.ChunksReadUseful }))
	c.allStats(add(int64(len(msg.Piece)), func(cs *ConnStats) *Count { return &cs.BytesReadUsefulData }))
	if deletedRequest {
		c.piecesReceivedSinceLastRequestUpdate++
		c.updateRequests()
		c.allStats(add(int64(len(msg.Piece)), func(cs *ConnStats) *Count { return &cs.BytesReadUsefulIntendedData }))
	}
	for _, f := range c.t.cl.config.Callbacks.ReceivedUsefulData {
		f(ReceivedUsefulDataEvent{c, msg})
	}
	c.lastUsefulChunkReceived = time.Now()

	// Need to record that it hasn't been written yet, before we attempt to do
	// anything with it.
	piece.incrementPendingWrites()
	// Record that we have the chunk, so we aren't trying to download it while
	// waiting for it to be written to storage.
	piece.unpendChunkIndex(chunkIndex(req.ChunkSpec, t.chunkSize))

	// Cancel pending requests for this chunk from *other* peers.
	t.iterPeers(func(p *Peer) {
		if p == c {
			return
		}
		p.cancel(req)
	})

	err := func() error {
		cl.unlock()
		defer cl.lock()
		concurrentChunkWrites.Add(1)
		defer concurrentChunkWrites.Add(-1)
		// Write the chunk out. Note that the upper bound on chunk writing concurrency will be the
		// number of connections. We write inline with receiving the chunk (with this lock dance),
		// because we want to handle errors synchronously and I haven't thought of a nice way to
		// defer any concurrency to the storage and have that notify the client of errors. TODO: Do
		// that instead.
		return t.writeChunk(int(msg.Index), int64(msg.Begin), msg.Piece)
	}()

	piece.decrementPendingWrites()

	if err != nil {
		c.logger.WithDefaultLevel(log.Error).Printf("writing received chunk %v: %v", req, err)
		t.pendRequest(req)
		//t.updatePieceCompletion(pieceIndex(msg.Index))
		t.onWriteChunkErr(err)
		return nil
	}

	c.onDirtiedPiece(pieceIndex(req.Index))

	// We need to ensure the piece is only queued once, so only the last chunk writer gets this job.
	if t.pieceAllDirty(pieceIndex(req.Index)) && piece.pendingWrites == 0 {
		t.queuePieceCheck(pieceIndex(req.Index))
		// We don't pend all chunks here anymore because we don't want code dependent on the dirty
		// chunk status (such as the haveChunk call above) to have to check all the various other
		// piece states like queued for hash, hashing etc. This does mean that we need to be sure
		// that chunk pieces are pended at an appropriate time later however.
	}

	cl.event.Broadcast()
	// We do this because we've written a chunk, and may change PieceState.Partial.
	t.publishPieceChange(pieceIndex(req.Index))

	return nil
}

func (c *Peer) onDirtiedPiece(piece pieceIndex) {
	if c.peerTouchedPieces == nil {
		c.peerTouchedPieces = make(map[pieceIndex]struct{})
	}
	c.peerTouchedPieces[piece] = struct{}{}
	ds := &c.t.pieces[piece].dirtiers
	if *ds == nil {
		*ds = make(map[*Peer]struct{})
	}
	(*ds)[c] = struct{}{}
}

func (c *PeerConn) uploadAllowed() bool {
	if c.t.cl.config.NoUpload {
		return false
	}
	if c.t.dataUploadDisallowed {
		return false
	}
	if c.t.seeding() {
		return true
	}
	if !c.peerHasWantedPieces() {
		return false
	}
	// Don't upload more than 100 KiB more than we download.
	if c._stats.BytesWrittenData.Int64() >= c._stats.BytesReadData.Int64()+100<<10 {
		return false
	}
	return true
}

func (c *PeerConn) setRetryUploadTimer(delay time.Duration) {
	if c.uploadTimer == nil {
		c.uploadTimer = time.AfterFunc(delay, c.tickleWriter)
	} else {
		c.uploadTimer.Reset(delay)
	}
}

// Also handles choking and unchoking of the remote peer.
func (c *PeerConn) upload(msg func(pp.Message) bool) bool {
	// Breaking or completing this loop means we don't want to upload to the
	// peer anymore, and we choke them.
another:
	for c.uploadAllowed() {
		// We want to upload to the peer.
		if !c.unchoke(msg) {
			return false
		}
		for r, state := range c.peerRequests {
			if state.data == nil {
				continue
			}
			res := c.t.cl.config.UploadRateLimiter.ReserveN(time.Now(), int(r.Length))
			if !res.OK() {
				panic(fmt.Sprintf("upload rate limiter burst size < %d", r.Length))
			}
			delay := res.Delay()
			if delay > 0 {
				res.Cancel()
				c.setRetryUploadTimer(delay)
				// Hard to say what to return here.
				return true
			}
			more := c.sendChunk(r, msg, state)
			delete(c.peerRequests, r)
			if !more {
				return false
			}
			goto another
		}
		return true
	}
	return c.choke(msg)
}

func (cn *PeerConn) drop() {
	cn.t.dropConnection(cn)
}

func (cn *Peer) netGoodPiecesDirtied() int64 {
	return cn._stats.PiecesDirtiedGood.Int64() - cn._stats.PiecesDirtiedBad.Int64()
}

func (c *Peer) peerHasWantedPieces() bool {
	return !c._pieceRequestOrder.IsEmpty()
}

func (c *Peer) numLocalRequests() int {
	return len(c.actualRequestState.Requests)
}

func (c *Peer) deleteRequest(r Request) bool {
	delete(c.nextRequestState.Requests, r)
	if _, ok := c.actualRequestState.Requests[r]; !ok {
		return false
	}
	delete(c.actualRequestState.Requests, r)
	for _, f := range c.callbacks.DeletedRequest {
		f(PeerRequestEvent{c, r})
	}
	c.updateExpectingChunks()
	pr := c.t.pendingRequests
	pr[r]--
	n := pr[r]
	if n == 0 {
		delete(pr, r)
	}
	if n < 0 {
		panic(n)
	}
	return true
}

func (c *Peer) deleteAllRequests() {
	for r := range c.actualRequestState.Requests {
		c.deleteRequest(r)
	}
	if l := len(c.actualRequestState.Requests); l != 0 {
		panic(l)
	}
	c.nextRequestState.Requests = nil
	// for c := range c.t.conns {
	// 	c.tickleWriter()
	// }
}

// This is called when something has changed that should wake the writer, such as putting stuff into
// the writeBuffer, or changing some state that the writer can act on.
func (c *PeerConn) tickleWriter() {
	c.messageWriter.writeCond.Broadcast()
}

func (c *PeerConn) sendChunk(r Request, msg func(pp.Message) bool, state *peerRequestState) (more bool) {
	c.lastChunkSent = time.Now()
	return msg(pp.Message{
		Type:  pp.Piece,
		Index: r.Index,
		Begin: r.Begin,
		Piece: state.data,
	})
}

func (c *PeerConn) setTorrent(t *Torrent) {
	if c.t != nil {
		panic("connection already associated with a torrent")
	}
	c.t = t
	c.logger.WithDefaultLevel(log.Debug).Printf("set torrent=%v", t)
	t.reconcileHandshakeStats(c)
}

func (c *Peer) peerPriority() (peerPriority, error) {
	return bep40Priority(c.remoteIpPort(), c.t.cl.publicAddr(c.remoteIp()))
}

func (c *Peer) remoteIp() net.IP {
	host, _, _ := net.SplitHostPort(c.RemoteAddr.String())
	return net.ParseIP(host)
}

func (c *Peer) remoteIpPort() IpPort {
	ipa, _ := tryIpPortFromNetAddr(c.RemoteAddr)
	return IpPort{ipa.IP, uint16(ipa.Port)}
}

func (c *PeerConn) pexPeerFlags() pp.PexPeerFlags {
	f := pp.PexPeerFlags(0)
	if c.PeerPrefersEncryption {
		f |= pp.PexPrefersEncryption
	}
	if c.outgoing {
		f |= pp.PexOutgoingConn
	}
	if c.utp() {
		f |= pp.PexSupportsUtp
	}
	return f
}

// This returns the address to use if we want to dial the peer again. It incorporates the peer's
// advertised listen port.
func (c *PeerConn) dialAddr() PeerRemoteAddr {
	if !c.outgoing && c.PeerListenPort != 0 {
		switch addr := c.RemoteAddr.(type) {
		case *net.TCPAddr:
			dialAddr := *addr
			dialAddr.Port = c.PeerListenPort
			return &dialAddr
		case *net.UDPAddr:
			dialAddr := *addr
			dialAddr.Port = c.PeerListenPort
			return &dialAddr
		}
	}
	return c.RemoteAddr
}

func (c *PeerConn) pexEvent(t pexEventType) pexEvent {
	f := c.pexPeerFlags()
	addr := c.dialAddr()
	return pexEvent{t, addr, f}
}

func (c *PeerConn) String() string {
	return fmt.Sprintf("connection %p", c)
}

func (c *Peer) trust() connectionTrust {
	return connectionTrust{c.trusted, c.netGoodPiecesDirtied()}
}

type connectionTrust struct {
	Implicit            bool
	NetGoodPiecesDirted int64
}

func (l connectionTrust) Less(r connectionTrust) bool {
	return multiless.New().Bool(l.Implicit, r.Implicit).Int64(l.NetGoodPiecesDirted, r.NetGoodPiecesDirted).Less()
}

// Returns the pieces the peer could have based on their claims. If we don't know how many pieces
// are in the torrent, it could be a very large range the peer has sent HaveAll.
func (cn *PeerConn) PeerPieces() bitmap.Bitmap {
	cn.locker().RLock()
	defer cn.locker().RUnlock()
	return cn.newPeerPieces()
}

// Returns a new Bitmap that includes bits for all pieces the peer could have based on their claims.
func (cn *Peer) newPeerPieces() bitmap.Bitmap {
	ret := cn._peerPieces.Copy()
	if cn.peerSentHaveAll {
		if cn.t.haveInfo() {
			ret.AddRange(0, bitmap.BitRange(cn.t.numPieces()))
		} else {
			ret.AddRange(0, bitmap.ToEnd)
		}
	}
	return ret
}

func (cn *Peer) stats() *ConnStats {
	return &cn._stats
}

func (p *Peer) TryAsPeerConn() (*PeerConn, bool) {
	pc, ok := p.peerImpl.(*PeerConn)
	return pc, ok
}

func (p *PeerConn) onNextRequestStateChanged() {
	p.tickleWriter()
}
