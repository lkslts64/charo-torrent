package torrent

import (
	"io"
	"strings"
)

//AddPeers establishes connections with peers.
func (t *Torrent) AddPeers(peers ...Peer) {
	switch t.state {
	case Added:
	case Closed:
		return
	case Running:
		ch := t.locker.lock()
		defer t.locker.unlock(ch)
	default:
		panic("unknown state")
	}
	t.gotPeers(peers)
}

func (t *Torrent) Download() <-chan struct{} {
	t.state = Running
	go t.mainLoop()
	return t.downloadedAll
}

//WriteStatus writes to w a human readable message about the status of the Torrent.
func (t *Torrent) WriteStatus(w io.Writer) {
	switch t.state {
	case Added:
	case Closed:
		return
	case Running:
		ch := t.locker.lock()
		defer t.locker.unlock(ch)
	default:
		panic("unknown state")
	}
	b := new(strings.Builder)
	t.writeStatus(b)
	w.Write([]byte(b.String()))
}

//dificult to use this due to type safety
func (t *Torrent) withLockContext(f func()) {
	switch t.state {
	case Added:
	case Closed:
		return
	case Running:
		ch := t.locker.lock()
		defer t.locker.unlock(ch)
	}
	f()
}
