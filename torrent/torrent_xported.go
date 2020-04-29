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

//Download downloads all the torrent's data. It requires the info first.
//After the download is complete, the Torrent transitions in seeding mode
//(i.e altruistically upload) until it's closed.
//If the data are already there,Download returns immediatly and Torrent
//transists in seeding mode.
func (t *Torrent) Download() error {
	if err := t.download(); err != nil {
		return err
	}
	select {
	case <-t.downloadedDataC:
		return nil
	case <-t.closed:
		return errTorrentClosed
	}
}

func (t *Torrent) download() error {
	l := t.newLocker()
	l.lock()
	if l.closed {
		return errTorrentClosed
	}
	defer l.unlock()
	if !t.haveInfo() {
		return errors.New("can't download data without having the info first")
	}
	if t.downloadRequest {
		return errors.New("already downloading data or seeding")
	}
	t.downloadRequest = true
	if !t.infoWasDownloaded() && len(t.conns) > 0 {
		panic("why have conns?")
	}
	if !t.haveAll() {
		//notify conns to start downloading
		t.pieces.setDownloadAllowed()
		t.broadcastCommand(requestsAvailable{})
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
