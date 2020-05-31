package torrent

import (
	"crypto/sha1"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/missinggo/bitmap"
	"github.com/dustin/go-humanize"
	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/torrent/storage"
	"github.com/lkslts64/charo-torrent/tracker"
)

var maxEstablishedConnsDefault = 55

var maxRequestBlockSz = 1 << 14

const metadataPieceSz = 1 << 14

//Torrent represents a torrent and maintains state about it.Multiple goroutines may
//invoke methods on a Torrent simultaneously.
type Torrent struct {
	cl     *Client
	logger *log.Logger
	//channel we receive messages from conns
	recvC       chan msgWithConn
	openStorage storage.Open
	storage     storage.Storage
	//These are active connections
	conns                     []*connInfo
	halfOpenmu                sync.Mutex
	halfOpen                  map[string]Peer
	maxHalfOpenConns          int
	maxEstablishedConnections int
	//we should make effort to obtain new peers if they are below this threshold
	wantPeersThreshold int
	peers              []Peer
	newConnC           chan *connInfo
	pieces             *pieces
	choker             *choker
	//the number of outstanding request messages we support
	//without dropping any. The default in in libtorrent is 250.
	reqq                         int
	blockRequestSize             int
	trackerAnnouncerTimer        *time.Timer
	canAnnounceTracker           bool
	trackerAnnouncerResponseC    chan trackerAnnouncerResponse
	trackerAnnouncerSubmitEventC chan trackerAnnouncerEvent
	lastAnnounceResp             *tracker.AnnounceResp
	numAnnounces                 int
	numTrackerAnnouncesSend      int
	//
	dhtAnnounceResp  *dht.Announce
	dhtAnnounceTimer *time.Timer
	canAnnounceDht   bool
	numDhtAnnounces  int
	//fires when an exported method wants to be invoked
	userC chan chan interface{}
	//these bools are set true when we should actively download/upload the torrent's data.
	//the download of the info is not controled with this variable
	uploadEnabled   bool
	downloadEnabled bool
	//closes when the Torrent closes
	ClosedC  chan struct{}
	isClosed bool
	//closes when all pieces have been downloaded
	DownloadedDataC chan struct{}
	//closes when we get the info dictionary.
	InfoC chan struct{}
	//when this closes it signals all conns to exit
	dropC chan struct{}
	//channel to send requests to piece hasher goroutine
	pieceQueuedHashingC chan int
	//response channel of piece hasher
	pieceHashedC          chan pieceHashed
	queuedForVerification map[int]struct{}
	mi                    *metainfo.MetaInfo
	utmetadata            *utMetadatas
	//length of data to be downloaded
	length         int
	stats          Stats
	connMsgsRecv   int
	msgsSentToConn int
}

func newTorrent(cl *Client) *Torrent {
	t := &Torrent{
		cl:                        cl,
		openStorage:               cl.config.OpenStorage,
		reqq:                      250, //libtorent also has this default
		recvC:                     make(chan msgWithConn, maxEstablishedConnsDefault*sendCSize),
		newConnC:                  make(chan *connInfo, maxEstablishedConnsDefault),
		halfOpen:                  make(map[string]Peer),
		userC:                     make(chan chan interface{}),
		maxEstablishedConnections: cl.config.MaxEstablishedConns,
		maxHalfOpenConns:          55,
		wantPeersThreshold:        100,
		dropC:                     make(chan struct{}),
		DownloadedDataC:           make(chan struct{}),
		InfoC:                     make(chan struct{}),
		ClosedC:                   make(chan struct{}),
		trackerAnnouncerResponseC: make(chan trackerAnnouncerResponse, 1),
		trackerAnnouncerTimer:     newExpiredTimer(),
		dhtAnnounceTimer:          newExpiredTimer(),
		dhtAnnounceResp:           new(dht.Announce),
		queuedForVerification:     make(map[int]struct{}),
		logger:                    log.New(cl.logger.Writer(), "torrent", log.LstdFlags),
		canAnnounceDht:            true,
		canAnnounceTracker:        true,
		utmetadata: &utMetadatas{
			m: make(map[int64]*utMetadata),
		},
	}
	if t.cl.trackerAnnouncer != nil {
		t.trackerAnnouncerSubmitEventC = cl.trackerAnnouncer.trackerAnnouncerSubmitEventCh
	}
	t.choker = newChoker(t)
	return t
}

