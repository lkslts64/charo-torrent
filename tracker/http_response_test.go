package tracker

import (
	"net"
	"testing"

	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeHTTPResponsePeerDicts(t *testing.T) {
	var resp httpAnnounceResponse
	require.NoError(t, bencode.Decode(
		[]byte("d5:peersl"+
			"d2:ip7:1.2.3.47:peer id20:thisisthe20bytepeeri4:porti9999ee"+
			"d7:peer id20:thisisthe20bytepeeri2:ip39:2001:0db8:85a3:0000:0000:8a2e:0370:73344:porti9998ee"+
			"e"+
			"8:intervali43242e"+
			"e"),
		&resp))

	require.Len(t, resp.Peers, 2)
	assert.Equal(t, []byte("thisisthe20bytepeeri"), resp.Peers[0].ID)
	assert.EqualValues(t, 9999, resp.Peers[0].Port)
	assert.EqualValues(t, 9998, resp.Peers[1].Port)
	assert.NotNil(t, resp.Peers[0].IP)
	assert.NotNil(t, resp.Peers[1].IP)
	require.NoError(t, resp.parse())
	assert.EqualValues(t, net.ParseIP("1.2.3.4"), resp.Peers[0].IP)
	assert.EqualValues(t, net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334"), resp.Peers[1].IP)
}

func TestDecodeHttpResponseCheapPeers(t *testing.T) {
	var resp httpAnnounceResponse
	require.NoError(t, bencode.Decode(
		[]byte("d8:intervali765e5:peers18:\x01\x02\x03\x0422123434123456e"),
		&resp,
	))
	require.Len(t, resp.Peers, 0)
	assert.Len(t, resp.CheapPeers, 18)
	require.NoError(t, resp.parse())
	//ensure Peers struct was field from cheapPeers after parsing
	assert.Len(t, resp.Peers, 3)
	assert.EqualValues(t, net.IPv4(1, 2, 3, 4).To16(), resp.Peers[0].IP)
}

func TestDecodeHttpResponseEmptyPeers(t *testing.T) {
	var resp httpAnnounceResponse
	require.NoError(t, bencode.Decode(
		[]byte("d8:intervali34242e5:peerslee"),
		&resp,
	))
	var resp2 httpAnnounceResponse
	require.NoError(t, bencode.Decode(
		[]byte("d8:intervali34242e5:peers0:e"),
		&resp2,
	))
}
