package torrent

import (
	"errors"
	"io"
	"strings"

	"github.com/lkslts64/charo-torrent/metainfo"
)

var errTorrentClosed = errors.New("torrent closed")
var errMetainfoNotAvailable = errors.New("metainfo not yet available")

//AddPeers adds peers to the Torrent and (if needed) tries to
//establish connections with them. Returns error if the torrent is closed.
func (t *Torrent) AddPeers(peers ...Peer) error {
	l := t.newLocker()
	if l.lock(); l.closed {
		return errTorrentClosed
	}
	defer l.unlock()
	t.gotPeers(peers)
	return nil
}

//Swarm returns all known peers associated with this torrent.
func (t *Torrent) Swarm() []Peer {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	return t.swarm()
}

//TransferData enables downloading/uploading the torrent's data.
// It should be called once for each Torrent.
// It requires the info first.
//After the download is complete, the Torrent transits in seeding mode
//(i.e altruistically upload) until it's closed.
//If the data are already there,TransferData returns immediatly and
//seeds the torrent.
func (t *Torrent) TransferData() error {
	if err := t.transferData(); err != nil {
		return err
	}
	select {
	case <-t.downloadedDataC:
		return nil
	case <-t.closed:
		return errTorrentClosed
	}
}

func (t *Torrent) transferData() error {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	if l.closed {
		return errTorrentClosed
	}
	if !t.haveInfo() {
		return errors.New("can't download data without having the info first")
	}
	if t.uploadEnabled || t.downloadEnabled {
		return errors.New("already downloading data or seeding")
	}
	t.uploadEnabled, t.downloadEnabled = true, true
	if !t.infoWasDownloaded() && len(t.conns) > 0 {
		panic("why have conns?")
	}
	if !t.haveAll() {
		//notify conns to start downloading
		t.pieces.setDownloadEnabled(true)
		t.broadcastToConns(requestsAvailable{})
	}
	t.tryAnnounceAll()
	t.dialConns()
	return nil
}

//WriteStatus writes to w a human readable message about the status of the Torrent.
func (t *Torrent) WriteStatus(w io.Writer) {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	b := new(strings.Builder)
	t.writeStatus(b)
	w.Write([]byte(b.String()))
}

//Metainfo returns the metainfo of the torrent or nil of its not available
func (t *Torrent) Metainfo() *metainfo.MetaInfo {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	return t.mi
}

//HaveAllPieces returns whether all torrent's data has been downloaded
func (t *Torrent) HaveAllPieces() bool {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	return t.haveAll()
}

//Seeding returns true if `HaveAllPieces` returns true and a call to `Download` has been made
//for this torrent
func (t *Torrent) Seeding() bool {
	l := t.newLocker()
	l.lock()
	if l.closed {
		return false
	}
	defer l.unlock()
	return t.seeding()
}

func (t *Torrent) Stats() Stats {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	return t.stats
}

//Pieces returns all pieces of the torrent. If Info is not available or t is closed it returns nil.
func (t *Torrent) Pieces() []Piece {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	if t.pieces == nil {
		return nil
	}
	p := t.pieces
	p.mu.Lock()
	defer p.mu.Unlock()
	ret := make([]Piece, len(p.pcs))
	for i, piece := range p.pcs {
		ret[i] = *piece
	}
	return ret
}

/*
// SetMaxInFlightPieces caps the number of different pieces that are requested simultaneously
// requested at any given time. It requires that Info is available.
// For unlimited, provide n >= t.NumPieces(). Provide n == 0 to stop all outgouing requests.
// Initially this value is set to t.NumPieces().
func (t *Torrent) SetMaxInFlightPieces(n int) error {
	if n < 0 {
		return errors.New("n should be positive")
	}
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	if t.pieces == nil {
		return errors.New("info required or torrent is closed")
	}
	p := t.pieces
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.maxInFlightPieces == n {
		return nil
	}
	if p.maxInFlightPieces > t.numPieces() {
		n = t.numPieces()
	}
	if p.maxInFlightPieces == 0 {
		// notify conns that they can start requesting again.
		// It doesn't matter that we set the value after the broadcast,
		// conns won't see it in the meantime as we have the lock acquired.
		t.broadcastToConns(requestsAvailable{})
	}
	p.maxInFlightPieces = n
	return nil
}
*/

// EnableDataDownload re-enables downloading the torrent's data.
// To bootstrap the data downloading, one should call TransferData first.
// It is an error to call this before calling TransferData.
// Usually EnableDataDownload is called after a call to DisableDataDownload.
func (t *Torrent) EnableDataDownload() error {
	return t.setDataDownloadEnable(true)
}

// DisableDataDownload pauses the process of downloading the torrent's data.
// It is an error to call this before calling TransferData.
func (t *Torrent) DisableDataDownload() error {
	return t.setDataDownloadEnable(false)
}

func (t *Torrent) setDataDownloadEnable(v bool) error {
	l := t.newLocker()
	l.lock()
	defer l.unlock()
	if t.pieces == nil {
		return errors.New("info required or torrent is closed")
	}
	p := t.pieces
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setDownloadEnabled(v)
	if p.downloadEnabled == v {
		return nil
	}
	p.downloadEnabled = v
	for _, c := range t.conns {
		if v {
			c.interested()
		} else {
			c.notInterested()
		}
	}
	if v {
		t.broadcastToConns(requestsAvailable{})
	}
	return nil
}

//Closed returns whether the torrent is closed or not.
func (t *Torrent) Closed() bool {
	select {
	case <-t.closed:
		return true
	default:
		return false
	}
}

//Close removes the torrent from the Client and closes all connections with peers.
//Close is safe to be called multiple times on the same torrent.
func (t *Torrent) Close() {
	if err := t.closeWithLock(); err != nil {
		return
	}
	t.cl.dropTorrent(t.mi.Info.Hash)
}

func (t *Torrent) closeWithLock() error {
	l := t.newLocker()
	l.lock()
	if l.closed {
		return errTorrentClosed
	}
	defer l.unlock()
	t.close()
	return nil
}
