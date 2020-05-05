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

func TestPiece(t *testing.T) {
	blockSz := 1 << 14
	pieceLen := 8*int(blockSz) + 100
	lastPieceLen := int(blockSz) - 200
	tr := newTestTorrent(4, pieceLen, lastPieceLen, blockSz)
	p := newPiece(tr, 0)
	assert.Equal(t, 0, p.index)
	assert.Equal(t, 9, p.blocks)
	assert.Equal(t, 100, p.lastBlockLen)
	assert.Equal(t, true, allBlocksUnrequested(p))
	assert.Equal(t, 0, p.completeBlocks.Len())
	assert.Equal(t, 5, len(p.unrequestedBlocksSlc(5)))
	_, err := p.blockLenSafe(5*blockSz + 20)
	require.Error(t, err)
	_, err = p.blockLenSafe(8*blockSz + 20)
	require.Error(t, err)
	block, err := p.blockLenSafe(8 * blockSz)
	require.NoError(t, err)
	assert.Equal(t, p.lastBlockLen, block)
	p.setBlockPending(blockSz)
	lastp := newPiece(tr, 3)
	assert.Equal(t, 3, lastp.index)
	assert.Equal(t, 1, lastp.blocks)
	assert.Equal(t, lastPieceLen, lastp.lastBlockLen)
	assert.Equal(t, lastp.blocks, lastp.unrequestedBlocks.Len())
	assert.Equal(t, 0, lastp.PendingBlocks())
	assert.Equal(t, 0, lastp.completeBlocks.Len())
}

func allBlocksUnrequested(p *Piece) bool {
	return p.UnrequestedBlocks() == p.Blocks()
}
func TestPiecesState(t *testing.T) {
	tr := newTestTorrent(300, 10*(1<<14)+245, 1<<13, 1<<14)
	tr.cl, _ = NewClient(nil)
	p := newPieces(tr)
	p.setDownloadEnabled(true)
	var bm bitmap.Bitmap
	bm.Add(1, 29, 30)
	reqs := make([]block, maxOnFlight)
	n := p.fillRequests(bm, reqs)
	reqs = reqs[:n]
	assert.EqualValues(t, maxOnFlight, n)
	for _, req := range reqs {
		piece := p.pcs[req.pc]
		assert.True(t, piece.pendingGet(req.off))
		assert.True(t, req.pc == 1 || req.pc == 29 || req.pc == 30)
	}
	p.discardRequests(reqs)
	for _, p := range p.pcs {
		assert.True(t, allBlocksUnrequested(p))
	}
}

//test how DefaultPieceSelector performs
func TestPiecePrioritization(t *testing.T) {
	tr := newTestTorrent(100, 3, 3, 1)
	tr.cl, _ = NewClient(nil)
	p := newPieces(tr)
	//we want to prioritize by rarity
	p.selector.OnPieceDownload(-1) //the piece index is irrelevant
	p.setDownloadEnabled(true)
	var bm bitmap.Bitmap
	bm.AddRange(0, tr.numPieces())
	//make piece 50 have the highest completeness score
	p.pcs[50].setBlockPending(2)
	p.pcs[50].setBlockPending(1)
	//make piece 40 have the second highest completeness score
	p.pcs[40].setBlockPending(2)
	//all blocks of piece 60 are pending (lowest priority)
	p.pcs[60].setBlockPending(0)
	p.pcs[60].setBlockPending(1)
	p.pcs[60].setBlockPending(2)
	//pieces are sorted by rarity in ascending order
	for i, piece := range p.pcs {
		piece.rarity = tr.numPieces() - i
	}
	//take all blocks of the torrent
	reqs := make([]block, tr.numPieces()*3)
	n := p.fillRequests(bm, reqs)
	assert.Greater(t, n, tr.numPieces())
	reqs = reqs[:n]
	assert.Equal(t, 50, reqs[0].pc)
	assert.Equal(t, 40, reqs[1].pc)
	assert.Equal(t, 40, reqs[2].pc)
	reqs = reqs[3:]
	//expect to find remaining pieces sorted by rarity in descending order
	curr, prev := 0, tr.numPieces()
	for _, req := range reqs {
		if req.pc == 60 {
			t.Fatal("got fully requested piece in fillRequests")
		}
		curr = req.pc
		assert.LessOrEqual(t, curr, prev)
		prev = req.pc
	}
	//last will be the one with the lowest priority
	//assert.Equal(t, 60, p.prioritizedPcs[len(p.prioritizedPcs)-1].index)
}

func TestEndGame(t *testing.T) {
	tr := newTestTorrent(100, 3, 1, 1)
	tr.cl, _ = NewClient(nil)
	p := newPieces(tr)
	p.setDownloadEnabled(true)
	//end game will be activated for pieces 0 and 1
	p.ownedPieces.AddRange(2, 100)
	//make all complete except 0 and 1
	for _, piece := range p.pcs {
		if piece.index == 0 || piece.index == 1 {
			continue
		}
		piece.completeBlocks, piece.unrequestedBlocks = piece.unrequestedBlocks, piece.completeBlocks
	}
	//make all blocks of pieces 1 and 2 pending
	for i := 0; i < 2; i++ {
		for j := 0; j < 3; j++ {
			p.pcs[i].setBlockPending(j)
		}
	}
	p.maybeStartEndgame()
	assert.True(t, allBlocksUnrequested(p.pcs[0]) && allBlocksUnrequested(p.pcs[1]))
	var bm bitmap.Bitmap
	bm.AddRange(0, tr.numPieces())
	//simulate 2 conns getting requests.The same blocks should be returned over and over again
	for i := 0; i < 2; i++ {
		reqs := make([]block, 10)
		n := p.fillRequests(bm, reqs)
		reqs = reqs[:n]
		for _, req := range reqs {
			assert.True(t, req.pc == 0 || req.pc == 1)
		}
	}
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
