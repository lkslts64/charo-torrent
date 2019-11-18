package torrent

import (
	"errors"
	"fmt"
	"github.com/eapache/channels"
	"io"
	"net"
	"time"

	"github.com/lkslts64/charo-torrent/peer_wire"
)

const (
	requestQueueChSize = 10
	readChSize         = 50
	jobChSize          = 50
	eventChSize        = 50
)

const (
	keepAliveInterval time.Duration = 2 * time.Minute //2 mins
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
	eventCh chan event
	//reset timer when we write to conn
	keepAliveTimer *time.Timer
	lastPieceMsg   time.Time
	//snubbed        bool
	//we use chan as a limiting queue - there is no concurency
	//maybe use something else for efficiency?
	requestQueueCh chan struct{} //size = 10
	reqQueue       queue
	//maybe store msg and not pointers to msgs (we can check for duplicates)
	pendingRequests map[*peer_wire.Msg]struct{}
	stats           PeerConnStats
	amSeeding       bool
	haveMetadata    bool
}

type PeerConnStats struct {
	uploadedUseful   int
	downloadedUseful int
}

func (pcs *PeerConnStats) updateUsefulDownload(bytes int) {
	pcs.downloadedUseful += bytes
}

func (pcs *PeerConnStats) updateUsefulUpload(bytes int) {
	pcs.uploadedUseful += bytes
}

type readResult struct {
	msg *peer_wire.Msg
	err error
}

/*func NewPeerConn() *PeerConn {

}*/

func (pc *PeerConn) init() {
	//pc.jobCh = make(chan job, jobChSize)
	pc.jobCh = channels.NewInfiniteChannel()
	pc.eventCh = make(chan event, eventChSize)
	pc.requestQueueCh = make(chan struct{}, requestQueueChSize)
	pc.pendingRequests = make(map[*peer_wire.Msg]struct{})
}

func (pc *PeerConn) mainLoop() error {
	var err error
	//pc.initChans()
	readCh := make(chan readResult, readChSize)
	quit, idle := make(chan struct{}), make(chan struct{})
	//TODO:wrap all this at peerconn close
	//-----------------
	defer close(quit)
	defer close(pc.eventCh)
	defer pc.conn.Close()
	//-----------------
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
		case <-time.After(2 * time.Minute):
			close(idle)
			break loop
		case readCh <- <-readDone:
			if err != nil {
				break loop
			}
		case <-quit:
			break loop
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
		case peer_wire.NotInterested:
			pc.info.amInterested = false
		case peer_wire.Choke:
			pc.info.amChoking = true
		case peer_wire.Unchoke:
			pc.info.amChoking = false
		case peer_wire.Request:
			if !pc.onRequestBlockJob(v) {
				err = nil
				return
			}
		}
		err = pc.sendMsg(v)
	}
	return
}

//TODO:optimize:master can send multipl reqs at once?
func (pc *PeerConn) onRequestBlockJob(msg *peer_wire.Msg) bool {
	if !pc.info.canDownload() {
		//send unsatisfied
		pc.eventCh <- event{msg}
		return false
	}
	pc.pendingRequests[msg] = struct{}{}
	select {
	//msg can be sent immediately
	case pc.requestQueueCh <- struct{}{}:
		return true
	//we should wait because we have max reqeust on flight
	default:
		pc.reqQueue.push(msg)
		return false
	}
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
	handleStateMsg := func(infoVal bool, evnt eventID) bool {
		if infoVal {
			//we will close conn doesn't matter what we send
			//peer send msg that did not change our state (maybe is bad peer?)
			return infoVal
		}
		pc.eventCh <- event{evnt}
		return !infoVal
	}
	switch msg.Kind {
	case peer_wire.KeepAlive, peer_wire.Port:
	case peer_wire.Interested:
		pc.info.isInterested = handleStateMsg(pc.info.isInterested, isInterested)
	case peer_wire.NotInterested:
		pc.info.isInterested = handleStateMsg(!pc.info.isInterested, isNotInterested)
	case peer_wire.Choke:
		//discard pending reqs
		pc.onChokeMsg()
		pc.info.isChoking = handleStateMsg(pc.info.isChoking, isChoking)
	case peer_wire.Unchoke:
		pc.info.isChoking = handleStateMsg(!pc.info.isChoking, isUnchoking)
	case peer_wire.Piece:
		err = pc.onPieceMsg(msg)
	case peer_wire.Request:
		err = pc.onRequestMsg(msg)
	case peer_wire.Have, peer_wire.Bitfield:
		pc.eventCh <- event{msg}
	case peer_wire.Extended:
		pc.onExtended(msg)
	default:
		err = errors.New("unknown msg kind received")
	}
	return
}

