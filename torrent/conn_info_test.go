package torrent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestConnInfoDuration(t *testing.T) {
	ci := connInfo{}
	testDuration(t, &ci, connInfoFuncs{
		ci.stoppedDownloading, ci.startedDownloading, ci.durationDownloading, revertMyInterest,
	})
	testDuration(t, &ci, connInfoFuncs{
		ci.stoppedUploading, ci.startedUploading, ci.durationUploading, revertPeerInterest,
	})
}

func revertMyInterest(ci *connInfo) {
	ci.state.amInterested = !ci.state.amInterested
}

func revertPeerInterest(ci *connInfo) {
	ci.state.isInterested = !ci.state.isInterested
}

type connInfoFuncs struct {
	stop           func()
	start          func()
	duration       func() time.Duration
	revertInterest func(ci *connInfo)
}

func testDuration(t *testing.T, ci *connInfo, cf connInfoFuncs) {
	//t.Log(ci.stats.sumDownloading)
	assert.EqualValues(t, 0, cf.duration())
	cf.revertInterest(ci)
	cf.start()
	time.Sleep(time.Millisecond)
	assert.Greater(t, int64(cf.duration()), int64(0))
	cf.revertInterest(ci)
	cf.stop()
	time.Sleep(time.Second)
	assert.Less(t, int64(cf.duration()), int64(time.Second))
}
