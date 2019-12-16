package torrent

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/eapache/channels"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/tevino/abool"
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

//PeerConn represents a peer connection (like a worker)
//TODO: peer_wire.Msg should be passed through pointers everywhere
type PeerConn struct {
	cl *Client
	t  *Torrent
	//tcp connection with peer
	conn net.Conn
	exts peer_wire.Extensions
	//main goroutine also has this info - needs to be synced between
	//the two goroutines
	info peerInfo
	//chanel that we receive msgs to be sent (generally)
	//jobCh chan job - infinite chan
	//we want to avoid deadlocks between jobCh and eventCh.
	//Also we dont want master to block until workers(peerConns)
	//receive jobs (they are doing heavy I/O).
	//jobs distributed to each peerConn are controlled by
	//master so the latter will try to limit the rate of them
	jobCh channels.Channel
	//channel that we send received msgs
	eventCh chan interface{}
	//reset timer when we write to conn
	keepAliveTimer *time.Timer
	rq             requestQueuer //front size = 10, back = 40
	amSeeding      bool
	haveMetadata   bool
	canDownload    *abool.AtomicBool
	peerBfMu       sync.Mutex
	peerBf         peer_wire.BitField
	isSeeding      bool
	myBf           peer_wire.BitField
}

type readResult struct {
	msg *peer_wire.Msg
	err error
}

func (pc *PeerConn) init() {
	//pc.jobCh = make(chan job, jobChSize)
	pc.jobCh = channels.NewInfiniteChannel()
	pc.eventCh = make(chan interface{}, eventChSize)
}

func (pc *PeerConn) close() {
	pc.conn.Close()
	//send event that we closed conn
	close(pc.eventCh)
}

func (pc *PeerConn) mainLoop() error {
	var err error
	//pc.initChans()
	readCh := make(chan readResult, readChSize)
	quit, idle := make(chan struct{}), make(chan struct{})
	defer pc.close()
	defer close(quit)
	go pc.readMsgs(readCh, idle, quit)
	go pc.readRequestJobs(quit)
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
		//msg available from peer
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
			}).Write(pc.conn)
			if err != nil {
				return err
			}
			pc.keepAliveTimer.Reset(keepAliveSendFreq)
		}
	}
}

//read msgs from remote peer
//run on seperate goroutine
func (pc *PeerConn) readMsgs(readCh chan<- readResult, idle, quit chan struct{}) {
	var msg *peer_wire.Msg
	var err error
	readDone := make(chan readResult)
loop:
	for {
		//we won't block on read forever cause conn will close
		//if main goroutine exits
		go func() {
			msg, err = peer_wire.Read(pc.conn)
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

//acquiring two locks + atomic operation at this call but this is not a
//bottleneck since only two goroutines are sharing data
func (pc *PeerConn) canProcessRequest(bl block) bool {
	pc.peerBfMu.Lock()
	defer pc.peerBfMu.Unlock()
	return pc.peerBf.HasPiece(bl.pc) && !pc.rq.full() &&
		pc.canDownload.IsSet()
}

//run on seperate goroutine
func (pc *PeerConn) readRequestJobs(quit chan struct{}) {
	var bl block
	gotBlock := make(chan block)
loop:
	for {
		go func() {
			cqueue := &pc.t.pieces.cqueue
			cqueue.cond.L.Lock()
			for cqueue.empty() || !pc.canProcessRequest(cqueue.peek()) {
				cqueue.cond.Wait()
			}
			bl = cqueue.pop()
			cqueue.cond.L.Unlock()
			gotBlock <- bl
		}()
		select {
		case b := <-gotBlock:
			var ok bool
			var ready bool
			if ok, ready = pc.rq.queue(b); !ok {
				panic("request queuer full")
			}
			if ready {
				pc.jobCh.In() <- b
			}
		case <-quit:
			break loop
			//TODO:signal to stop if we downloaded all pieces
		}
	}
}

func (pc *PeerConn) sendKeepAlive() error {
	err := (&peer_wire.Msg{
		Kind: peer_wire.KeepAlive,
	}).Write(pc.conn)
	if err != nil {
		return err
	}
	pc.keepAliveTimer.Reset(keepAliveSendFreq)
	return nil
}

func (pc *PeerConn) parseJob(j job) (err error) {
	switch v := j.val.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested:
			pc.info.amInterested = true
			if !pc.info.isChoking {
				pc.canDownload.Set()
			}
		case peer_wire.NotInterested:
			pc.canDownload.UnSet()
			pc.info.amInterested = false
		case peer_wire.Choke:
			pc.info.amChoking = true
		case peer_wire.Unchoke:
			pc.info.amChoking = false
		case peer_wire.Have:
			pc.myBf.SetPiece(v.Index)
			//Surpress have msgs.
			//From https://wiki.theory.org/index.php/BitTorrentSpecification:
			//At the same time, it may be worthwhile to send a HAVE message
			//to a peer that has that piece already since it will be useful in
			//determining which piece is rare.
			if pc.peerBf.HasPiece(v.Index) {
				return
			}
		case peer_wire.Bitfield:
			pc.myBf = v.Bf
		case peer_wire.Request:
		}
		err = pc.sendMsg(v)
	}
	return
}

func (pc *PeerConn) sendMsg(msg *peer_wire.Msg) error {
	//is mandatory to stop timer and recv from chan
	//in order to reset it
	if !pc.keepAliveTimer.Stop() {
		<-pc.keepAliveTimer.C
		err := (&peer_wire.Msg{
			Kind: peer_wire.KeepAlive,
		}).Write(pc.conn)
		if err != nil {
			return err
		}
	}
	pc.keepAliveTimer.Reset(keepAliveSendFreq)
	err := msg.Write(pc.conn)
	if err != nil {
		return err
	}
	return nil
}

func (pc *PeerConn) parsePeerMsg(msg *peer_wire.Msg) (err error) {
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
		pc.info.amInterested = stateChange(pc.info.amInterested, true)
	case peer_wire.NotInterested:
		pc.info.amInterested = stateChange(pc.info.amInterested, false)
	case peer_wire.Choke:
		pc.info.amChoking = stateChange(pc.info.amChoking, true)
		pc.sendUnsatisfiedRequests()
		pc.canDownload.UnSet()
	case peer_wire.Unchoke:
		pc.info.amChoking = stateChange(pc.info.amChoking, false)
		if pc.info.amInterested {
			pc.canDownload.Set()
		}
	case peer_wire.Piece:
		err = pc.onPieceMsg(msg)
	case peer_wire.Request:
		err = pc.upload(msg)
	case peer_wire.Have:
		if pc.peerBf.HasPiece(msg.Index) {
			//log this
		}
		pc.peerBf.SetPiece(msg.Index)
		pc.eventCh <- msg
	case peer_wire.Bitfield:
		if pc.peerBf != nil {
			err = errors.New("peer: send bitfield twice")
		}
		if pc.haveMetadata {
			if !pc.bitFieldValid() {
				err = errors.New("peer: wrong bitfield length")
				return
			}
		}
		func() {
			pc.peerBfMu.Lock()
			defer pc.peerBfMu.Unlock()
			pc.peerBf = msg.Bf
		}()
		pc.eventCh <- msg
	case peer_wire.Extended:
		pc.onExtended(msg)
	default:
		err = errors.New("unknown msg kind received")
	}
	return
}

