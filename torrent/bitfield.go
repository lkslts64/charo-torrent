package torrent

import "math"

type BitField []byte

func NewBitField(numPieces int) BitField {
	return make([]byte, int(math.Ceil(float64(numPieces)/8.0)))
}

func (bf BitField) hasPiece(i uint32) bool {
	index := i / 8
	mask := byte(1 << (7 - i%8))
	if bf[index]&mask > 0 {
		return true
	}
	return false
}

func (bf BitField) setPiece(i uint32) {
	index := i / 8
	mask := byte(1 << (7 - i%8))
	bf[index] |= mask
}
