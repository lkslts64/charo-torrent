package torrent

import (
	"errors"
	"sort"
	"sync"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

const (
	randomPiecePickingStrategyThreshold = 1
)

//TODO:reconsider how we store pcs - maybe a map would be better?
//and store them depending on their state (dispatched,unrequested etc).
type pieces struct {
	t           *Torrent
	ownedPieces bitmap.Bitmap
	//every conn can call getRequests and discardRequests.This mutex protects the fields
	//that these methods access.
	mu       sync.Mutex
	pcs      []*Piece
	selector PieceSelector
	//pieces sorted by their priority.All verified pieces are not present in this slice
	prioritizedPcs []*Piece
	endGame        bool
	// caps the number of different pieces that are requested simultaneously.
	maxInFlightPieces int
	//if false we shouldn't make any requests
	downloadAllowed bool
}

func newPieces(t *Torrent) *pieces {
	numPieces := int(t.numPieces())
	pcs := make([]*Piece, numPieces)
	for i := 0; i < numPieces; i++ {
		pcs[i] = newPiece(t, i)
	}
	sorted := make([]*Piece, numPieces)
	copy(sorted, pcs)
	p := &pieces{
		t:                 t,
		pcs:               pcs,
		prioritizedPcs:    sorted,
		selector:          t.cl.config.SelectorF(),
		maxInFlightPieces: t.numPieces(),
	}
	p.selector.SetTorrent(t)
	return p
}

func (p *pieces) onHave(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pcs[i].rarity++
}

func (p *pieces) onBitfield(bm bitmap.Bitmap) {
	p.mu.Lock()
	defer p.mu.Unlock()
	bm.IterTyped(func(piece int) bool {
		p.pcs[piece].rarity++
		return true
	})
}

func (p *pieces) setDownloadAllowed() {
	p.mu.Lock()
	p.downloadAllowed = true
	p.mu.Unlock()
}

//fills the provided slice with requests. Returns how many were filled.
func (p *pieces) fillRequests(peerPieces bitmap.Bitmap, requests []block) (n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.downloadAllowed {
		return
	}
	if p.maxInFlightPieces <= 0 {
		return
	}
	pending := filterPieces(p.pcs, func(p *Piece) bool {
		return p.PendingBlocks() > 0
	})
	if len(pending) > p.maxInFlightPieces {
		panic("we shouldn't have that much pending pieces")
	}
	unrequested := filterPieces(p.pcs, func(p *Piece) bool {
		return p.UnrequestedBlocks() > 0 && peerPieces.Get(p.index)
	})
	if len(unrequested) == 0 {
		return
	}
	//reorder pieces based on selector Less func.
	sort.Slice(unrequested, func(i, j int) bool {
		p1 := unrequested[i]
		p2 := unrequested[j]
		return p.selector.Less(p1, p2)
	})
	inFlightToAdd := p.maxInFlightPieces - len(pending)
	unrequested = filterPieces(unrequested, func(p *Piece) bool {
		if p.PendingBlocks() > 0 {
			return true
		}
		inFlightToAdd--
		//this piece has zero pending blocks and if we already have
		//(or we are going to schedule) p.maxInFlightRecs requests in flight
		//then don't request it
		return inFlightToAdd >= 0
	})
	var unreq []block
	total, _n := len(requests), 0
	for _, piece := range unrequested {
		unreq = piece.unrequestedBlocksSlc(len(requests))
		if !p.endGame {
			for _, b := range unreq {
				piece.setBlockPending(b.off)
			}
		}
		_n = copy(requests, unreq)
		n += _n
		if n > total {
			panic("filled with more requests than conn wanted")
		}
		if n == total {
			//got as many requests as asked for
			return
		}
		requests = requests[_n:]
	}
	return
}

/*
// Insertion sort
func insertionSort(data Interface, a, b int) {
	for i := a + 1; i < b; i++ {
		for j := i; j > a && data.Less(j, j-1); j-- {
			data.Swap(j, j-1)
		}
	}
}
*/

func (p *pieces) discardRequests(requests []block) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, req := range requests {
		p.pcs[req.pc].setBlockUnrequsted(req.off)
	}
}

