package torrent

import "errors"

//this is the locking mechanism when someone wants to call exported methods on a
//Torrent. Locking is implementing with channels
type torrentLocker struct {
	ch     chan chan interface{}
	closed chan struct{}
}

func (l *torrentLocker) lock() (chan interface{}, error) {
	ch := make(chan interface{})
	select {
	//wait until mainLoop is notified that we want to take ownership of the data
	case l.ch <- ch:
		return ch, nil
	case <-l.closed:
		return nil, errors.New("torrent is closed")
	}
}

func (l *torrentLocker) unlock(ch chan interface{}, v interface{}) {
	if ch == nil {
		panic("tlocker: unlock")
	}
	//notify mainLoop that we finished
	ch <- v
}
