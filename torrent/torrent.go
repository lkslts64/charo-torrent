package torrent

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

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
	cl     *Client
	logger *log.Logger
	//pconns   []*PeerConn
	//pstats   []peerStats
	//size of len(pconns) * eventChSize is reasonable
	events chan event

	storage storage.Storage
	//These are active conns
	conns []*connInfo
	//conn can send a mgs to this chan so master goroutine knows a new connection
	//was established/closed
	//this chan has size = maxConns / 2 ?
	newConnCh chan *connChansInfo
	pieces    *pieces

	choker *choker

	//are we seeding
	seeding bool
	//An integer, the number of outstanding request messages we support
	//without dropping any. The default in in libtorrent is 250.
	reqq int

	blockRequestSize              int
	trackerAnnouncerTimer         *time.Timer
	canAnnounce                   bool
	trackerAnnouncerResponseCh    chan trackerAnnouncerResponse
	trackerAnnouncerSubmitEventCh chan trackerAnnouncerEvent
	lastAnnounceResp              *tracker.AnnounceResp
	numAnnounces                  int
	numAnnouncesSend              int
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
	mi *metainfo.MetaInfo
	//haveInfo bool
	infoSize int64
	//we serve metadata only if we have it all.
	//lock only when writing
	metadataMu          sync.Mutex
	infoRaw             []byte
	ownedMetadataPieces []bool
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
		cl:                            cl,
		reqq:                          250, //libtorent also has this default
		done:                          make(chan struct{}),
		events:                        make(chan event, maxConns*eventChSize),
		newConnCh:                     make(chan *connChansInfo, maxConns),
		downloadedAll:                 make(chan struct{}),
		drop:                          make(chan struct{}),
		displayCh:                     make(chan chan []byte, 10),
		close:                         make(chan chan struct{}),
		trackerAnnouncerSubmitEventCh: cl.announcer.trackerAnnouncerSubmitEventCh,
		trackerAnnouncerResponseCh:    make(chan trackerAnnouncerResponse, 1),
		trackerAnnouncerTimer:         newExpiredTimer(),
		queuedForVerification:         make(map[int]struct{}),
		infoSizeFreq:                  newFreqMap(),
		stats:                         Stats{},
		logger:                        log.New(cl.logWriter, "torrent", log.LstdFlags),
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
	//if we dont announce tracker.Started event then we won't announce anything at all
	//throughout the lifetime of the Torrent.
	t.sendAnnounceEvent(tracker.Started)
	t.choker.startTicker()
	defer t.choker.ticker.Stop()
	defer t.trackerAnnouncerTimer.Stop()
	for {
		select {
		case e := <-t.events:
			t.parseEvent(e)
			t.eventsReceived++
		case res := <-t.pieceHashedCh:
			t.pieceHashed(res.pieceIndex, res.ok)
			if res.ok && t.pieces.allVerified() {
				t.sendAnnounceEvent(tracker.Completed)
				t.startSeeding()
			}
		case cc := <-t.newConnCh: //we established a new connection
			if !t.addConn(cc) {
				t.logger.Println("rejected a connection with peer")
			} else {
				t.choker.reviewUnchokedPeers()
			}
		case <-t.choker.ticker.C:
			t.choker.reviewUnchokedPeers()
		case tresp := <-t.trackerAnnouncerResponseCh:
			t.trackerAnnounced(tresp)
		case <-t.trackerAnnouncerTimer.C:
			t.canAnnounce = true
			if t.wantPeers() {
				t.sendAnnounceEvent(tracker.None)
			}
		case statusBytesCh := <-t.displayCh:
			var b strings.Builder
			t.writeStatus(&b)
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
	//wait until all conns close and then close `doneClosing`.
	for _, c := range t.conns {
		<-c.dropped
		//TODO:logging when conn closes is bad. find another way to log dropped conns
		//stats.Maybe hold a droppedConns slice, or maybe dont care about droppedConns?
		//t.logger.Println(c.String())
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
		case peer_wire.NotInterested:
			e.conn.state.isInterested = false
			if !e.conn.state.amChoking {
				t.choker.reviewUnchokedPeers()
			}
		case peer_wire.Choke:
			e.conn.state.isChoking = true
			if e.conn.state.amInterested {
				e.conn.stats.stopDownloading()
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
		if t.pieces.allVerified() {
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
	case net.Addr:
		e.conn.addr = v.String()
	}
}

func (t *Torrent) wantPeers() bool {
	return len(t.conns) < wantPeersThreshold
}

func (t *Torrent) sendAnnounceEvent(event tracker.Event) {
	if !t.cl.config.DisableTrackers {
		t.trackerAnnouncerSubmitEventCh <- trackerAnnouncerEvent{t, event, t.stats}
		t.numAnnouncesSend++
		t.canAnnounce = false
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
	addrsNotConnected := []string{}
	//TODO: whot told us that trackers won't give us duplicate peers?
nextPeer:
	for _, peer := range tresp.resp.Peers {
		for _, ci := range t.conns {
			if ci.addr == peer.String() {
				continue nextPeer
			}
		}
		addrsNotConnected = append(addrsNotConnected, peer.String())
	}
	t.cl.connectToPeers(t, addrsNotConnected...)
	t.resetNextAnnounce(tresp.resp.Interval)
	t.lastAnnounceResp = tresp.resp
}

func (t *Torrent) resetNextAnnounce(interval int32) {
	nextAnnounce := time.Duration(interval) * time.Second
	if t.trackerAnnouncerTimer == nil {
		t.trackerAnnouncerTimer = time.NewTimer(nextAnnounce)
	} else {
		select {
		case <-t.trackerAnnouncerTimer.C:
			//panic("not drained chan")
		default:
		}
		if t.trackerAnnouncerTimer.Stop() {
			panic("announce before tracker specified interval")
		}
		t.trackerAnnouncerTimer.Reset(nextAnnounce)
	}
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
	b.WriteString("Tracker: " + t.mi.Announce + "\tAnnounce: " + func() string {
		if t.lastAnnounceResp != nil {
			return "OK"
		}
		return "Not Available"
	}() + "\t#AnnouncesSend: " + strconv.Itoa(t.numAnnouncesSend) + "\n")
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
	//t.logger.Printf("operating in seeding mode...")
	t.seeding = true
	t.broadcastCommand(seeding{})
	for _, c := range t.conns {
		c.notInterested()
	}
	close(t.downloadedAll)
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
		t.pieces.pieceSuccesfullyVerified(i)
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

func (t *Torrent) addConn(cc *connChansInfo) bool {
	if len(t.conns) >= maxConns {
		cc.commandCh <- drop{}
		//close(cc.drop) //signal that conn should be dropped
		return false
	}
	ci := &connInfo{
		t:         t,
		commandCh: cc.commandCh,
		eventCh:   cc.eventCh,
		dropped:   cc.dropped,
		state:     newConnState(),
		stats:     newConnStats(),
	}
	t.conns = append(t.conns, ci)
	//notify conn that we have metainfo or we are seeding by closing these chans
	if t.haveInfo() {
		ci.sendCommand(haveInfo{})
	}
	if t.seeding {
		ci.sendCommand(seeding{})
	}
	//if we have some pieces, we should sent a bitfield
	if t.pieces.ownedPieces.Len() > 0 {
		ci.sendBitfield()
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
	if t.canAnnounce && t.wantPeers() {
		t.sendAnnounceEvent(tracker.None)
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
	t.infoRaw = make([]byte, infoSize)
	isLastSmaller := infoSize%metadataPieceSz != 0
	numPieces := infoSize / metadataPieceSz
	if isLastSmaller {
		numPieces++
	}
	t.ownedMetadataPieces = make([]bool, numPieces)
	//send requests to all conns
	return true
}

func (t *Torrent) downloadedMetadata() bool {
	for _, v := range t.ownedMetadataPieces {
		if !v {
			return false
		}
	}
	return true
}

func (t *Torrent) writeMetadataPiece(b []byte, i int) error {
	//tm.metadataMu.Lock()
	//defer tm.metadataMu.Unlock()
	if t.ownedMetadataPieces[i] {
		return nil
	}
	//TODO:log this
	if i*metadataPieceSz >= len(t.infoRaw) {
		return errors.New("write metadata piece: out of range")
	}
	if len(b) > metadataPieceSz {
		return errors.New("write metadata piece: length of of piece too big")
	}
	if len(b) != metadataPieceSz && i != len(t.ownedMetadataPieces)-1 {
		return errors.New("write metadata piece:piece is not the last and length is not 16KB")
	}
	copy(t.infoRaw[i*metadataPieceSz:], b)
	t.ownedMetadataPieces[i] = true
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
	if i*metadataPieceSz >= len(t.infoRaw) {
		return errors.New("read metadata piece: out of range")
	}

	//last piece case
	if (i+1)*metadataPieceSz >= len(t.infoRaw) {
		b = t.infoRaw[i*metadataPieceSz:]
	} else {
		b = t.infoRaw[i*metadataPieceSz : (i+1)*metadataPieceSz]
	}
	return nil
}

func (t *Torrent) verifyInfoDict() bool {
	if sha1.Sum(t.infoRaw) != t.mi.Info.Hash {
		return false
	}
	if err := bencode.Decode(t.infoRaw, t.mi.Info); err != nil {
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
	t.storage, seeding = storage.OpenFileStorage(t.mi, t.cl.config.BaseDir, t.pieces.blocks(), t.logger)
	t.broadcastCommand(haveInfo{})
	if seeding {
		//mark all bocks completed and do all apropriate things when a piece
		//verification is succesfull
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

func (t *Torrent) pieceValid(piece int) bool {
	return piece >= 0 && piece < t.numPieces()
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
