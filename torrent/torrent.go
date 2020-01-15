package torrent

import (
	"context"
	"crypto/sha1"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/eapache/channels"
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

const maxUploadSlots = 4
const optimisticSlots = 1

//var blockSz = 1 << 14 //16KiB
var maxRequestBlockSz = 1 << 14

const metadataPieceSz = 1 << 14

//Torrent
type Torrent struct {
	cl     *Client
	logger *log.Logger
	//pconns   []*PeerConn
	//pstats   []peerStats
	//size of len(pconns) * eventChSize is reasonable
	events chan event

	storage *storage.Storage
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

	blockRequestSize int

	//surely we 'll need this
	done chan struct{}

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

	trackerURL tracker.TrackerURL

	//length of data to be downloaded
	length int

	stats *TorrentStats
}

func newTorrent(cl *Client) *Torrent {
	t := &Torrent{
		cl:           cl,
		reqq:         250, //libtorent also has this default
		done:         make(chan struct{}),
		events:       make(chan event, maxConns*eventChSize),
		newConnCh:    make(chan *connChansInfo, maxConns),
		infoSizeFreq: newFreqMap(),
	}
	t.choker = newChoker(t)
	return t
}

func (t *Torrent) mainLoop() {
	t.choker.startTicker()
	defer t.choker.ticker.Stop()
	for {
		select {
		case e := <-t.events:
			t.parseEvent(e)
		case cc := <-t.newConnCh: //we established a new connection
			if !t.addConn(cc) {
				t.logger.Println("rejected a connection with peer")
			} else {
				//TODO: check at paper if we should call this
				t.choker.reviewUnchokedPeers()
			}
		case <-t.choker.ticker.C:
			t.choker.reviewUnchokedPeers()
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
		t.pieces.pcs[v.pc].markBlockComplete(e.conn, int(v.off))
		//update interests
	case uploadedBlock:
		//add to stats
	case pieceVerificationOk:
		if v.ok {
			if t.pieces.pieceVerified(v.pieceIndex) {
				t.startSeeding()
			}
		} else {
			t.pieces.pieceVerificationFailed(v.pieceIndex)
		}
	case metainfoSize:
	case bitmap.Bitmap:
		e.conn.peerBf = v
		e.conn.reviewInterestsOnBitfield()
	//before conn goroutine exits, it sends a dropConn event
	case connDroped:
		t.removeConn(e.conn)
		t.choker.reviewUnchokedPeers()

	}
}

func (t *Torrent) startSeeding() {
	t.logger.Printf("operating in seeding mode...")
	t.seeding = true
	for _, c := range t.conns {
		close(c.amSeedingCh)
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
func (t *Torrent) aggregateEvents(ci *connInfo) {
	for e := range ci.eventCh {
		t.events <- event{ci, e}
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
	if len(t.conns) >= maxConns {
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
	if t.haveInfo() {
		close(ci.amSeedingCh)
	}
	if t.seeding {
		close(ci.haveInfoCh)
	}
	//if we have some pieces, we should sent a bitfield
	if t.pieces.ownedPieces.Len() > 0 {
		ci.sendJob(t.pieces.ownedPieces.Copy())
	}
	go t.aggregateEvents(ci)
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
	if t.pieces != nil {
		if _, ok := t.pieces.requestExpecters[ci]; ok {
			delete(t.pieces.requestExpecters, ci)
		}
	}
	return true
}

//TODO: maybe wrap these at the same func passing as arg the func (read/write)
func (t *Torrent) writeBlock(data []byte, piece, begin int) error {
	off := int64(piece*t.mi.Info.PieceLen + begin)
	n, err := t.storage.WriteBlock(data, off)
	if n != t.blockRequestSize {
		if errors.Is(err, storage.ErrAlreadyWritten) {
			//TODO: if not in endgame mode, then panic.
			t.logger.Printf("attempted to write same block twice")
			return nil
		}
		t.logger.Printf("coudn't write whole block from storage, written only %d bytes\n", n)
	}
	if err != nil {
		t.logger.Printf("storage write err %s\n", err)
	}
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
	//setInfo
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
	if t.downloadedMetadata() {
		t.verifyInfoDict()
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
	t.gotInfo()
	return true
}

//TODO:open storage & create pieces etc.
func (t *Torrent) gotInfo() {
	t.length = t.mi.Info.TotalLength()
	t.stats.Left = t.length
	t.blockRequestSize = t.blockSize()
	t.pieces = newPieces(t)
	t.storage = storage.Open(t.mi, t.cl.config.baseDir, t.pieces.blocks(), t.logger)
	//notify conns that we have the info
	for _, c := range t.conns {
		close(c.haveInfoCh)
	}
}

func (t *Torrent) PieceLen(i uint32) (pieceLen int) {
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

func (t *Torrent) announceTracker(event tracker.Event) (*tracker.AnnounceResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req := tracker.AnnounceReq{
		InfoHash:   t.mi.Info.Hash,
		PeerID:     peerID,
		Downloaded: int64(t.stats.Downloaded),
		Left:       int64(t.stats.Left),
		Uploaded:   int64(t.stats.Uploaded),
		Event:      event,
		Numwant:    -1,
		Port:       t.cl.port,
	}
	return t.trackerURL.Announce(ctx, req)
}

func (t *Torrent) scrapeTracker() (*tracker.ScrapeResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return t.trackerURL.Scrape(ctx, t.mi.Info.Hash)
}

func (t *Torrent) haveInfo() bool {
	return t.mi.Info != nil
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

//connInfo sends msgs to conn .It also holds
//some informations like state,bitmap which also conn holds too -
//we dont share, we communicate so we have some duplicate data-.
type connInfo struct {
	t *Torrent
	//we communicate with conn with these channels - conn also has them
	jobCh       channels.Channel
	eventCh     chan interface{}
	amSeedingCh chan struct{}
	haveInfoCh  chan struct{}
	//drop        chan struct{}
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
	if !cn.t.haveInfo() || cn.t.seeding {
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
	if !cn.t.haveInfo() || cn.t.seeding {
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
		//Set last piece msg the first time we get into `downloading` state.
		//We didn't got any piece msg but we want to have an initial time to check
		//if we are snubbed.
		if cn.stats.lastPieceMsg.IsZero() {
			cn.stats.lastPieceMsg = time.Now()
		}
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
	if !cn.t.haveInfo() { //we don't know if it has all (maybe he has)
		return false
	}
	return cn.peerBf.Len() == cn.t.numPieces()
}