type metainfoSize int64

func (pc *PeerConn) onExtended(msg *peer_wire.Msg) (err error) {
	switch v := msg.ExtendedMsg.(type) {
	case peer_wire.ExtHandshakeDict:
		pc.exts, err = v.Extensions()
		if _, ok := pc.exts[peer_wire.ExtMetadataName]; ok {
			if msize, ok := v.MetadataSize(); ok {
				pc.eventCh <- event{metainfoSize(msize)}
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

//discard pending and queued requests
//and send to master what we didn't manage
//to download as a slice of requests
func (pc *PeerConn) onChokeMsg() {
	if pc.info.canDownload() {
		pc.reqQueue.clear()
		if len(pc.pendingRequests) > 0 {
			unsatisfiedRequests := make([]*peer_wire.Msg, len(pc.pendingRequests))
			var i int
			for req := range pc.pendingRequests {
				unsatisfiedRequests[i] = req
				i++
			}
			pc.eventCh <- event{unsatisfiedRequests}
		}
	}
}

//TODO:Store bad peer so we wont accept them again if they try to reconnect
func (pc *PeerConn) onRequestMsg(msg *peer_wire.Msg) error {
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
	if tmeta.ownPiece(msg.Index) {
		return fmt.Errorf("peer requested piece we do not have")
	}
	//ensure the we dont exceed the end of the piece
	if endOff := msg.Begin + msg.Len; endOff > uint32(tmeta.mi.Info.PieceLen) {
		return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
	}
	//read from db
	//and write to conn
	pc.stats.updateUsefulUpload(int(msg.Len))
	//TODO:make event for upload
	pc.eventCh <- event{uploadedBlock(reqMsgToBlock(msg))}
	return nil
}

func (pc *PeerConn) onPieceMsg(msg *peer_wire.Msg) error {
	req := msg.Request()
	var notRequestedButUseful bool
	if _, ok := pc.pendingRequests[req]; !ok {
		//TODO: log it
		if pc.t.tm.ownPiece(msg.Index) {
			//we dont need this
			return nil
		}
		//we can store at db, god gave us a gift
		notRequestedButUseful = true
		//return nil
		//return fmt.Errorf("hadn't requested this block: %v", msg)
	}
	//all good?
	pc.lastPieceMsg = time.Now()
	pc.stats.updateUsefulDownload(int(msg.Len))
	//store at db
	//free one slot at request Queue and add one
	//if we had requested this block
	if !notRequestedButUseful {
		select {
		case <-pc.requestQueueCh:
		default:
			panic("received piece but request queue was empty")
		}
		if !pc.reqQueue.empty() {
			select {
			case pc.requestQueueCh <- struct{}{}:
				msg := pc.reqQueue.pop()
				pc.sendMsg(msg)
			default:
				panic("received piece but request queue is full")
			}
		}
		//remove from pending
		delete(pc.pendingRequests, req)
	}
	//notify master
	pc.eventCh <- event{downloadedBlock(reqMsgToBlock(msg))}
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

type queue []*peer_wire.Msg

func (q *queue) push(msg *peer_wire.Msg) {
	*q = append(*q, msg)
}

func (q *queue) peek() *peer_wire.Msg {
	if q.empty() {
		return nil
	}
	return (*q)[0]
}

func (q *queue) pop() (head *peer_wire.Msg) {
	if q.empty() {
		return
	}
	head = (*q)[0]
	*q = (*q)[1:]
	return
}

func (q *queue) empty() bool {
	return len(*q) == 0
}

func (q *queue) clear() {
	q = new(queue)
}

//TODO: master hold piece struct and manages them.
//master sends to peer conns block requests
type block struct {
	pc  uint32
	off uint32
}

func reqMsgToBlock(msg *peer_wire.Msg) *block {
	return &block{
		pc:  msg.Index,
		off: msg.Begin,
	}
}

type downloadedBlock *block
type uploadedBlock *block

/*func (b *block) requestMsg() *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: b.pc,
		Begin: b.off,
		Len:   b.len,
	}
}*/
