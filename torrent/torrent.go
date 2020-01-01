package torrent

import (
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/eapache/channels"
	"github.com/lkslts64/charo-torrent/peer_wire"
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

var blockSz = 1 << 14           //16KiB
var maxRequestBlockSz = 1 << 15 //32KiB
//spec doesn't say anything about this by we enfore it
var minRequestBlockSz = 1 << 13 //8KiB

//Torrent
type Torrent struct {
	tm *TorrentMeta
	//pconns   []*PeerConn
	//pstats   []peerStats
	//size of len(pconns) * eventChSize is reasonable
	events chan event

	//alternative:
	//---------------
	//active peer conns
	//we take msg from peerconn to add it at this map
	//if numConns < maxConns then we add it,else conn
	//should get dropped
	conns []*connInfo
	//conn can send a mgs to this chan so master goroutine knows a new connection
	//was established/closed
	//this chan has size = maxConns / 2 ?
	newConnCh chan *connChansInfo
	numConns  int
	//conns map[*connection]struct{}

	//bitFields []peer_wire.BitField

	//eventCh chan event
	pieces pieces

	chk *choker

	//are we seeding
	seeding bool

	//surely we 'll need this
	done chan struct{}
}

func (t *Torrent) mainLoop() {
	t.chk.startTicker()
	defer t.chk.ticker.Stop()
	for {
		select {
		case _ = <-t.events:

		case cc := <-t.newConnCh: //we established a new connection
			if !t.addConn(cc) {
				//log that we rejected a conn
			}
		case <-t.chk.ticker.C:
			t.chk.reviewUnchokedPeers()
		}
	}

}

func (t *Torrent) parseEvent(e event) {
	switch v := e.val.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested:
			e.conn.state.isInterested = true
			if !e.conn.state.amChoking {
				t.chk.reviewUnchokedPeers()
			}
		case peer_wire.NotInterested:
			e.conn.state.isInterested = false
			if !e.conn.state.amChoking {
				t.chk.reviewUnchokedPeers()
			}
		case peer_wire.Choke:
			e.conn.state.isChoking = true
			if e.conn.state.amInterested {
				e.conn.stats.stopDownloading()
			}
			//forward unsatisfied to other interested-unchoked peers
		case peer_wire.Unchoke:
			//update local state
			e.conn.state.isChoking = false
			e.conn.startDownloading()
			//give this peer some requests
		case peer_wire.Have:
			e.conn.reviewInterestsOnHave(int(v.Index))
			e.conn.peerBf.Set(int(v.Index), true)
		}
	case downloadedBlock:
		t.pieces.markBlockComplete(int(v.pc), int(v.off))
		//update interests
	case uploadedBlock:
		//add to stats
	case metainfoSize:
	case bitmap.Bitmap:
		e.conn.peerBf = v
		e.conn.reviewInterestsOnBitfield()
	//before conn goroutine exits, it sends a dropConn event
	case connDroped:
		t.removeConn(e.conn)
		t.chk.reviewUnchokedPeers()

	}
}

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
func (t *Torrent) addAggregated(i int, conn *connInfo) {
	for e := range conn.eventCh {
		t.events <- event{conn, e}
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
	for _, conn := range t.conns {
		conn.jobCh.In() <- job
	}
}

func (t *Torrent) reviewInterestsOnPieceDownload(i int) {
	if t.seeding {
		return
	}
	for _, c := range t.conns {
		if c.peerBf.Get(i) {
			c.numWant--
			if c.numWant <= 0 {
				c.notInterested()
			}
		}
	}
}

func (t *Torrent) addConn(cc *connChansInfo) bool {
	if t.numConns >= maxConns {
		return false
	}
	ci := &connInfo{
		t:           t,
		jobCh:       cc.jobCh,
		eventCh:     cc.eventCh,
		amSeedingCh: cc.amSeedingCh,
		haveInfoCh:  cc.haveInfoCh,
		state:       newConnState(),
		stats:       newConnStats(),
	}
	t.conns = append(t.conns, ci)
	//notify conn that we have metainfo or we are seeding by closing these chans
	if t.tm.haveInfo() {
		close(ci.amSeedingCh)
	}
	if t.seeding {
		close(ci.haveInfoCh)
	}
	if t.pieces.ownedPieces.Len() > 0 {
		ci.jobCh.In() <- t.pieces.ownedPieces.Copy()
	}
	return true
}

func (t *Torrent) removeConn(ci *connInfo) bool {
	index := -1
	for i, cn := range t.conns {
		if cn == ci {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	t.conns[index] = nil //ensure is garbage collected (maybe not required)
	t.conns = append(t.conns[:index], t.conns[index+1:]...)
	return true
}

func (t *Torrent) uploaders() (uploaders []*connInfo) {
	for _, c := range t.conns {
		if c.state.canDownload() {
			uploaders = append(uploaders, c)
		}
	}
	return
}

func (t *Torrent) numPieces() int {
	return t.tm.mi.Info.NumPieces()
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

//connInfo sends msgs to conn using two chans.It also holds
//some informations like state,bitmap which also conn holds too -
//we dont share, we communicate so we have some duplicate data-.
type connInfo struct {
	t *Torrent
	//we communicate with conn with these channels - conn also has them
	jobCh       channels.Channel
	eventCh     chan interface{}
	amSeedingCh chan struct{}
	haveInfoCh  chan struct{}
	//peer's bitmap
	peerBf  bitmap.Bitmap //also conn has this
	numWant int           //how many pieces are we interested to download from peer
	state   *connState    //also conn has this
	stats   *connStats
}

func (cn *connInfo) sendJob(job interface{}) {
	cn.jobCh.In() <- job
}

func (cn *connInfo) choke() {
	if !cn.state.amChoking {
		cn.sendJob(&peer_wire.Msg{
			Kind: peer_wire.Choke,
		})
		cn.state.amChoking = !cn.state.amChoking
	}
}

func (cn *connInfo) unchoke() {
	if cn.state.amChoking {
		cn.sendJob(&peer_wire.Msg{
			Kind: peer_wire.Unchoke,
		})
		cn.state.amChoking = !cn.state.amChoking
	}
}

func (cn *connInfo) interested() {
	if !cn.state.amInterested {
		cn.sendJob(&peer_wire.Msg{
			Kind: peer_wire.Interested,
		})
		cn.state.amInterested = !cn.state.amInterested
		cn.startDownloading()
	}
}

func (cn *connInfo) notInterested() {
	if cn.state.amInterested {
		cn.sendJob(&peer_wire.Msg{
			Kind: peer_wire.NotInterested,
		})
		cn.state.amInterested = !cn.state.amInterested
		if !cn.state.isChoking {
			cn.stats.stopDownloading()
		}
	}
}

func (cn *connInfo) have(i int) {
	if !cn.t.pieces.ownedPieces.Get(i) {
		panic("send have without having piece")
	}
	cn.sendJob(&peer_wire.Msg{
		Kind:  peer_wire.Have,
		Index: uint32(i),
	})
}

func (cn *connInfo) sendBitfield() {
	cn.sendJob(cn.t.pieces.ownedPieces)
}

//manages if we are interested in peer after a sending us bitfield msg
func (cn *connInfo) reviewInterestsOnBitfield() {
	if !cn.t.tm.haveInfo() || cn.t.seeding {
		return
	}
	for i := 0; i < cn.t.numPieces(); i++ {
		if !cn.t.pieces.ownedPieces.Get(i) && cn.peerBf.Get(i) {
			cn.numWant++
		}
	}
	if cn.numWant > 0 {
		cn.interested()
	}
}

//manages if we are interested in peer after sending us a have msg
func (cn *connInfo) reviewInterestsOnHave(i int) {
	if !cn.t.tm.haveInfo() || cn.t.seeding {
		return
	}
	if !cn.t.pieces.ownedPieces.Get(i) {
		if cn.numWant <= 0 {
			cn.interested()
		}
		cn.numWant++
	}
}

func (cn *connInfo) durationDownloading() time.Duration {
	if cn.state.canDownload() {
		return cn.stats.sumDownloading + time.Since(cn.stats.lastStartedDownloading)
	}
	return cn.stats.sumDownloading
}

func (cn *connInfo) durationUploading() time.Duration {
	if cn.state.canUpload() {
		return cn.stats.sumUploading + time.Since(cn.stats.lastStartedUploading)
	}
	return cn.stats.sumUploading
}

func (cn *connInfo) startDownloading() {
	if cn.state.canDownload() {
		cn.stats.lastStartedDownloading = time.Now()
	}
}

func (cn *connInfo) startUploading() {
	if cn.state.canUpload() {
		cn.stats.lastStartedUploading = time.Now()
	}
}

func (cn *connInfo) isSnubbed() bool {
	if cn.t.seeding {
		return false
	}
	return cn.stats.isSnubbed()
}

func (cn *connInfo) peerSeeding() bool {
	if !cn.t.tm.haveInfo() { //we don't know if it has all (maybe he has)
		return false
	}
	return cn.peerBf.Len() == cn.t.numPieces()
}
