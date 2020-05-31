package torrent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/torrent/storage"
)

const (
	maxOnFlight = 20
	//the size could be t.reqq?
	readCSize = 250
	sendCSize = 10
	recvCSize = sendCSize
)

const (
	keepAliveInterval time.Duration = 2 * time.Minute
)

//conn represents a peer connection.
//This is controled by worker goroutines.
//TODO: peer_wire.Msg should be passed through pointers everywhere
type conn struct {
	cl     *Client
	t      *Torrent
	logger *log.Logger
	//tcp connection with peer
	cn      net.Conn
	bwriter *bufio.Writer
	peer    Peer
	exts    peer_wire.Extensions
	//main goroutine also has this state - needs to be synced between
	//the two goroutines
	state connState
	//we receive messages from Torrent on this channel.
	recvC chan interface{}
	//we send messages to Torrent from this channel.
	sendC chan interface{}
	//chan to signal that conn is dropped
	droppedC chan struct{}
	//the last message a conn may be willing to set to Torrent.
	lastMsg interface{}
	//whether we should black list the peer after close of connection
	ban            bool
	keepAliveTimer *time.Timer
	peerReqs       map[block]struct{}
	onFlightReqs   map[block]struct{}
	muPeerReqs     sync.Mutex
	haveInfo       bool

	numMetadataRespSent   int
	metadataRequestTicker *time.Ticker
	//TODO: change to map
	metadataRejectedBlocks []int
	metadataPendingBlocks  map[int]struct{}
	metadataSize           int

	//
	debugVerifications int
	//The number of outstanding request messages this peer supports
	//without dropping any. The default in in libtorrent is 250.
	peerReqq int
	reserved peer_wire.Reserved
	peerBf   bitmap.Bitmap
	myBf     bitmap.Bitmap
	peerID   []byte
}

//just wraps a msg with an error
func newConn(t *Torrent, cn net.Conn, peer Peer, reserved peer_wire.Reserved) *conn {
	logPrefix := t.cl.logger.Prefix() + "CN"
	if peer.P.ID != nil {
		logPrefix += fmt.Sprintf("%x ", peer.P.ID[14:])
	} else {
		logPrefix += "------ "
	}
	return &conn{
		cl:                     t.cl,
		t:                      t,
		logger:                 log.New(t.cl.logger.Writer(), logPrefix, log.LstdFlags),
		cn:                     cn,
		bwriter:                bufio.NewWriter(cn),
		peer:                   peer,
		state:                  newConnState(),
		recvC:                  make(chan interface{}, recvCSize),
		sendC:                  make(chan interface{}, sendCSize),
		droppedC:               make(chan struct{}),
		onFlightReqs:           make(map[block]struct{}),
		peerBf:                 bitmap.Bitmap{RB: roaring.NewBitmap()},
		peerReqs:               make(map[block]struct{}),
		reserved:               reserved,
		metadataRequestTicker:  new(time.Ticker),
		metadataRejectedBlocks: make([]int, 0),
		metadataPendingBlocks:  make(map[int]struct{}, 0),
	}
}

//inform Torrent about the new connection
func (c *conn) informTorrent() error {
	select {
	case c.t.newConnC <- c.connInfo():
		return nil
	case <-c.t.dropC:
		return errors.New("torrent closed")
	case <-time.After(time.Minute):
		c.cl.logger.Println("conn: couldn't inform torrent about the connection")
		return errors.New("inform timeout")
	}
}

func (c *conn) connInfo() *connInfo {
	return &connInfo{
		t: c.t,
		// Torrent should see c.recvC as its send channel and c.sendC as its receive channel.
		sendC:    c.recvC,
		recvC:    c.sendC,
		droppedC: c.droppedC,
		peer:     c.peer,
		reserved: c.reserved,
		state:    c.state,
	}
}

