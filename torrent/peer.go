package torrent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/lkslts64/charo-torrent/peer_wire"
)

//Peer represents a peer connection (like a worker)
//TODO: include mtainfo?
type Peer struct {
	t *Torrent
	//tcp connection with peer
	conn net.Conn
	exts peer_wire.Extensions
	//main goroutine also has this info
	//avoid duplication if we dont access this very often in main goroutine
	//and use mutex
	info peerInfo
	//chanel that we receive msgs to be sent (generally)
	jobCh chan job
	//channel that we send received msgs
	eventCh chan event
	//reset timer when we write to conn
	keepAliveTimer    *time.Timer
	keepAliveInterval time.Duration //2 mins
	keepAliveSendFreq time.Duration //a bit less than 2 mins - conservative
	snubbedTimer      *time.Timer
	//pieces that have been requested from us
	pieces map[uint32]*Piece
	//we use chan as a limiting queue - there is no concurency
	//maybe use something else for efficiency?
	requestQueueCh chan struct{} //size = 10
	reqQueue       queue
	//chan to signal quit to every gouritne a peer has spawned
	quit chan struct{}
	//send err to the main routine of peer
	errCh  chan error
	readCh chan readResult

	stats   PeerStats
	seeding bool
}

type PeerStats struct {
	//mutex needed probably
	uploadedUseful   int
	downloadedUseful int
}

type readResult struct {
	msg *peer_wire.Msg
	err error
}

//TODO:Implement rate limiter with r = 10 b = 10 (anacrolix)
//one case for rate limiter (e.g: case <- WaitN(..,..):) that means
//we should wrap waitN method to return a channel when ready
func (p *Peer) mainLoop() error {
	defer close(p.quit)
	defer p.conn.Close()
	checkEOF := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	p.keepAliveTimer = time.NewTimer(p.keepAliveSendFreq)
	p.snubbedTimer = time.NewTimer(time.Minute)
	for {
		select {
		case <-p.t.done:
		//action came from main routine
		case act := <-p.jobCh:
			p.parseJob(act)
		//msg available from peer
		case res := <-p.readCh:
			if res.err != nil {
				return checkEOF(res.err)
			}
		case err := <-p.errCh:
			return checkEOF(err)
		case <-p.snubbedTimer.C:
			//snubbed
			//choke and go optimistik unchoke
		case <-p.keepAliveTimer.C:
			p.sendMsg(&peer_wire.Msg{
				Kind: peer_wire.KeepAlive,
			})
			p.keepAliveTimer.Reset(p.keepAliveSendFreq)
		}
	}
}

func (p *Peer) parseJob(j job) {
	var err error
	msg := j.val.(peer_wire.Msg)
	switch j.id {
	case amChoking, amUnchoking, amInterested, amNotInterested, have,
		bitfield, cancel, metadataExt:
		p.sendMsg(&msg)
	case request:
	}
	if err != nil {
		//err may be already sent by another goroutine so if has been
		//sent, then we just exit
		trySendErr(err, p.errCh)
	}
}

func (p *Peer) onRequestJob(msg *peer_wire.Msg) {
	//whole block case - usual case
	if int(msg.Len) == p.t.tm.meta.Info.PieceLen && msg.Begin == 0 {
		if _, ok := p.pieces[msg.Index]; ok {
			panic("request piece action already sent")
		}
		p.pieces[msg.Index] = NewPiece(p.t, msg.Index)
		//add all blocks at pending state
		p.pieces[msg.Index].addAllBlocks(uint32(p.t.tm.meta.Info.PieceLen))
		p.reqPiece(msg)
	} else {
		//send only one block req - typically at end game
		if _, ok := p.pieces[msg.Index]; !ok {
			p.pieces[msg.Index] = NewPiece(msg.Index, false)
		}
		if msg.Begin%blockSz != 0 {
			panic(msg)
		}
		p.pieces[msg.Index].addBlock(msg.Begin)
		p.sendMsg(msg)
	}

}

func (p *Peer) reqPiece(msg *peer_wire.Msg) {
	lastBlockSz := blockSz % uint32(p.t.tm.meta.Info.PieceLen)
	numBlocksWithoutLast := uint32(p.t.tm.meta.Info.PieceLen) / blockSz
	push := func(begin, len uint32) {
		p.reqQueue.push(peer_wire.Msg{
			Kind:  peer_wire.Request,
			Index: msg.Index,
			Begin: uint32(begin),
			Len:   uint32(len),
		})
	}
	for i := uint32(0); i < numBlocksWithoutLast; i++ {
		push(i*blockSz, blockSz)
	}
	if lastBlockSz != 0 {
		push(numBlocksWithoutLast*blockSz, lastBlockSz)
	}
	//send as much as possible at requestQueue
loop:
	for !p.reqQueue.empty() {
		select {
		case p.requestQueueCh <- struct{}{}:
			msg := p.reqQueue.pop()
			p.sendMsg(&msg)
		default:
			break loop
		}

	}
}

func (p *Peer) readMsgs() {
	var msg *peer_wire.Msg
	var err error
	readDone := make(chan readResult)
loop:
	for {
		//we won't block on read forever cause conn will close
		//if main goroutine exits
		go func() {
			msg, err = peer_wire.Read(p.conn)
			readDone <- readResult{msg, err}
		}()
		select {
		case <-time.After(p.keepAliveInterval):
			trySendErr(errors.New("peer idle"), p.errCh)
		case p.readCh <- <-readDone:
			if err != nil {
				break loop
			}
		case <-p.quit:
			break loop
		}
	}
}

