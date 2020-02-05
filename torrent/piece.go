package torrent

import (
	"errors"
	"math/rand"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring"
	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

//The strategy for piece picking. In order for a piece to be picked, all piece's blocks
//should be unrequested.Selects a piece for a conn to download.
//'ok' set to false means no piece in peerPieces has all blocks unrequested,
//so we cant pick any.
//
//The elemnts in the provided slice will be rearranged according to the strategy in use.
type requestStrategy func(pieces []int)

//TODO:reconsider how we store pcs - maybe a map would be better?
//and store them depending on their state (dispatched,unrequested etc).
type pieces struct {
	t *Torrent
	//every conn can call getRequests and discardRequests.This mutex protects the fields
	//that these methods access.
	mu                 sync.Mutex
	pcs                []*piece
	unrequested        bitmap.Bitmap
	partiallyRequested bitmap.Bitmap
	//random until we get the first piece, rarest onwards
	//TODO:maybe pick one by one? insteadof sorting or permute every time?
	strategy    requestStrategy
	ownedPieces bitmap.Bitmap
}

//TODO:add allVerified boolean at constructor parameter
func newPieces(t *Torrent) *pieces {
	numPieces := int(t.numPieces())
	//TODO:shuffle pieces
	pcs := make([]*piece, numPieces)
	for i := 0; i < numPieces; i++ {
		pcs[i] = newPiece(t, i)
	}
	var (
		unrequested bitmap.Bitmap
	)
	unrequested.AddRange(0, t.numPieces())
	p := &pieces{
		t:                  t,
		pcs:                pcs,
		unrequested:        unrequested,
		partiallyRequested: bitmap.Bitmap{RB: roaring.NewBitmap()},
	}
	p.strategy = shuffle
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

func (p *pieces) getRequests(peerPieces bitmap.Bitmap, requests []block) (n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	//first fill with blocks that we already have requested and if they
	//are not sufficient, fill with the fully unrequested ones.
	pieces := bitmap.Bitmap{RB: roaring.And(peerPieces.RB, p.partiallyRequested.RB)}.ToSortedSlice()
	if n = p.fillWithRequests(pieces, requests); n == len(requests) {
		//got as many requests as asked for
		return
	}
	pieces = bitmap.Bitmap{RB: roaring.And(peerPieces.RB, p.unrequested.RB)}.ToSortedSlice()
	//prioritize piece picking by our current strategy
	p.strategy(pieces)
	n += p.fillWithRequests(pieces, requests[n:])
	return
}

//fills the provided slice with requests. Returns how many were filled.
func (p *pieces) fillWithRequests(pieces []int, requests []block) (n int) {
	var unreq []block
	total, _n := len(requests), 0
	for _, i := range pieces {
		piece := p.pcs[i]
		unreq = piece.unrequestedBlocksSlc(len(requests))
		for _, b := range unreq {
			p.addBlockPending(b.pc, b.off)
		}
		_n = copy(requests, unreq)
		n += _n
		if n > total {
			panic("read requests")
		}
		if n == total {
			//got as many requests as asked for
			return
		}
		requests = requests[_n:]
	}
	return
}

//pick the rarest pieces first.
//store results in pieces arg.
func (p *pieces) sortByRarity(pieces []int) {
	unreq := make([]*piece, len(pieces))
	for i := 0; i < len(unreq); i++ {
		unreq[i] = p.pcs[pieces[i]]
	}
	sort.Slice(unreq, func(i, j int) bool {
		return unreq[i].rarity < unreq[j].rarity
	})
	for i, p := range unreq {
		pieces[i] = p.index
	}
}

//pick randomly
func shuffle(pieces []int) {
	_pieces := make([]int, len(pieces))
	copy(_pieces, pieces)
	perm := rand.Perm(len(pieces))
	for i, randPieceIndex := range perm {
		pieces[i] = _pieces[randPieceIndex]
	}
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
		p.addBlockUnrequsted(req.pc, req.off)
	}
}

func (p *pieces) markBlockComplete(i int, off int, ci *connInfo) {
	piece := p.pcs[i]
	p.mu.Lock()
	piece.markBlockComplete(ci, off)
	p.reviewPieceState(i)
	p.mu.Unlock()
	if piece.complete() {
		p.t.queuePieceForHashing(i)
	}
}

func (p *pieces) addBlockUnrequsted(i, off int) {
	piece := p.pcs[i]
	if piece.addBlockUnrequsted(off) {
		p.reviewPieceState(i)
	}
}

func (p *pieces) addBlockPending(i, off int) {
	piece := p.pcs[i]
	if piece.addBlockPending(off) {
		p.reviewPieceState(i)
	}
}

func (p *pieces) reviewPieceState(i int) {
	var partially, unrequested bool
	switch piece := p.pcs[i]; {
	case piece.allBlocksUnrequested():
		unrequested = true
	case piece.isPartialyRequested():
		partially = true
	default:
	}
	p.unrequested.Set(i, unrequested)
	p.partiallyRequested.Set(i, partially)
}

func (p *pieces) pieceSuccesfullyVerified(i int) {
	p.pcs[i].verificationSuccess()
	//change strategy after completion of first block
	if p.ownedPieces.Len() == 0 {
		func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.strategy = p.sortByRarity
		}()
	}
	p.ownedPieces.Set(i, true)
}