//close closes all connections with peers that were associated with this Torrent.
func (t *Torrent) close() {
	if t.isClosed {
		panic("attempt to close torrent but is already closed")
	}
	defer func() {
		close(t.ClosedC)
		t.isClosed = true
	}()
	t.dropAllConns()
	t.choker.ticker.Stop()
	t.trackerAnnouncerTimer.Stop()
	t.dhtAnnounceTimer.Stop()
	t.choker = nil
	t.trackerAnnouncerResponseC = nil
	t.recvC = nil
	t.newConnC = nil
	t.pieces = nil
	t.peers = nil
	//t.logger = nil
	//TODO: clear struct fields
}

func (t *Torrent) dropAllConns() {
	t.closeDhtAnnounce()
	//signal conns to close and wait until all conns actually close
	//maybe we don't want to wait?
	close(t.dropC)
	for _, c := range t.conns {
		<-c.droppedC
	}
	t.conns = nil
}

func (t *Torrent) mainLoop() {
	/*defer func() {
		if r := recover(); r != nil {
			t.logger.Fatal(r)
		}
	}()*/
	t.tryAnnounceAll()
	t.choker.startTicker()
	for {
		select {
		case e := <-t.recvC:
			t.onConnMsg(e)
			t.connMsgsRecv++
		case res := <-t.pieceHashedC:
			t.pieceHashed(res.pieceIndex, res.ok)
			if t.pieces.haveAll() {
				t.sendAnnounceToTracker(tracker.Completed)
				t.downloadedAll()
			}
		case ci := <-t.newConnC: //we established a new connection
			t.establishedConnection(ci)
		case <-t.choker.ticker.C:
			t.choker.reviewUnchokedPeers()
		case tresp := <-t.trackerAnnouncerResponseC:
			t.trackerAnnounced(tresp)
		case <-t.trackerAnnouncerTimer.C:
			t.canAnnounceTracker = true
			t.tryAnnounceAll()
		case pvs, ok := <-t.dhtAnnounceResp.Peers:
			if !ok {
				t.dhtAnnounceResp.Peers = nil
			}
			t.dhtAnnounced(pvs)
		case <-t.dhtAnnounceTimer.C:
			t.canAnnounceDht = true
			//close the previous one and try announce again (kind of weird but I think anacrolix does it that way)
			t.closeDhtAnnounce()
			t.tryAnnounceAll()
		//an exported method wants to be invoked
		case userDone := <-t.userC:
			<-userDone
			if t.isClosed {
				return
			}
		}
	}
}

func (t *Torrent) onConnMsg(e msgWithConn) {
	switch v := e.val.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested, peer_wire.NotInterested:
			e.conn.peerInterestChanged()
		case peer_wire.Choke, peer_wire.Unchoke:
			e.conn.peerChokeChanged()
		case peer_wire.Have:
			e.conn.peerBf.Set(int(v.Index), true)
			e.conn.reviewInterestsOnHave(int(v.Index))
		}
	case downloadedBlock:
		t.blockDownloaded(e.conn, block(v))
	case uploadedBlock:
		t.blockUploaded(e.conn, block(v))
	case downloadedMetadata:
		t.metadataCompleted(int64(v))
	case bitmap.Bitmap:
		e.conn.peerBf = v
		e.conn.reviewInterestsOnBitfield()
	case connDroped:
		t.droppedConn(e.conn)
	case discardedRequests:
		t.broadcastToConns(requestsAvailable{})
	}
}

//true if we would like to initiate new connections
func (t *Torrent) wantConns() bool {
	return len(t.conns) < t.maxEstablishedConnections && t.dataTransferAllowed()
}

func (t *Torrent) wantPeers() bool {
	wantPeersThreshold := t.wantPeersThreshold
	if len(t.conns) > 0 {
		//if we have many active conns increase the wantPeersThreshold.
		fullfilledConnSlotsRatio := float64(t.maxEstablishedConnections / len(t.conns))
		if fullfilledConnSlotsRatio < 10/9 {
			wantPeersThreshold = int(float64(t.wantPeersThreshold) + (1/fullfilledConnSlotsRatio)*10)
		}
	}
	return len(t.peers) < wantPeersThreshold && t.dataTransferAllowed()
}

func (t *Torrent) seeding() bool {
	return t.haveAll() && t.uploadEnabled
}

func (t *Torrent) haveAll() bool {
	if !t.haveInfo() {
		return false
	}
	return t.pieces.haveAll()
}

//true if we are allowed to download/upload torrent data or we need info
func (t *Torrent) dataTransferAllowed() bool {
	return !t.haveInfo() || t.uploadEnabled || t.downloadEnabled
}