func (c *conn) close() {
	c.discardBlocks(false, false)
	//notify Torrent that conn closed
	close(c.droppedC)
	//because we closed `dropped`, there is no deadlock possibility
	select {
	case <-c.t.dropC:
	case c.sendC <- c.lastMsg:
	}
	select {
	case <-c.t.dropC:
	case c.sendC <- discardedRequests{}:
	}
	select {
	case <-c.t.dropC:
	case c.sendC <- connDroped{}:
	}
	//close chan - avoid leak goroutines
	close(c.sendC)
}

type readResult struct {
	msg *peer_wire.Msg
	err error
}

func (c *conn) mainLoop() error {
	var err error
	readC := make(chan *peer_wire.Msg, readCSize)
	readErrC := make(chan error, 1)
	//we need o quit chan in order to not leak readMsgs goroutine
	quit := make(chan struct{})
	defer func() {
		/*if r := recover(); r != nil {
			//c.logger.Println(r)
			panic(r)
		}*/
		close(quit)
		c.close()
		err = nilOnEOF(err)
	}()
	go c.readPeerMsgs(readC, quit, readErrC)
	c.keepAliveTimer = time.NewTimer(keepAliveInterval)
	//debugTick := time.NewTicker(2 * time.Second)
	//defer debugTick.Stop()
	for {
		select {
		case cmd := <-c.recvC:
			err = c.onTorrentMsg(cmd)
		case msg := <-readC:
			err = c.onPeerMsg(msg)
		case err = <-readErrC:
		case <-c.t.dropC:
			return nil
		case <-c.keepAliveTimer.C:
			err = c.sendKeepAlive()
		case <-c.metadataRequestTicker.C:
			if !c.haveInfo {
				c.metadataRejectedBlocks = make([]int, 0)
				err = c.requestMetadata()
			} else {
				c.metadataRequestTicker.Stop()
			}

		}
		if err != nil {
			return err
		}
		if err = c.flushWriter(); err != nil {
			return err
		}
	}
}

func nilOnEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

//read msgs from remote peer
//run on seperate goroutine
func (c *conn) readPeerMsgs(readC chan<- *peer_wire.Msg, quit chan struct{},
	errC chan error) {
	for {
		c.cn.SetReadDeadline(time.Now().Add(keepAliveInterval + time.Minute)) //lets be forbearing
		msg, err := peer_wire.Decode(c.cn)
		if err != nil {
			errC <- err
			return
		}
		c.cl.counters.Add(fmt.Sprintf("%s received", msg.Kind.String()), 1)
		var sendToChan bool
		switch msg.Kind {
		case peer_wire.Request:
			b := reqMsgToBlock(msg)
			c.muPeerReqs.Lock()
			//avoid flooding readCh by not sending requests that are
			//already requested from peer and by discarding requests when
			//our buffer is full.
			switch _, ok := c.peerReqs[b]; {
			case ok:
				c.cl.counters.Add("duplicateRequestsReceived", 1)
			case len(c.peerReqs) >= c.t.reqq:
				//should we drop?
				c.logger.Print("peer requests buffer is full\n")
			default: //all good
				c.peerReqs[b] = struct{}{}
				sendToChan = true
			}
			c.muPeerReqs.Unlock()
		case peer_wire.Cancel:
			b := reqMsgToBlock(msg)
			c.muPeerReqs.Lock()
			if _, ok := c.peerReqs[b]; ok {
				delete(c.peerReqs, b)
			} else {
				//these are cancels that peers send to us but it was too late because
				//we had already processed the request
				c.cl.counters.Add("latecomerCancels", 1)
			}
			c.muPeerReqs.Unlock()
		default:
			sendToChan = true
		}
		if sendToChan {
			select {
			case readC <- msg:
			case <-quit: //we must care for quit here
				return
			}
		}
	}
}

func (c *conn) wantBlocks() bool {
	return !c.amSeeding() && c.haveInfo && c.state.canDownload() &&
		c.peerBf.Len() > 0 && len(c.onFlightReqs) < maxOnFlight/2
}

