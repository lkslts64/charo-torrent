package torrent

import (
	"errors"
	"math/rand"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/peer_wire"
)

//The strategy for piece picking
//Select a piece for a PeerConn to download.
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
}

func newPieces(t *Torrent) *pieces {
	numPieces := int(t.mi.Info.NumPieces())
	pcs := make([]*piece, numPieces)
	for i := 0; i < numPieces; i++ {
		pcs[i] = newPiece(t, i)
	}
	p := &pieces{
		t:   t,
		pcs: pcs,
	}
	p.strategy = p.randomStrategy
	return p
}

func (p *pieces) valid(i int) bool {
	return i < len(p.pcs)
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
		if p.pcs[i].hasUnrequestedBlocks() && p.pcs[i].hasPendingBlocks() {
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
	cn.sendJob(unreqBlocks)
	return
}

func (p *pieces) randomStrategy(availablePieces []int) (pc int, ok bool) {
	//pick a random piece and if it is completely unrequested we have a hit.
	//we have a high chance to hit because we utilize random strategy only
	//for the first pieces we download (all blocks are unreqeusted initially).
	count := 0
	for {
		if count > 10 {
			panic("random strategy: failed to pick many times")
		}
		pc = rand.Intn(len(availablePieces))
		if p.pcs[availablePieces[pc]].allBlocksUnrequested() {
			ok = true
			return
		}
		count++
	}
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

//also checks if whole piece is complete
func (p *pieces) markBlockComplete(i, off int) {
	if p.pcs[i].markBlockComplete(off) {
		//change strategy after completion of first block
		if p.ownedPieces.Len() == 0 {
			p.strategy = p.rarestStrategy
		}
		p.ownedPieces.Set(i, true)
		p.t.reviewInterestsOnPieceDownload(i)
	}
}

//these blocks should be at pending state.
//delete them before adding to unrequested.
//Usually, we should avoid a call to this func.
//We should optimize and give another peer the
//unsatisfied reqs instead of adding them and
//popping them again
func (p *pieces) addUnsatisfied(bls []block) {
	for _, bl := range bls {
		p.pcs[bl.pc].addBlockUnrequsted(int(bl.off))
	}
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
	t     *Torrent
	index int
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
}

func newPiece(t *Torrent, i int) *piece {
	pieceLen := t.PieceLen(uint32(i))
	lastBlockLen := pieceLen % blockSz
	isLastSmaller := lastBlockLen != 0
	var extra int
	if isLastSmaller {
		extra = 1
	} else {
		lastBlockLen = blockSz
	}
	blocks := pieceLen/blockSz + extra
	//set all blocks to unrequested
	var unrequestedBlocks bitmap.Bitmap
	for j := 0; j < blocks; j++ {
		unrequestedBlocks.Set(j*blockSz, true)
	}
	return &piece{
		index:             i,
		unrequestedBlocks: unrequestedBlocks,
		blocks:            blocks,
		lastBlockLen:      lastBlockLen,
	}
}

//length of block at offset off
//TODO:rename to blockLen()
func (p *piece) len(off int) int {
	bl := int(off / blockSz)
	if bl == p.blocks-1 {
		return p.lastBlockLen
	}
	return int(blockSz)
}

var errLargeOffset = errors.New("offset too big")

var errDivOffset = errors.New("offset remainder with block size is not zero")

//length of block at offset off
func (p *piece) lenSafe(off int) (int, error) {
	if off%blockSz != 0 {
		return 0, errDivOffset
	}
	switch bl := int(off / blockSz); {
	case bl == p.blocks-1:
		return p.lastBlockLen, nil
	case bl < p.blocks-1:
		return int(blockSz), nil
	default:
		return 0, errLargeOffset
	}
}

func (p *piece) msgRequest(off int) *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: uint32(p.index),
		Begin: uint32(off),
		Len:   uint32(p.len(off)),
	}
}

func (p *piece) toBlock(off int) block {
	return block{
		pc:  p.index,
		off: off,
		len: blockSz,
	}
}

func (p *piece) unrequestedToBlockQueue() *blockQueue {
	bq := newBlockQueue(p.blocks) //don't limit length of queue
	for off := range p.unrequestedBlocks.ToSortedSlice() {
		bq.push(p.toBlock(off))
	}
	return bq
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

func changeBlockState(curr, new bitmap.Bitmap, off int) {
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
	changeBlockState(p.pendingBlocks, p.unrequestedBlocks, off)
}

func (p *piece) addBlockPending(off int) {
	changeBlockState(p.unrequestedBlocks, p.pendingBlocks, off)
}

func (p *piece) markBlockComplete(off int) bool {
	changeBlockState(p.pendingBlocks, p.completeBlocks, off)
	return p.complete() && p.markComplete()
}

func (p *piece) complete() bool {
	return p.completeBlocks.Len() == int(p.blocks)
}

func (p *piece) markComplete() bool {
	//read from storage and check hash
	return true
}
