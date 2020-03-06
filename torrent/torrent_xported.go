package torrent

import (
	"errors"
	"io"
	"strings"
)

//AddPeers establishes connections with peers.
func (t *Torrent) AddPeers(peers ...Peer) error {
	ch, err := t.locker.lock()
	if err != nil {
		return err
	}
	defer t.locker.unlock(ch, struct{}{})
	t.gotPeers(peers)
	return nil
}

//Swarm returns all known peers associated with this torrent.
func (t *Torrent) Swarm() ([]Peer, error) {
	ch, err := t.locker.lock()
	if err != nil {
		return nil, err
	}
	defer t.locker.unlock(ch, struct{}{})
	return t.swarm(), nil
}

//Download downloads all the torrent's data. It requires the info first.
func (t *Torrent) Download() error {
	if err := t.download(); err != nil {
		return err
	}
	select {
	case <-t.downloadedData:
		return nil
	case <-t.closed:
		return errors.New("torrent closed")
	}
}

func (t *Torrent) download() error {
	ch, err := t.locker.lock()
	if err != nil {
		return err
	}
	if t.downloadRequest {
		return errors.New("already downloading")
	}
	t.downloadRequest = true
	defer t.locker.unlock(ch, struct{}{})
	return nil
}

//WriteStatus writes to w a human readable message about the status of the Torrent.
func (t *Torrent) WriteStatus(w io.Writer) error {
	ch, err := t.locker.lock()
	if err != nil {
		return err
	}
	defer t.locker.unlock(ch, struct{}{})
	b := new(strings.Builder)
	t.writeStatus(b)
	w.Write([]byte(b.String()))
	return nil
}

//Close closes all connections with peers.When called, all methods on this torrent return errors.
//Close is safe to be called multiple times on the same torrent.
func (t *Torrent) Close() {
	if err := t.closeWithLock(); err != nil {
		return
	}
	t.cl.dropTorrent(t.mi.Info.Hash)
}

func (t *Torrent) closeWithLock() error {
	ch, err := t.locker.lock()
	if err != nil {
		return err
	}
	defer t.locker.unlock(ch, struct{}{})
	t.close()
	return nil
}
