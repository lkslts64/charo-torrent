package torrent

type pieceHasher struct {
	t *Torrent
}

func (p *pieceHasher) Run() {
	for {
		select {
		case piece := <-p.t.pieceQueuedHashingCh:
			correct := p.t.storage.HashPiece(piece, p.t.pieceLen(uint32(piece)))
			p.t.pieceHashedCh <- pieceHashed{
				pieceIndex: piece,
				ok:         correct,
			}
		case <-p.t.drop:
			//TODO:wait in torrent to finish or not?
			return
		}
	}
}
