package torrent

import "github.com/lkslts64/charo-torrent/peer_wire"

import "sync"

type condQueue struct {
	q    blockQueue
	cond sync.Cond
}

func (cq *condQueue) push(bl block) {
	cq.q.push(bl)
}

func (cq *condQueue) peek() block {
	return cq.q.peek()
}

func (cq *condQueue) pop() block {
	return cq.q.pop()
}

func (cq *condQueue) empty() bool {
	return cq.q.empty()
}

func (cq *condQueue) full() bool {
	return cq.q.full()
}

type queue struct {
	msgs []*peer_wire.Msg
	len  int
}

func newQueue(len int) *queue {
	return &queue{
		len: len,
	}
}

func (q *queue) push(msg *peer_wire.Msg) bool {
	if !q.full() {
		q.msgs = append(q.msgs, msg)
		return true
	}
	return false
}

func (q *queue) peek() (head *peer_wire.Msg) {
	if q.empty() {
		return
	}
	head = q.msgs[0]
	return
}

func (q *queue) pop() (head *peer_wire.Msg) {
	if q.empty() {
		return
	}
	head = q.msgs[0]
	q.msgs = q.msgs[1:]
	q.msgs[0] = nil
	return
}

func (q *queue) clear() {
	q.msgs = []*peer_wire.Msg{}
}

func (q *queue) empty() bool {
	return len(q.msgs) == 0
}

func (q *queue) full() bool {
	return len(q.msgs) == q.len
}

type blockQueue struct {
	blocks []block
	len    int
}

func newBlockQueue(len int) *blockQueue {
	return &blockQueue{
		len: len,
	}
}

func (q *blockQueue) push(bl block) bool {
	if !q.full() {
		q.blocks = append(q.blocks, bl)
		return true
	}
	return false
}

func (q *blockQueue) peek() (head block) {
	if q.empty() {
		return
	}
	head = q.blocks[0]
	return
}

func (q *blockQueue) pop() (head block) {
	if q.empty() {
		return
	}
	head = q.blocks[0]
	q.blocks = q.blocks[1:]
	return
}

func (q *blockQueue) clear() {
	q.blocks = []block{}
}

func (q *blockQueue) empty() bool {
	return len(q.blocks) == 0
}

func (q *blockQueue) full() bool {
	return len(q.blocks) == q.len
}