func (c *conn) maybeSendRequests() error {
	if !c.wantBlocks() {
		return nil
	}
	sz := maxOnFlight - len(c.onFlightReqs)
	if sz <= 0 {
		panic("on flight queue is full")
	}
	requests := make([]block, sz)
	n := c.t.pieces.fillRequests(c.peerBf, requests)
	if n == 0 {
		if (requests[0] != block{}) {
			panic("send requests")
		}
		c.cl.counters.Add("nonUsefulRequestReads", 1)
		return nil
	}
	requests = requests[:n]
	for _, req := range requests {
		if _, ok := c.onFlightReqs[req]; ok {
			continue
		}
		c.onFlightReqs[req] = struct{}{}
		if err := c.sendMsgToPeer(req.reqMsg(), false); err != nil {
			return err
		}
	}
	return nil
}

//returns false if its worth to keep the conn alive
func (c *conn) notUseful() bool {
	if !c.haveInfo {
		return false
	}
	return c.peerSeeding() && c.amSeeding()
}

func (c *conn) peerSeeding() bool {
	if !c.haveInfo {
		panic("require info")
	}
	return c.peerBf.Len() == c.t.numPieces()
}

func (c *conn) amSeeding() bool {
	if !c.haveInfo {
		return false
	}
	return c.myBf.Len() == c.t.numPieces()
}

func (c *conn) sendKeepAlive() error {
	return c.sendMsgToPeer(&peer_wire.Msg{
		Kind: peer_wire.KeepAlive,
	}, false)
}

func (c *conn) onTorrentMsg(cmd interface{}) (err error) {
	switch v := cmd.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Port:
		case peer_wire.Interested:
			c.state.amInterested = true
			defer func() {
				err = c.maybeSendRequests()
			}()
		case peer_wire.NotInterested:
			c.state.amInterested = false
			//due to inconsistent states between Torrent and conn we may have requests on flight
			//which we'll be lost (hopefully I think this doesn't happen too much)
			c.cl.counters.Add("lostBlocksDueToSync", int64(len(c.onFlightReqs)))
			err = c.discardBlocks(true, true)
			if err != nil {
				return err
			}
		case peer_wire.Choke:
			c.state.amChoking = true
			c.muPeerReqs.Lock()
			c.peerReqs = make(map[block]struct{})
			c.muPeerReqs.Unlock()
		case peer_wire.Unchoke:
			c.state.amChoking = false
		case peer_wire.Have:
			c.myBf.Set(int(v.Index), true)
			if c.notUseful() {
				err = io.EOF
				return
			}
			//Surpress have msgs.
			//From https://wiki.theory.org/index.php/BitTorrentSpecification:
			//At the same time, it may be worthwhile to send a HAVE message
			//to a peer that has that piece already since it will be useful in
			//determining which piece is rare.
			if c.peerSeeding() || (c.peerBf.Get(int(v.Index)) && flipCoin()) {
				return
			}
		case peer_wire.Cancel:
			if _, ok := c.onFlightReqs[reqMsgToBlock(v)]; !ok {
				return
			}
		default:
			panic("unknown msg type")
		}
		err = c.sendMsgToPeer(v, false)
	case bitmap.Bitmap: //this equivalent to bitfield, we encode and write to wire.
		c.myBf = v
		if c.notUseful() {
			err = io.EOF
			return
		}
		err = c.sendMsgToPeer(&peer_wire.Msg{
			Kind: peer_wire.Bitfield,
			Bf:   c.bitfield(c.myBf),
		}, false)
	case requestsAvailable:
		if c.haveInfo {
			err = c.maybeSendRequests()
		} else {
			err = c.requestMetadata()
		}
	case haveInfo:
		c.haveInfo = true
		if c.peerBf.Len() > 0 {
			c.t.pieces.onBitfield(c.peerBf)
		}
		err = c.maybeSendRequests()
	case drop:
		err = io.EOF
	}
	return
}

