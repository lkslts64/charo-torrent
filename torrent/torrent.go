package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/tracker"
)

//Active conns are the ones that we download/upload from/to
//Passive are the ones that we are not-interested/choking or not-interested/choked

//Every conn is managed on a seperate goroutine

//Concurency:
//We have one PaireventID chan. All conn goroutines
//send data to master goroutine to signify Events.

//Every goroutine is associated with a chan of Events.
//When master gouritine wants to change state of a particular
//conn goroutine it sends an Event through this channel.

//Event signals that something happend
//at a peer connection, like the client
//is snubed or it was dropped.
type Event int

var maxConns = 55

var blockSz = uint32(1 << 14)           //16KiB
var maxRequestBlockSz = uint32(1 << 15) //32KiB
//spec doesn't say anything about this by we enfore it
var minRequestBlockSz = uint32(1 << 13) //16KiB

//Torrent
type Torrent struct {
	tm *TorrentMeta
	//concurrent access at conns. Only nil conns
	//get modified concurently so readers of non-nil conns can
	//have access without acquiring locks.Non nil
	//conns will be set to zero only by the goroutine that
	//manages them so no prob
	connMu   sync.Mutex
	numConns int
	peers    []Peer
	//TODO: a slice of Peer
	/*conns       []net.Conn
	chans       []chan Event
	extensions  []peer_wire.Extensions
	peersStatus []peerInfo*/
	//bit map of peer's owned pieces
	bitFields []BitField

	eventCh chan event

	//surely we 'll need this
	done chan struct{}
}

//Useful for master goroutine
/*case choke, unchoke, interested, notInterested:
	(&peer_wire.Msg{
		Kind: peer_wire.MessageID(a.id),
	}).Write(p.conn)
case have:
	i := a.val.(uint32)
	(&peer_wire.Msg{
		Kind:  peer_wire.MessageID(a.id),
		Index: i,
	}).Write(p.conn)
case bitfield:
	bf := a.val.(BitField)
	(&peer_wire.Msg{
		Kind:     peer_wire.MessageID(a.id),
		Bitfield: []byte(bf),
	}).Write(p.conn)
case request, cancel:
*/

type readResult struct {
	msg *peer_wire.Msg
	err error
}

//should close conn on exit - avoids leaks
func (t *Torrent) peerMain(i int) error {
	peer := t.peers[i]
	const readChanSz = 10
	const requestQueueSz = 10
	//reading goroutine sends results with this chan
	readCh := make(chan readResult, readChanSz)
	//in order to terminate reading goroutine
	quit := make(chan struct{})
	//queuing of requests
	requestCh := make(chan struct{}, requestQueueSz)
	//first error we catch we abandon and we
	//broadcast with quit so rest of goroutines exit
	errCh := make(chan error, 1)
	defer close(quit) //notify reader goroutine and writers
	//defer close(peer.actCh) //notify master
	defer peer.conn.Close()
	//read msgs until err happens
	go peer.readMsgs(readCh, quit)
	ticker := time.NewTicker(peer.keepAliveInterval)
	defer ticker.Stop()
	lastRead := time.Now()
	checkEOF := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}
	for {
		select {
		case act := <-peer.actCh:
			go t.proccessAction(act, peer, errCh, requestCh, quit)
		case err := <-errCh:
			return checkEOF(err)
		case <-t.done:
			return nil
		case res := <-readCh:
			if res.err != nil {
				return checkEOF(res.err)
			}
			lastRead = time.Now()
			//shouldn't block
			//<-requestCh
			//process msg
			//if msg type == data
		case t := <-ticker.C:
			(&peer_wire.Msg{
				Kind: peer_wire.KeepAlive,
			}).Write(peer.conn)
			if t.Sub(lastRead) >= peer.keepAliveInterval &&
				t.Sub(peer.lastKeepAlive) >= peer.keepAliveInterval {
				return nil //drop conn
			}
		}
	}
}

