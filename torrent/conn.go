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
	readChSize = 250
	//jobChSize   = maxConns * eventChSize * 10
	eventChSize = 250
)

//jobs that we 'll get are proportional to events a conn emits.
//this is upper bound on jobs to be produced - we can decrease it more
var jobChSize = maxConns*eventChSize*(maxOnFlight+10) + 10

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
	//jobCh chan job - infinite chan
	//we want to avoid deadlocks between jobCh and eventCh.
	//Also we dont want master to block until workers(peerConns)
	//receive jobs (they are doing heavy I/O).
	jobCh chan interface{}
	//channel that we send received msgs
	eventCh        chan interface{}
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
	//drop        chan struct{}
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
		logger:  log.New(os.Stdout, string(peerID), log.LstdFlags),
		cn:      cn,
		state:   newConnState(),
		jobCh:   make(chan interface{}, jobChSize),
		eventCh: make(chan interface{}, eventChSize),
		rq:      newRequestQueuer(),
		//drop:        make(chan struct{}),
		//peerReqq: peerReqq,
		peerReqs: make(map[block]struct{}),
	}
	//inform others about the new conn
	t.newConnCh <- &connChansInfo{
		c.jobCh,
		c.eventCh,
	}
	return c
}

func (pc *conn) close() {
	pc.cn.Close()
	//send event that we closed and after close chan - avoid leak goroutines
	pc.eventCh <- *new(connDroped)
	close(pc.eventCh)
	pc.logger.Println("closed connection")
}

type readResult struct {
	msg *peer_wire.Msg
	err error
}

func (pc *conn) mainLoop() error {
	var err error
	//pc.init()
	readCh := make(chan *peer_wire.Msg, readChSize)
	readErrCh := make(chan error, 1)
	//we need o quit chan in order to not leak readMsgs goroutine
	quit, idle := make(chan struct{}), make(chan struct{})
	defer pc.close()
	defer close(quit)
	go pc.readMsgs(readCh, idle, quit, readErrCh)
	nilOnEOF := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	pc.keepAliveTimer = time.NewTimer(keepAliveSendFreq)
	for {
		//prioritize jobs
		select {
		case act := <-pc.jobCh:
			//TODO:pass values to job ch as interface{}
			//no need for job type wrapper
			err = pc.parseJob(act.(job))
		default:
			select {
			case act := <-pc.jobCh:
				//TODO:pass values to job ch as interface{}
				//no need for job type wrapper
				err = pc.parseJob(act.(job))
			case <-pc.t.done:
				return nil
			case msg := <-readCh:
				err = pc.parsePeerMsg(msg)
			case err = <-readErrCh:
			case <-idle:
				err = errors.New("peer idle")
			case <-pc.keepAliveTimer.C:
				err = pc.sendKeepAlive()
			}
		}
		if err != nil {
			switch err = nilOnEOF(err); err {
			case nil:
				pc.logger.Println("lost connection with peer")
			default:
				pc.logger.Println(err)
			}
			return err
		}
		if pc.wantBlocks() {
			pc.eventCh <- *new(wantBlocks)
			pc.waitingBlocks = maxOnFlight
			pc.logger.Println("want blocks send")
		}
		if pc.notUseful() {
			pc.logger.Println("conn is not useful anymore for both ends")
			return nil
		}
	}
}

