package tracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lkslts64/charo-torrent/bencode"
)

//var defaultHTTPUserAgent = "Go-Torrent"

//if both cheapPeers and Peers are set then we pick only one to decode
//based on bencode/decode.go
func TestDecodeHTTPResponsePeerDicts(t *testing.T) {
	var resp Response
	require.NoError(t, bencode.Decode(
		[]byte("d5:peersl"+
			"d2:ip7:1.2.3.47:peer id20:thisisthe20bytepeeri4:porti9999ee"+
			"d7:peer id20:thisisthe20bytepeeri2:ip39:2001:0db8:85a3:0000:0000:8a2e:0370:73344:porti9998ee"+
			"e"+
			//"6:peers618:123412341234123456"+
			"8:intervali43242e"+
			"e"),
		&resp))

	require.Len(t, resp.Peers, 2)
	assert.Equal(t, []byte("thisisthe20bytepeeri"), resp.Peers[0].ID)
	assert.EqualValues(t, 9999, resp.Peers[0].Port)
	assert.EqualValues(t, 9998, resp.Peers[1].Port)
	assert.NotNil(t, resp.Peers[0].IP)
	assert.NotNil(t, resp.Peers[1].IP)
	require.NoError(t, resp.Parse())

	//assert.Len(t, resp.Peers6, 1)
	//assert.EqualValues(t, "1234123412341234", resp.Peers6[0].IP)
	//assert.EqualValues(t, 0x3536, resp.Peers6[0].Port)
}

func TestDecodeHttpResponseNoPeers(t *testing.T) {
	var resp Response
	require.NoError(t, bencode.Decode(
		[]byte("d8:intervali765e5:peers18:123412341234123456e"),
		&resp,
	))
	require.Len(t, resp.Peers, 0)
	assert.Len(t, resp.CheapPeers, 18)
	require.NoError(t, resp.Parse())
}

func TestDecodeHttpResponsePeers6NotCompact(t *testing.T) {
	var resp Response
	require.NoError(t, bencode.Decode(
		[]byte("d8:intervali34242e5:peerslee"),
		&resp,
	))
	var resp2 Response
	require.NoError(t, bencode.Decode(
		[]byte("d8:intervali34242e5:peers0:e"),
		&resp2,
	))
}
