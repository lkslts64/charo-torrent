package torrent

import (
	"github.com/lkslts64/charo-torrent/peer_wire"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

//Active conns are the ones that we download/upload from/to
//Passive are the ones that we are not-interested/choking or not-interested/choked

//Every conn is managed on a seperate goroutine

//Concurency:
//when master wants to send something at peerconns
//he should try to send it (without blocking) because
//a peerconn may be closed or reading/writing to db
//So, we should hold a queue of jobs and try send them
//every time or !!!reflect.SelectCase!!!

//Every goroutine is associated with a chan of Events.
//When master gouritine wants to change state of a particular
//conn goroutine it sends an Event through this channel.

var maxConns = 55

const maxUploadSlots = 4
const optimisticSlots = 1

var blockSz = uint32(1 << 14)           //16KiB
var maxRequestBlockSz = uint32(1 << 15) //32KiB
//spec doesn't say anything about this by we enfore it
var minRequestBlockSz = uint32(1 << 13) //16KiB

//Torrent
type Torrent struct {
	tm *TorrentMeta
	//concurrent access at conns. Only nil conns
	//get modified concurently so readers of non-nil conns can
	//have access without acquiring locks.Non nil
	//conns will be set to zero only by the goroutine that
	//manages them so no prob
	connMu   sync.Mutex
	numConns int
	//pconns   []*PeerConn
	//pstats   []peerStats
	//size of len(pconns) * eventChSize is reasonable
	events chan event2

	//alternative:
	//---------------
	//active peer conns
	//we take msg from peerconn to add it at this map
	//if numConns < maxConns then we add it,else conn
	//should get dropped
	conns []*connection
	//conns map[*connection]struct{}

	bitFields []BitField

	//eventCh chan event
	pieces Pieces

	currChokerRound int
	optimistic      *connection

	//surely we 'll need this
	done chan struct{}
}

type connection struct {
	pc    *PeerConn
	stats peerStats
}

type peerStats struct {
	//duplicate - also peer conn has these three
	uploadUseful   int
	downloadUseful int
	info           peerInfo
	//last time we were interested and peer was unchoking
	lastStartedDownloading time.Time
	//duration we were in downloading state
	sumDownloading time.Duration
	snubbed        bool
	isSeeding      bool
	amSeeding      bool
}

func (ps *peerStats) uploadLimitsReached() bool {
	//if we have uploaded 100KiB more than downloaded (anacrolix has 100)
	return ps.uploadUseful-ps.downloadUseful > (1<<10)*300
}

func (ps *peerStats) isSnubbed() bool {
	if ps.amSeeding {
		return false
	}
	if ps.uploadLimitsReached() {
		return true
	}
	/*TODO:take into consideration this condition
	if !ps.lastPieceMsg.IsZero() && time.Since(ps.lastPieceMsg) >= 30*time.Second {
		return true
	}*/
	return false
}

func (t *Torrent) sendJob(conn *connection, job interface{}) {
	conn.pc.jobCh.In() <- job
}

func (t *Torrent) choke(conn *connection) {
	if !conn.stats.info.amChoking {
		t.sendJob(conn, &peer_wire.Msg{
			Kind: peer_wire.Choke,
		})
		conn.stats.info.amChoking = !conn.stats.info.amChoking
	}
}

func (t *Torrent) unchoke(conn *connection) {
	if conn.stats.info.amChoking {
		t.sendJob(conn, &peer_wire.Msg{
			Kind: peer_wire.Unchoke,
		})
		conn.stats.info.amChoking = !conn.stats.info.amChoking
	}
}

func (t *Torrent) interested(conn *connection) {
	if !conn.stats.info.amInterested {
		t.sendJob(conn, &peer_wire.Msg{
			Kind: peer_wire.Interested,
		})
		conn.stats.info.amInterested = !conn.stats.info.amInterested
	}
}

func (t *Torrent) notInterested(conn *connection) {
	if conn.stats.info.amInterested {
		t.sendJob(conn, &peer_wire.Msg{
			Kind: peer_wire.NotInterested,
		})
		conn.stats.info.amInterested = !conn.stats.info.amInterested
	}
}

func (t *Torrent) have(conn *connection, i uint32) {
	if !t.tm.ownPiece(i) {
		panic("send have without having piece")
	}
	t.sendJob(conn, &peer_wire.Msg{
		Kind:  peer_wire.Have,
		Index: i,
	})
}

func (t *Torrent) mainLoop() {
	chokerTicker := time.NewTicker(10 * time.Second)
	defer chokerTicker.Stop()
	for {
		select {
		case e := <-t.events:

		case <-chokerTicker.C:
			t.choking()
		}
	}

}

func (t *Torrent) parseEvent(e event2) {
	switch v := e.val.(type) {
	case eventID:
		switch v {
		case isInterested:
			e.conn.stats.info.isInterested = true
			if !e.conn.stats.info.amChoking {
				t.choking()
			}
		case isNotInterested:
			e.conn.stats.info.isInterested = false
			if !e.conn.stats.info.amChoking {
				t.choking()
			}
		case isChoking:
			//forward unsatisfied to other interested-unchoked peers
		case isUnchoking:
			//give this peer some requests
		}
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Have:
		case peer_wire.Bitfield:
		}
	case *downloadedBlock:
		//update interests
	case *uploadedBlock:
		//add to stats
	case metainfoSize:
	}
}

