package torrent

import (
	"math/rand"
	"sort"
	"time"
)

const optimisticSlots = 1
const defaultMaxUploadSlots = 4
const chokerTriggeringInterval = 10 * time.Second

//choker is responsible for unchoking peers
type choker struct {
	t                *Torrent
	currRound        int
	optimistic       *connInfo
	ticker           *time.Ticker
	enableOptimistic bool
	//optimistic unchoke is not included in these slots
	maxUploadSlots int
}

func newChoker(t *Torrent) *choker {
	return &choker{
		t:                t,
		maxUploadSlots:   defaultMaxUploadSlots,
		enableOptimistic: true,
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
	if c.enableOptimistic && c.currRound%5 == 0 {
		c.pickOptimisticUnchoke()
	}
	if c.optimistic != nil {
		c.optimistic.unchoke()
	}
	// We will try to unchoke `c.maxUploadSlots` conns which have the best upload rate and are
	// interested (excluding optimistic one which is already unchoked). We name these
	// conns 'downloaders'. If 'downloaders' are not sufficient, then we'll select the
	// remaining ones randomly. If the optimistic is one among the `downloaders` we'll pick another
	// optimistic randomly.
	bestPeers, optimisticCandidates := []*connInfo{}, []*connInfo{}
	for _, conn := range c.t.conns {
		switch {
		case conn.peerSeeding():
			conn.choke()
		case conn.isSnubbed() || !conn.state.isInterested:
			optimisticCandidates = append(optimisticCandidates, conn)
		default:
			bestPeers = append(bestPeers, conn)

		}
	}
	sort.Sort(byRate(bestPeers))
	toUnchoke := c.maxUploadSlots
	i := 0
	for ; i < len(bestPeers) && i < c.maxUploadSlots; i++ {
		conn := bestPeers[i]
		if conn == c.optimistic {
			continue
		}
		conn.unchoke()
		toUnchoke--
	}
	optimisticCandidates = append(optimisticCandidates, bestPeers[i:]...)
	//TODO: maybe we could not unchoke randomly but based on upload rate again?
	for _, i := range rand.Perm(len(optimisticCandidates)) {
		switch conn := optimisticCandidates[i]; {
		case c.optimistic == conn:
		case toUnchoke <= 0:
			conn.choke()
		default:
			conn.unchoke()
			if conn.state.isInterested {
				toUnchoke--
			}
		}
	}
}
