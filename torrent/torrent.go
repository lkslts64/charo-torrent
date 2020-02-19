package torrent

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log"
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

var maxRequestBlockSz = 1 << 14

const metadataPieceSz = 1 << 14

//If we only have less than `wantPeersThreshold` connected peers
//we actively form new connections
const wantPeersThreshold = 30

//Torrent
type Torrent struct {
	cl          *Client
	logger      *log.Logger
	events      chan event
	openStorage storage.Open
	storage     storage.Storage
	//These are active connections
	conns []*connInfo
	//These are conns that we are not connected to anymore
	droppedConns []*connInfo
	newConnCh    chan *connInfo
	pieces       *pieces
	choker       *choker
	//are we seeding
	seeding bool
	//the number of outstanding request messages we support
	//without dropping any. The default in in libtorrent is 250.
	reqq                          int
	blockRequestSize              int
	trackerAnnouncerTimer         *time.Timer
	canAnnounceTracker            bool
	trackerAnnouncerResponseCh    chan trackerAnnouncerResponse
	trackerAnnouncerSubmitEventCh chan trackerAnnouncerEvent
	lastAnnounceResp              *tracker.AnnounceResp
	numAnnounces                  int
	numTrackerAnnouncesSend       int
	//
	dhtAnnounceResp    *dht.Announce
	dhtAnnounceRequest chan struct{} //with size of 1,select when want to send
	//if blocks,means we have other announce currently happening so abort.
	dhtAnnounceTimer *time.Timer
	canAnnounceDht   bool
	numDhtAnnounces  int

	//surely we 'll need this
	done chan struct{}
	//client sends to this chan when client.Close is called
	close chan chan struct{}
	//when close,signals all conns to be dropped
	drop          chan struct{}
	downloadedAll chan struct{}
	//for displaying the state of torrent and conns
	displayCh             chan chan []byte
	discarded             chan struct{}
	pieceQueuedHashingCh  chan int
	pieceHashedCh         chan pieceHashed
	queuedForVerification map[int]struct{}
	//Info field of `mi` is nil if we dont have it.
	//Restrict access to metainfo before we get the
	//whole mi.Info part.
	mi       *metainfo.MetaInfo
	infoSize int64
	//we serve metadata only if we have it all.
	//lock only when writing
	infoMu sync.Mutex
	//used only when we dont have infoDict part of metaInfo
	infoBytes       []byte
	ownedInfoBlocks []bool
	//frequency map of infoSizes we have received
	infoSizeFreq freqMap
	//length of data to be downloaded
	length int

	stats          Stats
	eventsReceived int
	commandsSent   int
}

func newTorrent(cl *Client) *Torrent {
	t := &Torrent{
		cl:                         cl,
		openStorage:                cl.config.OpenStorage,
		reqq:                       250, //libtorent also has this default
		done:                       make(chan struct{}),
		events:                     make(chan event, maxConns*eventChSize),
		newConnCh:                  make(chan *connInfo, maxConns),
		downloadedAll:              make(chan struct{}),
		drop:                       make(chan struct{}),
		displayCh:                  make(chan chan []byte, 10),
		close:                      make(chan chan struct{}),
		trackerAnnouncerResponseCh: make(chan trackerAnnouncerResponse, 1),
		trackerAnnouncerTimer:      newExpiredTimer(),
		dhtAnnounceTimer:           newExpiredTimer(),
		dhtAnnounceResp:            new(dht.Announce),
		queuedForVerification:      make(map[int]struct{}),
		infoSizeFreq:               newFreqMap(),
		stats:                      Stats{},
		logger:                     log.New(cl.logger.Writer(), "torrent", log.LstdFlags),
		canAnnounceDht:             true,
		canAnnounceTracker:         true,
	}
	if t.cl.trackerAnnouncer != nil {
		t.trackerAnnouncerSubmitEventCh = cl.trackerAnnouncer.trackerAnnouncerSubmitEventCh
	}
	t.choker = newChoker(t)
	return t
}