//Useful for master goroutine
/*case choke, unchoke, interested, notInterested:
	(&peer_wire.Msg{
		Kind: peer_wire.MessageID(a.id),
	}).Write(p.conn)
case have:
	i := a.val.(uint32)
	(&peer_wire.Msg{
		Kind:  peer_wire.MessageID(a.id),
		Index: i,
	}).Write(p.conn)
case bitfield:
	bf := a.val.(BitField)
	(&peer_wire.Msg{
		Kind:     peer_wire.MessageID(a.id),
		Bitfield: []byte(bf),
	}).Write(p.conn)
case request, cancel:
*/

/*func NewTorrent(filename string) (*Torrent, error) {
	t, err := NewTorrentMeta(filename)
	if err != nil {
		return nil, err
	}
	return &Torrent{
		tm:    t,
		conns: make([]net.Conn, maxConns),
		chans: make([]chan Event, maxConns),
		actCh: make(chan interface{}, maxConns),
	}, nil
	return nil, nil
}*/

//this func is started in its own goroutine.
//when we close eventCh of pconn, the goroutine
//exits
func (t *Torrent) addAggregated(i int, conn *connection) {
	for e := range conn.pc.eventCh {
		t.events <- event2{conn, e}
	}
}

/*careful when using this, we might 'range' over nil chan
func (t *Torrent) aggregateEvents() {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	for i, conn := range t.pconns {
		if conn != nil {
			go t.addAggregated(i, conn)
		}
	}
}*/

//careful when using this, we might send over nil chan
func (t *Torrent) broadcastJob(job interface{}) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	for _, conn := range t.conns {
		conn.pc.jobCh.In() <- job
	}
}

func (t *Torrent) pickOptimisticUnchoke() {
	possibleOptimisticUnchokes := []*connection{}
	for i, conn := range t.conns {
		if conn.stats.info.amChoking && conn.stats.info.isInterested {
			possibleOptimisticUnchokes = append(possibleOptimisticUnchokes, conn)
			//newly connected peers have better chances
			if i >= len(t.conns)-3 { //pulled from my ass
				possibleOptimisticUnchokes = append(possibleOptimisticUnchokes, conn, conn)
			}
		}
	}
	if len(possibleOptimisticUnchokes) == 0 {
		t.optimistic = nil
	} else {
		t.optimistic = possibleOptimisticUnchokes[rand.Intn(len(possibleOptimisticUnchokes))]
	}
}

type unchokingCandidate *connection

type unchokingCandidates []unchokingCandidate

func (uc unchokingCandidates) Len() int { return len(uc) }

func (uc unchokingCandidates) Less(i, j int) bool {
	rate := func(index int) int {
		if uc[index].stats.sumDownloading == 0 {
			return 0
		}
		//weird (hacky) way to determine if we are in seeding mode
		//from a connection
		if uc[index].pc.t.conns[index].stats.amSeeding {
			return uc[index].stats.uploadUseful / int(uc[index].stats.sumDownloading)
		}
		return uc[index].stats.downloadUseful / int(uc[index].stats.sumDownloading)
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

//choking algorithm similar to the one used at mainline client
func (t *Torrent) choking() {
	if t.currChokerRound%5 == 0 {
		t.pickOptimisticUnchoke()
	}
	bestPeers := unchokingCandidates{}
	for _, conn := range t.conns {
		if conn.stats.isSnubbed() || !conn.stats.info.isInterested || conn.stats.isSeeding {
			continue
		}
		bestPeers = append(bestPeers, conn)
	}
	sort.Sort(bestPeers)
	uploadSlots := int(math.Min(maxUploadSlots, float64(len(bestPeers))))
	//peers that have best upload rates
	bestPeers = bestPeers[:uploadSlots]
	optimistics := unchokingCandidates{}
	for _, conn := range t.conns {
		if bestPeers.contains(conn) {
			t.unchoke(conn)
		} else {
			optimistics = append(optimistics, conn)
		}
	}
	numOptimistics := int(math.Max(optimisticSlots, float64(maxUploadSlots-uploadSlots)))
	//if optimistic unchoke belongs to 'downloaders',
	//we 'll unchoke one more peer randomly
	if t.optimistic != nil && !bestPeers.contains(t.optimistic) {
		t.unchoke(t.optimistic)
		numOptimistics--
	}
	var optimisticCount int
	//unchoke optimistics randomly
	//only if bestPeers are not sufficient
	indices := rand.Perm(len(optimistics))
	for _, i := range indices {
		if optimistics[i].stats.isSeeding {
			t.choke(optimistics[i])
		} else if optimisticCount >= numOptimistics {
			t.choke(optimistics[i])
		} else {
			t.unchoke(optimistics[i])
			if optimistics[i].stats.info.isInterested {
				optimisticCount++
			}
		}
	}
	t.currChokerRound++
}

func (t *Torrent) addConn(conn *connection) bool {
	if t.numConns >= maxConns {
		return false
	}
	t.conns = append(t.conns, conn)
	return true
}

func (t *Torrent) removeConn(conn *connection) bool {
	index := -1
	for i, cn := range t.conns {
		if cn == conn {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	t.conns = append(t.conns[:index], t.conns[index+1:]...)
	return true
}

/*func (t *Torrent) addConn(conn net.Conn) (int, error) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.numConns >= maxConns {
		return -1, errors.New("maxConns reached")
	}
	t.numConns++
	//Fill one of the gaps of this slice
	var index int
	for i, c := range t.pconns {
		if c.conn == nil {
			c.conn = conn
			index = i
			return index, nil
		}
	}
	panic("maxConns reached")
}

func (t *Torrent) removeConn(i int) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	t.numConns--
	t.pconns[i].conn = nil
}*/
