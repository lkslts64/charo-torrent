package torrent

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

const maxUploadSlots = 4
const optimisticSlots = 1
const chokerTriggeringInterval = 10 * time.Second

type choker struct {
	t          *Torrent
	currRound  int
	optimistic *connInfo
	ticker     *time.Ticker
}

func newChoker(t *Torrent) *choker {
	return &choker{
		t: t,
	}
}

func (c *choker) startTicker() {
	c.ticker = time.NewTicker(chokerTriggeringInterval)
}

func (c *choker) pickOptimisticUnchoke() {
	optimisticUnchokeCandidates := []*connInfo{}
	for i, conn := range c.t.conns {
		if conn.state.amChoking && conn.state.isInterested {
			optimisticUnchokeCandidates = append(optimisticUnchokeCandidates, conn)
			//newly connected peers (last 3) have 3X chances to be next optimistic
			if i >= len(c.t.conns)-3 {
				optimisticUnchokeCandidates = append(optimisticUnchokeCandidates, conn, conn)
			}
		}
	}
	if len(optimisticUnchokeCandidates) == 0 {
		c.optimistic = nil
	} else {
		c.optimistic = optimisticUnchokeCandidates[rand.Intn(len(optimisticUnchokeCandidates))]
	}
}

type byRate []*connInfo

func (br byRate) Len() int { return len(br) }

func (br byRate) Less(i, j int) bool {
	return br[i].rate() > br[j].rate()
}

func (br byRate) Swap(i, j int) {
	br[i], br[j] = br[j], br[i]
}

//reviewUnchokedPeers algorithm similar to the one used at mainline client
func (c *choker) reviewUnchokedPeers() {
	if len(c.t.conns) == 0 {
		return
	}
	if c.currRound%5 == 0 {
		c.pickOptimisticUnchoke()
	}
	if c.optimistic != nil {
		c.optimistic.unchoke()
	}
	bestPeers := []*connInfo{}
	optimisticCandidates := []*connInfo{}
	for _, conn := range c.t.conns {
		if c.optimistic != nil && conn == c.optimistic {
			continue
		}
		if conn.isSnubbed() || !conn.state.isInterested || conn.peerSeeding() {
			optimisticCandidates = append(optimisticCandidates, conn)
			continue
		}
		bestPeers = append(bestPeers, conn)
	}
	sort.Sort(byRate(bestPeers))
	uploadSlots := int(math.Min(maxUploadSlots, float64(len(bestPeers))))
	optimisticCandidates = append(optimisticCandidates, bestPeers[uploadSlots:]...)
	//peers that have best upload rates
	bestPeers = bestPeers[:uploadSlots]
	for _, conn := range bestPeers {
		conn.unchoke()
	}
	numOptimistics := int(math.Max(optimisticSlots, float64(maxUploadSlots-uploadSlots)))
	var optimisticCount int
	//unchoke optimistics in random order (only if bestPeers are not sufficient)
	//and choke the remaining ones.
	indices := rand.Perm(len(optimisticCandidates))
	for _, i := range indices {
		if optimisticCandidates[i].peerSeeding() {
			optimisticCandidates[i].choke()
		} else if optimisticCount >= numOptimistics {
			optimisticCandidates[i].choke()
		} else {
			optimisticCandidates[i].unchoke()
			if optimisticCandidates[i].state.isInterested {
				optimisticCount++
			}
		}
	}
	c.currRound++
}

func connsindex(conns []*connInfo, cand *connInfo) (int, bool) {
	for i, c := range conns {
		if c == cand {
			return i, true
		}
	}
	return -1, false
}
