package torrent

//this is the locking mechanism when someone wants to invoke exported methods on a
//Torrent. Synchronization is implemented with channels
type torrentLocker struct {
	t      *Torrent
	syncCh chan interface{}
	v      interface{}
	closed bool
}

func (l *torrentLocker) lock() {
	l.syncCh = make(chan interface{})
	select {
	case <-l.t.closed:
		l.closed = true
	case l.t.userCh <- l.syncCh:
	}
}

func (l *torrentLocker) unlock() {
	if l.closed {
		//optimization
		return
	}
	select {
	case <-l.t.closed: //fires only if Close methods is invoked.
	case l.syncCh <- l.v:
	}
}

func (l *torrentLocker) setV(v interface{}) {
	l.v = v
}
