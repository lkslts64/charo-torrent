package torrent

import (
	"log"
	"os"
	"testing"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//TODO:maybe create a benchmark for the dispatching time

/*func TestPieces(t *testing.T) {
	tr := newTestTorrent(300, 50*(1<<14)+245, 1<<13, 1<<14) //random
	p := newPieces(tr)
	pc, ok := p.randomStrategy([]int{299, 43, 53})
	assert.Equal(t, true, ok)
	assert.Equal(t, true, pc == 299 || pc == 43 || pc == 53)
	chSize := 1 << 12
	ci := &connInfo{
		commandCh: make(chan interface{}, chSize), //make chan big enough
	}
	assert.Equal(t, true, cap(ci.commandCh) > tr.numPieces())
	tr.conns = append(tr.conns, ci)
	for i := 0; i < tr.numPieces(); i++ {
		ci.peerBf.Set(i, true)
	}
	//We 'll make piece 0 is partialy requested
	//and we expect blocks of piece 0 to be dispatched
	p.pcs[0].addBlockPending(0)
	p.dispatch(ci)
	v := <-ci.commandCh
	reqs := v.([]block)
	for _, r := range reqs {
		assert.Equal(t, 0, r.pc)
	}
	//dispatch until we haven't any more and receive all reqs dispatched
	for len(p.requestExpecters) <= 0 {
		p.dispatch(ci)
	}
	for len(ci.commandCh) > 0 {
		<-ci.commandCh
	}
	//expect to get more after a verification failure
	randPieceIndex := rand.Intn(tr.numPieces())
	randPiece := p.pcs[randPieceIndex]
	//mark all blocks of a random piece complete
	//and expect a verification job
	for i := 0; i < randPiece.blocks; i++ {
		randPiece.markBlockComplete(ci, i*(1<<14))
	}
	v = <-ci.commandCh
	switch v.(type) {
	case verifyPiece:
	default:
		t.FailNow()
	}
	//expect more blocks on verification failure
	p.pieceVerificationFailed(randPieceIndex)
	select {
	case <-ci.commandCh:
	default:
		t.Fail()
	}
}*/

func TestPiece(t *testing.T) {
	blockSz := 1 << 14
	pieceLen := 8*int(blockSz) + 100
	lastPieceLen := int(blockSz) - 200
	tr := newTestTorrent(4, pieceLen, lastPieceLen, blockSz)
	p := newPiece(tr, 0)
	assert.Equal(t, 0, p.index)
	assert.Equal(t, 9, p.blocks)
	assert.Equal(t, 100, p.lastBlockLen)
	assert.Equal(t, true, p.allBlocksUnrequested())
	assert.Equal(t, 0, p.completeBlocks.Len())
	assert.Equal(t, 5, len(p.unrequestedBlocksSlc(5)))
	_, err := p.blockLenSafe(5*blockSz + 20)
	require.Error(t, err)
	_, err = p.blockLenSafe(8*blockSz + 20)
	require.Error(t, err)
	block, err := p.blockLenSafe(8 * blockSz)
	require.NoError(t, err)
	assert.Equal(t, p.lastBlockLen, block)
	p.makeBlockPending(blockSz)

	lastp := newPiece(tr, 3)
	assert.Equal(t, 3, lastp.index)
	assert.Equal(t, 1, lastp.blocks)
	assert.Equal(t, lastPieceLen, lastp.lastBlockLen)
	assert.Equal(t, lastp.blocks, lastp.unrequestedBlocks.Len())
	assert.Equal(t, 0, lastp.pendingBlocks())
	assert.Equal(t, 0, lastp.completeBlocks.Len())
}

func TestPiecesState(t *testing.T) {
	tr := newTestTorrent(300, 10*(1<<14)+245, 1<<13, 1<<14)
	p := newPieces(tr)
	var bm bitmap.Bitmap
	bm.Add(1, 29, 30)
	reqs := make([]block, maxOnFlight)
	n := p.getRequests(bm, reqs)
	reqs = reqs[:n]
	assert.EqualValues(t, maxOnFlight, n)
	for _, req := range reqs {
		piece := p.pcs[req.pc]
		assert.True(t, piece.pendingGet(req.off))
		assert.True(t, req.pc == 1 || req.pc == 29 || req.pc == 30)
	}
	p.discardRequests(reqs)
	for _, p := range p.pcs {
		assert.True(t, p.allBlocksUnrequested())
	}
}

func TestPiecePrioritization(t *testing.T) {
	tr := newTestTorrent(100, 3, 1, 1)
	p := newPieces(tr)
	p.strategy = lessByRarity
	var bm bitmap.Bitmap
	bm.AddRange(0, tr.numPieces()-1)
	//make piece 50 have the highest completeness score
	p.pcs[50].makeBlockPending(2)
	p.pcs[50].makeBlockPending(1)
	//make piece 50 have the second highest completeness score
	p.pcs[40].makeBlockPending(2)
	//all blocks of piece 60 are pending (lowest priority)
	p.pcs[60].makeBlockPending(0)
	p.pcs[60].makeBlockPending(1)
	p.pcs[60].makeBlockPending(2)
	//pieces are sorted by rarity in ascending order
	for i, piece := range p.pcs {
		piece.rarity = tr.numPieces() - i
	}
	//take all blocks of the torrent
	reqs := make([]block, tr.numPieces()*3)
	n := p.getRequests(bm, reqs)
	assert.Greater(t, n, tr.numPieces())
	reqs = reqs[:n]
	assert.Equal(t, 50, reqs[0].pc)
	assert.Equal(t, 40, reqs[1].pc)
	assert.Equal(t, 40, reqs[2].pc)
	reqs = reqs[3:]
	//expect to find remaining pieces sorted by rarity in descending order
	curr, prev := 0, tr.numPieces()
	for _, req := range reqs {
		curr = req.pc
		assert.LessOrEqual(t, curr, prev)
		prev = req.pc
	}
	//last will be the one with the lowest priority
	assert.Equal(t, 60, p.prioritizedPcs[len(p.prioritizedPcs)-1].index)
}

func newTestTorrent(numPieces, pieceLen, lastPieceLen, blockSz int) *Torrent {
	return &Torrent{
		mi: &metainfo.MetaInfo{
			Info: &metainfo.InfoDict{
				Pieces:   make([]byte, numPieces*20),
				PieceLen: pieceLen,
			},
		},
		length:           (numPieces-1)*pieceLen + lastPieceLen,
		blockRequestSize: blockSz,
		logger:           log.New(os.Stdout, "test", log.Flags()),
	}
}
