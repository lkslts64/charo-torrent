package torrent

//this is the locking mechanism when someone wants to invoke exported methods on a
//Torrent. Synchronization is implemented with channels
type torrentLocker struct {
	t      *Torrent
	syncC  chan interface{}
	v      interface{}
	closed bool
}

func (l *torrentLocker) lock() {
	l.syncC = make(chan interface{})
	select {
	case <-l.t.ClosedC:
		l.closed = true
	case l.t.userC <- l.syncC:
	}
}

func (l *torrentLocker) unlock() {
	if l.closed {
		//optimization
		return
	}
	select {
	case <-l.t.ClosedC: //fires only if Close methods is invoked.
	case l.syncC <- l.v:
	}
}

func (l *torrentLocker) setV(v interface{}) {
	l.v = v
}
