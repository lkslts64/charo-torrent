package tracker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var reqs = []HTTPAnnounceReqParams{
	{[20]byte{}, [20]byte{}, 6881, 0, 0, 387278, true, false, "started", 0, []byte("5321735827432hfsdhf3hf"), []byte{}},
	{[20]byte{}, [20]byte{}, 6894, 43242, 8908090, 98093217, false, true, "stopped", 0, []byte{}, []byte("43782hfjs831/")},
}

func TestURL(t *testing.T) {
	announce := "http://lol.omg.tracker/announce"
	//var u *url.URL
	var err error
	for _, req := range reqs {
		_, err = req.buildURL(announce)
		require.NoError(t, err)
		//fmt.Println(u.String(), req)
	}
}
