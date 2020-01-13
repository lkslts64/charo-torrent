package storage

import "sync"

type piece struct {
	blocks int
	//The blocks of the piece we have written.
	mu          sync.Mutex
	dirtyBlocks map[int64]struct{}
	//true if piece is hashed and result is correct. Protected by `mu`.
	verified bool
}

func (p *piece) reserveOffset(off int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.dirtyBlocks[off]; ok || p.verified {
		return false
	}
	p.dirtyBlocks[off] = struct{}{}
	return true
}

func (p *piece) readyForVerification() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dirtyBlocks) == p.blocks
}

func (p *piece) markComplete() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.verified {
		panic("storage: already verified")
	}
	p.verified = true
	//clear dirtiers
	p.dirtyBlocks = make(map[int64]struct{})
}

func (p *piece) markNotComplete() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verified = false
	//clear dirtiers
	p.dirtyBlocks = make(map[int64]struct{})
}

func (p *piece) isVerified() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verified
}
