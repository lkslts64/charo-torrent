package torrent

import (
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
	keepAliveSendFreq               = keepAliveInterval - 10*time.Second
)

//conn represents a peer connection.
//This is controled by worker goroutines.
//TODO: peer_wire.Msg should be passed through pointers everywhere
type conn struct {
	cl     *Client
	t      *Torrent
	logger *log.Logger
	//tcp connection with peer
	cn   net.Conn
	peer Peer
	exts peer_wire.Extensions
	//main goroutine also has this state - needs to be synced between
	//the two goroutines
	state connState
	//we receive messages from Torrent on this channel.
	recvC chan interface{}
	//we send messages to Torrent from this channel.
	sendC chan interface{}
	//chans to signal that conn should is dropped
	dropped        chan struct{}
	keepAliveTimer *time.Timer
	peerReqs       map[block]struct{}
	onFlightReqs   map[block]struct{}
	muPeerReqs     sync.Mutex
	haveInfo       bool

	//
	debugVerifications int
	//The number of outstanding request messages this peer supports
	//without dropping any. The default in in libtorrent is 250.
	peerReqq int
	reserved peer_wire.Reserved
	//TODO: we 'll need this later.
	peerBf bitmap.Bitmap
	myBf   bitmap.Bitmap
	peerID []byte
}

//just wraps a msg with an error
func newConn(t *Torrent, cn net.Conn, peer Peer) *conn {
	logPrefix := t.cl.logger.Prefix() + "CN"
	if peer.P.ID != nil {
		logPrefix += fmt.Sprintf("%x ", peer.P.ID[14:])
	} else {
		logPrefix += "------ "
	}
	return &conn{
		cl:           t.cl,
		t:            t,
		logger:       log.New(t.cl.logger.Writer(), logPrefix, log.LstdFlags),
		cn:           cn,
		peer:         peer,
		state:        newConnState(),
		recvC:        make(chan interface{}, recvCSize),
		sendC:        make(chan interface{}, sendCSize),
		dropped:      make(chan struct{}),
		onFlightReqs: make(map[block]struct{}),
		peerBf:       bitmap.Bitmap{RB: roaring.NewBitmap()},
		peerReqs:     make(map[block]struct{}),
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
		panic("conn: couldn't inform torrent about the connection")
	}
}

func (c *conn) connInfo() *connInfo {
	return &connInfo{
		t: c.t,
		// Torrent should see c.recvC as its send channel and c.sendC as its receive channel.
		sendC:    c.recvC,
		recvC:    c.sendC,
		droppedC: c.dropped,
		peer:     c.peer,
		reserved: c.reserved,
		state:    c.state,
	}
}

func (c *conn) close() {
	c.discardBlocks(false, false)
	//notify Torrent that conn closed
	select {
	case <-c.dropped:
	default:
		close(c.dropped)
	}
	//because we closed `dropped`, there is no deadlock possibility
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
		close(quit)
		c.close()
		err = nilOnEOF(err)
	}()
	go c.readPeerMsgs(readC, quit, readErrC)
	c.keepAliveTimer = time.NewTimer(keepAliveSendFreq)
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
		}
		if err != nil {
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
		msg, err := peer_wire.Read(c.cn)
		if err != nil {
			errC <- err
			return
		}
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

func (c *conn) sendRequests() {
	if !c.wantBlocks() {
		return
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
		return
	}
	requests = requests[:n]
	for _, req := range requests {
		if _, ok := c.onFlightReqs[req]; ok {
			continue
		}
		c.onFlightReqs[req] = struct{}{}
		c.sendMsgToPeer(req.reqMsg())
	}
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
	err := (&peer_wire.Msg{
		Kind: peer_wire.KeepAlive,
	}).Write(c.cn)
	if err != nil {
		return err
	}
	c.keepAliveTimer.Reset(keepAliveSendFreq)
	return nil
}

func (c *conn) onTorrentMsg(cmd interface{}) (err error) {
	switch v := cmd.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Port:
		case peer_wire.Interested:
			c.state.amInterested = true
			defer c.sendRequests()
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
		err = c.sendMsgToPeer(v)
	case bitmap.Bitmap: //this equivalent to bitfield, we encode and write to wire.
		c.myBf = v
		if c.notUseful() {
			err = io.EOF
			return
		}
		err = c.sendMsgToPeer(&peer_wire.Msg{
			Kind: peer_wire.Bitfield,
			Bf:   c.bitfield(c.myBf),
		})
	case requestsAvailable:
		c.sendRequests()
	case haveInfo:
		c.haveInfo = true
		if c.peerBf.Len() > 0 {
			c.t.pieces.onBitfield(c.peerBf)
		}
	case drop:
		err = io.EOF
	}
	return
}

