package torrent

import (
	"log"
	"math/rand"
	"os"
	"testing"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//TODO:maybe create a benchmark for the dispatching time

func TestPieces(t *testing.T) {
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
}

func TestRarestStrategy(t *testing.T) {
	tr := newTestTorrent(40, 1*(1<<14)+1, 5*1<<12, 1<<13) //random
	p := newPieces(tr)
	ci, ci2, ci3 := &connInfo{t: tr}, &connInfo{t: tr}, &connInfo{t: tr}
	//peers ci & c2 have pieces {0,1} so piece 2 is the rarest
	ci.peerBf.Add(0, 1)
	ci2.peerBf.Add(0, 1)
	tr.conns = append(tr.conns, ci, ci2, ci3)
	pc, ok := p.rarestStrategy([]int{0, 1, 2})
	assert.Equal(t, true, ok)
	assert.Equal(t, 2, pc)
}

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
	assert.Equal(t, false, p.hasPendingBlocks())
	assert.Equal(t, false, p.isPartialyRequested())
	assert.Equal(t, 0, p.completeBlocks.Len())
	assert.Equal(t, 5, len(p.unrequestedBlocksSlc(5)))
	_, err := p.blockLenSafe(5*blockSz + 20)
	require.Error(t, err)
	_, err = p.blockLenSafe(8*blockSz + 20)
	require.Error(t, err)
	block, err := p.blockLenSafe(8 * blockSz)
	require.NoError(t, err)
	assert.Equal(t, p.lastBlockLen, block)
	p.addBlockPending(blockSz)
	assert.Equal(t, true, p.hasPendingBlocks())

	lastp := newPiece(tr, 3)
	assert.Equal(t, 3, lastp.index)
	assert.Equal(t, 1, lastp.blocks)
	assert.Equal(t, lastPieceLen, lastp.lastBlockLen)
	assert.Equal(t, lastp.blocks, lastp.unrequestedBlocks.Len())
	assert.Equal(t, 0, lastp.pendingBlocks.Len())
	assert.Equal(t, 0, lastp.completeBlocks.Len())
}

func newTestTorrent(numPieces, pieceLen, lastPieceLen, blockSz int) *Torrent {
	return &Torrent{
		mi: &metainfo.MetaInfo{
			Info: &metainfo.InfoDict{
				Pieces:   make([]byte, numPieces*20),
				PieceLen: pieceLen,
			},
		},
		length:           3*pieceLen + lastPieceLen,
		blockRequestSize: blockSz,
		logger:           log.New(os.Stdout, "test", log.Flags()),
	}
}
