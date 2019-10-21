package tracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScrapeURL(t *testing.T) {
	var u1 trackerURL = "omg://gfhjds231/dfs42342/1312321/announce/fsdfds"
	var u2 trackerURL = "omg://fdsfsd/487234/1312321/announce.php"
	s1 := u1.ScrapeURL()
	assert.EqualValues(t, s1, "")
	s2 := u2.ScrapeURL()
	assert.EqualValues(t, s2, "omg://fdsfsd/487234/1312321/scrape.php")
}
