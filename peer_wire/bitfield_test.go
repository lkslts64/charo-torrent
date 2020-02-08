package peer_wire

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBitfield(t *testing.T) {
	bf := NewBitField(16)
	assert.Equal(t, 2, len(bf))
	bf = NewBitField(15)
	assert.Equal(t, 2, len(bf))
	bf = NewBitField(17)
	assert.Equal(t, 3, len(bf))
	bf.SetPiece(10)
	assert.Equal(t, byte(0x20), bf[1])
	assert.Equal(t, true, bf.HasPiece(10))
	bf.SetPiece(17)
	assert.Equal(t, byte(0x40), bf[2])
	bf.SetPiece(16)
	assert.Equal(t, byte(0xc0), bf[2])
	for i := 0; i < 17; i++ {
		switch i {
		case 10, 17, 16:
			assert.Equal(t, true, bf.HasPiece(i))
		default:
			assert.Equal(t, false, bf.HasPiece(i))
		}
	}
}