func (t *Torrent) Download() <-chan struct{} {
	go t.mainLoop()
	return t.downloadedAll
}

//jobs that we 'll send are proportional to the # of events we have received.
//TODO:count max jobs we can send in an iteration (estimate < 10)
func (t *Torrent) mainLoop() {
	t.sendAnnounceToTracker(tracker.Started)
	t.announceDht()
	t.choker.startTicker()
	defer func() {
		t.choker.ticker.Stop()
		t.trackerAnnouncerTimer.Stop()
		t.dhtAnnounceTimer.Stop()
	}()
	for {
		select {
		case e := <-t.events:
			t.parseEvent(e)
			t.eventsReceived++
		case res := <-t.pieceHashedCh:
			t.pieceHashed(res.pieceIndex, res.ok)
			if res.ok && t.pieces.haveAll() {
				t.sendAnnounceToTracker(tracker.Completed)
				t.startSeeding()
			}
		case ci := <-t.newConnCh: //we established a new connection
			t.establishedConnection(ci)
		case <-t.choker.ticker.C:
			t.choker.reviewUnchokedPeers()
		case tresp := <-t.trackerAnnouncerResponseCh:
			t.trackerAnnounced(tresp)
		case <-t.trackerAnnouncerTimer.C:
			t.canAnnounceTracker = true
			if t.wantPeers() {
				t.sendAnnounceToTracker(tracker.None)
			}
		case pvs := <-t.dhtAnnounceResp.Peers:
			if len(pvs.Peers) > 0 {
				t.dhtAnnounced(pvs)
			}
		case <-t.dhtAnnounceTimer.C:
			//new announce is permited only after the timer expires
			t.canAnnounceDht = true
			t.closeDhtAnnounce()
		case statusBytesCh := <-t.displayCh:
			b := new(strings.Builder)
			t.writeStatus(b)
			statusBytesCh <- []byte(b.String())
		case doneClosing := <-t.close:
			t.Close()
			close(doneClosing)
			return
		}
	}
}

//Close closes all connections with peers that were associated with this Torrent.
func (t *Torrent) Close() {
	close(t.drop) //signal conns to close
	t.closeDhtAnnounce()
	//wait until all conns close and then close `doneClosing`.
	for _, c := range t.conns {
		<-c.dropped
	}
}

func (t *Torrent) parseEvent(e event) {
	switch v := e.val.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested:
			e.conn.state.isInterested = true
			if !e.conn.state.amChoking {
				t.choker.reviewUnchokedPeers()
			}
			e.conn.startUploading()
		case peer_wire.NotInterested:
			e.conn.state.isInterested = false
			if !e.conn.state.amChoking {
				t.choker.reviewUnchokedPeers()
			}
			if !e.conn.state.amChoking {
				e.conn.stopUploading()
			}
		case peer_wire.Choke:
			e.conn.state.isChoking = true
			if e.conn.state.amInterested {
				e.conn.stopDownloading()
			}
		case peer_wire.Unchoke:
			e.conn.state.isChoking = false
			e.conn.startDownloading()
		case peer_wire.Have:
			e.conn.peerBf.Set(int(v.Index), true)
			e.conn.reviewInterestsOnHave(int(v.Index))
		}
	case downloadedBlock:
		t.blockDownloaded(e.conn, block(v))
	case uploadedBlock:
		t.blockUploaded(e.conn, block(v))
	case pieceHashed:
		e.conn.stats.onPieceHashed()
		t.pieceHashed(v.pieceIndex, v.ok)
		if t.pieces.haveAll() {
			t.startSeeding()
		}
	case metainfoSize:
	case bitmap.Bitmap:
		e.conn.peerBf = v
		e.conn.reviewInterestsOnBitfield()
	case connDroped:
		t.droppedConn(e.conn)
	case discardedRequests:
		t.broadcastCommand(requestsAvailable{})
	}
}

func (t *Torrent) wantPeers() bool {
	return len(t.conns) < wantPeersThreshold
}