func (p *Peer) sendMsg(msg *peer_wire.Msg) {
	//is mandatory to stop timer and recv from chan
	//in order to reset it
	if !p.keepAliveTimer.Stop() {
		<-p.keepAliveTimer.C
		p.sendMsg(&peer_wire.Msg{
			Kind: peer_wire.KeepAlive,
		})
	}
	p.keepAliveTimer.Reset(p.keepAliveSendFreq)
	err := msg.Write(p.conn)
	if err != nil {
		trySendErr(err, p.errCh)
	}
}

func (p *Peer) parsePeerMsg(msg peer_wire.Msg, errCh chan error) {
	handleSimpleMsg := func(kind peer_wire.MessageID, infoVal bool, eventVal eventID) bool {
		if infoVal {
			trySendErr(errors.New("peer msg useless resent"), errCh)
			//we will close conn doesn't matter what we send
			return false
		}
		p.eventCh <- event{eventVal, msg}
		return !infoVal
	}
	switch msg.Kind {
	case peer_wire.KeepAlive, peer_wire.Port:
	case peer_wire.Interested:
		p.info.isInterested = handleSimpleMsg(msg.Kind, p.info.isInterested, isInterested)
	case peer_wire.NotInterested:
		p.info.isInterested = handleSimpleMsg(msg.Kind, !p.info.isInterested, isNotInterested)
	case peer_wire.Choke:
		p.info.isChoking = handleSimpleMsg(msg.Kind, p.info.isChoking, isChoking)
	case peer_wire.Unchoke:
		p.info.isChoking = handleSimpleMsg(msg.Kind, !p.info.isChoking, isUnchoking)
	case peer_wire.Piece:
		p.onPieceMsg(msg)
	case peer_wire.Request:
		p.onRequestMsg(msg)
	default:
		trySendErr(errors.New("unknown msg kind received"), errCh)
	}
}

//TODO:Store bad peer so we wont accept them again if they try to reconnect
func (p *Peer) onRequestMsg(msg peer_wire.Msg) error {
	tmeta := p.t.tm
	if !p.info.isDownloader() {
		//maybe we have choked the peer, but he hasnt been informed and
		//thats why he send us request, dont drop conn just ignore the req
		return nil
	}
	if msg.Len > maxRequestBlockSz || msg.Len < minRequestBlockSz {
		return fmt.Errorf("request length is too small or too big: %d", msg.Len)
	}
	//check that we have the specified pice
	tmeta.ownMu.RLock()
	if !tmeta.ownedPieces.hasPiece(msg.Index) {
		return fmt.Errorf("peer requested piece we do not have")
	}
	tmeta.ownMu.RUnlock()
	//ensure the we dont exceed the end of the piece
	if endOff := msg.Begin + msg.Len; endOff > uint32(tmeta.meta.Info.PieceLen) {
		return fmt.Errorf("peer request exceeded length of piece: %d", endOff)
	}
	if !p.canUpload() {
		//choke peer
	}
	//read from db
	//and write to conn
	p.stats.uploadedUseful += int(msg.Len)
	return nil
}

func (p *Peer) canUpload() bool {
	if p.seeding {
		return true
	}
	//if we have uploaded 100KiB more than downloaded (anacrolix)
	if p.stats.uploadedUseful-p.stats.downloadedUseful > (1<<10)*100 {
		return false
	}
	return true
}

func (p *Peer) onPieceMsg(msg peer_wire.Msg) error {
	if !p.snubbedTimer.Stop() {
		<-p.snubbedTimer.C
		//propably choke peer and go for optimistic unchoke
	}
	p.snubbedTimer.Reset(time.Minute)
	var piece *Piece
	var ok bool
	if piece, ok = p.pieces[msg.Index]; !ok {
		return fmt.Errorf("hadn't requested this piece: %d", msg.Index)
	}
	if _, ok = piece.pendingBlocks[msg.Begin]; !ok {
		return fmt.Errorf("hadn't requested this block: %d", msg.Begin)
	}
	//all good?

	p.stats.downloadedUseful += int(msg.Len)
	//store at db
	//free one slot at request Queue and add one
	select {
	case <-p.requestQueueCh:
	default:
		panic("received piece but request queue was empty")
	}
	if !p.reqQueue.empty() {
		select {
		case p.requestQueueCh <- struct{}{}:
			msg := p.reqQueue.pop()
			p.sendMsg(&msg)
		default:
			panic("received piece but request queue is full")
		}
	}
	delete(piece.pendingBlocks, msg.Begin)
	if len(piece.pendingBlocks) == 0 {
		//notify master that we downloaded what was requested
		if piece.whole {
			//mark complete

		}
	}
	return nil
}

type peerInfo struct {
	amInterested bool
	amChoking    bool
	isInterested bool
	isChoking    bool
}

func (pi *peerInfo) isDownloader() bool {
	return !pi.amChoking && pi.isInterested
}

type queue []peer_wire.Msg

func (q *queue) push(msg peer_wire.Msg) {
	*q = append(*q, msg)
}

func (q *queue) peek() peer_wire.Msg {
	return (*q)[0]
}

func (q *queue) pop() (head peer_wire.Msg) {
	head = (*q)[0]
	*q = (*q)[1:]
	return
}

func (q *queue) empty() bool {
	return len(*q) == 0
}