func (c *conn) sendMsgToPeer(msg *peer_wire.Msg, mustFlush bool) error {
	limit := 1 << 16 //64KiB
	n, err := c.bwriter.Write(msg.Encode())
	if n > 0 {
		c.cl.counters.Add(fmt.Sprintf("%s sent", msg.Kind.String()), 1)
	}
	if err != nil {
		return err
	}
	if c.bwriter.Buffered() > limit || mustFlush {
		return c.flushWriter()
	}
	return nil
}

func (c *conn) flushWriter() error {
	if c.bwriter.Buffered() <= 0 {
		return nil
	}
	if c.keepAliveTimer != nil {

		if !c.keepAliveTimer.Stop() {
			select {
			case <-c.keepAliveTimer.C:
			default:
			}
		}
		c.keepAliveTimer.Reset(keepAliveInterval)
	}
	return c.bwriter.Flush()
}

func (c *conn) onPeerMsg(msg *peer_wire.Msg) (err error) {
	changeState := func(currState *bool, futureState bool) error {
		if *currState == futureState {
			return nil
		}
		*currState = futureState
		return c.sendMsgToTorrent(msg)
	}
	switch msg.Kind {
	case peer_wire.KeepAlive:
	case peer_wire.Interested:
		err = changeState(&c.state.isInterested, true)
	case peer_wire.NotInterested:
		err = changeState(&c.state.isInterested, false)
	case peer_wire.Choke:
		err = c.discardBlocks(true, false)
		if err != nil {
			return
		}
		err = changeState(&c.state.isChoking, true)
	case peer_wire.Unchoke:
		err = changeState(&c.state.isChoking, false)
		if err != nil {
			return err
		}
		err = c.maybeSendRequests()
	case peer_wire.Piece:
		err = c.onPieceMsg(msg)
	case peer_wire.Request:
		err = c.upload()
	case peer_wire.Have:
		if c.peerBf.Get(int(msg.Index)) {
			//c.logger.Printf("peer send duplicate Have msg of piece %d\n", msg.Index)
			err = errors.New("peer send duplicate Have msg of piece")
			return
		}
		c.peerBf.Set(int(msg.Index), true)
		if c.notUseful() {
			err = io.EOF
			return
		}
		if c.haveInfo {
			c.t.pieces.onHave(int(msg.Index))
		}
		err = c.sendMsgToTorrent(msg)
	case peer_wire.Bitfield:
		if !c.peerBf.IsEmpty() {
			err = errors.New("peer: send bitfield twice or have before bitfield")
			return
		}
		var peerBfPtr *bitmap.Bitmap
		if peerBfPtr, err = c.bitmap(msg.Bf); err != nil {
			return
		}
		c.peerBf = *peerBfPtr
		if c.notUseful() {
			err = io.EOF
			return
		}
		if c.haveInfo {
			c.t.pieces.onBitfield(c.peerBf)
		}
		err = c.sendMsgToTorrent(c.peerBf.Copy())
	case peer_wire.Extended:
		c.onExtended(msg)
	case peer_wire.Port:
		pingAddr, err := net.ResolveUDPAddr("udp", c.cn.RemoteAddr().String())
		if err != nil {
			panic(err)
		}
		if msg.Port != 0 {
			pingAddr.Port = int(msg.Port)
		}
		if c.t.cl.dhtServer != nil {
			go c.t.cl.dhtServer.Ping(pingAddr, nil)
		}
	default:
		err = errors.New("unknown msg kind received")
	}
	return
}