func (p *pieces) setBlockComplete(i int, off int, ci *connInfo) {
	var transitionIntoEndGame bool
	piece := p.pcs[i]
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		//send to channels without acquiring the lock
		//
		if piece.complete() {
			p.t.queuePieceForHashing(i)
		}
		if transitionIntoEndGame {
			p.t.broadcastToConns(requestsAvailable{})
			//No need to send cancels just after entered end game
			return
		}
		if p.endGame {
			p.t.sendCancels(block{
				pc:  i,
				off: off,
				len: piece.blockLen(off),
			})
		}
	}()
	piece.setBlockComplete(ci, off)
	if p.maybeStartEndgame() {
		transitionIntoEndGame = true
	}
}

func (p *pieces) pieceHashed(i int, correct bool) {
	if correct {
		p.ownedPieces.Set(i, true)
		p.pcs[i].verificationSuccess()
	}
	p.mu.Lock()
	if correct {
		//Call this after unlocking to avoid starvation of other goroutines
		defer p.selector.OnPieceDownload(i)
		//remove the piece i from priotirzedPcs, we'll never make requests for it again.
		for j, piece := range p.prioritizedPcs {
			if piece.index == i {
				p.prioritizedPcs = append(p.prioritizedPcs[:j], p.prioritizedPcs[j+1:]...)
				break
			}
		}
	} else {
		p.pcs[i].verificationFailed()
	}
	p.mu.Unlock()
}

func (p *pieces) maybeStartEndgame() bool {
	if p.endGame || !p.allRequested() {
		return false
	}
	p.endGame = true
	notOwned := bitmap.Flip(p.ownedPieces, 0, p.t.numPieces())
	notOwned.IterTyped(func(piece int) bool {
		p.pcs[piece].setAllUnrequested()
		return true
	})
	return true
}

//return a slice containing how many blocks a piece has.
//Index i corresponds at piece i
func (p *pieces) blocks() []int {
	ret := []int{}
	for _, piece := range p.pcs {
		ret = append(ret, piece.blocks)
	}
	return ret
}

func (p *pieces) isValid(i int) bool {
	if p == nil {
		panic("isValid: nil")
	}
	return i >= 0 && i < len(p.pcs)
}

func (p *pieces) isLast(i int) bool {
	return i == len(p.pcs)-1
}

func (p *pieces) haveAll() bool {
	return p.ownedPieces.Len() == p.t.numPieces()
}

//true if all the pieces have been requested (or completed).
func (p *pieces) allRequested() bool {
	for _, piece := range p.pcs {
		if piece.hasUnrequestedBlocks() {
			return false
		}
	}
	return true
}

func filterPieces(pieces []*Piece, f func(*Piece) bool) []*Piece {
	ret := []*Piece{}
	for _, p := range pieces {
		if f(p) {
			ret = append(ret, p)
		}
	}
	//TODO: maybe this escapes to heap and it is bad practice?
	return ret
}

//Piece represents a single bitTorrent piece. A piece is divided in blocks.
//A piece's block can be at one of the following three states: unrequested,pending or completed.
//All blocks are initially unrequested.When we request a block,
//then it becomes pending and when we download it it becomes completed.
type Piece struct {
	t        *Torrent
	index    int
	verified bool
	//num of blocks
	blocks       int
	lastBlockLen int
	rarity       int
	//keeps track of which blocks are not requested for download
	unrequestedBlocks bitmap.Bitmap
	//keeps track of which blocks we have been downloaded
	completeBlocks bitmap.Bitmap
	//conns that we have received blocks for this piece
	//TODO: ban a conn with most maliciousness (see conn_stats.go)
	//on verification failure.
	contributors []*connInfo
}

//Index return the piece's index
func (p *Piece) Index() int {
	return p.index
}

//Torrent returns the underlying Torrent of the piece
func (p *Piece) Torrent() *Torrent {
	return p.t
}

// Rarity returns how many peers in the swarm have been known to own this piece. Note that is
// only an approximation and we can't know for sure the exact number of peers owning
// a piece.
func (p *Piece) Rarity() int {
	return p.rarity
}

//Verified returns wheter the piece is downloaded and verified or not
func (p *Piece) Verified() bool {
	return p.verified
}

//Blocks returns the total blocks of the piece
func (p *Piece) Blocks() int {
	return p.blocks
}

//PendingBlocks returns the number of pendingBlocks the piece has
func (p *Piece) PendingBlocks() int {
	return p.blocks - (p.unrequestedBlocks.Len() + p.completeBlocks.Len())
}