//we should have metadata to call this
func (pc *PeerConn) bitFieldValid() bool {
	numPieces := pc.t.tm.mi.Info.NumPieces()
	//no need to lock peerBf - no one will modify it
	return len(pc.peerBf) != int(math.Ceil(float64(numPieces)/8.0))
}

type metainfoSize int64

func (pc *PeerConn) onExtended(msg *peer_wire.Msg) (err error) {
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
			if !pc.haveMetadata {
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

func (pc *PeerConn) sendUnsatisfiedRequests() {
	if !pc.rq.empty() {
		unsatisfiedRequests := pc.rq.discardAll()
		pc.eventCh <- unsatisfiedRequests
	}
}

//TODO:Store bad peer so we wont accept them again if they try to reconnect
func (pc *PeerConn) upload(msg *peer_wire.Msg) error {
	tmeta := pc.t.tm
	if !pc.info.canUpload() {
		//maybe we have choked the peer, but he hasnt been informed and
		//thats why he send us request, dont drop conn just ignore the req
		return nil
	}
	if msg.Len > maxRequestBlockSz || msg.Len < minRequestBlockSz {
		return fmt.Errorf("request length out of range: %d", msg.Len)
	}
	//check that we have the requested pice
	if pc.myBf.HasPiece(msg.Index) {
		return fmt.Errorf("peer requested piece we do not have")
	}
	//ensure the we dont exceed the end of the piece
	//TODO: check for the specific piece
	if endOff := msg.Begin + msg.Len; endOff > uint32(tmeta.mi.Info.PieceLen) {
		return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
	}
	//read from db
	//and write to conn
	pc.eventCh <- uploadedBlock(reqMsgToBlock(msg))
	return nil
}

func (pc *PeerConn) onPieceMsg(msg *peer_wire.Msg) error {
	var ready block
	var ok bool
	bl := reqMsgToBlock(msg.Request())
	if ready, ok = pc.rq.deleteCompleted(bl); !ok {
		if !pc.t.pieces.valid(bl.pc) {
			return errors.New("peer: piece doesn't exist")
		}
		//peer send us a block we already have
		//TODO:log this
		if pc.myBf.HasPiece(msg.Index) {
			return nil
		}
		var length int
		var err error
		if length, err = pc.t.pieces.pcs[bl.pc].lenSafe(bl.off); err != nil {
			return err
		}
		if uint32(length) != bl.len {
			return errors.New("peer: wrong msg length")
		}
	}
	if (ready != block{}) {
		pc.sendMsg(ready.reqMsg())
	}
	//all good?
	//store at db
	pc.eventCh <- downloadedBlock(reqMsgToBlock(msg))
	return nil
}

//TODO:sync.atomic ??
type peerInfo struct {
	amInterested bool
	amChoking    bool
	isInterested bool
	isChoking    bool
}

func (pi *peerInfo) canUpload() bool {
	return !pi.amChoking && pi.isInterested
}

func (pi *peerInfo) canDownload() bool {
	return !pi.isChoking && pi.amInterested
}

//TODO: master hold piece struct and manages them.
//master sends to peer conns block requests
type block struct {
	pc  uint32
	off uint32
	len uint32
}

func (b *block) reqMsg() *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: b.pc,
		Begin: b.off,
		Len:   b.len,
	}
}

func reqMsgToBlock(msg *peer_wire.Msg) block {
	return block{
		pc:  msg.Index,
		off: msg.Begin,
		len: msg.Len,
	}
}

type downloadedBlock block
type uploadedBlock block

/*func (b *block) requestMsg() *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: b.pc,
		Begin: b.off,
		Len:   b.len,
	}
}*/
