package tracker

import (
	"context"
	_ "fmt"
	"math/rand"
	"net"
	"net/url"
	"sync"
	"testing"

	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var reqs = []AnnounceReq{
	{[20]byte{}, [20]byte{}, 6881, 0, 0, 2, 0, 89789, 48, 6987},
	{
		InfoHash:   [20]byte{0: '/'},
		PeerID:     [20]byte{},
		Downloaded: 6894,
		Left:       43242,
		Uploaded:   8908090,
		Port:       6981,
		Numwant:    -1,
	},
}

var tracker = HTTPTrackerURL{url: "http://lol.omg.tracker/announce"}

func TestHTTPURL(t *testing.T) {
	var err error
	var u *url.URL
	u, err = reqs[0].buildURL(tracker.url)
	require.NoError(t, err)
	assert.EqualValues(t, "http://lol.omg.tracker/announce?compact=1&downloaded=6881&event=started&info_hash=%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&key=89789&left=0&no_peer_id=1&numwant=48&peer_id=%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&port=6987&uploaded=0", u.String())
	u, err = reqs[1].buildURL(tracker.url)
	require.NoError(t, err)
	assert.EqualValues(t, "http://lol.omg.tracker/announce?compact=1&downloaded=6894&info_hash=%2F%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&left=43242&no_peer_id=1&peer_id=%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&port=6981&uploaded=8908090", u.String())
}

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

func TestHTTPScrapeResponse(t *testing.T) {
	var sr httpScrapeResp
	data := "d5:files" +
		"d20:01234567890123456789" +
		"d8:completei4e10:downloadedi5e10:incompletei6e4:name6:randomeee"
	require.NoError(t, bencode.Decode([]byte(data), &sr))
	assert.NotNil(t, sr.Files["01234567890123456789"])
}

var httpTrackers = []string{
	"http://tracker.bt4g.com:2095/announce",
	"http://open.trackerlist.xyz:80/announce",
}

func TestThirdPartyRequest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.SkipNow()
	}
	ctx := context.Background()
	var peerID [20]byte
	var ihash [20]byte
	rand.Read(peerID[:])
	rand.Read(ihash[:])
	var wg sync.WaitGroup
	for _, trackerURL := range udpTrackers {
		wg.Add(1)
		go func(trackerURL string) {
			defer wg.Done()
			tr, err := NewTrackerURL(trackerURL)
			require.NoError(t, err)
			resp, err := tr.Announce(ctx, AnnounceReq{
				InfoHash: ihash,
				PeerID:   peerID,
				Port:     6891,
				Numwant:  -1,
				Event:    Stopped,
			})
			require.NoError(t, err)
			if resp.Leechers != 0 || resp.Seeders != 0 || len(resp.Peers) != 0 {
				// The info hash we generated was random in 2^160 space. If we
				// get a hit, something is weird.
				t.FailNow()
			}
		}(trackerURL)
	}
	wg.Wait()
}
