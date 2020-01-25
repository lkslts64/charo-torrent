package torrent

import (
	"errors"
	"math/rand"
	"sort"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

//The strategy for piece picking.In order for a piece to be picked, all piece's blocks
//should be unrequested.Selects a piece for a conn to download.
//'ok' set to false means no piece in peerPieces has all blocks unrequested,
//so we cant pick any.
type requestStrategy func(availablePieces []int) (pc int, ok bool)

//TODO:reconsider how we store pcs - maybe a map would be better?
//and store them depending on their state (dispatched,unrequested etc).
type pieces struct {
	t   *Torrent
	pcs []*piece
	//random until we get the first piece, rarest onwards
	strategy requestStrategy
	//cache of downloaded pieces - dont ask db every time
	ownedPieces bitmap.Bitmap
	//conns that want requests but we couldn't give them when they asked us.
	//we need in case we did't have anything unrequested but something happened
	// (e.g a piece verification failed or we droped a piece) and we need to forward requests
	//again.
	//when a conn is dropped, it is removed from here too.
	requestExpecters map[*connInfo]struct{}
}

func newPieces(t *Torrent) *pieces {
	numPieces := int(t.numPieces())
	pcs := make([]*piece, numPieces)
	for i := 0; i < numPieces; i++ {
		pcs[i] = newPiece(t, i)
	}
	p := &pieces{
		t:                t,
		pcs:              pcs,
		requestExpecters: make(map[*connInfo]struct{}),
	}
	p.strategy = p.randomStrategy
	return p
}

//First dispatch blocks of pieces that have been already partially dispatched (if any and if
//peer behind `cn` has the pieces).These blocks have priority because if we have one block
//from a piece, then we want the rest blocks of this piece sooner that the others in order to
//start uploading sooner.
//Dispatch `maxOnFlight` blocks to connection
func (p *pieces) dispatch(cn *connInfo) {
	//availablePieces := bitmap.Flip(cn.peerBf, 0, p.t.numPieces()).ToSortedSlice()
	availablePieces := cn.peerBf.ToSortedSlice()
	remaining := maxOnFlight
	for _, i := range availablePieces {
		if p.pcs[i].isPartialyRequested() {
			if remaining = p._dispatch(i, cn, remaining); remaining == 0 {
				return
			}
		}
	}
	var (
		pc int
		ok bool
	)
	for {
		if pc, ok = p.strategy(availablePieces); !ok {
			//we didn't manage to give conn as much requests as wanted.
			//Howerver,we might give in the future
			p.requestExpecters[cn] = struct{}{}
			return
		}
		if remaining = p._dispatch(pc, cn, remaining); remaining == 0 {
			return
		}
	}
}

//try to give peerConn `blocksToDispatch` blocks
func (p *pieces) _dispatch(i int, cn *connInfo, blocksToDispatch int) (remaining int) {
	unreqBlocks := p.pcs[i].unrequestedBlocksSlc(blocksToDispatch)
	remaining = blocksToDispatch - len(unreqBlocks)
	for _, bl := range unreqBlocks {
		p.pcs[i].addBlockPending(int(bl.off))
	}
	cn.sendCommand(unreqBlocks)
	return
}

func (p *pieces) randomStrategy(availablePieces []int) (pc int, ok bool) {
	piecesPerm := rand.Perm(len(availablePieces))
	for _, i := range piecesPerm {
		if p.pcs[i].allBlocksUnrequested() {
			ok = true
			pc = availablePieces[i]
			return
		}
	}
	return
}

func (p *pieces) rarestStrategy(availablePieces []int) (pc int, ok bool) {
	conns := p.t.conns
	fmap := make(freqMap)
	unrequestedPieces := []int{}
	for _, i := range availablePieces {
		if p.pcs[i].allBlocksUnrequested() {
			fmap.initKey(int64(i))
			unrequestedPieces = append(unrequestedPieces, i)
		}
	}
	if len(unrequestedPieces) == 0 {
		return
	}
	for _, c := range conns {
		//seeders can't help to determine the rarest piece
		if c.peerSeeding() {
			continue
		}
		for _, i := range unrequestedPieces {
			if c.peerBf.Get(i) {
				fmap.add(int64(i))
			}
		}
	}
	pc = int(fmap.pickRandom(fmap[fmap.min()]))
	ok = true
	return
}

func (p *pieces) pieceSuccesfullyVerified(i int) {
	p.pcs[i].verificationSuccess()
	//change strategy after completion of first block
	if p.ownedPieces.Len() == 0 {
		p.strategy = p.rarestStrategy
	}
	p.ownedPieces.Set(i, true)
}

func (p *pieces) pieceVerificationFailed(i int) {
	p.pcs[i].verificationFailed()
	p.dispatchToExpecters()
}

//try to forward blocks to conns that wanted blocks in the past
func (p *pieces) dispatchToExpecters() {
	for c := range p.requestExpecters {
		p.dispatch(c)
	}
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

//these blocks should be at pending state.
//delete them before adding to unrequested.
//Usually, we should avoid a call to this func.
//We should optimize and give another peer the
//unsatisfied reqs instead of adding them and
//forwarding them again
func (p *pieces) addDiscarded(bls []block) {
	for _, bl := range bls {
		p.pcs[bl.pc].addBlockUnrequsted(int(bl.off))
	}
	p.dispatchToExpecters()
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

/*func (p *piece) unrequestedToBlockQueue() *blockQueue {
	bq := newBlockQueue(p.blocks) //don't limit length of queue
	for off := range p.unrequestedBlocks.ToSortedSlice() {
		bq.push(p.toBlock(off))
	}
	return bq
}*/

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

/*func (p *Piece) addBlockDownloaded(bl *block) {
	if _, ok := p.pendingBlocks[bl.off]; ok {
		panic("block already downloaded")
	}
	p.downloadedBlocks[bl.off] = struct{}{}
}*/

func (p *piece) allBlocksUnrequested() bool {
	return p.unrequestedBlocks.Len() == p.blocks
}

func (p *piece) hasPendingBlocks() bool {
	return p.pendingBlocks.Len() > 0
}

func (p *piece) hasUnrequestedBlocks() bool {
	return p.unrequestedBlocks.Len() > 0
}

func (p *piece) isPartialyRequested() bool {
	return p.hasUnrequestedBlocks() && p.hasPendingBlocks()
}

func changeBlockState(curr, new *bitmap.Bitmap, off int) {
	if !curr.Get(off) {
		panic("piece: exepcted to find block here")
	}
	curr.Set(off, false)
	if new.Get(off) {
		panic("piece: didn't expect to find block here")
	}
	new.Set(off, true)
}

//assume block was in pending before
func (p *piece) addBlockUnrequsted(off int) {
	changeBlockState(&p.pendingBlocks, &p.unrequestedBlocks, off)
}

func (p *piece) addBlockPending(off int) {
	changeBlockState(&p.unrequestedBlocks, &p.pendingBlocks, off)
}

func (p *piece) markBlockComplete(ci *connInfo, off int) {
	changeBlockState(&p.pendingBlocks, &p.completeBlocks, off)
	p.contributors = append(p.contributors, ci)
	if p.complete() {
		p.sendVerificationJob()
	}
}

func (p *piece) complete() bool {
	return p.completeBlocks.Len() == int(p.blocks)
}

func (p *piece) sendVerificationJob() {
	//give the conn with the worst download rate the verification job
	_conns := make([]*connInfo, len(p.t.conns))
	copy(_conns, p.t.conns)
	sort.Sort(byRate(_conns))
	worse := _conns[len(_conns)-1]
	hp := verifyPiece(p.index)
	worse.sendCommand(hp)
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
	p.verified = true
	for _, c := range p.contributors {
		c.stats.goodPiecesContributions++
	}
}
