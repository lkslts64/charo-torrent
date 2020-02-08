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
	//the size could be t.reqq?
	readChSize    = 250
	eventChSize   = 10
	commandChSize = eventChSize
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
	exts peer_wire.Extensions
	//main goroutine also has this state - needs to be synced between
	//the two goroutines
	state connState
	//chanel that we receive msgs to be sent (generally)
	//commandCh chan job - infinite chan
	//we want to avoid deadlocks between commandCh and eventCh.
	//Also we dont want master to block until workers(peerConns)
	//receive jobs (they are doing heavy I/O).
	commandCh chan interface{}
	//channel that we send received msgs
	eventCh chan interface{}
	//chans to signal that conn should is dropped
	dropped        chan struct{}
	keepAliveTimer *time.Timer
	peerReqs       map[block]struct{}
	onFlightReqs   map[block]struct{}
	muPeerReqs     sync.Mutex
	haveInfo       bool
	amSeeding      bool

	//
	debugVerifications int
	//An integer, the number of outstanding request messages this peer supports
	//without dropping any. The default in in libtorrent is 250.
	peerReqq int
	//TODO: we 'll need this later.
	peerBf bitmap.Bitmap
	myBf   bitmap.Bitmap
	peerID []byte
}

//just wraps a msg with an error
func newConn(t *Torrent, cn net.Conn, peerID []byte) *conn {
	c := &conn{
		cl:           t.cl,
		t:            t,
		logger:       log.New(t.cl.logWriter, fmt.Sprintf("%x", peerID), log.LstdFlags),
		cn:           cn,
		state:        newConnState(),
		commandCh:    make(chan interface{}, commandChSize),
		eventCh:      make(chan interface{}, eventChSize),
		dropped:      make(chan struct{}),
		onFlightReqs: make(map[block]struct{}),
		peerBf:       bitmap.Bitmap{RB: roaring.NewBitmap()},
		peerReqs:     make(map[block]struct{}),
	}
	//inform Torrent about the new conn
	t.newConnCh <- &connChansInfo{
		c.commandCh,
		c.eventCh,
		c.dropped,
	}
	c.eventCh <- c.cn.RemoteAddr()
	return c
}

func (c *conn) close() {
	c.discardBlocks()
	//close chan - avoid leak goroutines
	close(c.eventCh)
	//notify Torrent that conn closed
	close(c.dropped)
}

type readResult struct {
	msg *peer_wire.Msg
	err error
}

