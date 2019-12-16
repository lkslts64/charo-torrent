package peer_wire

import "math"

import "math/bits"

//BitField is zero based index
type BitField []byte

func NewBitField(numPieces int) BitField {
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

func (bf BitField) FilterNotSet() (set []int) {
	for i := 0; i < len(bf)*8; i++ {
		if bf.HasPiece(uint32(i)) {
			set = append(set, i)
		}
	}
	return
}

func (bf BitField) BitsSet() (sum int) {
	for i := 0; i < len(bf); i++ {
		sum += bits.OnesCount(uint(bf[i]))
	}
	return
}
