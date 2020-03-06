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
			commandCh: make(chan interface{}, 50), //ensure we have length
			state: connState{
				isInterested: true,
				amChoking:    true,
				isChoking:    true,
			},
			stats: connStats{
				downloadUsefulBytes: i,
				sumDownloading:      time.Minute,
			},
		}
		conns = append(conns, ci)
	}
	chk := &choker{
		t: &Torrent{
			conns: conns,
		},
		maxUploadSlots:   4,
		enableOptimistic: true,
	}
	chk.reviewUnchokedPeers()
	//ensure we unchoked optimistic
	assert.NotNil(t, chk.optimistic)
	assert.Equal(t, false, chk.optimistic.state.amChoking)
	//only last `maxUploadSlots` conns will be unchoked.
	var extraOptimistic, onceFlag bool
	for i, c := range conns {
		if c == chk.optimistic {
			//optimistic belongs to bestPeers
			if i >= numConns-chk.maxUploadSlots {
				extraOptimistic = true
			}
			continue
		}
		assertion := i <= len(conns)-1-chk.maxUploadSlots == c.state.amChoking
		if !assertion {
			if onceFlag || !extraOptimistic {
				t.Fail()
			}
			onceFlag = true
		}
	}
	testNumOfUnchokes(t, chk, numConns, numConns)
	testNumOfUnchokes(t, chk, 0, chk.maxUploadSlots+optimisticSlots)
	testNumOfUnchokes(t, chk, numConns-2, numConns)
}

func testNumOfUnchokes(t *testing.T, chk *choker, numNotInterested, expectedUnchoked int) {
	resetConns(chk, numNotInterested)
	chk.reviewUnchokedPeers()
	unchoked := 0
	for _, c := range chk.t.conns {
		if !c.state.amChoking {
			unchoked++
		}
	}
	assert.Equal(t, unchoked, expectedUnchoked)
}

func resetConns(chk *choker, numNotInterested int) {
	chk.currRound = 0
	for i, c := range chk.t.conns {
		c.state.isInterested = i >= numNotInterested
		c.state.amChoking = true
	}
}
