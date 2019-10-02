package tracker

import (
	"testing"

	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPScrapeResponse(t *testing.T) {
	var sr HTTPScrapeResp
	data := "d5:files" +
		"d20:01234567890123456789" +
		"d8:completei4e10:downloadedi5e10:incompletei6e4:name6:randomeee"
	require.NoError(t, bencode.Decode([]byte(data), &sr))
	assert.NotNil(t, sr.Files["01234567890123456789"])
}
