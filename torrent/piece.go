package torrent

import "errors"

type Piece struct {
	t     *Torrent
	index uint32
	//num of blocks
	blocks uint32
	//keeps track of which blocks are pending for download
	pendingBlocks map[uint32]bool
	//keeps track of which blocks we have downloaded
	downloadedBlocks map[uint32]bool
}

func NewPiece(t *Torrent, i uint32) *Piece {
	isLastSmaller := t.tm.meta.Info.PieceLen != 0
	var extra uint32
	if isLastSmaller {
		extra = 1
	}
	blocks := uint32(t.tm.meta.Info.PieceLen)/blockSz + extra
	return &Piece{
		index:         i,
		pendingBlocks: make(map[uint32]bool),
		blocks:        blocks,
	}
}

func (p *Piece) addBlock(off uint32) { p.pendingBlocks[off] = true }

func (p *Piece) addAllBlocks() {
	for i := uint32(0); i < p.blocks; i++ {
		p.pendingBlocks[i*blockSz] = true
	}
}

func (p *Piece) removeBlock(off uint32) error {
	if _, ok := p.pendingBlocks[off]; !ok {
		return errors.New("this block doesnt exists in pending list")
	}
	delete(p.pendingBlocks, off)
	return nil
}