func (c *conn) mainLoop() error {
	var err error
	//pc.init()
	readCh := make(chan *peer_wire.Msg, readChSize)
	readErrCh := make(chan error, 1)
	//we need o quit chan in order to not leak readMsgs goroutine
	quit := make(chan struct{})
	defer func() {
		close(quit)
		c.close()
		err = nilOnEOF(err)
	}()
	go c.readPeerMsgs(readCh, quit, readErrCh)
	c.keepAliveTimer = time.NewTimer(keepAliveSendFreq)
	debugTick := time.NewTicker(2 * time.Second)
	defer debugTick.Stop()
	for {
		select {
		case cmd := <-c.commandCh:
			err = c.parseCommand(cmd)
		case msg := <-readCh:
			err = c.parsePeerMsg(msg)
		case err = <-readErrCh:
		case <-c.t.drop:
			return nil
		case <-c.keepAliveTimer.C:
			err = c.sendKeepAlive()
			/*case <-debugTick.C:
			if len(c.onFlightReqs) > 0 {
				c.logger.Printf("onFlight: %d canDownload: %t\n", len(c.onFlightReqs), c.state.canDownload())
			}*/
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
func (c *conn) readPeerMsgs(readCh chan<- *peer_wire.Msg, quit chan struct{},
	errCh chan error) {
	for {
		c.cn.SetReadDeadline(time.Now().Add(keepAliveInterval + time.Minute)) //lets be forbearing
		msg, err := peer_wire.Read(c.cn)
		if err != nil {
			errCh <- err
			return
		}
		//break from loops in not required
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
				duplicateRequestsReceived.Inc()
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
				latecomerCancels.Inc()
			}
			c.muPeerReqs.Unlock()
		default:
			sendToChan = true
		}
		if sendToChan {
			select {
			case readCh <- msg:
			case <-quit: //we must care for quit here
				return
			}
		}
	}
}

func (c *conn) wantBlocks() bool {
	return !c.amSeeding && c.haveInfo && c.state.canDownload() && len(c.onFlightReqs) < maxOnFlight/2
}

func (c *conn) sendRequests() {
	if c.wantBlocks() {
		requests := make([]block, maxOnFlight-len(c.onFlightReqs))
		n := c.t.pieces.getRequests(c.peerBf, requests)
		if n == 0 {
			if (requests[0] != block{}) {
				panic("send requests")
			}
			nonUsefulRequestReads.Inc()
			return
		}
		requests = requests[:n]
		for _, req := range requests {
			if _, ok := c.onFlightReqs[req]; ok {
				continue
			}
			c.onFlightReqs[req] = struct{}{}
			c.sendMsg(req.reqMsg())
		}
	}
}

//returns true if its worth to keep the conn alive
func (c *conn) notUseful() bool {
	if !c.haveInfo {
		return false
	}
	return c.peerSeeding() && c.amSeeding
}

func (c *conn) peerSeeding() bool {
	return c.peerBf.Len() == c.t.numPieces()
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

func (c *conn) parseCommand(cmd interface{}) (err error) {
	switch v := cmd.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested:
			c.state.amInterested = true
			defer c.sendRequests()
			//c.sendRequests()
		case peer_wire.NotInterested:
			c.state.amInterested = false
			lostBlocksDueToSync.Add(uint64(len(c.onFlightReqs)))
			//due to inconsistent states between Torrent and conn we may have onFlightReqs
			//which we'll be lost (hopefully I think this doesn't happen too much)
			c.discardBlocks()
		case peer_wire.Choke:
			c.state.amChoking = true
			c.muPeerReqs.Lock()
			c.peerReqs = make(map[block]struct{})
			c.muPeerReqs.Unlock()
		case peer_wire.Unchoke:
			c.state.amChoking = false
		case peer_wire.Have:
			c.myBf.Set(int(v.Index), true)
			//Surpress have msgs.
			//From https://wiki.theory.org/index.php/BitTorrentSpecification:
			//At the same time, it may be worthwhile to send a HAVE message
			//to a peer that has that piece already since it will be useful in
			//determining which piece is rare.
			//TODO:change to:
			// if pc.peerBf.Get(int(v.Index)) && !pc.peerBf.peerSeeding() {
			if c.peerBf.Get(int(v.Index)) {
				return
			}
		case peer_wire.Cancel:
			if _, ok := c.onFlightReqs[reqMsgToBlock(v)]; !ok {
				return
			}
		}
		err = c.sendMsg(v)
	case bitmap.Bitmap: //this equivalent to bitfield, we encode and write to wire.
		c.myBf = v
		c.sendMsg(&peer_wire.Msg{
			Kind: peer_wire.Bitfield,
			Bf:   c.encodeBitMap(c.myBf),
		})
	case verifyPiece:
		//c.logger.Printf("going to verify piece #%d\n", v)
		correct := c.t.storage.HashPiece(int(v), c.t.pieceLen(uint32(v)))
		/*c.logger.Printf("piece #%d verification %s", v, func(correct bool) string {
			if correct {
				return "successful"
			}
			return "failed"
		}(correct))*/
		err = c.sendPeerEvent(pieceHashed{
			pieceIndex: int(v),
			ok:         correct,
		})
	case requestsAvailable:
		c.sendRequests()
	case haveInfo:
		c.haveInfo = true
	case seeding:
		c.amSeeding = true
		if c.notUseful() {
			//c.logger.Println("conn is not useful anymore for both ends")
			err = io.EOF
		}
	case drop:
		err = io.EOF
	}
	return
}

