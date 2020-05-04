package torrent

import "math/rand"

// PieceSelector is responsible for selecting the order in which the
// torrent's pieces will be requested. Note that this is not the order
// in which they will be downloaded and in some cases it isn't event the exact order
// in which they will be requested (see Torrent.SetMaxInFlightPieces). For example,
// if MaxInFlightPieces == 1, the client has requested some blocks for piece 5 (and
// no other piece) and piece 4 gets prioritized over 5, then the client wont't make
// requests for piece 4 with respect to MaxInFlightPieces and
// it will continue to request blocks for piece 5 which has lower priority.
type PieceSelector interface {
	// Less reports whether p1 should be prioritized over p2.
	// p1 and p2 have both unrequested blocks.
	// Less will be executed every time the client wants to request pieces from a
	// remote peer so it's important that its execution time is short.
	// Moreover, Less should not call any of the torrent's methods because it will
	// cause deadlock - it is called with the torrent's internall lock acquired,
	// another reason to keep its execution time short-.
	Less(p1, p2 *Piece) bool
	// This is called when a piece is fully downloaded (and verified).
	// Useful if PieceSelector wants to change state after such an event.
	OnPieceDownload(i int)
	// Called when the PieceSelector is associated with a specific Torrent.
	// Maybe useful for some initial setup.
	SetTorrent(t *Torrent)
}

//DefaultPieceSelector is
type DefaultPieceSelector struct {
	next    nextF
	pausedC chan bool
}

func NewDefaultPieceSelector() PieceSelector {
	return &DefaultPieceSelector{
		next: nextRand,
	}
}

//Less is the default Less func. Pieces that have more pending and
//completed blocks have the highest priority.
//If the two pieces have both all blocks unrequested then:
// i) If no other piece in the Torrent has been downloaded, then we prioritize randomly.
// ii) Otherwise, by comparing their rarities. The one with the lower rarity gets
// prioritized.
func (dfs *DefaultPieceSelector) Less(p1, p2 *Piece) bool {
	switch {
	case p1.UnrequestedBlocks() == p1.Blocks() && p2.UnrequestedBlocks() == p2.Blocks():
		return dfs.next(p1, p2)
	default:
		return completeness(p1) > completeness(p2)
	}
}

func completeness(p *Piece) int {
	return p.CompletedBlocks() + p.PendingBlocks() - p.UnrequestedBlocks()
}

func (dfs *DefaultPieceSelector) OnPieceDownload(_ int) {
	dfs.next = nextByRarity
}

func (dfs *DefaultPieceSelector) SetTorrent(_ *Torrent) {}

//func to determine which piece to be selected among p1,p2 if they have
//all blocks unrequested.
type nextF func(p1, p2 *Piece) bool

func nextRand(p1, p2 *Piece) bool {
	return rand.Intn(2) == 1
}

func nextByRarity(p1, p2 *Piece) bool {
	return p1.rarity < p2.rarity
}