//read msgs from remote peer
//run on seperate goroutine
func (pc *conn) readMsgs(readCh chan<- *peer_wire.Msg, idle, quit chan struct{},
	errCh chan error) {
	var msg *peer_wire.Msg
	var err error
	readDone := make(chan readResult)
loop:
	for {
		//we won't block on read forever cause conn will close
		//if main goroutine exits
		go func() {
			msg, err = peer_wire.Read(pc.cn)
			readDone <- readResult{msg, err}
		}()
		//break from loops in not required
		select {
		case <-time.After(keepAliveInterval + time.Minute): //lets be forbearing
			close(idle)
		case res := <-readDone:
			if res.err != nil {
				errCh <- res.err
				break loop
			}
			var sendToChan bool
			switch res.msg.Kind {
			case peer_wire.Request:
				b := reqMsgToBlock(msg)
				pc.muPeerReqs.Lock()
				//avoid flooding readCh by not sending requests that are
				//already requested from peer and by discarding requests when
				//our buffer is full.
				switch _, ok := pc.peerReqs[b]; {
				case ok:
					pc.logger.Printf("peer send duplicate request for block %v\n", b)
				case len(pc.peerReqs) >= pc.t.reqq:
					pc.logger.Print("peer requests buffer is full\n")
				default: //all good
					pc.peerReqs[b] = struct{}{}
					sendToChan = true
				}
				pc.muPeerReqs.Unlock()
			case peer_wire.Cancel:
				b := reqMsgToBlock(msg)
				pc.muPeerReqs.Lock()
				if _, ok := pc.peerReqs[b]; ok {
					delete(pc.peerReqs, b)
				} else {
					latecomerCancels.Inc()
				}
				pc.muPeerReqs.Unlock()
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

func (pc *conn) sendKeepAlive() error {
	err := (&peer_wire.Msg{
		Kind: peer_wire.KeepAlive,
	}).Write(pc.cn)
	if err != nil {
		return err
	}
	pc.keepAliveTimer.Reset(keepAliveSendFreq)
	return nil
}

func (pc *conn) parseJob(j job) (err error) {
	switch v := j.val.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested:
			pc.state.amInterested = true
		case peer_wire.NotInterested:
			pc.state.amInterested = false
		case peer_wire.Choke:
			pc.state.amChoking = true
		case peer_wire.Unchoke:
			pc.state.amChoking = false
		case peer_wire.Have:
			pc.myBf.Set(int(v.Index), true)
			//Surpress have msgs.
			//From https://wiki.theory.org/index.php/BitTorrentSpecification:
			//At the same time, it may be worthwhile to send a HAVE message
			//to a peer that has that piece already since it will be useful in
			//determining which piece is rare.
			if pc.peerBf.Get(int(v.Index)) {
				return
			}
		}
		err = pc.sendMsg(v)
	case []block:
		pc.waitingBlocks -= len(v)
		var ok, ready bool
		for _, bl := range v {
			if ok, ready = pc.rq.queue(bl); !ok {
				panic("received blocks but can't queue them")
			}
			if ready {
				pc.sendMsg(bl.reqMsg())
			}
		}
	case bitmap.Bitmap: //this equivalent to bitfield, we encode and write to wire.
		pc.myBf = v
		pc.sendMsg(&peer_wire.Msg{
			Kind: peer_wire.Bitfield,
			Bf:   pc.encodeBitMap(pc.myBf),
		})
	case verifyPiece:
		correct := pc.t.storage.HashPiece(int(v), pc.t.PieceLen(uint32(v)))
		pc.eventCh <- pieceVerificationOk{
			pieceIndex: int(v),
			ok:         correct,
		}
	case haveInfo:
		pc.haveInfo = true
	case seeding:
		pc.amSeeding = true
	}
	return
}

func (pc *conn) sendMsg(msg *peer_wire.Msg) error {
	//is mandatory to stop timer and recv from chan
	//in order to reset it
	if !pc.keepAliveTimer.Stop() {
		<-pc.keepAliveTimer.C
		err := (&peer_wire.Msg{
			Kind: peer_wire.KeepAlive,
		}).Write(pc.cn)
		if err != nil {
			return err
		}
	}
	pc.keepAliveTimer.Reset(keepAliveSendFreq)
	err := msg.Write(pc.cn)
	if err != nil {
		return err
	}
	return nil
}

func (pc *conn) parsePeerMsg(msg *peer_wire.Msg) (err error) {
	stateChange := func(currState, futureState bool) bool {
		if currState == futureState {
			return futureState
		}
		pc.eventCh <- msg
		return futureState
	}
	switch msg.Kind {
	case peer_wire.KeepAlive, peer_wire.Port:
	case peer_wire.Interested:
		pc.state.isInterested = stateChange(pc.state.isInterested, true)
	case peer_wire.NotInterested:
		pc.state.isInterested = stateChange(pc.state.isInterested, false)
	case peer_wire.Choke:
		pc.state.isChoking = stateChange(pc.state.isChoking, true)
		pc.discardBlocks()
	case peer_wire.Unchoke:
		pc.state.isChoking = stateChange(pc.state.isChoking, false)
	case peer_wire.Piece:
		err = pc.onPieceMsg(msg)
	case peer_wire.Request:
		err = pc.upload(msg)
	case peer_wire.Have:
		if pc.peerBf.Get(int(msg.Index)) {
			pc.logger.Printf("peer send duplicate Have msg of piece %d\n", msg.Index)
			return
		}
		pc.peerBf.Set(int(msg.Index), true)
		pc.eventCh <- msg
	case peer_wire.Bitfield:
		if !pc.peerBf.IsEmpty() {
			err = errors.New("peer: send bitfield twice or have before bitfield")
		}
		var tmp *bitmap.Bitmap
		tmp, err = pc.decodeBitfield(msg.Bf)
		if err != nil {
			return
		}
		pc.peerBf = *tmp
		pc.eventCh <- pc.peerBf.Copy()
	case peer_wire.Extended:
		pc.onExtended(msg)
	default:
		err = errors.New("unknown msg kind received")
	}
	return
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

func (pc *conn) onExtended(msg *peer_wire.Msg) (err error) {
	switch v := msg.ExtendedMsg.(type) {
	case peer_wire.ExtHandshakeDict:
		pc.exts, err = v.Extensions()
		if _, ok := pc.exts[peer_wire.ExtMetadataName]; ok {
			if msize, ok := v.MetadataSize(); ok {
				pc.eventCh <- metainfoSize(msize)
			}
		}
	case peer_wire.MetadataExtMsg:
		switch v.Kind {
		case peer_wire.MetadataDataID:
		case peer_wire.MetadataRejID:
		case peer_wire.MetadataReqID:
			if !pc.haveInfo {
				pc.sendMsg(&peer_wire.Msg{
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

func (pc *conn) discardBlocks() {
	if !pc.rq.empty() {
		unsatisfiedRequests := pc.rq.discardAll()
		pc.eventCh <- unsatisfiedRequests
	}
}

//TODO:Store bad peer so we wont accept them again if they try to reconnect
func (pc *conn) upload(msg *peer_wire.Msg) error {
	bl := reqMsgToBlock(msg)
	if pc.isCanceled(bl) {
		return nil
	}
	//ensure we delete the request at every case
	defer func() {
		pc.muPeerReqs.Lock()
		defer pc.muPeerReqs.Unlock()
		delete(pc.peerReqs, bl)
	}()
	if !pc.haveInfo {
		return errors.New("peer send requested and we dont have info dict")
	}
	if !pc.state.canUpload() {
		//maybe we have choked the peer, but he hasnt been informed and
		//thats why he send us request, dont drop conn just ignore the req
		pc.logger.Print("peer send request msg while choked\n")
		return nil
	}
	if bl.len > maxRequestBlockSz {
		return fmt.Errorf("request length out of range: %d", msg.Len)
	}
	//check that we have the requested piece
	if !pc.myBf.Get(bl.pc) {
		return fmt.Errorf("peer requested piece we do not have")
	}
	//ensure the we dont exceed the end of the piece
	if endOff := bl.off + bl.len; endOff > pc.t.PieceLen(uint32(bl.pc)) {
		return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
	}
	//read from db
	//and write to conn
	data := make([]byte, bl.len)
	if err := pc.t.readBlock(data, bl.pc, bl.off); err != nil {
		return nil
	}
	pc.sendMsg(&peer_wire.Msg{
		Kind:  peer_wire.Piece,
		Index: msg.Index,
		Begin: msg.Begin,
		Block: data,
	})
	pc.eventCh <- uploadedBlock(bl)
	return nil
}

func (pc *conn) isCanceled(b block) bool {
	pc.muPeerReqs.Lock()
	defer pc.muPeerReqs.Unlock()
	_, ok := pc.peerReqs[b]
	return !ok
}

//store block and send another request if request queuer permits it.
func (pc *conn) onPieceMsg(msg *peer_wire.Msg) error {
	var ready block
	var ok bool
	bl := reqMsgToBlock(msg.Request())
	if ready, ok = pc.rq.deleteCompleted(bl); !ok {
		//We didnt' request this piece, but maybe it is useful
		pc.logger.Printf("received unexpected block %v\n", bl)
		if !pc.haveInfo {
			return errors.New("peer send piece while we dont have info")
		}
		if !pc.t.pieceValid(bl.pc) {
			return errors.New("peer send piece doesn't exist")
		}
		//TODO: check if we have the particular block
		if pc.myBf.Get(bl.pc) {
			pc.logger.Printf("peer send block of piece we already have: %v\n", bl)
			return nil
		}
		var length int
		var err error
		if length, err = pc.t.pieces.pcs[bl.pc].blockLenSafe(bl.off); err != nil {
			switch {
			case errors.Is(err, errDivOffset):
				//peer gave us a useful block but with offset % blockSz != 0
				//We dont punish him but we dont save to storage also, we dont
				//want to mix our block offsets (we have precomputed which blocks
				//to ask for).
				pc.logger.Println(err)
				return nil
			case errors.Is(err, errLargeOffset):
				return err
			}
		}
		if length != bl.len {
			pc.logger.Println("length of piece msg is wrong")
			return nil
		}
	}
	if pc.state.canDownload() && (ready != block{}) {
		pc.sendMsg(ready.reqMsg())
	}
	if err := pc.t.writeBlock(msg.Block, bl.pc, bl.off); err != nil {
		return nil
	}
	pc.eventCh <- downloadedBlock(bl)
	return nil
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