func (t *Torrent) sendAnnounceToTracker(event tracker.Event) {
	if !t.cl.config.DisableTrackers && t.cl.trackerAnnouncer != nil && t.mi.Announce != "" {
		t.trackerAnnouncerSubmitEventCh <- trackerAnnouncerEvent{t, event, t.stats}
		t.numTrackerAnnouncesSend++
		t.canAnnounceTracker = false
	}
}

func (t *Torrent) trackerAnnounced(tresp trackerAnnouncerResponse) {
	t.numAnnounces++
	if tresp.err != nil {
		t.logger.Printf("tracker error: %s\n", tresp.err)
		//announce after one minute if tracker send error
		t.resetNextAnnounce(60)
		return
	}
	t.resetNextAnnounce(tresp.resp.Interval)
	t.lastAnnounceResp = tresp.resp
	t.gotPeers(tresp.resp.Peers)
}

func (t *Torrent) filterNotConnected(peers []tracker.Peer) []string {
	addrsNotConnected := []string{}
nextPeer:
	for _, peer := range peers {
		for _, ci := range t.conns {
			if ci.addr.String() == peer.String() {
				continue nextPeer
			}
		}
		addrsNotConnected = append(addrsNotConnected, peer.String())
	}
	return addrsNotConnected
}

func (t *Torrent) resetNextAnnounce(interval int32) {
	nextAnnounce := time.Duration(interval) * time.Second
	if t.trackerAnnouncerTimer == nil {
		t.trackerAnnouncerTimer = time.NewTimer(nextAnnounce)
	} else {
		if t.trackerAnnouncerTimer.Stop() {
			panic("announce before tracker specified interval")
		}
		t.trackerAnnouncerTimer.Reset(nextAnnounce)
	}
}

func (t *Torrent) announceDht() {
	if !t.cl.config.DisableDHT && t.cl.dhtServer != nil {
		ann, err := t.cl.dhtServer.Announce(t.mi.Info.Hash, int(t.cl.port), true)
		if err != nil {
			t.logger.Printf("dht error: %s", err)
		}
		t.dhtAnnounceResp = ann
		if t.dhtAnnounceTimer.Stop() {
			panic("dht timer hasn't expired")
		}
		t.dhtAnnounceTimer.Reset(5 * time.Minute)
		t.canAnnounceDht = false
		t.numDhtAnnounces++
		t.logger.Println("dht announce")
	}
}

func (t *Torrent) dhtAnnounced(pvs dht.PeersValues) {
	peers := []tracker.Peer{}
	for _, peer := range pvs.Peers {
		if peer.Port == 0 {
			continue
		}
		peers = append(peers, tracker.Peer{
			IP:   peer.IP,
			Port: uint16(peer.Port),
		})
	}
	t.gotPeers(peers)
}

func (t *Torrent) closeDhtAnnounce() {
	if t.dhtAnnounceResp.Peers == nil {
		return
	}
	t.dhtAnnounceResp.Close()
	//invalidate the channel
	t.dhtAnnounceResp.Peers = nil
}

func (t *Torrent) gotPeers(peers []tracker.Peer) {
	addrsNotConnected := t.filterNotConnected(peers)
	t.cl.connectToPeers(t, addrsNotConnected...)
}

//TODO: change this to WriteStats(w *io.Writer)
func (t *Torrent) WriteStatus(w io.Writer) {
	//send a signal to mainLoop to fill a status buffer and send it back to us
	ch := make(chan []byte)
	t.displayCh <- ch
	b := <-ch
	w.Write(b)
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
	b.WriteString(fmt.Sprintf("Mode: %s\n", func() string {
		if t.seeding {
			return "seeding"
		}
		return "downloading"
	}()))
	b.WriteString(fmt.Sprintf("Downloaded: %s\tUploaded: %s\tRemaining: %s\n", humanize.Bytes(uint64(t.stats.BytesDownloaded)),
		humanize.Bytes(uint64(t.stats.BytesUploaded)), humanize.Bytes(uint64(t.stats.BytesLeft))))
	b.WriteString(fmt.Sprintf("Connected to %d peers\n", len(t.conns)))
	tabWriter := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tabWriter, "Address\t%\tUp\tDown\t")
	for _, ci := range t.conns {
		fmt.Fprintf(tabWriter, "%s\t%s\t%s\t%s\t\n", ci.addr,
			strconv.Itoa(int(float64(ci.peerBf.Len())/float64(t.numPieces())*100))+"%",
			humanize.Bytes(uint64(ci.stats.uploadUsefulBytes)),
			humanize.Bytes(uint64(ci.stats.downloadUsefulBytes)))
	}
	tabWriter.Flush()
}

