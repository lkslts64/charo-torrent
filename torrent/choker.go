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
	defer func() {
		c.currRound++
	}()
	if len(c.t.conns) == 0 {
		return
	}
	if c.currRound%5 == 0 {
		c.pickOptimisticUnchoke()
	}
	bestPeers, optimisticCandidates := []*connInfo{}, []*connInfo{}
	for _, conn := range c.t.conns {
		if conn.peerSeeding() {
			conn.choke()
		}
		if conn.isSnubbed() || !conn.state.isInterested {
			optimisticCandidates = append(optimisticCandidates, conn)
		} else {
			bestPeers = append(bestPeers, conn)
		}
	}
	sort.Sort(byRate(bestPeers))
	uploadSlots := int(math.Min(maxUploadSlots, float64(len(bestPeers))))
	optimisticCandidates = append(optimisticCandidates, bestPeers[uploadSlots:]...)
	//peers that have best upload rates (optimistic may belong to bestPeers)
	bestPeers = bestPeers[:uploadSlots]
	for _, conn := range bestPeers {
		conn.unchoke()
	}
	numOptimistics := optimisticSlots + (maxUploadSlots - uploadSlots)
	var optimisticCount int
	if containsConn(optimisticCandidates, c.optimistic) { //optimistic belongs to optimisticCandidates.
		c.optimistic.unchoke()
		optimisticCount++
	}
	//unchoke optimistics in random order and choke the remaining ones.
	indices := rand.Perm(len(optimisticCandidates))
	for _, i := range indices {
		//we've already handled this case
		if c.optimistic != nil && c.optimistic == optimisticCandidates[i] {
			continue
		}
		if optimisticCount >= numOptimistics {
			optimisticCandidates[i].choke()
		} else {
			//we'll get here only if bestPeers are not sufficient or if
			//optimistic belonged in bestPeers
			optimisticCandidates[i].unchoke()
			if optimisticCandidates[i].state.isInterested {
				optimisticCount++
			}
		}
	}
}

func containsConn(conns []*connInfo, cand *connInfo) bool {
	for _, c := range conns {
		if c == cand {
			return true
		}
	}
	return false
}
