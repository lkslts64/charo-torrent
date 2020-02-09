package torrent

import (
	"errors"
	"math/rand"
	"sort"
	"sync"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

const (
	endGameThreshold                    = 2
	randomPiecePickingStrategyThreshold = 1
)

//TODO:reconsider how we store pcs - maybe a map would be better?
//and store them depending on their state (dispatched,unrequested etc).
type pieces struct {
	t           *Torrent
	ownedPieces bitmap.Bitmap
	//every conn can call getRequests and discardRequests.This mutex protects the fields
	//that these methods access.
	mu  sync.Mutex
	pcs []*piece
	//pieces sorted by their priority.All verified pieces are not present in this slice
	prioritizedPcs []*piece
	endGame        bool
	//random until we get the first piece, rarest onwards
	piecePickStrategy lessFunc
}

//TODO:add allVerified boolean at constructor parameter
func newPieces(t *Torrent) *pieces {
	numPieces := int(t.numPieces())
	//TODO:shuffle pieces
	pcs := make([]*piece, numPieces)
	for i := 0; i < numPieces; i++ {
		pcs[i] = newPiece(t, i)
	}
	sorted := make([]*piece, numPieces)
	copy(sorted, pcs)
	p := &pieces{
		t:              t,
		pcs:            pcs,
		prioritizedPcs: sorted,
	}
	p.piecePickStrategy = lessRand
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
	//Prioritize pieces.Pieces that have more pending &
	//completed but still have unrequested blocks have the highest priority.
	//If two pieces have both all blocks unrequested then we pick the one selected by
	//our current strategy.
	sort.Slice(p.prioritizedPcs, func(i, j int) bool {
		p1 := p.prioritizedPcs[i]
		p2 := p.prioritizedPcs[j]
		switch {
		case !p1.hasUnrequestedBlocks():
			return false
		case !p2.hasUnrequestedBlocks():
			return true
		case p1.allBlocksUnrequested() && p2.allBlocksUnrequested():
			return p.piecePickStrategy(p1, p2)
		default:
			return p1.completeness() > p2.completeness()
		}
	})
	return p.fillWithRequests(peerPieces, requests)
}

//The strategy for piece picking. In order for a piece to be picked, all piece's blocks
//should be unrequested.
type lessFunc func(p1, p2 *piece) bool

func lessRand(p1, p2 *piece) bool {
	return rand.Intn(2) == 1
}

func lessByRarity(p1, p2 *piece) bool {
	return p1.rarity < p2.rarity
}

//fills the provided slice with requests. Returns how many were filled.
func (p *pieces) fillWithRequests(peerPieces bitmap.Bitmap, requests []block) (n int) {
	var unreq []block
	total, _n := len(requests), 0
	for _, piece := range p.prioritizedPcs {
		if peerPieces.Get(piece.index) {
			unreq = piece.unrequestedBlocksSlc(len(requests))
			if !p.endGame {
				for _, b := range unreq {
					piece.makeBlockPending(b.off)
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
		p.pcs[req.pc].makeBlockUnrequsted(req.off)
	}
}

func (p *pieces) makeBlockComplete(i int, off int, ci *connInfo) {
	var inEndGame bool
	piece := p.pcs[i]
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		if inEndGame {
			p.t.sendCancels(block{
				pc:  i,
				off: off,
				len: piece.blockLen(off),
			})
		}
		if piece.complete() {
			p.t.queuePieceForHashing(i)
		}
	}()
	piece.makeBlockComplete(ci, off)
	inEndGame = p.endGame
}

func (p *pieces) pieceVerified(i int) {
	var requestsAreAvailable bool
	p.ownedPieces.Set(i, true)
	p.pcs[i].verificationSuccess()
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		if requestsAreAvailable {
			p.t.broadcastCommand(requestsAvailable{})
		}
	}()
	//remove the piece from priotirzedPcs, we'll never make requests for it again.
	for j, piece := range p.prioritizedPcs {
		if piece.index == i {
			p.prioritizedPcs = append(p.prioritizedPcs[:j], p.prioritizedPcs[j+1:]...)
		}
	}
	switch owned := p.ownedPieces.Len(); {
	case p.t.numPieces()-owned == endGameThreshold:
		requestsAreAvailable = p.setupEndgame()
	case owned == randomPiecePickingStrategyThreshold:
		p.piecePickStrategy = lessByRarity
	}
}

func (p *pieces) pieceVerificationFailed(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pcs[i].verificationFailed()
}

func (p *pieces) setupEndgame() bool {
	if p.endGame {
		//shouldn't happen unless we drop a piece from storage
		return false
	}
	p.endGame = true
	notOwned := bitmap.Flip(p.ownedPieces, 0, p.t.numPieces())
	if notOwned.Len() > endGameThreshold {
		panic("endgame before threshold")
	}
	notOwned.IterTyped(func(piece int) bool {
		p.pcs[piece].makeAllUnrequested()
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

func (p *piece) hasCompletedBlocks() bool {
	return p.completeBlocks.Len() > 0
}

func (p *piece) pendingBlocks() int {
	return p.blocks - (p.unrequestedBlocks.Len() + p.completeBlocks.Len())
}

func (p *piece) pendingGet(off int) bool {
	return !(p.unrequestedBlocks.Get(off) || p.completeBlocks.Get(off))
}

//assume block was in pending before
func (p *piece) makeBlockUnrequsted(off int) {
	//if another conn got the block, there is nothing to do.
	if !p.completeBlocks.Get(off) {
		p.unrequestedBlocks.Set(off, true)
	}
}

func (p *piece) makeBlockPending(off int) {
	//if another conn got the block, there is nothing to do.
	if !p.completeBlocks.Get(off) {
		p.unrequestedBlocks.Set(off, false)
	}
}

func (p *piece) makeBlockComplete(ci *connInfo, off int) {
	p.completeBlocks.Set(off, true)
	//offset could be in unrequested if we received an unexpected block
	p.unrequestedBlocks.Set(off, false)
	p.contributors = append(p.contributors, ci)
}

func (p *piece) makeAllUnrequested() {
	for i := 0; i < p.blocks; i++ {
		p.makeBlockUnrequsted(i * p.t.blockRequestSize)
	}
}

func (p *piece) complete() bool {
	return p.completeBlocks.Len() == p.blocks
}

func (p *piece) completeness() int {
	return p.completeBlocks.Len() + p.pendingBlocks() - p.unrequestedBlocks.Len()
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