func (t *Torrent) proccessAction(a action, p Peer, errCh chan error,
	reqCh, quit chan struct{}) {
	var err error
	msg := a.val.(peer_wire.Msg)
	switch a.id {
	case amChoking, amUnchoking, amInterested, amNotInterested, have,
		bitfield, cancel, metadataExt:
		err = msg.Write(p.conn)
	case request:
		if v, ok := p.pieces[msg.Index]; ok {
			panic(v)
		}
		p.pieces[msg.Index] = NewPiece(msg.Index)
		//whole block case - usual case
		if int(msg.Len) == t.tm.meta.Info.PieceLen && msg.Begin == 0 {
			//add all blocks at pending state
			p.pieces[msg.Index].addAllBlocks(uint32(t.tm.meta.Info.PieceLen))
			err = t.reqPiece(&msg, p, reqCh, quit, errCh)
		} else {
			//send only one block req - typically at eng game
			if msg.Begin%blockSz != 0 {
				panic(msg)
			}
			p.pieces[msg.Index].addBlock(msg.Begin)
			err = msg.Write(p.conn)
		}
	}
	if err != nil {
		//err may be already sent by another goroutine so if has been
		//sent, then we just exit
		trySendErr(err, errCh)
	}
}

func (t *Torrent) reqPiece(msg *peer_wire.Msg, p Peer, reqCh, quit chan struct{},
	errCh chan<- error) error {
	lastBlockSz := blockSz % t.tm.meta.Info.PieceLen
	numBlocksWithoutLast := t.tm.meta.Info.PieceLen / blockSz

	req := func(begin, len uint32) error {
		//try push to request queue or quit if err from another
		//goroutine happend
		select {
		case reqCh <- struct{}{}:
			err := (&peer_wire.Msg{
				Kind:  peer_wire.Request,
				Index: msg.Index,
				Begin: uint32(begin),
				Len:   uint32(len),
			}).Write(p.conn)
			if err != nil {
				return err
			}
			p.pieces
		case <-quit:
		}
		return nil
	}

	for i := 0; i < uint32(t.tm.meta.Info.PieceLen)/blockSz; i++ {
		err := req(uint32(i)*blockSz, uint32(blockSz))
		if err != nil {
			return err
		}
	}
	if lastBlockSz != 0 {
		err := req(uint32(numBlocksWithoutLast*blockSz), uint32(lastBlockSz))
		if err != nil {
			return err
		}
	}
	return nil

}

func trySendErr(err error, errCh chan error) {
	select {
	case errCh <- err:
	default:
	}
}

func (t *Torrent) proccessPeerMsg(p *Peer, msg peer_wire.Msg, errCh chan error) {
	switch msg.Kind {
	case peer_wire.KeepAlive:
		p.lastKeepAlive = time.Now()
	case peer_wire.Interested:
		//update our local copy and main goroutine's
		if p.info.isInterested {
			trySendErr(fmt.Errorf("peer msg useless resent: %b", isInterested), errCh)
		}
		p.info.isInterested = true
		p.eventCh <- event{isInterested, msg}
	case peer_wire.NotInterested:
		if !p.info.isInterested {
			trySendErr(fmt.Errorf("peer msg useless resent: isInterested: %b", isInterested), errCh)
		}
		p.info.isInterested = false
		p.eventCh <- event{isNotInterested, msg}
	case peer_wire.Choke:
		if p.info.isChoking {
			trySendErr(fmt.Errorf("peer msg useless resent: isChoking: %b", isChoking), errCh)
		}
		p.info.isChoking = true
		p.eventCh <- event{isChoking, msg}
	case peer_wire.Unchoke:
		if !p.info.isChoking {
			trySendErr(fmt.Errorf("peer msg useless resent: isChoking: %b", isChoking), errCh)
		}
		p.info.isChoking = false
		p.eventCh <- event{isUnchoking, msg}
	case peer_wire.Piece:

		//store at db
		//TODO: check when a piece is complete? we need some infrastructure for this

		p.lastPieceMsg = time.Now()
	case peer_wire.Request:
		if p.info.amChoking || !p.info.isInterested {
			trySendErr(errors.New("peer requested data without being interested or unchoked"), errCh)
		}
		if msg.Len > maxRequestBlockSz {
			trySendErr(errors.New("request exceeded maximum requested block size"), errCh)
		}
		if msg.Len < minRequestBlockSz {
			trySendErr(errors.New("request exceeded minimum requested block size"), errCh)
		}
		//check that we have the specified pice
		t.tm.ownMu.RLock()
		if !t.tm.ownedPieces.hasPiece(msg.Index) {
			trySendErr(errors.New("peer requested piece we do not have"), errCh)
		}
		t.tm.ownMu.RUnlock()
		//ensure the we dont exceed the end of the piece
		if msg.Begin+msg.Len > uint32(t.tm.meta.Info.PieceLen) {
			trySendErr(errors.New("peer request exceeded length of piece"), errCh)
		}
		//read from db
		//and write to conn
	}
}