func (t *Torrent) sendAnnounceToTracker(event tracker.Event) {
	if t.cl.config.DisableTrackers || t.cl.trackerAnnouncer == nil || t.mi.Announce == "" {
		return
	}
	t.trackerAnnouncerSubmitEventC <- trackerAnnouncerEvent{t, event, t.stats}
	t.numTrackerAnnouncesSend++
	t.canAnnounceTracker = false
}

func (t *Torrent) trackerAnnounced(tresp trackerAnnouncerResponse) {
	t.numAnnounces++
	if tresp.err != nil {
		t.logger.Printf("tracker error: %s\n", tresp.err)
		//announce after one minute if tracker send error
		t.resetNextTrackerAnnounce(60)
		return
	}
	t.resetNextTrackerAnnounce(tresp.resp.Interval)
	t.lastAnnounceResp = tresp.resp
	peers := make([]Peer, len(tresp.resp.Peers))
	for i := 0; i < len(peers); i++ {
		peers[i] = Peer{
			P:      tresp.resp.Peers[i],
			Source: SourceTracker,
		}
	}
	t.gotPeers(peers)
}

func (t *Torrent) addFilteredPeers(peers []Peer, f func(peer Peer) bool) {
	for _, peer := range peers {
		if f(peer) {
			t.peers = append(t.peers, peer)
		}
	}
}

func (t *Torrent) resetNextTrackerAnnounce(interval int32) {
	nextAnnounce := time.Duration(interval) * time.Second
	if !t.trackerAnnouncerTimer.Stop() {
		select {
		//rare case - only when we announced with event Complete or Started
		case <-t.trackerAnnouncerTimer.C:
		default:
		}
	}
	t.trackerAnnouncerTimer.Reset(nextAnnounce)
}

func (t *Torrent) announceDht() {
	if t.cl.config.DisableDHT || t.cl.dhtServer == nil {
		return
	}
	ann, err := t.cl.dhtServer.Announce(t.mi.Info.Hash, int(t.cl.port), true)
	if err != nil {
		t.logger.Printf("dht error: %s", err)
	}
	t.dhtAnnounceResp = ann
	if !t.dhtAnnounceTimer.Stop() {
		select {
		//TODO:shouldn't happen
		case <-t.dhtAnnounceTimer.C:
		default:
		}
	}
	t.dhtAnnounceTimer.Reset(5 * time.Minute)
	t.canAnnounceDht = false
	t.numDhtAnnounces++
}

func (t *Torrent) dhtAnnounced(pvs dht.PeersValues) {
	peers := []Peer{}
	for _, peer := range pvs.Peers {
		if peer.Port == 0 {
			continue
		}
		peers = append(peers, Peer{
			P: tracker.Peer{
				IP:   peer.IP,
				Port: uint16(peer.Port),
			},
			Source: SourceDHT,
		})
	}
	t.gotPeers(peers)
}

func (t *Torrent) closeDhtAnnounce() {
	if t.cl.dhtServer == nil || t.dhtAnnounceResp.Peers == nil {
		return
	}
	t.dhtAnnounceResp.Close()
	//invalidate the channel
	t.dhtAnnounceResp.Peers = nil
}

func (t *Torrent) gotPeers(peers []Peer) {
	t.addFilteredPeers(peers, func(peer Peer) bool {
		return !t.cl.containsInBlackList(peer.P.IP)
	})
	t.dialConns()
}

func (t *Torrent) dialConns() {
	if !t.wantConns() {
		return
	}
	defer t.tryAnnounceAll()
	t.halfOpenmu.Lock()
	defer t.halfOpenmu.Unlock()
	for len(t.peers) > 0 && len(t.halfOpen) < t.maxHalfOpenConns {
		peer := t.popPeer()
		if t.peerInActiveConns(peer) || t.cl.containsInBlackList(peer.P.IP) {
			continue
		}
		t.halfOpen[peer.P.String()] = peer
		go t.cl.makeOutgoingConnection(t, peer)
	}
}

//has peer the same addr with any active connection
func (t *Torrent) peerInActiveConns(peer Peer) bool {
	for _, ci := range t.conns {
		if ci.peer.P.IP.Equal(peer.P.IP) && ci.peer.P.Port == peer.P.Port {
			return true
		}
	}
	return false
}

func (t *Torrent) popPeer() (p Peer) {
	i := rand.Intn(len(t.peers))
	p = t.peers[i]
	t.peers = append(t.peers[:i], t.peers[i+1:]...)
	return
}

