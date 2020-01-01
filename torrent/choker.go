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

type unchokingCandidate *connInfo

type unchokingCandidates []unchokingCandidate

func (uc unchokingCandidates) Len() int { return len(uc) }

func (uc unchokingCandidates) Less(i, j int) bool {
	rate := func(index int) time.Duration {
		if uc[index].stats.sumDownloading == 0 {
			return 0
		}
		t := uc[index].t
		c := (*connInfo)(uc[index])
		if t.seeding {
			return c.durationUploading()
		}
		return c.durationDownloading()
	}
	return rate(i) > rate(j)
}

func (uc unchokingCandidates) Swap(i, j int) {
	uc[i], uc[j] = uc[j], uc[i]
}

func (uc unchokingCandidates) contains(cand unchokingCandidate) bool {
	for _, c := range uc {
		if c == cand {
			return true
		}
	}
	return false
}

//reviewUnchokedPeers algorithm similar to the one used at mainline client
func (c *choker) reviewUnchokedPeers() {
	if c.currRound%5 == 0 {
		c.pickOptimisticUnchoke()
	}
	bestPeers := unchokingCandidates{}
	for _, conn := range c.t.conns {
		if conn.isSnubbed() || !conn.state.isInterested || conn.peerSeeding() {
			continue
		}
		bestPeers = append(bestPeers, conn)
	}
	sort.Sort(bestPeers)
	uploadSlots := int(math.Min(maxUploadSlots, float64(len(bestPeers))))
	//peers that have best upload rates
	bestPeers = bestPeers[:uploadSlots]
	optimistics := []*connInfo{}
	for _, conn := range c.t.conns {
		if bestPeers.contains(conn) {
			conn.unchoke()
		} else {
			optimistics = append(optimistics, conn)
		}
	}
	numOptimistics := int(math.Max(optimisticSlots, float64(maxUploadSlots-uploadSlots)))
	//if optimistic unchoke belongs to 'downloaders',
	//we 'll unchoke one more peer randomly
	if c.optimistic != nil && !bestPeers.contains(c.optimistic) {
		c.optimistic.unchoke()
		numOptimistics--
	}
	var optimisticCount int
	//unchoke optimistics in random order
	//only if bestPeers are not sufficient
	indices := rand.Perm(len(optimistics))
	for _, i := range indices {
		if optimistics[i].peerSeeding() {
			optimistics[i].choke()
		} else if optimisticCount >= numOptimistics {
			optimistics[i].choke()
		} else {
			optimistics[i].unchoke()
			if optimistics[i].state.isInterested {
				optimisticCount++
			}
		}
	}
	c.currRound++
}
