package peer_wire

import "math"

//BitField is zero based index
type BitField []byte

func newBitField(numPieces int) BitField {
	return make([]byte, int(math.Ceil(float64(numPieces)/8.0)))
}

func (bf BitField) HasPiece(i uint32) bool {
	index := i / 8
	mask := byte(1 << (7 - i%8))
	if bf[index]&mask > 0 {
		return true
	}
	return false
}

func (bf BitField) SetPiece(i uint32) {
	index := i / 8
	mask := byte(1 << (7 - i%8))
	bf[index] |= mask
}