func (c *conn) sendMsgToPeer(msg *peer_wire.Msg) error {
	//is mandatory to stop timer and recv from chan
	//in order to reset it
	if !c.keepAliveTimer.Stop() {
		<-c.keepAliveTimer.C
		err := (&peer_wire.Msg{
			Kind: peer_wire.KeepAlive,
		}).Write(c.cn)
		if err != nil {
			return err
		}
	}
	c.keepAliveTimer.Reset(keepAliveSendFreq)
	err := msg.Write(c.cn)
	if err != nil {
		return err
	}
	return nil
}

func (c *conn) onPeerMsg(msg *peer_wire.Msg) (err error) {
	stateChange := func(currState *bool, futureState bool) error {
		if *currState == futureState {
			return nil
		}
		*currState = futureState
		return c.sendMsgToTorrent(msg)
	}
	switch msg.Kind {
	case peer_wire.KeepAlive:
	case peer_wire.Interested:
		err = stateChange(&c.state.isInterested, true)
	case peer_wire.NotInterested:
		err = stateChange(&c.state.isInterested, false)
	case peer_wire.Choke:
		err = c.discardBlocks(true, false)
		if err != nil {
			return err
		}
		err = stateChange(&c.state.isChoking, true)
	case peer_wire.Unchoke:
		err = stateChange(&c.state.isChoking, false)
		c.sendRequests()
	case peer_wire.Piece:
		err = c.onPieceMsg(msg)
	case peer_wire.Request:
		err = c.upload(msg)
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
			close(c.dropped)
			//e is guranteed to be sent in sendC even if error occurs.
			//No deadlock possibility because we closed dropped chan
			select {
			case <-c.t.dropC:
			case c.sendC <- e:
			}
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

type metainfoSize int64

func (c *conn) onExtended(msg *peer_wire.Msg) (err error) {
	switch v := msg.ExtendedMsg.(type) {
	case peer_wire.ExtHandshakeDict:
		c.exts, err = v.Extensions()
		if _, ok := c.exts[peer_wire.ExtMetadataName]; ok {
			if msize, ok := v.MetadataSize(); ok {
				if err = c.sendMsgToTorrent(metainfoSize(msize)); err != nil {
					//error
				}
			}
		}
	case peer_wire.MetadataExtMsg:
		switch v.Kind {
		case peer_wire.MetadataDataID:
		case peer_wire.MetadataRejID:
		case peer_wire.MetadataReqID:
			if !c.haveInfo {
				c.sendMsgToPeer(&peer_wire.Msg{
					Kind:       peer_wire.Extended,
					ExtendedID: peer_wire.ExtMetadataID,
					ExtendedMsg: peer_wire.MetadataExtMsg{
						Kind:  peer_wire.MetadataRejID,
						Piece: v.Piece,
					},
				})
			}
		default:
			//unknown extension ID
		}
	default:
		//unknown extension ID
	}
	return nil
}

func (c *conn) discardBlocks(notifyTorrent, sendCancels bool) error {
	if len(c.onFlightReqs) > 0 {
		unsatisfiedRequests := []block{}
		for req := range c.onFlightReqs {
			if sendCancels {
				c.sendMsgToPeer(req.cancelMsg())
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
func (c *conn) upload(msg *peer_wire.Msg) error {
	bl := reqMsgToBlock(msg)
	if c.isCanceled(bl) {
		return nil
	}
	//ensure we delete the request at every case
	defer func() {
		c.muPeerReqs.Lock()
		defer c.muPeerReqs.Unlock()
		delete(c.peerReqs, bl)
	}()
	if !c.haveInfo {
		return errors.New("peer send requested and we dont have info dict")
	}
	if !c.state.canUpload() {
		//maybe we have choked the peer, but he hasnt been informed and
		//thats why he send us request, dont drop conn just ignore the req
		c.logger.Print("peer send request msg while choked\n")
		return nil
	}
	if bl.len > maxRequestBlockSz {
		return fmt.Errorf("request length out of range: %d", msg.Len)
	}
	//check that we have the requested piece
	if !c.myBf.Get(bl.pc) {
		return fmt.Errorf("peer requested piece we do not have")
	}
	//ensure the we dont exceed the end of the piece
	if endOff := bl.off + bl.len; endOff > c.t.pieceLen(uint32(bl.pc)) {
		return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
	}
	//read from db
	//and write to conn
	data := make([]byte, bl.len)
	if err := c.t.readBlock(data, bl.pc, bl.off); err != nil {
		return nil
	}
	c.sendMsgToPeer(&peer_wire.Msg{
		Kind:  peer_wire.Piece,
		Index: msg.Index,
		Begin: msg.Begin,
		Block: data,
	})
	return c.sendMsgToTorrent(uploadedBlock(bl))
}

func (c *conn) isCanceled(b block) bool {
	c.muPeerReqs.Lock()
	defer c.muPeerReqs.Unlock()
	_, ok := c.peerReqs[b]
	return !ok
}

//store block and send another request if request queuer permits it.
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
			return errors.New("peer send piece while we dont have info")
		}
		if !c.t.pieces.isValid(bl.pc) {
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
		c.sendRequests()
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
