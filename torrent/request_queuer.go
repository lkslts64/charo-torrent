package torrent

const maxOnFlight = 10

type requestQueuer struct {
	onFlight map[block]struct{}
	pending  *blockQueue
}

func (rq *requestQueuer) queue(bl block) (ready, ok bool) {
	switch {
	case len(rq.onFlight) < maxOnFlight:
		rq.onFlight[bl] = struct{}{}
		ready, ok = true, true
	case !rq.pending.full():
		rq.pending.push(bl)
		ok = true
	}
	return
}

func (rq *requestQueuer) deleteCompleted(bl block) (ready block, ok bool) {
	if ok = rq.frontRemove(bl); !ok {
		return
	}
	if rq.pending.empty() {
		return
	}
	ready = rq.pending.pop()
	rq.onFlight[ready] = struct{}{}
	return
}

func (rq *requestQueuer) discardAll() []block {
	blocks := make([]block, len(rq.pending.blocks))
	copy(blocks, rq.pending.blocks)
	rq.pending.clear()
	for req := range rq.onFlight {
		blocks = append(blocks, req)
	}
	rq.onFlight = make(map[block]struct{})
	return blocks
}

func (rq *requestQueuer) needMore() bool {
	return rq.pending.empty()
}

//lock is held during this call
func (rq *requestQueuer) frontRemove(bl block) bool {
	if rq.frontContains(bl) {
		delete(rq.onFlight, bl)
		return true
	}
	return false
}

//lock is held during this call
func (rq *requestQueuer) frontContains(bl block) bool {
	_, ok := rq.onFlight[bl]
	return ok
}

func (rq *requestQueuer) empty() bool {
	return len(rq.onFlight) == 0 && rq.pending.empty()
}

func (rq *requestQueuer) full() bool {
	return len(rq.onFlight) == maxOnFlight && rq.pending.full()
}