func NewTorrent(filename string) (*Torrent, error) {
	t, err := NewTorrentMeta(filename)
	if err != nil {
		return nil, err
	}
	return &Torrent{
		tm:    t,
		conns: make([]net.Conn, maxConns),
		chans: make([]chan Event, maxConns),
		actCh: make(chan interface{}, maxConns),
	}, nil
}

func (t *Torrent) addConn(conn net.Conn) (int, error) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.numConns >= maxConns {
		return -1, errors.New("maxConns reached")
	}
	t.numConns++
	//Fill one of the gaps of this slice
	var index int
	for i, c := range t.conns {
		if c == nil {
			c = conn
			index = i
			return index, nil
		}
	}
	panic("maxConns reached")
}

func (t *Torrent) removeConn(i int) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	t.numConns--
	t.conns[i] = nil
}

type PairEventID struct {
	ID    int
	event Event
}

//TorrentMeta stores metadata and stats
//about the torrent
type TorrentMeta struct {
	//nil if we dont have it
	//TODO:If we dont have it,one conn should be responsible for
	//getting the meta.Info part with metadata ext.
	//This conn should set the metaSize too
	//Restrict access to metainfo before we get the
	//whole meta.Info part.
	meta     *metainfo.MetaInfo
	metaSize int64

	trackerURL tracker.TrackerURL

	//length of data to be downloaded
	length int64

	//when a piece gets downloaded or uploaded, these fields are updated
	dataStatsMu sync.Mutex
	left        int64
	downloaded  int64
	//TODO:different mutex for upload?
	uploaded int64
	//TODO:make []bool and move marshaling into peer_wire?
	//access by peer conns and main goroutine concurently
	//cache of downloaded pieces - dont ask db every time
	ownMu       sync.RWMutex
	ownedPieces BitField
}

func NewTorrentMeta(filename string) (*TorrentMeta, error) {
	meta, err := metainfo.LoadMetainfoFile(filename)
	if err != nil {
		return nil, err
	}
	tURL, err := tracker.NewTrackerURL(meta.Announce)
	if err != nil {
		return nil, err
	}
	total := int64(meta.Info.TotalLength())
	return &TorrentMeta{
		meta:        meta,
		trackerURL:  tURL,
		length:      total,
		left:        total,
		ownedPieces: make([]byte, int(math.Ceil(float64(meta.Info.NumPieces())/8.0))),
	}, nil

}

//i == index of piece
func (t *TorrentMeta) addDownloaded(i int) {
	pieceLen := t.PieceLen(i)
	t.dataStatsMu.Lock()
	defer t.dataStatsMu.Unlock()
	t.downloaded += pieceLen
	t.left -= pieceLen
	//index holds the byte of pieceBitfield we are interested.
	//mask holds the bit of the byte we want to set
	t.ownedPieces.setPiece(i)
}

func (t *TorrentMeta) addUploaded(i int) {
	pieceLen := t.PieceLen(i)
	t.dataStatsMu.Lock()
	defer t.dataStatsMu.Unlock()
	t.uploaded += pieceLen
}

func (t *TorrentMeta) PieceLen(i int) (pieceLen int64) {
	numPieces := t.meta.Info.NumPieces()
	//last piece case
	if i == t.meta.Info.NumPieces()-1 {
		pieceLen = int64(numPieces*t.meta.Info.PieceLen) - t.length
	} else {
		pieceLen = int64(t.meta.Info.PieceLen)
	}
	return
}

func (t *TorrentMeta) announceTracker(event tracker.Event, port int16) (*tracker.AnnounceResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.dataStatsMu.Lock()
	req := tracker.AnnounceReq{
		InfoHash:   t.meta.Info.Hash,
		PeerID:     peerID,
		Downloaded: t.downloaded,
		Left:       t.left,
		Uploaded:   t.uploaded,
		Event:      event,
		Numwant:    -1,
		Port:       port,
	}
	t.dataStatsMu.Unlock()
	return t.trackerURL.Announce(ctx, req)
}

func (t *TorrentMeta) scrapeTracker() (*tracker.ScrapeResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return t.trackerURL.Scrape(ctx, t.meta.Info.Hash)
}
