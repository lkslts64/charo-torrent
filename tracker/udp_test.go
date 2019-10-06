package tracker

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ihash = [20]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

func TestWriteBinary(t *testing.T) {
	var buf bytes.Buffer
	req := AnnounceReq{ihash, [20]byte{}, 6881, 0, 0, 2, 0, 89789, 48, 6981}
	var i int32 = 5
	err := writeBinary(&buf, req, i)
	require.NoError(t, err)
	assert.EqualValues(t, buf.Len(), 2*20+3*8+4*4+2+4) //size of struct Announcereq
	ihashBytes := make([]byte, 20)
	buf.Read(ihashBytes)
	assert.EqualValues(t, ihashBytes, ihash[:])
	//test slice
	buf.Reset()
	var ihashes = [][20]byte{ihash, [20]byte{2: 1, 4: 5}}
	err = writeBinary(&buf, ihashes)
	require.NoError(t, err)
	assert.EqualValues(t, buf.Len(), 40)
	assert.EqualValues(t, buf.Bytes(), append(ihashes[0][:], ihashes[1][:]...))
}

func TestReadBinary(t *testing.T) {
	type fixed struct {
		Interval int16
		Leechers int16
		Seeders  int16
	}
	fixedData := fixed{}
	buf := bytes.NewBuffer([]byte{0x0, 0x0, 0x1, 0x1, 0x2, 0x2})
	err := readFromBinary(buf, &fixedData)
	require.NoError(t, err)
	assert.EqualValues(t, fixedData, fixed{0x0000, 0x0101, 0x0202})
	//test slice
	fixedData = fixed{1, 2, 3}
	slc := []byte{0x0, 0x1, 0x2, 0x3, 0x4, 0x5}
	slc = append(slc, slc...)
	buf = bytes.NewBuffer(slc)
	slcFixedData := make([]fixed, 2)
	err = readFromBinary(buf, &slcFixedData)
	require.NoError(t, err)
	assert.EqualValues(t, slcFixedData, []fixed{{0x0001, 0x0203, 0x0405}, {0x0001, 0x0203, 0x0405}})
}
