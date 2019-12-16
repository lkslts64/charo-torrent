package torrent

import (
	"errors"
	"math"
	"math/rand"

	"github.com/lkslts64/charo-torrent/peer_wire"
)

const maxDispatchedRequests = 100 //experiment -maybe not needd

type requestStrategy byte

const (
	random requestStrategy = iota
	rarest
)

//TODO:reconsider how we store pcs - maybe a map would be better?
//or store them depending on their state (dispatched,unrequested etc).
type Pieces struct {
	t   *Torrent
	pcs []Piece
	//blocks to request are pushed to this queue and workers send them to peers
	//TODO:length of this queue? maybe proportional to number of blocks per piece
	//e.g len = pieceLen/blockSz * constant
	cqueue condQueue
	//random until we get the first piece, rarest onwards
	strategy requestStrategy
	//cache of downloaded pieces - dont ask db every time
	ownedPieces peer_wire.BitField
	//how many pieces we have - its not O(1) to determine from ownedPieces or pcs
	numHave int
}

//TODO:add more
func NewPieces(t *Torrent) *Pieces {
	return &Pieces{
		ownedPieces: make([]byte, int(math.Ceil(float64(t.tm.mi.Info.NumPieces())/8.0))),
	}
}

func (p *Pieces) valid(i uint32) bool {
	return i < uint32(len(p.pcs))
}

//First dispatch blocks of pieces that have been already partially dispatched (if any and if
//peer behind `cn` has the pieces).These blocks have priority because if we have one block
//from a piece, then we want the rest blocks of this piece sooner that the others in order to
//start uploading sooner.
//Dispatch `maxOnFlight` blocks to connection
func (p *Pieces) dispatch(cn *connection) {
	peerPieces := cn.bf.FilterNotSet()
	remaining := maxOnFlight
	for _, i := range peerPieces {
		if p.pcs[i].hasUnrequestedBlocks() && p.pcs[i].hasPendingBlocks() {
			if remaining = p._dispatch(i, cn, remaining); remaining == 0 {
				return
			}
		}
	}
	var pc int
	var ok bool
	for {
		if pc, ok = p.pickNext(cn, peerPieces); !ok {
			return
		}
		if remaining = p._dispatch(pc, cn, remaining); remaining == 0 {
			return
		}
	}
}

//try to give peerConn `blocksToDispatch` blocks
func (p *Pieces) _dispatch(i int, cn *connection, blocksToDispatch int) (remaining int) {
	unreqBlocks := p.pcs[i].unrequestedBlocksSlc(blocksToDispatch)
	remaining = blocksToDispatch - len(unreqBlocks)
	cn.pc.jobCh.In() <- unreqBlocks
	return
}

//select a piece for a PeerConn to download.
//'ok' set to false means no piece has all blocks unrequested, so we cant pick
//any.
func (p *Pieces) pickNext(cn *connection, peerPieces []int) (pc int, ok bool) {
	switch p.strategy {
	case random:
		//pick a random piece and if it is completely unrequested we have a hit.
		//we have a high chance to hit because we utilize random strategy only
		//for the first pieces we download (all blocks are unreqeusted initially).
		for {
			pc = rand.Intn(len(peerPieces))
			if p.pcs[peerPieces[pc]].allBlocksUnrequested() {
				ok = true
				return
			}
		}
	case rarest:
		conns := p.t.conns
		fmap := make(freqMap)
		unrequestedPieces := []uint32{}
		for _, i := range peerPieces {
			if p.pcs[i].allBlocksUnrequested() {
				fmap.initKey(int64(i))
				unrequestedPieces = append(unrequestedPieces, uint32(i))
			}
		}
		if len(unrequestedPieces) == 0 {
			return
		}
		for _, c := range conns {
			//seeders can't help to determine the rarest piece
			if c.stats.isSeeding {
				continue
			}
			for _, i := range unrequestedPieces {
				if c.bf.HasPiece(i) {
					fmap.add(int64(i))
				}
			}
		}
		pc = int(fmap.min())
		ok = true
		return
	default:
		panic("unknown request strategy")
	}
}

/*func (p *Pieces) ownPiece(i uint32) bool {
	p.ownMu.RLock()
	defer p.ownMu.RUnlock()
	return p.ownedPieces.HasPiece(i)
}

func (p *Pieces) setPiece(i uint32) {
	p.ownMu.RLock()
	defer p.ownMu.RUnlock()
	p.ownedPieces.SetPiece(i)
}*/

//also checks if whole piece is complete
func (p *Pieces) markBlockComplete(i, off int) {
	if p.pcs[i].markBlockComplete(off) {
		p.ownedPieces.SetPiece(uint32(i))
		if p.strategy == random {
			p.strategy = rarest
		}
		p.t.reviewInterestsOnPieceDownload(i)
	}
}

//these blocks should be at pending state.
//delete them before adding to unrequested.
//Usually, we should avoid a call to this func.
//We should optimize and give another peer the
//unsatisfied reqs instead of adding them and
//popping them again
func (p *Pieces) addUnsatisfied(bls []block) {
	for _, bl := range bls {
		if _, ok := p.pcs[bl.pc].pendingBlocks[int(bl.off)]; !ok {
			panic("add unsatisfied without being in pending")
		}
		delete(p.pcs[bl.pc].pendingBlocks, int(bl.off))
		p.pcs[bl.pc].addBlockUnrequsted(int(bl.off))
	}
}

func (p *Pieces) hasUnrequestedBlocks() bool {
	for i := 0; i < len(p.pcs); i++ {
		if p.pcs[i].hasUnrequestedBlocks() {
			return true
		}
	}
	return false
}