func (t *Torrent) blockDownloaded(c *connInfo, b block) {
	c.stats.onBlockDownload(b.len)
	t.stats.blockDownloaded(b.len)
	t.pieces.makeBlockComplete(b.pc, b.off, c)
}

func (t *Torrent) blockUploaded(c *connInfo, b block) {
	c.stats.onBlockUpload(b.len)
	t.stats.blockUploaded(b.len)
}

func (t *Torrent) startSeeding() {
	close(t.downloadedAll)
	t.seeding = true
	t.broadcastCommand(seeding{})
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
	case t.pieceQueuedHashingCh <- i:
	default:
		panic("queue piece hash: should not block")
	}
}

func (t *Torrent) pieceHashed(i int, correct bool) {
	delete(t.queuedForVerification, i)
	if correct {
		t.pieces.pieceVerified(i)
		t.onPieceDownload(i)
	} else {
		t.pieces.pieceVerificationFailed(i)
	}
}

//this func is started in its own goroutine.
//when we close eventCh of conn, the goroutine
//exits
func (t *Torrent) aggregateEvents(ci *connInfo) {
	for e := range ci.eventCh {
		t.events <- event{ci, e}
	}
	t.events <- event{ci, connDroped{}}
}

//careful when using this, we might send over nil chan
func (t *Torrent) broadcastCommand(cmd interface{}) {
	for _, ci := range t.conns {
		ci.sendCommand(cmd)
	}
}

func (t *Torrent) sendCancels(b block) {
	t.broadcastCommand(b.cancelMsg())
}