func (t *Torrent) swarm() (peers []Peer) {
	for _, c := range t.conns {
		peers = append(peers, c.peer)
	}
	func() {
		t.halfOpenmu.Lock()
		defer t.halfOpenmu.Unlock()
		for _, p := range t.halfOpen {
			peers = append(peers, p)
		}
	}()
	for _, p := range t.peers {
		peers = append(peers, p)
	}
	return
}

func (t *Torrent) writeStatus(b *strings.Builder) {
	if t.haveInfo() {
		b.WriteString(fmt.Sprintf("Name: %s\n", t.mi.Info.Name))
	}
	b.WriteString(fmt.Sprintf("#DhtAnnounces: %d\n", t.numDhtAnnounces))
	b.WriteString("Tracker: " + t.mi.Announce + "\tAnnounce: " + func() string {
		if t.lastAnnounceResp != nil {
			return "OK"
		}
		return "Not Available"
	}() + "\t#AnnouncesSend: " + strconv.Itoa(t.numTrackerAnnouncesSend) + "\n")
	if t.lastAnnounceResp != nil {
		b.WriteString(fmt.Sprintf("Seeders: %d\tLeechers: %d\tInterval: %d(secs)\n", t.lastAnnounceResp.Seeders, t.lastAnnounceResp.Seeders, t.lastAnnounceResp.Interval))
	}
	b.WriteString(fmt.Sprintf("State: %s\n", t.state()))
	b.WriteString(fmt.Sprintf("Downloaded: %s\tUploaded: %s\tRemaining: %s\n", humanize.Bytes(uint64(t.stats.BytesDownloaded)),
		humanize.Bytes(uint64(t.stats.BytesUploaded)), humanize.Bytes(uint64(t.stats.BytesLeft))))
	b.WriteString(fmt.Sprintf("Connected to %d peers\n", len(t.conns)))
	tabWriter := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tabWriter, "Address\t%\tUp\tDown\t")
	for _, ci := range t.conns {
		fmt.Fprintf(tabWriter, "%s\t%s\t%s\t%s\t\n", ci.peer.P.IP.String(),
			strconv.Itoa(int(float64(ci.peerBf.Len())/float64(t.numPieces())*100))+"%",
			humanize.Bytes(uint64(ci.stats.uploadUsefulBytes)),
			humanize.Bytes(uint64(ci.stats.downloadUsefulBytes)))
	}
	tabWriter.Flush()
}

func (t *Torrent) state() string {
	if t.isClosed {
		return "closed"
	}
	if t.seeding() {
		return "seeding"
	}
	if !t.haveInfo() {
		return "downloading info"
	}
	switch {
	case t.uploadEnabled && t.downloadEnabled:
		return "uploading/downloading"
	case t.uploadEnabled:
		return "uploading only"
	case t.downloadEnabled:
		return "downloading only"
	}
	return "waiting for downloading request"
}

func (t *Torrent) blockDownloaded(c *connInfo, b block) {
	c.stats.onBlockDownload(b.len)
	t.stats.blockDownloaded(b.len)
	t.pieces.setBlockComplete(b.pc, b.off, c)
}

func (t *Torrent) blockUploaded(c *connInfo, b block) {
	c.stats.onBlockUpload(b.len)
	t.stats.blockUploaded(b.len)
}

func (t *Torrent) downloadedAll() {
	close(t.DownloadedDataC)
	for _, c := range t.conns {
		c.notInterested()
	}
}

func (t *Torrent) queuePieceForHashing(i int) {
	if _, ok := t.queuedForVerification[i]; ok || t.pieces.pcs[i].verified {
		//piece is already queued or verified
		return
	}
	t.queuedForVerification[i] = struct{}{}
	select {
	case t.pieceQueuedHashingC <- i:
	default:
		panic("queue piece hash: should not block")
	}
}

func (t *Torrent) pieceHashed(i int, correct bool) {
	delete(t.queuedForVerification, i)
	t.pieces.pieceHashed(i, correct)
	if correct {
		t.onPieceDownload(i)
	} else {
		t.banPeer()
	}
}

func (t *Torrent) metadataCompleted(infoSize int64) {
	ut := t.utmetadata.metadata(infoSize)
	if sha1.Sum(ut.infoBytes) != t.mi.Info.Hash {
		t.logger.Println("metadata didn't pass the verification")
		//ban a random IP amongst the contributors
		t.banIP(ut.contributors[rand.Intn(len(ut.contributors))])
		t.broadcastToConns(requestsAvailable{})
		t.utmetadata.reset(int(infoSize))
	} else if err := bencode.Decode(ut.infoBytes, t.mi.Info); err != nil {
		//TODO: close gracefully (dont leak goroutines)
		t.close()
	} else {
		t.utmetadata.setCorrect(infoSize)
		t.gotInfo()
	}
}

