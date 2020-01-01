package torrent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/eapache/channels"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

const (
	requestQueueChSize = 10
	readChSize         = 50
	jobChSize          = 50
	eventChSize        = 50
)

const (
	keepAliveInterval time.Duration = 2 * time.Minute
	keepAliveSendFreq               = keepAliveInterval - 10*time.Second
)

//conn represents a peer connection.
//This is controled by worker goroutines.
//TODO: peer_wire.Msg should be passed through pointers everywhere
type conn struct {
	cl *Client
	t  *Torrent
	//tcp connection with peer
	cn   net.Conn
	exts peer_wire.Extensions
	//main goroutine also has this state - needs to be synced between
	//the two goroutines
	state *connState
	//chanel that we receive msgs to be sent (generally)
	//jobCh chan job - infinite chan
	//we want to avoid deadlocks between jobCh and eventCh.
	//Also we dont want master to block until workers(peerConns)
	//receive jobs (they are doing heavy I/O).
	//TODO:get rid of the inf channel - and avoid deadlocks
	//by prioritizing jobCh at select in mainLoop?
	jobCh channels.Channel
	//channel that we send received msgs
	eventCh        chan interface{}
	keepAliveTimer *time.Timer
	rq             *requestQueuer //front size = 10, back = 10
	waitingBlocks  bool
	amSeedingCh    chan struct{}
	haveInfoCh     chan struct{}
	peerBf         bitmap.Bitmap
	myBf           bitmap.Bitmap
}

//just wraps a msg with an error
type readResult struct {
	msg *peer_wire.Msg
	err error
}

func newConn(t *Torrent, cl *Client, cn net.Conn) *conn {
	c := &conn{
		cl:          cl,
		t:           t,
		cn:          cn,
		state:       newConnState(),
		jobCh:       channels.NewInfiniteChannel(),
		eventCh:     make(chan interface{}, eventChSize),
		rq:          newRequestQueuer(),
		amSeedingCh: make(chan struct{}),
		haveInfoCh:  make(chan struct{}),
	}
	//inform master about new conn
	t.newConnCh <- &connChansInfo{
		c.jobCh,
		c.eventCh,
		c.amSeedingCh,
		c.haveInfoCh,
	}
	return c
}

func (pc *conn) close() {
	pc.cn.Close()
	//send event that we closed and after close chan - avoid leak goroutines
	pc.eventCh <- *new(connDroped)
	close(pc.eventCh)
}

func (pc *conn) mainLoop() error {
	var err error
	//pc.init()
	readCh := make(chan readResult, readChSize)
	//we need o quit chan in order to not leak readMsgs goroutine
	quit, idle := make(chan struct{}), make(chan struct{})
	defer pc.close()
	defer close(quit)
	go pc.readMsgs(readCh, idle, quit)
	nilOnEOF := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	pc.keepAliveTimer = time.NewTimer(keepAliveSendFreq)
	//TODO:set when the peer becomes downloader
	for {
		select {
		case <-pc.t.done:
		//job came from main routine
		case act := <-pc.jobCh.Out():
			//TODO:pass values to job ch as interface{}
			//no need for job type wrapper
			err = pc.parseJob(act.(job))
			if err != nil {
				return nilOnEOF(err)
			}
		case res := <-readCh:
			if res.err != nil {
				return nilOnEOF(res.err)
			}
			pc.parsePeerMsg(res.msg)
		case <-idle:
			return errors.New("peer idle")
		case <-pc.keepAliveTimer.C:
			err = (&peer_wire.Msg{
				Kind: peer_wire.KeepAlive,
			}).Write(pc.cn)
			if err != nil {
				return err
			}
			pc.keepAliveTimer.Reset(keepAliveSendFreq)
		}
		if pc.wantBlocks() {
			pc.eventCh <- *new(wantBlocks)
			pc.waitingBlocks = true
		}
	}
}

//read msgs from remote peer
//run on seperate goroutine
func (pc *conn) readMsgs(readCh chan<- readResult, idle, quit chan struct{}) {
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
		case <-time.After(keepAliveInterval + 1*time.Minute): //lets be more resilient
			close(idle)
		case res := <-readDone:
			select {
			case readCh <- res:
			case <-quit: //we must care for quit here too
				break loop
			}
		case <-quit:
			break loop
		}
	}
}

func (pc *conn) wantBlocks() bool {
	return pc.state.canDownload() && !pc.waitingBlocks && pc.rq.needMore()
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
		pc.waitingBlocks = false
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
		pc.state.amInterested = stateChange(pc.state.amInterested, true)
	case peer_wire.NotInterested:
		pc.state.amInterested = stateChange(pc.state.amInterested, false)
	case peer_wire.Choke:
		pc.state.amChoking = stateChange(pc.state.amChoking, true)
		pc.discardBlocks()
	case peer_wire.Unchoke:
		pc.state.amChoking = stateChange(pc.state.amChoking, false)
	case peer_wire.Piece:
		err = pc.onPieceMsg(msg)
	case peer_wire.Request:
		err = pc.upload(msg)
	case peer_wire.Have:
		if pc.peerBf.Get(int(msg.Index)) {
			//log this
			return
		}
		pc.peerBf.Set(int(msg.Index), true)
		pc.eventCh <- msg
	case peer_wire.Bitfield:
		if !pc.peerBf.IsEmpty() {
			err = errors.New("peer: send bitfield twice")
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
	var bm *bitmap.Bitmap
	if c.haveInfo() {
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
	return bm, nil
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

func (c *conn) seeding() bool {
	return isClosed(c.amSeedingCh)
}

func (c *conn) haveInfo() bool {
	return isClosed(c.haveInfoCh)
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
			if !pc.haveInfo() {
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
	tmeta := pc.t.tm
	bl := reqMsgToBlock(msg)
	if !pc.state.canUpload() {
		//maybe we have choked the peer, but he hasnt been informed and
		//thats why he send us request, dont drop conn just ignore the req
		return nil
	}
	if bl.len > maxRequestBlockSz || bl.len < minRequestBlockSz {
		return fmt.Errorf("request length out of range: %d", msg.Len)
	}
	//check that we have the requested pice
	if pc.myBf.Get(bl.pc) {
		return fmt.Errorf("peer requested piece we do not have")
	}
	//ensure the we dont exceed the end of the piece
	//TODO: check for the specific piece
	if endOff := bl.off + bl.len; endOff > tmeta.mi.Info.PieceLen {
		return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
	}
	//read from db
	//and write to conn
	pc.eventCh <- uploadedBlock(bl)
	return nil
}

//store block and send another request if request queuer permits it.
func (pc *conn) onPieceMsg(msg *peer_wire.Msg) error {
	var ready block
	var ok bool
	bl := reqMsgToBlock(msg.Request())
	if ready, ok = pc.rq.deleteCompleted(bl); !ok {
		if !pc.t.pieces.valid(bl.pc) {
			return errors.New("peer: piece doesn't exist")
		}
		//peer send us a block we already have
		//TODO:log this
		if pc.myBf.Get(bl.pc) {
			return nil
		}
		var length int
		var err error
		if length, err = pc.t.pieces.pcs[bl.pc].lenSafe(bl.off); err != nil {
			return err
		}
		if length != bl.len {
			return errors.New("peer: wrong msg length")
		}
	}
	if pc.state.canDownload() && (ready != block{}) {
		pc.sendMsg(ready.reqMsg())
	}
	//all good?
	//store at db
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