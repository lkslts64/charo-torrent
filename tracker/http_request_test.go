package tracker

import (
	_ "fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var reqs = []AnnounceReq{
	{[20]byte{}, [20]byte{}, 6881, 0, 0, 2, []byte{43, 43, 65, 243}, 89789, 48, 6987},
	{
		InfoHash:   [20]byte{0: '/'},
		PeerID:     [20]byte{},
		Downloaded: 6894,
		Left:       43242,
		Uploaded:   8908090,
		Port:       6981,
	},
}

var tracker = HTTPTracker{URL: "http://lol.omg.tracker/announce"}

func TestHTTPURL(t *testing.T) {
	var err error
	var u *url.URL
	u, err = reqs[0].buildURL(tracker.URL)
	require.NoError(t, err)
	assert.EqualValues(t, "http://lol.omg.tracker/announce?compact=1&downloaded=6881&event=started&info_hash=%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&key=89789&left=0&no_peer_id=1&numwant=48&peer_id=%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&port=6987&uploaded=0", u.String())
	u, err = reqs[1].buildURL(tracker.URL)
	require.NoError(t, err)
	assert.EqualValues(t, "http://lol.omg.tracker/announce?compact=1&downloaded=6894&info_hash=%2F%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&left=43242&no_peer_id=1&peer_id=%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00%00&port=6981&uploaded=8908090", u.String())
}