func (c *conn) sendMsgToTorrent(e interface{}) error {
	var err error
	for {
		//we dont just send e to eventCh because if the former chan is full,
		//then commandCh may also become full and we will deadlock.
		select {
		case c.sendC <- e:
			return nil
		default:
			//receive command while waiting for event to be send, we dont want
			//to deadlock.
			select {
			case cmd := <-c.recvC:
				err = c.onTorrentMsg(cmd)
			case c.sendC <- e:
				return nil
			case <-c.t.dropC:
				//pretend we lost connection
				err = io.EOF
			case <-c.keepAliveTimer.C:
				err = c.sendKeepAlive()
			}
		}
		if err != nil {
			//save the msg we couldn't send to conn.
			c.lastMsg = e
			return err
		}
	}
}

func (c *conn) bitfield(bm bitmap.Bitmap) peer_wire.BitField {
	bf := peer_wire.NewBitField(c.t.numPieces())
	bm.IterTyped(func(piece int) bool {
		bf.SetPiece(piece)
		return true
	})
	return bf
}

func (c *conn) bitmap(bf peer_wire.BitField) (*bitmap.Bitmap, error) {
	var bm bitmap.Bitmap
	if c.haveInfo && !bf.Valid(c.t.numPieces()) {
		c.ban = true
		return nil, errors.New("bf length is not valid")
	}
	for i := 0; i < len(bf)*8; i++ {
		if bf.HasPiece(i) {
			bm.Set(i, true)
		}
	}
	return &bm, nil
}

//in order for this function to work correctly,no values should
//be ever sent at `ch`.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (c *conn) remoteTCPAddr() (addr *net.TCPAddr) {
	var err error
	if addr, err = net.ResolveTCPAddr("tcp", c.cn.RemoteAddr().String()); err != nil {
		panic(err)
	}
	return
}

type metainfoSize int64

func (c *conn) onExtended(msg *peer_wire.Msg) (err error) {
	if !c.cl.reserved.SupportExtended() || !c.reserved.SupportExtended() {
		err = errors.New("received ext while not supporting it")
	}
	switch v := msg.ExtendedMsg.(type) {
	case peer_wire.ExtHandshakeDict:
		c.exts, err = v.Extensions()
		if err != nil {
			return
		}
		if _, ok := c.exts[peer_wire.ExtMetadataName]; ok {
			if msize, ok := v.MetadataSize(); ok {
				c.metadataSize = int(msize)
				if c.t.utmetadata.add(msize) {
					err = c.requestMetadata()
				} else {
					c.ban = true
					err = io.EOF
				}
			}
		}
	case peer_wire.MetadataExtMsg:
		if c.exts == nil {
			err = errors.New("haven't received ext handshake yet")
			return
		}
		switch v.Kind {
		case peer_wire.MetadataDataID:
			var completed bool
			if v.TotalSz != c.metadataSize {
				err = errors.New("incompatible metadata size")
				return
			}
			completed, err = c.t.utmetadata.writeBlock(v.Data, v.Piece, v.TotalSz, c.remoteTCPAddr().IP)
			if err != nil {
				return
			}
			if _, ok := c.metadataPendingBlocks[v.Piece]; ok {
				delete(c.metadataPendingBlocks, v.Piece)
			} else {
				c.logger.Println("received unexpected metadata piece")
			}
			if completed {
				err = c.sendMsgToTorrent(downloadedMetadata(c.metadataSize))
			} else {
				err = c.requestMetadata()
			}
		case peer_wire.MetadataRejID:
			c.metadataRejectedBlocks = append(c.metadataRejectedBlocks, v.Piece)
			if _, ok := c.metadataPendingBlocks[v.Piece]; ok {
				delete(c.metadataPendingBlocks, v.Piece)
			} else {
				c.logger.Println("received unexpected metadata piece")
			}
			//maybe after 5 seconds the peer will have the block
			if c.metadataRequestTicker.C == nil {
				c.metadataRequestTicker = time.NewTicker(5 * time.Second)
			}
		case peer_wire.MetadataReqID:
			var extMsg peer_wire.MetadataExtMsg
			ut := c.t.utmetadata.correct()
			if !c.haveInfo || c.numMetadataRespSent > 3*ut.numPieces || ut == nil ||
				v.Piece >= ut.numPieces {
				extMsg = peer_wire.MetadataExtMsg{
					Kind:  peer_wire.MetadataRejID,
					Piece: v.Piece,
				}
			} else {
				b := make([]byte, ut.metadataPieceLength(v.Piece))
				c.t.utmetadata.readBlock(b, v.Piece)
				c.numMetadataRespSent++
				extMsg = peer_wire.MetadataExtMsg{
					Kind:    peer_wire.MetadataDataID,
					Piece:   v.Piece,
					TotalSz: len(ut.infoBytes),
					Data:    b,
				}
			}
			m, err := c.extensionMsg(peer_wire.ExtMetadataName, extMsg)
			if err != nil {
				return err
			}
			err = c.sendMsgToPeer(m, false)
		default:
			//ignore uknown id
		}
	default:
		return fmt.Errorf("unknown extended msg type %T", v)
	}
	return
}