func (p *Pieces) hasUnrequestedPieces() bool {
	for i := 0; i < len(p.pcs); i++ {
		if p.pcs[i].allBlocksUnrequested() {
			return true
		}
	}
	return false
}

//Piece is constrcuted by master and is managed entirely
//by him.Blocks can be at three states and each block lies
//on a single state at each time. All blocks are initially
//unrequested.When we request a block through the workers,
// then a block becomes pending and after downloaded.
//If a worker gets choked and has requests unsatisfied
//then master puts them again at unrequested state
//At all cases,this equality should hold true:
//len(pendingBlocks)+len(downloadedBlocks)+len(unrequestedBlocks) == totalBlocks
type Piece struct {
	t     *Torrent
	index int
	hash  []byte //sha1 hash
	//num of blocks
	blocks       int
	lastBlockLen int
	//keeps track of which blocks are not requested for download
	unrequestedBlocks map[int]struct{}
	//keeps track of which blocks are pending for download
	pendingBlocks map[int]struct{}
	//keeps track of which blocks we have been downloaded
	completeBlocks map[int]struct{}
}

func NewPiece(t *Torrent, i int) *Piece {
	_blockSz := int(blockSz)
	pieceLen := t.tm.PieceLen(uint32(i))
	lastBlockLen := pieceLen % _blockSz
	isLastSmaller := lastBlockLen != 0
	var extra int
	if isLastSmaller {
		extra = 1
	} else {
		lastBlockLen = _blockSz
	}
	blocks := pieceLen/_blockSz + extra
	//set all blocks to unrequested
	unrequestedBlocks := make(map[int]struct{})
	for j := 0; j < blocks; j++ {
		unrequestedBlocks[j*_blockSz] = struct{}{}
	}
	return &Piece{
		index:             i,
		unrequestedBlocks: unrequestedBlocks,
		pendingBlocks:     make(map[int]struct{}),
		completeBlocks:    make(map[int]struct{}),
		blocks:            blocks,
		lastBlockLen:      lastBlockLen,
	}
}

//length of block at offset off
//TODO:rename to blockLen()
func (p *Piece) len(off uint32) int {
	bl := int(off / blockSz)
	if bl == p.blocks-1 {
		return p.lastBlockLen
	}
	return int(blockSz)
}

//length of block at offset off
func (p *Piece) lenSafe(off uint32) (int, error) {
	switch bl := int(off / blockSz); {
	case bl == p.blocks-1:
		return p.lastBlockLen, nil
	case bl < p.blocks-1:
		return int(blockSz), nil
	default:
		return 0, errors.New("piece: offset out of range")
	}
}

func (p *Piece) msgRequest(off uint32) *peer_wire.Msg {
	return &peer_wire.Msg{
		Kind:  peer_wire.Request,
		Index: uint32(p.index),
		Begin: off,
		Len:   uint32(p.len(off)),
	}
}

func (p *Piece) toBlock(off int) block {
	return block{
		pc:  uint32(p.index),
		off: uint32(off),
		len: blockSz,
	}
}

func (p *Piece) unrequestedToBlockQueue() *blockQueue {
	bq := newBlockQueue(p.blocks) //don't limit length of queue
	for off := range p.unrequestedBlocks {
		bq.push(p.toBlock(off))
	}
	return bq
}

func (p *Piece) unrequestedBlocksSlc(limit int) (blocks []block) {
	count := 0
	for off := range p.unrequestedBlocks {
		if count >= limit {
			break
		}
		blocks = append(blocks, p.toBlock(off))
		count++
	}
	return
}

/*func (p *Piece) addBlockDownloaded(bl *block) {
	if _, ok := p.pendingBlocks[bl.off]; ok {
		panic("block already downloaded")
	}
	p.downloadedBlocks[bl.off] = struct{}{}
}*/

func (p *Piece) allBlocksUnrequested() bool {
	return len(p.unrequestedBlocks) == p.blocks
}

func (p *Piece) hasPendingBlocks() bool {
	return len(p.pendingBlocks) > 0
}

func (p *Piece) hasUnrequestedBlocks() bool {
	return len(p.unrequestedBlocks) > 0
}

func (p *Piece) addBlockUnrequsted(off int) {
	p.unrequestedBlocks[off] = struct{}{}
}

func (p *Piece) addBlockPending(off int) {
	if _, ok := p.unrequestedBlocks[off]; !ok {
		panic("this block doesnt exists in unrequested list")
	}
	delete(p.unrequestedBlocks, off)
	if _, ok := p.pendingBlocks[off]; !ok {
		panic("block already exists in downloadedBlocks")
	}
	p.pendingBlocks[off] = struct{}{}
}

func (p *Piece) addAllBlocks() {
	for i := 0; i < p.blocks; i++ {
		p.pendingBlocks[i*int(blockSz)] = struct{}{}
	}
}

func (p *Piece) markBlockComplete(off int) bool {
	if _, ok := p.pendingBlocks[off]; !ok {
		panic("this block doesnt exists in pending list")
	}
	delete(p.pendingBlocks, off)
	if _, ok := p.completeBlocks[off]; !ok {
		panic("block already exists in downloadedBlocks")
	}
	p.completeBlocks[off] = struct{}{}
	return p.complete() && p.markComplete()
}

func (p *Piece) complete() bool {
	return len(p.completeBlocks) == int(p.blocks)
}

func (p *Piece) markComplete() bool {
	//read from storage and check hash
	return false
}