//this func is started in its own goroutine.
//when we close eventCh of conn, the goroutine
//exits
func (t *Torrent) aggregateEvents(ci *connInfo) {
	for e := range ci.recvC {
		t.recvC <- msgWithConn{ci, e}
	}
}

//careful when using this, we might send over nil chan
func (t *Torrent) broadcastToConns(cmd interface{}) {
	for _, ci := range t.conns {
		ci.sendMsgToConn(cmd)
	}
}

func (t *Torrent) sendCancels(b block) {
	t.broadcastToConns(b.cancelMsg())
}

//TODO: make iter around t.conns and dont iterate twice over conns
func (t *Torrent) onPieceDownload(i int) {
	t.stats.onPieceDownload(t.pieceLen(uint32(i)))
	t.reviewInterestsOnPieceDownload(i)
	t.sendHaves(i)
}

func (t *Torrent) reviewInterestsOnPieceDownload(i int) {
	if t.haveAll() {
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

func (t *Torrent) sendHaves(i int) {
	for _, c := range t.conns {
		c.have(i)
	}
}

func (t *Torrent) establishedConnection(ci *connInfo) bool {
	if !t.wantConns() || t.cl.containsInBlackList(ci.peer.P.IP) {
		ci.sendMsgToConn(drop{})
		t.closeDhtAnnounce()
		t.logger.Printf("rejected a connection with peer %v\n", ci.peer.P)
		return false
	}
	defer t.choker.reviewUnchokedPeers()
	t.conns = append(t.conns, ci)
	//notify conn that we have metainfo
	if t.haveInfo() {
		ci.sendMsgToConn(haveInfo{})
	}
	if t.pieces != nil && t.pieces.ownedPieces.Len() > 0 {
		ci.sendBitfield()
	}
	if ci.reserved.SupportDHT() && t.cl.reserved.SupportDHT() && t.cl.dhtServer != nil {
		ci.sendPort()
	}
	go t.aggregateEvents(ci)
	return true
}

func (t *Torrent) banPeer() {
	max := math.MinInt32
	var toBan *connInfo
	for _, c := range t.conns {
		if m := c.stats.malliciousness(); m > max {
			max = m
			toBan = c
		}
	}
	if toBan == nil {
		return
	}
	t.banIP(toBan.peer.P.IP)
}

func (t *Torrent) banIP(ip net.IP) {
	//myAddr := t.cl.listener.Addr().(*net.TCPAddr)
	myAddr := getOutboundIP()
	if ip.Equal(myAddr) || ip.Equal(net.ParseIP("127.0.0.1")) {
		return
	}
	t.cl.banIP(ip)
	for _, c := range t.conns {
		if c.peer.P.IP.Equal(ip) {
			c.sendMsgToConn(drop{})
		}
	}
}

//conn notified us that it was dropped
//returns false if we have already dropped it.
func (t *Torrent) droppedConn(ci *connInfo) bool {
	var (
		i  int
		ok bool
	)
	if i, ok = t.connIndex(ci); !ok {
		return false
	}
	defer t.choker.reviewUnchokedPeers()
	defer t.dialConns()
	t.removeConn(ci, i)
	//If there is a large time gap between the time we download the info and before the user
	//requests to download the data we may lose some connections (seeders will close because
	//we won't request any pieces). So, we may have to store the peers that droped us during
	//that period in order to reconnect.
	if t.infoWasDownloaded() && !t.dataTransferAllowed() {
		t.peers = append(t.peers, ci.peer)
	}
	return true
}

//bool is true if was found
func (t *Torrent) connIndex(ci *connInfo) (int, bool) {
	for i, cn := range t.conns {
		if cn == ci {
			return i, true
		}
	}
	return -1, false
}

func (t *Torrent) connsIter(f func(ci *connInfo) bool) {
	for _, ci := range t.conns {
		if !f(ci) {
			break
		}
	}
}

func (t *Torrent) tryAnnounceAll() {
	if !t.wantPeers() {
		return
	}
	if t.canAnnounceTracker {
		t.sendAnnounceToTracker(tracker.None)
	}
	if t.canAnnounceDht {
		t.announceDht()
	}
}

//clear the fields that included the conn
func (t *Torrent) removeConn(ci *connInfo, index int) {
	t.conns = append(t.conns[:index], t.conns[index+1:]...)
}

func (t *Torrent) removeHalfOpen(addr string) {
	t.halfOpenmu.Lock()
	delete(t.halfOpen, addr)
	t.halfOpenmu.Unlock()
	t.dialConns()
}

//TODO: maybe wrap these at the same func passing as arg the func (read/write)
func (t *Torrent) writeBlock(data []byte, piece, begin int) error {
	off := int64(piece*t.mi.Info.PieceLen + begin)
	_, err := t.storage.WriteBlock(data, off)
	return err
}

func (t *Torrent) readBlock(data []byte, piece, begin int) error {
	off := int64(piece*t.mi.Info.PieceLen + begin)
	n, err := t.storage.ReadBlock(data, off)
	if n != len(data) {
		t.logger.Printf("coudn't read whole block from storage, read only %d bytes\n", n)
	}
	if err != nil {
		t.logger.Printf("storage read err %s\n", err)
	}
	return err
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
	return t.mi.Info.NumPieces()
}

func (t *Torrent) gotInfoHash() {
	logPrefix := t.cl.logger.Prefix() + fmt.Sprintf("TR%x", t.mi.Info.Hash[14:])
	t.logger = log.New(t.cl.logger.Writer(), logPrefix, log.LstdFlags)
}

func (t *Torrent) gotInfo() {
	defer close(t.InfoC)
	if t.mi.InfoBytes != nil {
		//we had the info from start up. Create a utmetadata struct in order
		//to upload if asked from a peer
		if len(t.utmetadata.m) > 0 {
			panic("got info")
		}
		infoBytes := t.mi.InfoBytes
		ut := newUtMetadata(len(infoBytes))
		// At this point, no connection has being made because t hasn't exposed its
		// existance to Client (so no incoming connections) and we haven't actively try
		// to connect to peers (outgoing) so it is safe to acess these without lock
		t.utmetadata.correctSize = int64(len(infoBytes))
		t.utmetadata.m[int64(len(infoBytes))] = ut
		ut.infoBytes = t.mi.InfoBytes
	} else {
		//we downloaded info
		t.logger.Println("info sucessfully downloaded")
	}
	t.length = t.mi.Info.TotalLength()
	t.stats.BytesLeft = t.length
	t.blockRequestSize = t.blockSize()
	t.pieces = newPieces(t)
	t.pieceQueuedHashingC = make(chan int, t.numPieces())
	t.pieceHashedC = make(chan pieceHashed, t.numPieces())
	t.broadcastToConns(haveInfo{})
	var haveAll bool
	t.storage, haveAll = t.openStorage(t.mi, t.cl.config.BaseDir, t.pieces.blocks(), t.logger)
	if haveAll {
		//mark all bocks completed and do all apropriate things when a piece
		//hashing is succesfull
		for i, p := range t.pieces.pcs {
			p.unrequestedBlocks, p.completeBlocks = p.completeBlocks, p.unrequestedBlocks
			t.pieceHashed(i, true)
		}
		t.downloadedAll()
	} else {
		ph := pieceHasher{t: t}
		go ph.Run()
	}
}

func (t *Torrent) pieceLen(i uint32) (pieceLen int) {
	numPieces := int(t.mi.Info.NumPieces())
	//last piece case
	if int(i) == numPieces-1 {
		pieceLen = t.length % int(t.mi.Info.PieceLen)
	} else {
		pieceLen = t.mi.Info.PieceLen
	}
	return
}

//call this when we get info
func (t *Torrent) blockSize() int {
	if maxRequestBlockSz > t.mi.Info.PieceLen {
		return t.mi.Info.PieceLen
	}
	return maxRequestBlockSz
}

/*
func (t *Torrent) scrapeTracker() (*tracker.ScrapeResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return t.trackerURL.Scrape(ctx, t.mi.Info.Hash)
}
*/

func (t *Torrent) haveInfo() bool {
	//return t.mi.Info != nil
	//TODO: this is not good (but it is safe since only torrent can write to this variable)
	return t.utmetadata.correctSize != 0
}

//true if we hadn't the info on start up and downloaded/downloadiing it via metadata extension.
func (t *Torrent) infoWasDownloaded() bool {
	return t.mi.InfoBytes == nil
}

func (t *Torrent) newLocker() *torrentLocker {
	return &torrentLocker{
		t: t,
	}
}