func (c *conn) requestMetadata() error {
	ut := c.t.utmetadata.correct()
	if ut != nil || c.haveInfo {
		return nil
	}
	if c.metadataSize == 0 || c.exts == nil {
		return nil
	}
	if _, ok := c.exts[peer_wire.ExtMetadataName]; !ok {
		return nil
	}
	toRequest := 5 - len(c.metadataPendingBlocks)
	if toRequest <= 0 {
		return nil
	}
	blocks := c.t.utmetadata.fillRequest(c.metadataSize, toRequest, c.metadataRejectedBlocks)
	for _, piece := range blocks {
		msg, err := c.extensionMsg(peer_wire.ExtMetadataName, peer_wire.MetadataExtMsg{
			Kind:  peer_wire.MetadataReqID,
			Piece: piece,
		})
		if err != nil {
			return nil
		}
		if _, ok := c.metadataPendingBlocks[piece]; !ok {
			c.metadataPendingBlocks[piece] = struct{}{}
			err = c.sendMsgToPeer(msg, false)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *conn) extensionMsg(extname peer_wire.ExtensionName, value interface{}) (*peer_wire.Msg, error) {
	id, ok := c.exts[extname]
	if !ok {
		return nil, errors.New("extension not supported")
	}
	return &peer_wire.Msg{
		Kind:        peer_wire.Extended,
		ExtendedID:  id,
		ExtendedMsg: value,
	}, nil
}

func (c *conn) discardBlocks(notifyTorrent, sendCancels bool) error {
	if len(c.onFlightReqs) > 0 {
		unsatisfiedRequests := []block{}
		for req := range c.onFlightReqs {
			if sendCancels {
				if err := c.sendMsgToPeer(req.cancelMsg(), false); err != nil {
					return err
				}
			}
			unsatisfiedRequests = append(unsatisfiedRequests, req)
		}
		c.t.pieces.discardRequests(unsatisfiedRequests)
		c.onFlightReqs = make(map[block]struct{})
		var err error
		if notifyTorrent {
			err = c.sendMsgToTorrent(discardedRequests{})
		}
		return err
	}
	return nil
}

//TODO:Store bad peer so we wont accept them again if they try to reconnect
func (c *conn) upload() error {
	c.muPeerReqs.Lock()
	defer c.muPeerReqs.Unlock()
	for req := range c.peerReqs {
		delete(c.peerReqs, req)
		if !c.haveInfo {
			c.ban = true
			return errors.New("peer send requested and we dont have info dict")
		}
		if !c.state.canUpload() {
			//maybe we have choked the peer, but he hasnt been informed and
			//thats why he send us request, dont drop conn just ignore the req
			c.logger.Print("peer send request msg while choked\n")
			continue
		}
		if req.len > maxRequestBlockSz {
			c.ban = true
			return fmt.Errorf("request length out of range: %d", req.len)
		}
		//check that we have the requested piece
		//dont ban we may have dropped a piece
		if !c.myBf.Get(req.pc) {
			return fmt.Errorf("peer requested piece we do not have")
		}
		//ensure the we dont exceed the end of the piece
		if endOff := req.off + req.len; endOff > c.t.pieceLen(uint32(req.pc)) {
			c.ban = true
			return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
		}
		data := make([]byte, req.len)
		err := func() error {
			c.muPeerReqs.Unlock()
			defer c.muPeerReqs.Lock()
			if err := c.t.readBlock(data, req.pc, req.off); err != nil {
				return nil
			}
			err := c.sendMsgToPeer(&peer_wire.Msg{
				Kind:  peer_wire.Piece,
				Index: uint32(req.pc),
				Begin: uint32(req.off),
				Block: data,
			}, false)
			if err != nil {
				return err
			}
			return c.sendMsgToTorrent(uploadedBlock(req))
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) requestCanceldOrFullfiled(b block) bool {
	c.muPeerReqs.Lock()
	defer c.muPeerReqs.Unlock()
	_, ok := c.peerReqs[b]
	return !ok
}

func (c *conn) onPieceMsg(msg *peer_wire.Msg) error {
	//var ready block
	var ok bool
	bl := reqMsgToBlock(msg.Request())
	if _, ok = c.onFlightReqs[bl]; !ok {
		//remote peer send us a block that doesn't exists in onFlight requests.
		//we may have requested it previously and discard it, but remote peer didn't,
		//maybe because of network inconsistencies (latency).
		c.cl.counters.Add("unexpectedBlockRecv", 1)
		if !c.haveInfo {
			c.ban = true
			return errors.New("peer send piece while we dont have info")
		}
		if !c.t.pieces.isValid(bl.pc) {
			c.ban = true
			return errors.New("peer send piece doesn't exist")
		}
		//TODO: check if we have the particular block
		if c.myBf.Get(bl.pc) {
			c.logger.Printf("peer send block of piece we already have: %v\n", bl)
			return nil
		}
		var length int
		var err error
		if length, err = c.t.pieces.pcs[bl.pc].blockLenSafe(bl.off); err != nil {
			switch {
			case errors.Is(err, errDivOffset):
				//peer gave us a useful block but with a not prefered offset.
				//We dont punish him but we dont save to storage also, we dont
				//want to mix our block offsets (we have precomputed which blocks
				//to ask for).
				c.logger.Println(err)
				return nil
			case errors.Is(err, errLargeOffset):
				return err
			}
		}
		if length != bl.len {
			c.logger.Println("length of piece msg is wrong")
			return nil
		}
	}
	if ok {
		delete(c.onFlightReqs, bl)
		err := c.maybeSendRequests()
		if err != nil {
			return err
		}
	}
	if err := c.t.writeBlock(msg.Block, bl.pc, bl.off); err != nil {
		if errors.Is(err, storage.ErrAlreadyWritten) {
			//propably another conn got the same block
			c.cl.counters.Add("duplicateBlocksReceived", 1)
			c.logger.Printf("attempt to write the same block: %v", bl)
			return nil
		}
		c.t.logger.Fatalf("storage write err %s\n", err)
		return err
	}
	return c.sendMsgToTorrent(downloadedBlock(bl))
}

//TODO: master hold piece struct and manages them.
//master sends to peer conns block requests
type block struct {
	pc  int
	off int
	len int
}

func (b *block) reqMsg() *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: uint32(b.pc),
		Begin: uint32(b.off),
		Len:   uint32(b.len),
	}
}

func (b *block) cancelMsg() *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Cancel,
		Index: uint32(b.pc),
		Begin: uint32(b.off),
		Len:   uint32(b.len),
	}
}

func reqMsgToBlock(msg *peer_wire.Msg) block {
	return block{
		pc:  int(msg.Index),
		off: int(msg.Begin),
		len: int(msg.Len),
	}
}
