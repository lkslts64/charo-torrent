package torrent

import (
	"testing"
	"time"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/stretchr/testify/assert"
)

func TestChoker(t *testing.T) {
	conns := []*connInfo{}
	//create conns by ascending download rate order.
	numConns := 55
	for i := 0; i < numConns; i++ {
		ci := &connInfo{
			t: &Torrent{
				mi: &metainfo.MetaInfo{},
			},
			commandCh: make(chan interface{}, 1),
			state: connState{
				isInterested: true,
				amChoking:    true,
				isChoking:    true,
			},
			stats: connStats{
				downloadUseful: i,
				sumDownloading: time.Minute,
			},
		}
		conns = append(conns, ci)
	}
	chk := newChoker(&Torrent{
		conns: conns,
	})
	chk.reviewUnchokedPeers()
	//ensure we unchoked optimistic
	assert.NotNil(t, chk.optimistic)
	assert.Equal(t, false, chk.optimistic.state.amChoking)
	//only last `maxUploadSlots` conns will be unchoked.
	var extraOptimistic, onceFlag bool
	for i, c := range conns {
		if c == chk.optimistic {
			//optimistic belongs to bestPeers
			if i >= numConns-maxUploadSlots {
				extraOptimistic = true
			}
			continue
		}
		assertion := i <= len(conns)-1-maxUploadSlots == c.state.amChoking
		if !assertion {
			if onceFlag || !extraOptimistic {
				t.FailNow()
			}
			onceFlag = true
		}
	}
	//Test choker when there is no peer interested
	for i := 0; i < numConns; i++ {
		chk.t.conns[i].state.isInterested = false
	}
	chk.reviewUnchokedPeers()
	for i := 0; i < numConns; i++ {
		assert.Equal(t, false, chk.t.conns[i].state.amChoking)
	}
}
