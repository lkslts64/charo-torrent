package torrent

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

const (
	//the size could be t.reqq?
	readChSize    = 250
	eventChSize   = 20
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
	peerIdle       chan struct{}
	keepAliveTimer *time.Timer
	rq             *requestQueuer //front size = 10, back = 10
	peerReqs       map[block]struct{}
	haveInfo       bool
	amSeeding      bool
	//An integer, the number of outstanding request messages this peer supports
	//without dropping any. The default in in libtorrent is 250.
	peerReqq   int
	muPeerReqs sync.Mutex
	//how many blocks we are waiting to be forwarded to us
	waitingBlocks int
	//TODO: we 'll need this later.
	peerBf bitmap.Bitmap
	myBf   bitmap.Bitmap
	peerID []byte
}

//just wraps a msg with an error
func newConn(t *Torrent, cn net.Conn, peerID []byte) *conn {
	c := &conn{
		cl: t.cl,
		t:  t,
		//TODO:change logger output to a file
		logger:    log.New(os.Stdout, fmt.Sprintf("%x", peerID), log.LstdFlags),
		cn:        cn,
		state:     newConnState(),
		commandCh: make(chan interface{}, commandChSize),
		eventCh:   make(chan interface{}, eventChSize),
		dropped:   make(chan struct{}),
		peerIdle:  make(chan struct{}),
		rq:        newRequestQueuer(),
		peerReqs:  make(map[block]struct{}),
	}
	//inform others about the new conn
	t.newConnCh <- &connChansInfo{
		c.commandCh,
		c.eventCh,
		c.dropped,
	}
	return c
}

func (c *conn) close() {
	c.cn.Close()
	//close chan - avoid leak goroutines
	close(c.eventCh)
	//notify Torrent that conn closed
	close(c.dropped)
	c.logger.Println("closed connection")
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
	defer c.close()
	defer close(quit)
	go c.readMsgs(readCh, quit, readErrCh)
	nilOnEOF := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	c.keepAliveTimer = time.NewTimer(keepAliveSendFreq)
	for {
		select {
		case cmd := <-c.commandCh:
			err = c.parseCommand(cmd)
		case msg := <-readCh:
			err = c.parsePeerMsg(msg)
		case err = <-readErrCh:
		case <-c.t.done:
			return nil
		case <-c.peerIdle:
			err = errors.New("peer idle")
			return nil
		case <-c.keepAliveTimer.C:
			err = c.sendKeepAlive()
		}
		if err != nil {
			switch err = nilOnEOF(err); err {
			case nil:
				c.logger.Println("lost connection with peer")
			default:
				c.logger.Println(err)
			}
			return err
		}
		if c.wantBlocks() {
			err = c.sendPeerEvent(wantBlocks{})
			if err != nil {
				return err
			}
			c.waitingBlocks = maxOnFlight
			c.logger.Println("want blocks send")
		}
		if c.notUseful() {
			c.logger.Println("conn is not useful anymore for both ends")
			return nil
		}
	}
}

//read msgs from remote peer
//run on seperate goroutine
func (c *conn) readMsgs(readCh chan<- *peer_wire.Msg, quit chan struct{},
	errCh chan error) {
	var msg *peer_wire.Msg
	var err error
	readDone := make(chan readResult)
loop:
	for {
		//we won't block on read forever cause conn will close
		//if main goroutine exits
		go func() {
			msg, err = peer_wire.Read(c.cn)
			readDone <- readResult{msg, err}
		}()
		//break from loops in not required
		select {
		case <-time.After(keepAliveInterval + time.Minute): //lets be forbearing
			close(c.peerIdle)
		case res := <-readDone:
			if res.err != nil {
				errCh <- res.err
				break loop
			}
			var sendToChan bool
			switch res.msg.Kind {
			case peer_wire.Request:
				b := reqMsgToBlock(msg)
				c.muPeerReqs.Lock()
				//avoid flooding readCh by not sending requests that are
				//already requested from peer and by discarding requests when
				//our buffer is full.
				switch _, ok := c.peerReqs[b]; {
				case ok:
					c.logger.Printf("peer send duplicate request for block %v\n", b)
				case len(c.peerReqs) >= c.t.reqq:
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
				case readCh <- res.msg:
				case <-quit: //we must care for quit here too
					break loop
				}
			}
		case <-quit:
			break loop
		}
	}
}