func (c *conn) sendMsg(msg *peer_wire.Msg) error {
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

func (c *conn) parsePeerMsg(msg *peer_wire.Msg) (err error) {
	stateChange := func(currState *bool, futureState bool) error {
		if *currState == futureState {
			return nil
		}
		*currState = futureState
		return c.sendPeerEvent(msg)
	}
	switch msg.Kind {
	case peer_wire.KeepAlive, peer_wire.Port:
	case peer_wire.Interested:
		err = stateChange(&c.state.isInterested, true)
	case peer_wire.NotInterested:
		err = stateChange(&c.state.isInterested, false)
	case peer_wire.Choke:
		c.discardBlocks()
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
		c.t.pieces.onHave(int(msg.Index))
		err = c.sendPeerEvent(msg)
	case peer_wire.Bitfield:
		if !c.peerBf.IsEmpty() {
			err = errors.New("peer: send bitfield twice or have before bitfield")
			return
		}
		var tmp *bitmap.Bitmap
		if tmp, err = c.decodeBitfield(msg.Bf); err != nil {
			return
		}
		c.peerBf = *tmp
		c.t.pieces.onBitfield(c.peerBf)
		err = c.sendPeerEvent(c.peerBf.Copy())
	case peer_wire.Extended:
		c.onExtended(msg)
	default:
		err = errors.New("unknown msg kind received")
	}
	return
}

func (c *conn) sendPeerEvent(e interface{}) error {
	var err error
	for {
		//we dont just send e to eventCh because if the former chan is full,
		//then commandCh may also become full and we will deadlock.
		select {
		case c.eventCh <- e:
			return nil
		default:
			//receive command while waiting for event to be send, we dont want
			//to deadlock.
			select {
			case cmd := <-c.commandCh:
				err = c.parseCommand(cmd)
			case c.eventCh <- e:
				return nil
			//pretend we lost connection
			case <-c.t.drop:
				err = io.EOF
			case <-c.keepAliveTimer.C:
				err = c.sendKeepAlive()
			}
		}
		if err != nil {
			return err
		}
	}
}

func (c *conn) encodeBitMap(bm bitmap.Bitmap) (bf peer_wire.BitField) {
	bf = peer_wire.NewBitField(c.t.numPieces())
	bm.IterTyped(func(piece int) bool {
		bf.SetPiece(piece)
		return true
	})
	return
}

func (c *conn) decodeBitfield(bf peer_wire.BitField) (*bitmap.Bitmap, error) {
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
				if err = c.sendPeerEvent(metainfoSize(msize)); err != nil {
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
				c.sendMsg(&peer_wire.Msg{
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

func (c *conn) discardBlocks() {
	if len(c.onFlightReqs) > 0 {
		unsatisfiedRequests := []block{}
		for req := range c.onFlightReqs {
			unsatisfiedRequests = append(unsatisfiedRequests, req)
		}
		c.t.pieces.discardRequests(unsatisfiedRequests)
		c.onFlightReqs = make(map[block]struct{})
		c.sendPeerEvent(discardedRequests{})
	}
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
	c.sendMsg(&peer_wire.Msg{
		Kind:  peer_wire.Piece,
		Index: msg.Index,
		Begin: msg.Begin,
		Block: data,
	})
	return c.sendPeerEvent(uploadedBlock(bl))
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
		if !c.haveInfo {
			return errors.New("peer send piece while we dont have info")
		}
		if !c.t.pieceValid(bl.pc) {
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
			duplicateBlocksReceived.Inc()
			return nil
		}
		c.t.logger.Fatalf("storage write err %s\n", err)
		return err
	}
	return c.sendPeerEvent(downloadedBlock(bl))
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
