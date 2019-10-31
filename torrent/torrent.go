package torrent

import (
	"context"
	"errors"
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

	actCh chan action

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

//should close conn on exit - avoids leaks
func (t *Torrent) peerMain(i int) error {
	peer := t.peers[i]
	const readChanSz = 10
	type readResult struct {
		msg *peer_wire.Msg
		err error
	}
	readCh := make(chan readResult, readChanSz)
	quit := make(chan struct{})
	defer close(quit)
	defer peer.conn.Close()
	/*go func() {
		var err error
		var msg *peer_wire.Msg
		readDone := make(chan struct{})
		for {
			go func() {
				msg, err = peer_wire.Read(peer.conn)
				readDone <- struct{}{}
			}()
			select {
			case <-readDone:
				if err != nil {
					readErr <- err
				}
				//process msg
			}
		}
	}()*/
	go func() {
		for {
			//we won't block on read forever cause conn will close
			//if main goroutine exits
			msg, err := peer_wire.Read(peer.conn)
			//we pass error and main goroutine exits so quit
			//chan closes and then we break we enter the 2nd case
			//(eventually)
			select {
			case readCh <- readResult{msg, err}:
			case <-quit:
				break
			}
		}
	}()
	keepAliveInterval := 2 * time.Minute
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	lastRead := time.Now()
	for {
		select {
		case act := <-t.actCh:
			err := t.proccessAction(act, peer)
			if err != nil {
				if !errors.Is(err, io.EOF) &&
					!errors.Is(err, io.ErrUnexpectedEOF) {
					return err
				}
				return nil
			}
		case <-t.done:
			return nil
		case res := <-readCh:
			if res.err != nil {
				if !errors.Is(res.err, io.EOF) &&
					!errors.Is(res.err, io.ErrUnexpectedEOF) {
					return res.err
				}
				return nil
			}
			lastRead = time.Now()
			//process msg
		case t := <-ticker.C:
			(&peer_wire.Msg{
				Kind: peer_wire.KeepAlive,
			}).Write(peer.conn)
			if t.Sub(lastRead) >= keepAliveInterval {
				return nil //drop conn
			}
		}
	}
}

func (t *Torrent) proccessAction(a action, p Peer) (err error) {
	msg := a.val.(peer_wire.Msg)
	switch a.id {
	case choke, unchoke, interested, notInterested, have,
		bitfield, cancel, metadataExt:
		err = msg.Write(p.conn)
	case request:
		if int(msg.Len) == t.tm.meta.Info.PieceLen {
			//download whole block
		}
		err = msg.Write(p.conn)
		//send only req

	}
	return
}

func (t *Torrent) proccessMsg(p Peer) {

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
	pieceBitfield BitField
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
		meta:          meta,
		trackerURL:    tURL,
		length:        total,
		left:          total,
		pieceBitfield: make([]byte, int(math.Ceil(float64(meta.Info.NumPieces())/8.0))),
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
	t.pieceBitfield.setPiece(i)
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