func (c *conn) wantBlocks() bool {
	return c.state.canDownload() && c.waitingBlocks == 0 && c.rq.needMore()
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
		case peer_wire.NotInterested:
			c.state.amInterested = false
		case peer_wire.Choke:
			c.state.amChoking = true
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
		}
		err = c.sendMsg(v)
	case []block:
		c.waitingBlocks -= len(v)
		var ok, ready bool
		for _, bl := range v {
			if ok, ready = c.rq.queue(bl); !ok {
				panic("received blocks but can't queue them")
			}
			if ready {
				c.sendMsg(bl.reqMsg())
			}
		}
	case bitmap.Bitmap: //this equivalent to bitfield, we encode and write to wire.
		c.myBf = v
		c.sendMsg(&peer_wire.Msg{
			Kind: peer_wire.Bitfield,
			Bf:   c.encodeBitMap(c.myBf),
		})
	case verifyPiece:
		correct := c.t.storage.HashPiece(int(v), c.t.pieceLen(uint32(v)))
		c.logger.Printf("piece #%d verification %s", v, func(correct bool) string {
			if correct {
				return "successful"
			}
			return "failed"
		}(correct))
		err = c.sendPeerEvent(pieceHashed{
			pieceIndex: int(v),
			ok:         correct,
		})
	case haveInfo:
		c.haveInfo = true
	case seeding:
		c.amSeeding = true
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
	stateChange := func(currState, futureState bool) (bool, error) {
		if currState == futureState {
			return futureState, nil
		}
		return futureState, c.sendPeerEvent(msg)
	}
	switch msg.Kind {
	case peer_wire.KeepAlive, peer_wire.Port:
	case peer_wire.Interested:
		c.state.isInterested, err = stateChange(c.state.isInterested, true)
	case peer_wire.NotInterested:
		c.state.isInterested, err = stateChange(c.state.isInterested, false)
	case peer_wire.Choke:
		c.state.isChoking, err = stateChange(c.state.isChoking, true)
		err = c.discardBlocks()
	case peer_wire.Unchoke:
		c.state.isChoking, err = stateChange(c.state.isChoking, false)
	case peer_wire.Piece:
		err = c.onPieceMsg(msg)
	case peer_wire.Request:
		err = c.upload(msg)
	case peer_wire.Have:
		if c.peerBf.Get(int(msg.Index)) {
			c.logger.Printf("peer send duplicate Have msg of piece %d\n", msg.Index)
			return
		}
		c.peerBf.Set(int(msg.Index), true)
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
			case <-c.t.done:
				err = io.EOF
			case <-c.peerIdle:
				err = errors.New("peer idle")
				return nil
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
	var bfLen int
	var bm bitmap.Bitmap
	if c.haveInfo {
		numPieces := c.t.numPieces()
		if !bf.Valid(numPieces) {
			return nil, errors.New("bf length is not valid")
		}
		bfLen = peer_wire.BfLen(numPieces)
	} else {
		bfLen = len(bf) * 8
	}
	for i := 0; i < bfLen; i++ {
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

/*//we should have metadata in order to call this
func (pc *conn) peerBitFieldValid() bool {
	return pc.peerBf.Valid(pc.t.tm.mi.Info.NumPieces())
}*/

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

func (c *conn) discardBlocks() (err error) {
	if !c.rq.empty() {
		unsatisfiedRequests := c.rq.discardAll()
		err = c.sendPeerEvent(discardedRequests(unsatisfiedRequests))
	}
	return
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
	var ready block
	var ok bool
	bl := reqMsgToBlock(msg.Request())
	if ready, ok = c.rq.deleteCompleted(bl); !ok {
		//We didnt' request this piece, but maybe it is useful
		c.logger.Printf("received unexpected block %v\n", bl)
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
				//peer gave us a useful block but with offset % blockSz != 0
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
	if c.state.canDownload() && (ready != block{}) {
		c.sendMsg(ready.reqMsg())
	}
	if err := c.t.writeBlock(msg.Block, bl.pc, bl.off); err != nil {
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

func reqMsgToBlock(msg *peer_wire.Msg) block {
	return block{
		pc:  int(msg.Index),
		off: int(msg.Begin),
		len: int(msg.Len),
	}
}
