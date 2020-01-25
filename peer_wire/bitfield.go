package peer_wire

import "math"

import "math/bits"

//BitField is zero based index
type BitField []byte

//how much length a BitField will have if created with numPieces as argument
func BfLen(numPieces int) int {
	return int(math.Ceil(float64(numPieces) / 8))
}

func NewBitField(numPieces int) BitField {
	return make([]byte, BfLen(numPieces))
}

func (bf BitField) Valid(expectedLen int) bool {
	return len(bf) == BfLen(expectedLen)
}

func (bf BitField) HasPiece(i int) bool {
	index := i / 8
	mask := uint8(1 << uint(7-i%8))
	return uint8(bf[index]&mask) > 0
}

func (bf BitField) SetPiece(i int) {
	index := i / 8
	mask := byte(1 << uint(7-i%8))
	bf[index] |= mask
}

func (bf BitField) FilterNotSet() (set []int) {
	for i := 0; i < len(bf)*8; i++ {
		if bf.HasPiece(i) {
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