func (p *pieces) pieceVerificationFailed(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pcs[i].verificationFailed()
	p.reviewPieceState(i)
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

func (p *pieces) valid(i int) bool {
	return i >= 0 && i < len(p.pcs)
}

func (p *pieces) isLast(i int) bool {
	return i == len(p.pcs)-1
}

func (p *pieces) allVerified() bool {
	for _, piece := range p.pcs {
		if !piece.verified {
			return false
		}
	}
	return true
}

func (p *pieces) hasUnrequestedBlocks() bool {
	for i := 0; i < len(p.pcs); i++ {
		if p.pcs[i].hasUnrequestedBlocks() {
			return true
		}
	}
	return false
}

func (p *pieces) hasUnrequestedPieces() bool {
	for i := 0; i < len(p.pcs); i++ {
		if p.pcs[i].allBlocksUnrequested() {
			return true
		}
	}
	return false
}

//piece is constrcuted by master and is managed entirely
//by him.Blocks can be at three states and each block lies
//on a single state at each time. All blocks are initially
//unrequested.When we request a block through the workers,
// then a block becomes pending and after downloaded.
//If a worker gets choked and has requests unsatisfied
//then master puts them again at unrequested state
//At all cases,this equality should hold true:
//len(pendingBlocks)+len(downloadedBlocks)+len(unrequestedBlocks) == totalBlocks
type piece struct {
	t        *Torrent
	index    int
	verified bool
	//hash  []byte
	//num of blocks
	blocks       int
	lastBlockLen int
	rarity       int
	//keeps track of which blocks are not requested for download
	unrequestedBlocks bitmap.Bitmap
	//keeps track of which blocks are pending for download
	pendingBlocks bitmap.Bitmap
	//keeps track of which blocks we have been downloaded
	completeBlocks bitmap.Bitmap
	//conns that we have received blocks for this piece
	//TODO: ban a conn with most maliciousness (see conn_stats.go)
	//on verification failure.
	contributors []*connInfo
}

func newPiece(t *Torrent, i int) *piece {
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
	return &piece{
		t:                 t,
		index:             i,
		unrequestedBlocks: unrequestedBlocks,
		blocks:            blocks,
		lastBlockLen:      lastBlockLen,
	}
}

//length of block at offset off
func (p *piece) blockLen(off int) int {
	bl := off / p.t.blockRequestSize
	if bl == p.blocks-1 {
		return p.lastBlockLen
	}
	return p.t.blockRequestSize
}

var errLargeOffset = errors.New("offset too big")

var errDivOffset = errors.New("offset remainder with block size is not zero")

//length of block at offset off
func (p *piece) blockLenSafe(off int) (int, error) {
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

func (p *piece) msgRequest(off int) *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: uint32(p.index),
		Begin: uint32(off),
		Len:   uint32(p.blockLen(off)),
	}
}

func (p *piece) toBlock(off int) block {
	return block{
		pc:  p.index,
		off: off,
		len: p.blockLen(off),
	}
}
func (p *piece) unrequestedBlocksSlc(limit int) (blocks []block) {
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

func (p *piece) allBlocksUnrequested() bool {
	return p.unrequestedBlocks.Len() == p.blocks
}

func (p *piece) hasUnrequestedBlocks() bool {
	return p.unrequestedBlocks.Len() > 0
}
func (p *piece) hasPendingBlocks() bool {
	return p.pendingBlocks.Len() > 0
}

func (p *piece) hasCompletedBlocks() bool {
	return p.completeBlocks.Len() > 0
}

func (p *piece) isPartialyRequested() bool {
	return p.hasUnrequestedBlocks() && (p.hasPendingBlocks() || p.hasCompletedBlocks())
}

func changeBlockState(curr, new *bitmap.Bitmap, off int) {
	curr.Set(off, false)
	new.Set(off, true)
}

//assume block was in pending before
//returns whether we modified state
func (p *piece) addBlockUnrequsted(off int) bool {
	//if another conn got the block, there is nothing to do.
	if !p.completeBlocks.Get(off) {
		changeBlockState(&p.pendingBlocks, &p.unrequestedBlocks, off)
		return true
	}
	return false
}

//returns whether we modified state
func (p *piece) addBlockPending(off int) bool {
	//if another conn got the block, there is nothing to do.
	if !p.completeBlocks.Get(off) {
		changeBlockState(&p.unrequestedBlocks, &p.pendingBlocks, off)
		return true
	}
	return false
}

//TODO: assert some things
func (p *piece) markBlockComplete(ci *connInfo, off int) {
	p.completeBlocks.Set(off, true)
	p.pendingBlocks.Set(off, false)
	//offset could be in unrequested if we received an unexpected block
	p.unrequestedBlocks.Set(off, false)
	p.contributors = append(p.contributors, ci)
}

func (p *piece) complete() bool {
	return p.completeBlocks.Len() == p.blocks
}

func (p *piece) verificationFailed() {
	for _, c := range p.contributors {
		c.stats.badPiecesContributions++
	}
	if !p.complete() {
		panic("pieceVerificationFailed: piece was not complete")
	}
	p.unrequestedBlocks, p.completeBlocks = p.completeBlocks, p.unrequestedBlocks
	p.contributors = []*connInfo{}
}

func (p *piece) verificationSuccess() {
	if p.verified {
		panic("already verified")
	}
	p.verified = true
	for _, c := range p.contributors {
		c.stats.goodPiecesContributions++
	}
}