//UnrequestedBlocks returns the number of unrequested the piece has
func (p *Piece) UnrequestedBlocks() int {
	return p.unrequestedBlocks.Len()
}

//CompletedBlocks returns the number of completed the piece has
func (p *Piece) CompletedBlocks() int {
	return p.completeBlocks.Len()
}

func newPiece(t *Torrent, i int) *Piece {
	pieceLen := t.pieceLen(uint32(i))
	lastBlockLen := t.blockRequestSize
	var extra int
	if lastBytes := pieceLen % t.blockRequestSize; lastBytes != 0 {
		extra = 1
		lastBlockLen = lastBytes
	}
	blocks := pieceLen/t.blockRequestSize + extra
	//set all blocks to unrequested
	var unrequestedBlocks bitmap.Bitmap
	for j := 0; j < blocks; j++ {
		unrequestedBlocks.Set(j*t.blockRequestSize, true)
	}
	return &Piece{
		t:                 t,
		index:             i,
		unrequestedBlocks: unrequestedBlocks,
		blocks:            blocks,
		lastBlockLen:      lastBlockLen,
	}
}

//length of block at offset off
func (p *Piece) blockLen(off int) int {
	bl := off / p.t.blockRequestSize
	if bl == p.blocks-1 {
		return p.lastBlockLen
	}
	return p.t.blockRequestSize
}

var errLargeOffset = errors.New("offset too big")

var errDivOffset = errors.New("offset remainder with block size is not zero")

//length of block at offset off
func (p *Piece) blockLenSafe(off int) (int, error) {
	if off%p.t.blockRequestSize != 0 {
		return 0, errDivOffset
	}
	switch bl := int(off / p.t.blockRequestSize); {
	case bl == p.blocks-1:
		return p.lastBlockLen, nil
	case bl < p.blocks-1:
		return int(p.t.blockRequestSize), nil
	default:
		return 0, errLargeOffset
	}
}

func (p *Piece) msgRequest(off int) *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: uint32(p.index),
		Begin: uint32(off),
		Len:   uint32(p.blockLen(off)),
	}
}

func (p *Piece) toBlock(off int) block {
	return block{
		pc:  p.index,
		off: off,
		len: p.blockLen(off),
	}
}
func (p *Piece) unrequestedBlocksSlc(limit int) (blocks []block) {
	count := 0
	p.unrequestedBlocks.IterTyped(func(off int) bool {
		if count < limit {
			blocks = append(blocks, p.toBlock(off))
			count++
			return true
		}
		return false
	})
	return
}

func (p *Piece) hasUnrequestedBlocks() bool {
	return p.unrequestedBlocks.Len() > 0
}

func (p *Piece) pendingGet(off int) bool {
	return !(p.unrequestedBlocks.Get(off) || p.completeBlocks.Get(off))
}

//assume block was in pending before
func (p *Piece) setBlockUnrequsted(off int) {
	//if another conn got the block, there is nothing to do.
	if !p.completeBlocks.Get(off) {
		p.unrequestedBlocks.Set(off, true)
	}
}

func (p *Piece) setBlockPending(off int) {
	//if another conn got the block, there is nothing to do.
	if !p.completeBlocks.Get(off) {
		p.unrequestedBlocks.Set(off, false)
	}
}

func (p *Piece) setBlockComplete(ci *connInfo, off int) {
	p.completeBlocks.Set(off, true)
	//offset could be in unrequested if we received an unexpected block
	p.unrequestedBlocks.Set(off, false)
	p.contributors = append(p.contributors, ci)
}

func (p *Piece) setAllUnrequested() {
	for i := 0; i < p.blocks; i++ {
		p.setBlockUnrequsted(i * p.t.blockRequestSize)
	}
}

func (p *Piece) complete() bool {
	return p.completeBlocks.Len() == p.blocks
}

func (p *Piece) verificationFailed() {
	for _, c := range p.contributors {
		c.stats.badPiecesContributions++
	}
	if !p.complete() {
		panic("pieceVerificationFailed: piece was not complete")
	}
	p.unrequestedBlocks, p.completeBlocks = p.completeBlocks, p.unrequestedBlocks
	p.contributors = []*connInfo{}
}

func (p *Piece) verificationSuccess() {
	if p.verified {
		panic("already verified")
	}
	p.verified = true
	for _, c := range p.contributors {
		c.stats.goodPiecesContributions++
	}
}