//TODO: make iter around t.conns and dont iterate twice over conns
func (t *Torrent) onPieceDownload(i int) {
	t.stats.onPieceDownload(t.pieceLen(uint32(i)))
	t.reviewInterestsOnPieceDownload(i)
	t.sendHaves(i)
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

func (t *Torrent) sendHaves(i int) {
	for _, c := range t.conns {
		c.have(i)
	}
}

func (t *Torrent) establishedConnection(ci *connInfo) bool {
	if len(t.conns) >= maxConns {
		ci.commandCh <- drop{}
		t.closeDhtAnnounce()
		t.logger.Printf("rejected a connection with peer %s\n", ci.addr.String())
		return false
	}
	defer t.choker.reviewUnchokedPeers()
	t.conns = append(t.conns, ci)
	//notify conn that we have metainfo or we are seeding
	if t.haveInfo() {
		ci.sendCommand(haveInfo{})
	}
	if t.seeding {
		ci.sendCommand(seeding{})
	}
	//
	//TODO:send here extension msg
	//
	//if we have some pieces, we should sent a bitfield
	if t.pieces.ownedPieces.Len() > 0 {
		ci.sendBitfield()
	}
	if ci.reserved.SupportDHT() && t.cl.reserved.SupportDHT() && t.cl.dhtServer != nil {
		ci.sendPort()
	}
	go t.aggregateEvents(ci)
	return true
}

//we would like to drop the conn
func (t *Torrent) punishPeer(i int) {
	t.conns[i].sendCommand(drop{})
	t.removeConn(t.conns[i], i)
	t.choker.reviewUnchokedPeers()
	//TODO: black list in some way?
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
	t.removeConn(ci, i)
	t.choker.reviewUnchokedPeers()
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

//clear the fields that included the conn
func (t *Torrent) removeConn(ci *connInfo, index int) {
	t.conns = append(t.conns[:index], t.conns[index+1:]...)
	if t.wantPeers() {
		if t.canAnnounceTracker {
			t.sendAnnounceToTracker(tracker.None)
		}
		if t.canAnnounceDht {
			t.announceDht()
		}
	}
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

func (t *Torrent) downloadMetadata() bool {
	//take the infoSize that we have seen most times from peers
	infoSize := t.infoSizeFreq.max()
	if infoSize == 0 || infoSize > 10000000 { //10MB,anacrolix pulled from his ass
		return false
	}
	t.infoBytes = make([]byte, infoSize)
	isLastSmaller := infoSize%metadataPieceSz != 0
	numPieces := infoSize / metadataPieceSz
	if isLastSmaller {
		numPieces++
	}
	t.ownedInfoBlocks = make([]bool, numPieces)
	//send requests to all conns
	return true
}

func (t *Torrent) downloadedMetadata() bool {
	for _, v := range t.ownedInfoBlocks {
		if !v {
			return false
		}
	}
	return true
}

func (t *Torrent) writeMetadataPiece(b []byte, i int) error {
	//tm.metadataMu.Lock()
	//defer tm.metadataMu.Unlock()
	if t.ownedInfoBlocks[i] {
		return nil
	}
	//TODO:log this
	if i*metadataPieceSz >= len(t.infoBytes) {
		return errors.New("write metadata piece: out of range")
	}
	if len(b) > metadataPieceSz {
		return errors.New("write metadata piece: length of of piece too big")
	}
	if len(b) != metadataPieceSz && i != len(t.ownedInfoBlocks)-1 {
		return errors.New("write metadata piece:piece is not the last and length is not 16KB")
	}
	copy(t.infoBytes[i*metadataPieceSz:], b)
	t.ownedInfoBlocks[i] = true
	if t.downloadedMetadata() && t.verifyInfoDict() {
		t.gotInfo()
	} else {
		//pick the max freq `infoSize`
	}
	return nil
}

func (t *Torrent) readMetadataPiece(b []byte, i int) error {
	if !t.haveInfo() {
		panic("read metadata piece:we dont have info")
	}
	//out of range
	if i*metadataPieceSz >= len(t.infoBytes) {
		return errors.New("read metadata piece: out of range")
	}

	//last piece case
	if (i+1)*metadataPieceSz >= len(t.infoBytes) {
		b = t.infoBytes[i*metadataPieceSz:]
	} else {
		b = t.infoBytes[i*metadataPieceSz : (i+1)*metadataPieceSz]
	}
	return nil
}

func (t *Torrent) verifyInfoDict() bool {
	if sha1.Sum(t.infoBytes) != t.mi.Info.Hash {
		return false
	}
	if err := bencode.Decode(t.infoBytes, t.mi.Info); err != nil {
		t.logger.Fatal("cant decode info dict")
	}
	return true
}

//TODO:open storage & create pieces etc.
func (t *Torrent) gotInfo() {
	t.length = t.mi.Info.TotalLength()
	t.stats.BytesLeft = t.length
	t.blockRequestSize = t.blockSize()
	t.pieces = newPieces(t)
	t.pieceQueuedHashingCh = make(chan int, t.numPieces())
	t.pieceHashedCh = make(chan pieceHashed, t.numPieces())
	ph := pieceHasher{t: t}
	go ph.Run()
	var seeding bool
	t.storage, seeding = t.openStorage(t.mi, t.cl.config.BaseDir, t.pieces.blocks(), t.logger)
	t.broadcastCommand(haveInfo{})
	if seeding {
		//mark all bocks completed and do all apropriate things when a piece
		//hashing is succesfull
		for i, p := range t.pieces.pcs {
			p.unrequestedBlocks, p.completeBlocks = p.completeBlocks, p.unrequestedBlocks
			p.t.pieceHashed(i, true)
		}
		t.startSeeding()
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
	return t.mi.Info != nil
}
