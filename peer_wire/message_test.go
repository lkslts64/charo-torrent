package peer_wire

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteUnchoke(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	go func() {
		defer w.Close()
		_, err := w.Write((&Msg{
			Kind: Unchoke,
		}).Encode())
		require.NoError(t, err)
	}()
	b := make([]byte, 5)
	_, err := r.Read(b)
	require.NoError(t, err)
	assert.EqualValues(t, []byte{0, 0, 0, 1, 1}, b)
}

func TestReadChoke(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	go func() {
		defer w.Close()
		w.Write([]byte{0, 0, 0, 1, 0})
	}()
	msg, err := Decode(r)
	require.NoError(t, err)
	assert.EqualValues(t, &Msg{
		Kind: Choke,
	}, msg)
}

func ReadWrite(t *testing.T, expect *Msg) {
	r, w := io.Pipe()
	defer r.Close()
	go func() {
		defer w.Close()
		_, err := w.Write(expect.Encode())
		require.NoError(t, err)
	}()
	msg, err := Decode(r)
	require.NoError(t, err)
	assert.EqualValues(t, expect, msg)
}

func TestReadWrite(t *testing.T) {
	ReadWrite(t, &Msg{
		Kind:  Piece,
		Index: 342,
		Begin: 0x44,
		Block: []byte{0xff, 0xa0},
	})
	ReadWrite(t, &Msg{
		Kind: KeepAlive,
	})
	ReadWrite(t, &Msg{
		Kind: Bitfield,
		Bf:   []byte{0x43, 0x83, 0x42},
	})
}

func TestWriteExtension(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	write := func(extID ExtensionID, extMsg interface{}) {
		w.Write((&Msg{
			Kind:        Extended,
			ExtendedID:  extID, //handshake
			ExtendedMsg: extMsg,
		}).Encode())
	}
	test := func(extID byte, expected string) {
		b := make([]byte, 4+1+1+len(expected))
		_, err := r.Read(b)
		require.NoError(t, err)
		assert.EqualValues(t, uint32(1+1+len(expected)), binary.BigEndian.Uint32(b[:4]))
		assert.EqualValues(t, []byte{Extended, extID}, b[4:6])
		assert.EqualValues(t, expected, string(b[6:]))
	}
	go write(ExtHandshakeID, ExtHandshakeDict{
		"m":      10,
		"loukas": 100,
		"anon":   256,
	})
	expected := "d4:anoni256e6:loukasi100e1:mi10ee"
	test(byte(ExtHandshakeID), expected)
	go write(ExtMetadataID, MetadataExtMsg{
		Kind:  MetadataDataID,
		Piece: 1003,
		Data:  []byte{0x43, 0x43, 0x54, 0x86, 0x99},
	})
	expected = "d8:msg_typei1e5:piecei1003ee\x43\x43\x54\x86\x99"
	test(byte(ExtMetadataID), expected)
}

func TestReadExtension(t *testing.T) {
	//ext handshake
	r, w := io.Pipe()
	expected := "d4:anond4:aaaai444e3:bbb2:ose6:loukasli1ei3333ei444ee1:mi10ee"
	defer r.Close()
	write := func(ExtID byte, expected string) {
		len := uint32(1 + 1 + len(expected))
		buf := make([]byte, len+4)
		binary.BigEndian.PutUint32(buf[:4], len)
		buf[4] = Extended
		buf[5] = ExtID
		copy(buf[6:], []byte(expected))
		w.Write(buf)
	}
	go write(byte(ExtHandshakeID), expected)
	msg, err := Decode(r)
	require.NoError(t, err)
	dict := msg.ExtendedMsg.(ExtHandshakeDict)
	//anon val is a dict
	anonVal := dict["anon"].(map[string]interface{})
	assert.EqualValues(t, 444, anonVal["aaaa"].(int64))
	assert.EqualValues(t, "os", anonVal["bbb"].(string))
	//loukas val is a list
	loukasVal := dict["loukas"].([]interface{})
	assert.EqualValues(t, 1, loukasVal[0].(int64))
	assert.EqualValues(t, 3333, loukasVal[1].(int64))
	assert.EqualValues(t, 444, loukasVal[2].(int64))
	//m val is int
	mVal := dict["m"].(int64)
	assert.EqualValues(t, mVal, 10)
	//ext metadata
	expected = "d8:msg_typei0e5:piecei2e10:total_sizei3452ee"
	go write(byte(ExtMetadataID), expected)
	msg, err = Decode(r)
	require.NoError(t, err)
	metaExt := msg.ExtendedMsg.(MetadataExtMsg)
	assert.EqualValues(t, metaExt.Kind, 0)
	assert.EqualValues(t, metaExt.Piece, 2)
	assert.EqualValues(t, metaExt.TotalSz, 3452)
	//ext metadata data msg
	expected = "d8:msg_typei1e5:piecei2e10:total_sizei3452ee\x00\x11\x22\x33\x44"
	go write(byte(ExtMetadataID), expected)
	msg, err = Decode(r)
	require.NoError(t, err)
	metaExt = msg.ExtendedMsg.(MetadataExtMsg)
	assert.EqualValues(t, metaExt.Kind, 1)
	assert.EqualValues(t, metaExt.Piece, 2)
	assert.EqualValues(t, metaExt.TotalSz, 3452)
	assert.EqualValues(t, metaExt.Data, []byte("\x00\x11\x22\x33\x44"))
}
