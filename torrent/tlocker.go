package torrent

//this is the locking mechanism when someone wants to call exported methods on a
//Torrent.
type torrentLocker struct {
	ch chan chan struct{}
}

func (l *torrentLocker) lock() chan struct{} {
	ch := make(chan struct{})
	l.ch <- ch
	return ch
}

func (l *torrentLocker) unlock(ch chan struct{}) {
	ch <- struct{}{}
}
