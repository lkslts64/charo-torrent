package torrent

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

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
	possibleOptimisticUnchokes := []*connInfo{}
	for i, conn := range c.t.conns {
		if conn.state.amChoking && conn.state.isInterested {
			possibleOptimisticUnchokes = append(possibleOptimisticUnchokes, conn)
			//newly connected peers (last 3) have 3X chances to be next optimistic
			if i >= len(c.t.conns)-3 {
				possibleOptimisticUnchokes = append(possibleOptimisticUnchokes, conn, conn)
			}
		}
	}
	if len(possibleOptimisticUnchokes) == 0 {
		c.optimistic = nil
	} else {
		c.optimistic = possibleOptimisticUnchokes[rand.Intn(len(possibleOptimisticUnchokes))]
	}
}

//type unchokingCandidate *connInfo

type byRate []*connInfo

func (uc byRate) Len() int { return len(uc) }

func (uc byRate) Less(i, j int) bool {
	rate := func(index int) int {
		t := uc[index].t
		c := (*connInfo)(uc[index])
		var dur time.Duration
		if t.seeding {
			dur = c.durationUploading()
			if dur == 0 {
				return 0
			}
			return c.stats.uploadUseful / int(c.durationUploading())
		}
		dur = c.durationDownloading()
		if dur == 0 {
			return 0
		}
		return c.stats.downloadUseful / int(c.durationDownloading())
	}
	return rate(i) > rate(j)
}

func (uc byRate) Swap(i, j int) {
	uc[i], uc[j] = uc[j], uc[i]
}

//reviewUnchokedPeers algorithm similar to the one used at mainline client
func (c *choker) reviewUnchokedPeers() {
	if len(c.t.conns) == 0 {
		return
	}
	if c.currRound%5 == 0 {
		c.pickOptimisticUnchoke()
	}
	bestPeers := []*connInfo{}
	for _, conn := range c.t.conns {
		if conn.isSnubbed() || !conn.state.isInterested || conn.peerSeeding() {
			continue
		}
		bestPeers = append(bestPeers, conn)
	}
	sort.Sort(byRate(bestPeers))
	uploadSlots := int(math.Min(maxUploadSlots, float64(len(bestPeers))))
	optimisticCandidates := bestPeers[uploadSlots:]
	//peers that have best upload rates
	bestPeers = bestPeers[:uploadSlots]
	for _, conn := range bestPeers {
		conn.unchoke()
	}
	numOptimistics := int(math.Max(optimisticSlots, float64(maxUploadSlots-uploadSlots)))
	//if we haven't yet unchoked optimistic peer,then do it
	if c.optimistic != nil && !contains(bestPeers, c.optimistic) {
		c.optimistic.unchoke()
		numOptimistics--
	}
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

func contains(conns []*connInfo, cand *connInfo) bool {
	for _, c := range conns {
		if c == cand {
			return true
		}
	}
	return false
}
