package torrent

import (
	"errors"
	"net"
	"time"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

//Peer represents a peer connection (like a worker)
//TODO: include mtainfo?
type Peer struct {
	conn net.Conn
	exts peer_wire.Extensions
	//main goroutine also has this info
	//avoid duplication if we dont access this very often in main goroutine
	//and use mutex
	info peerInfo
	//chanel that we receive msgs to be sent (generally)
	actCh chan action
	//channel that we send received msgs
	eventCh           chan event
	lastKeepAlive     time.Time
	keepAliveInterval time.Duration //2 mins
	meta              *metainfo.MetaInfo
	pieces            map[uint32]*Piece
	//useful for snubb
	lastPieceMsg time.Time
}

func (p Peer) readMsgs(readCh chan readResult, quit chan struct{}) {
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
		//case <-time.After(time.Minute):
		//if peer is active?
		//is snubbed?
		//choke peer
		case readCh <- <-readDone:
			if err != nil {
				break loop
			}
		case <-quit:
			break loop
		}
	}
}

type Piece struct {
	index uint32
	//keeps track of which blocks are pending for download
	pendingBlocks map[uint32]bool
	//keeps track of which blocks we have downloaded
	downloadedBlocks map[int]bool
}

func NewPiece(i uint32) *Piece {
	return &Piece{
		index:         i,
		pendingBlocks: make(map[uint32]bool),
		//maybe we dont need this
		downloadedBlocks: make(map[int]bool),
	}
}

func (p *Piece) addBlock(off uint32) { p.pendingBlocks[off] = true }

func (p *Piece) addAllBlocks(pieceSz uint32) {
	isLastSmaller := pieceSz%blockSz != 0
	var extra uint32
	if isLastSmaller {
		extra = 1
	}
	for i := uint32(0); i < pieceSz/blockSz+extra; i++ {
		p.pendingBlocks[i*blockSz] = true
	}
}

func (p *Piece) removeBlock(off uint32) error {
	if _, ok := p.pendingBlocks[off]; !ok {
		return errors.New("this block doesnt exists in pending list")
	}
	delete(p.pendingBlocks, off)
	return nil
}

type peerInfo struct {
	amInterested bool
	amChoking    bool
	isInterested bool
	isChoking    bool
}
