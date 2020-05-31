package torrent

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/anacrolix/torrent"
	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/torrent/storage"
	"github.com/lkslts64/charo-torrent/tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTorrentNewConnection(t *testing.T) {
	_, tr := newClientWithTorrent(t, testingConfig(), helloWorldTorrentFile, nil)
	err := tr.StartDataTransfer()
	require.NoError(t, err)
	for i := 0; i < tr.maxEstablishedConnections; i++ {
		ci := &connInfo{
			t:        tr,
			recvC:    make(chan interface{}, sendCSize),
			sendC:    make(chan interface{}, recvCSize),
			droppedC: make(chan struct{}),
		}
		tr.newConnC <- ci
		switch (<-ci.sendC).(type) {
		case haveInfo:
		default:
			t.Fail()
		}
		switch (<-ci.sendC).(type) {
		case bitmap.Bitmap:
		default:
			t.Fail()
		}
	}
	assert.Equal(t, tr.maxEstablishedConnections, len(tr.conns))
}

func TestStatsUpdate(t *testing.T) {
	tr := &Torrent{
		mi: &metainfo.MetaInfo{},
		utmetadata: &utMetadatas{
			m: make(map[int64]*utMetadata),
		},
	}
	tr.downloadEnabled = true
	tr.uploadEnabled = true
	ci := &connInfo{
		t:        tr,
		recvC:    make(chan interface{}, sendCSize),
		sendC:    make(chan interface{}, recvCSize),
		droppedC: make(chan struct{}),
		state:    newConnState(),
		numWant:  1, //pretend that we want pieces
	}

	//test if durationUploading changes when our state changes
	tr.onConnMsg(msgWithConn{
		conn: ci,
		val: &peer_wire.Msg{
			Kind: peer_wire.Interested,
		},
	})
	ci.unchoke()
	sleepDur := time.Millisecond
	time.Sleep(sleepDur)
	assert.GreaterOrEqual(t, int64(ci.durationUploading()), int64(sleepDur))
	assert.Equal(t, int64(0), int64(ci.stats.sumUploading))
	ci.choke()
	assert.Greater(t, int64(ci.stats.sumUploading), int64(0))
	time.Sleep(sleepDur)
	assert.Less(t, int64(ci.durationUploading()), int64(2*sleepDur))
	//test how the download changes as time passes and as we download bytes
	assert.EqualValues(t, int64(0), int64(ci.durationDownloading()))
	ci.interested()
	tr.onConnMsg(msgWithConn{
		conn: ci,
		val: &peer_wire.Msg{
			Kind: peer_wire.Unchoke,
		},
	})
	time.Sleep(sleepDur)
	assert.GreaterOrEqual(t, int64(ci.durationDownloading()), int64(sleepDur))
	assert.Equal(t, float64(0), ci.rate())
	ci.stats.downloadUsefulBytes += 1 << 14
	r1 := ci.rate()
	assert.Greater(t, r1, float64(0))
	time.Sleep(sleepDur)
	r2 := ci.rate()
	assert.Less(t, r2, r1)
}

func testingConfig() *Config {
	return &Config{
		SelectorF:           NewDefaultPieceSelector,
		MaxOnFlightReqs:     250,
		MaxEstablishedConns: 55,
		BaseDir:             "./testdata/",
		DisableTrackers:     true,
		DisableDHT:          true,
		OpenStorage:         storage.OpenFileStorage,
		DialTimeout:         5 * time.Second,
		HandshakeTiemout:    4 * time.Second,
	}
}

var helloWorldTorrentFile = "./testdata/helloworld.torrent"
var helloWorldContents = "Hello World\n"
var blockchainTorrentFile = "./testdata/blockchain.torrent"

func TestLoadCompleteTorrent(t *testing.T) {
	_, tr := newClientWithTorrent(t, testingConfig(), helloWorldTorrentFile, nil)
	assert.Equal(t, true, tr.haveAll())
	data := make([]byte, tr.pieceLen(0))
	tr.readBlock(data, 0, 0)
	assert.EqualValues(t, 12, len(data))
	assert.EqualValues(t, helloWorldContents, string(data))
}

func TestSingleFileTorrentTransfer(t *testing.T) {
	testDataTransfer(t, dataTransferOpts{
		helloWorldTorrentFile,
		1,
	})
}

func TestMultiFileTorrentTransfer(t *testing.T) {
	testDataTransfer(t, dataTransferOpts{
		blockchainTorrentFile,
		7,
	})
}

func addrsToPeers(addrs []string) []Peer {
	peers := make([]Peer, len(addrs))
	for i, addr := range addrs {
		peers[i] = addrToPeer(addr, SourceUser)
	}
	return peers
}

type dataTransferOpts struct {
	filename    string
	numLeechers int
}

//create one seeder and multiple leechers and make them try to download the torrent cooperatively
func testDataTransfer(t *testing.T, opts dataTransferOpts) {
	seeder, seederTr := newClientWithTorrent(t, testingConfig(), helloWorldTorrentFile, func(tr *Torrent) {
		assert.True(t, tr.haveAll())
		require.NoError(t, tr.StartDataTransfer())
	})
	defer seeder.Close()
	//create leechers
	leechers := make([]*Client, opts.numLeechers)
	leechAddrs := make([]string, len(leechers))
	for i := range leechers {
		tcfg := testingConfig()
		tcfg.BaseDir += "/leecher" + strconv.Itoa(i)
		leechers[i], _ = newClientWithTorrent(t, tcfg, helloWorldTorrentFile, nil)
		leechAddrs[i] = leechers[i].addr()
		defer leechers[i].Close()
		defer os.RemoveAll(tcfg.BaseDir)
	}
	wg := sync.WaitGroup{}
	wg.Add(len(leechers))
	for i, leecher := range leechers {
		leecherTr := leecher.Torrents()[0]
		go func() {
			defer wg.Done()
			require.NoError(t, leecherTr.StartDataTransfer())
			<-leecherTr.DownloadedDataC
		}()
		leecherTr.AddPeers(addrsToPeers(append(leechAddrs[i+1:], seeder.addr()))...)
	}
	/*ticker := time.NewTicker(time.Second)
	for {

		select {
		case <-ticker.C:
			fmt.Println("----------- leeecher ------------")
			leechers[0].counters.Do(func(kv expvar.KeyValue) {
				fmt.Println(kv)
			})
			fmt.Println("----------- seederr ------------")
			seeder.counters.Do(func(kv expvar.KeyValue) {
				fmt.Println(kv)
			})
			for _, p := range leechers[0].Torrents()[0].Pieces() {
				if p.Verified() {
					fmt.Println(p.Index())
				}
			}
		}
	}*/
	//load seeder data
	dataSeeder := make([]byte, seederTr.length)
	err := seederTr.readBlock(dataSeeder, 0, 0)
	require.NoError(t, err)
	wg.Wait()
	for _, leecher := range leechers {
		leecherTr := leecher.Torrents()[0]
		assert.True(t, leecherTr.haveAll())
		testContents(t, dataSeeder, leecherTr)
	}
}

func testContents(t *testing.T, dataSeeder []byte, leecherTr *Torrent) {
	assert.Equal(t, len(dataSeeder), leecherTr.length)
	dataLeecher := make([]byte, leecherTr.length)
	err := leecherTr.readBlock(dataLeecher, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, dataSeeder, dataLeecher)
}

func testThirdPartyDataTransfer(t *testing.T, torrentFile string) {
	if testing.Short() {
		t.Skip("skiping test with third party torrent libriaries (anacrolix)")
	}
	//anacrolix client setup
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = "./testdata"
	cfg.NoDHT = true
	cfg.Seed = true
	cfg.DisablePEX = true
	seeder, err := torrent.NewClient(cfg)
	require.NoError(t, err)
	defer seeder.Close()
	seederTr, err := seeder.AddTorrentFromFile(torrentFile)
	require.NoError(t, err)
	seederTr.VerifyData()
	assert.True(t, seederTr.Seeding())
	//this library client setup
	tcfg := testingConfig()
	tcfg.BaseDir += "/leecher"
	leecher, leecherTr := newClientWithTorrent(t, tcfg, torrentFile, nil)
	defer os.RemoveAll(leecher.config.BaseDir)
	leecherTr.AddPeers(addrToPeer(seeder.ListenAddrs()[0].String(), SourceUser))
	leecherTr.StartDataTransfer()
	<-leecherTr.DownloadedDataC

	assert.True(t, leecherTr.haveAll())
	testContentsThirdParty(t, seederTr, leecherTr)
}

func testContentsThirdParty(t *testing.T, seederTr *torrent.Torrent, leecherTr *Torrent) {
	assert.EqualValues(t, seederTr.Length(), leecherTr.length)
	dataSeeder := make([]byte, seederTr.Length())
	dataLeecher := make([]byte, leecherTr.length)
	r := seederTr.NewReader()
	n, err := r.Read(dataSeeder)
	require.EqualValues(t, err, io.EOF)
	if n != len(dataSeeder) {
		t.Log("third party reader:", err)
	}
	assert.EqualValues(t, n, len(dataSeeder))
	err = leecherTr.readBlock(dataLeecher, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, dataSeeder, dataLeecher)
}

func TestThirdPartySingleFileDataTransfer(t *testing.T) {
	testThirdPartyDataTransfer(t, helloWorldTorrentFile)
}

func TestThirdPartyMultiFileDataTransfer(t *testing.T) {
	testThirdPartyDataTransfer(t, blockchainTorrentFile)
}

//dummy tracker will always respond to announces with the same peer
type dummyTracker struct {
	//close  chan struct{}
	myAddr       string
	t            *testing.T
	peer         tracker.Peer
	numAnnounces int
}

func (dt *dummyTracker) addr() string {
	return "http://" + dt.myAddr + "/announce"
}

type httpAnnounceResponse struct {
	Interval int32          `bencode:"interval"`
	Peers    []tracker.Peer `bencode:"peers" empty:"omit"`
}

func (dt *dummyTracker) announceHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	e, ok := q["event"]
	if ok {
		assert.Len(dt.t, e, 1)
		assert.EqualValues(dt.t, "started", e[0])
		assert.Equal(dt.t, 0, dt.numAnnounces)
	}
	bytes, _ := bencode.Encode(httpAnnounceResponse{
		Interval: 1,
		Peers: []tracker.Peer{
			dt.peer,
		},
	})
	w.Write(bytes)
	dt.numAnnounces++
}

func (dt *dummyTracker) serve() {
	port := strings.Split(dt.myAddr, ":")[1]
	http.HandleFunc("/announce", dt.announceHandler)
	go func() {
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}()
}

var localhost = "127.0.0.1"

//TODO:sometimes this fails (when running all package tests)
//Test the interaction with a tracker.Check that the announce goes as expected
//and that we dont connect to the same peer multiple times.
func TestTrackerAnnouncer(t *testing.T) {
	cfg := testingConfig()
	cfg.DisableTrackers = false
	cfg.BaseDir = ".testdata/utopia"
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	dt := &dummyTracker{
		myAddr: localhost + ":8081",
		t:      t,
		//tracker will always respond with the client's ip/port pair
		peer: tracker.Peer{
			ID:   cl.ID(),
			IP:   []byte(getOutboundIP().String()),
			Port: uint16(cl.port),
		},
	}
	dt.serve()
	tr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	tr.mi.Announce = dt.addr()
	require.NoError(t, tr.StartDataTransfer())
	//we want to announce multiple times so sleep for a bit
	time.Sleep(4 * time.Second)
	defer cl.Close()
	//Assert that we filtered duplicate ip/port pairs
	//We should have established only 2 connections (in essence 1 but actually, because we
	//have connected to ourselves there are 2 - one becaused we dialed in `client.connectToPeer` and that
	//triggered us to accept another one in `client.handleConn`.)
	assert.Equal(t, 2, len(tr.conns))
}

/*
//Open tracker should be running at port 8080
func TestWithOpenTracker(t *testing.T) {
	cfg := testingConfig()
	cfg.BaseDir = "./testdata/leecher"
	cfg.DisableTrackers = false
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	tr, err := cl.NewTorrentFromFile("./testdata/hello_world.torrent")
	require.NoError(t, err)
	tr.mi.Announce = "http://" + localhost + ":8080/announce"
	tr.Download()
	for {
		time.Sleep(100 * time.Millisecond)
		tr.WriteStatus(os.Stdout)
	}
	cl.Close()
}*/

//TODO:test piece verification failure by mocking storage (specifically ReadBlock())
//mock storage by giving user to options to provide its own OpenStorage impl in client
//config. OR mock net.Conn to write garbage

//TODO: test that we indeed unchoke by rate , tip: mock storage and decrease donwload rate
//by sleeping in mocked funcs

//wraps a storage and delays read operations
type readDelayedStorage struct {
	delay time.Duration
	//fs    storage.Storage
}

/*func OpenReadDelayedStorage(mi *metainfo.MetaInfo, baseDir string, blocks []int,
	logger *log.Logger) (s storage.Storage, seed bool) {
	fs, seed := storage.OpenFileStorage(mi, baseDir, blocks, logger)
	s = &readDelayedStorage{
		fs: fs,
	}
	return
}*/

func (ds *readDelayedStorage) ReadBlock(b []byte, off int64) (n int, err error) {
	time.Sleep(ds.delay)
	n = len(b)
	return
	//return ds.fs.ReadBlock(b, off)
}

func (ds *readDelayedStorage) WriteBlock(b []byte, off int64) (n int, err error) {
	time.Sleep(ds.delay)
	n = len(b)
	return
	//return ds.fs.WriteBlock(b, off)
}

func (ds *readDelayedStorage) HashPiece(pieceIndex int, len int) (correct bool) {
	time.Sleep(ds.delay)
	correct = true
	return
	//return ds.fs.HashPiece(pieceIndex, len)
}

func newClientWithTorrent(tb testing.TB, cfg *Config, filename string, callback func(tr *Torrent)) (*Client, *Torrent) {
	cl, err := NewClient(cfg)
	require.NoError(tb, err)
	tr, err := cl.AddFromFile(filename)
	require.NoError(tb, err)
	if callback != nil {
		callback(tr)
	}
	return cl, tr
}

//use this for piece validation
//TODO: dont parse again and again
//make option for multiple seeders and benchmark the parallel download
func benchmarkTorrentDownload(b *testing.B, filename string, numSeeds int, storage func() storage.Storage) {
	require.Greater(b, numSeeds, 0)
	cb := func(t *Torrent) {
		t.storage = storage()
		t.StartDataTransfer()
	}
	seeders := make([]*Client, numSeeds)
	seedAddrs := make([]string, len(seeders))
	for i := range seeders {
		seeders[i], _ = newClientWithTorrent(b, testingConfig(), filename, cb)
		seedAddrs[i] = seeders[i].addr()
		defer seeders[i].Close()
	}
	tcfg := testingConfig()
	tcfg.BaseDir = tcfg.BaseDir + "/leecher"
	leecher, err := NewClient(tcfg)
	require.NoError(b, err)
	defer leecher.Close()
	require.NoError(b, err)
	b.ResetTimer()
	b.SetBytes(int64(seeders[0].Torrents()[0].length))
	for i := 0; i < b.N; i++ {
		leecherTr, err := leecher.AddFromFile(filename)
		require.NoError(b, err)
		leecherTr.storage = storage()
		leecherTr.AddPeers(addrsToPeers(seedAddrs)...)
		require.NoError(b, leecherTr.StartDataTransfer())
		<-leecherTr.DownloadedDataC
		leecherTr.Close()
	}
}

func BenchmarkTorrentDownload(b *testing.B) {
	delayed := func() storage.Storage {
		return &readDelayedStorage{time.Millisecond}
	}
	dummy := func() storage.Storage {
		return &dummyStorage{}
	}
	tt := []struct {
		filename string
		numSeeds int
		storage  func() storage.Storage
	}{
		{helloWorldTorrentFile, 1, dummy},
		{blockchainTorrentFile, 1, delayed},
		{blockchainTorrentFile, 4, delayed},
	}
	for _, t := range tt {
		b.Run(fmt.Sprintf("%s,%d", t.filename, t.numSeeds), func(b *testing.B) {
			benchmarkTorrentDownload(b, t.filename, t.numSeeds, t.storage)
		})
	}
}

func TestMultipleClose(t *testing.T) {
	cl, err := NewClient(testingConfig())
	tr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	//it shouldn't be a problem to call close multiple times
	tr.Close()
	tr.Close()
	//should return error on closed torrent
	require.Error(t, tr.AddPeers(Peer{}))
	//call client close too,
	cl.Close()
}

func TestWantConnsAndPeers(t *testing.T) {
	cfg := testingConfig()
	cfg.BaseDir = "./leecher"
	cl, err := NewClient(cfg)
	tr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	assert.False(t, tr.wantConns())
	assert.False(t, tr.wantPeers())
	assert.Zero(t, len(tr.swarm()))
	tr.StartDataTransfer()
	assert.True(t, tr.wantConns())
	assert.True(t, tr.wantPeers())
}

//TODO:sometimes this fails (when running all package tests)
//In linux (and possibly in Windows) there is a limit to how many open file
//discriptors a process can have. If we dont enforce the limit, all reads/writes
//from sockets,files etc will fail, so eventually a fatal error will occur or the
//timer will expire.
func TestHalfOpenConnsLimit(t *testing.T) {
	cfg := testingConfig()
	cfg.DialTimeout = time.Millisecond
	cfg.BaseDir = "./leecher"
	cl, err := NewClient(cfg)
	tr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	tr.StartDataTransfer()
	addInvalidPeers := func(invalidAddrPrefix string) {
		peers := []Peer{}
		for i := 0; i <= 255; i++ {
			peers = append(peers, addrToPeer(invalidAddrPrefix+strconv.Itoa(i)+":9090", SourceUser))
		}
		require.NoError(t, tr.AddPeers(peers...))
	}
	//these are invalid IP addreses (https://stackoverflow.com/questions/10456044/what-is-a-good-invalid-ip-address-to-use-for-unit-tests)
	addInvalidPeers("192.0.2.")
	addInvalidPeers("198.51.100.")
	addInvalidPeers("203.0.113.")
	//wait until we have tried to connect to all peers
	failure := time.NewTimer(10 * time.Second)
	for {
		time.Sleep(30 * time.Millisecond)
		//hacky way to get all conns that we tried to dial but failed
		tried, err := strconv.Atoi(cl.counters.Get("could not dial").String())
		if err != nil {
			continue
		}
		if tried >= 3*256 {
			//we tried all conns
			break
		}
		select {
		case <-failure.C:
			t.FailNow()
		default:
		}
	}
}

//Test that is safe to invoke methods on torrent simultaneously and that after close some
//methods return errors as they should be.
func TestTorrentParallelXported(t *testing.T) {
	cfg := testingConfig()
	cfg.BaseDir = "./leecher"
	cl, err := NewClient(cfg)
	defer cl.Close()
	tr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	require.NoError(t, tr.StartDataTransfer())
	//download twice gives error
	require.Error(t, tr.StartDataTransfer())
	testXported := func(expectErr bool) {
		wg := sync.WaitGroup{}
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := tr.AddPeers(Peer{})
			if expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		}()
		go func() {
			defer wg.Done()
			var b bytes.Buffer
			tr.WriteStatus(&b)
			assert.Greater(t, b.Len(), 0)
		}()
		wg.Wait()
	}
	testXported(false)
	tr.Close()
	assert.True(t, tr.Closed())
	testXported(true)
}

func TestTorrentParallelClose(t *testing.T) {
	cfg := testingConfig()
	cl, err := NewClient(cfg)
	defer cl.Close()
	tr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	wg := sync.WaitGroup{}
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			tr.Close()
		}()
	}
	wg.Wait()
	assert.True(t, tr.Closed())
}

func TestMetadataExtension(t *testing.T) {
	seeder, err := NewClient(testingConfig())
	require.NoError(t, err)
	seederTr, err := seeder.AddFromFile(blockchainTorrentFile)
	require.NoError(t, err)
	require.NoError(t, seederTr.StartDataTransfer())
	cfg := testingConfig()
	cfg.BaseDir += "/leecher"
	defer os.RemoveAll(cfg.BaseDir)
	leecher, err := NewClient(cfg)
	require.NoError(t, err)
	leecherTr, err := leecher.AddFromParser(&metainfo.InfoHashParser{seederTr.mi.Info.Hash})
	require.NoError(t, err)
	leecherTr.AddPeers(addrToPeer(seeder.addr(), SourceIncoming))
	<-leecherTr.InfoC
	assert.Equal(t, seederTr.mi.Info, leecherTr.mi.Info)
	require.NoError(t, leecherTr.StartDataTransfer())
	<-leecherTr.DownloadedDataC
}

func addrsFromClients(clients []*Client) []string {
	ret := make([]string, len(clients))
	for i, c := range clients {
		ret[i] = c.addr()
	}
	return ret
}

func TestMetadata2Extension(t *testing.T) {
	size := (1 << 16) + 300
	uts := []*utMetadata{
		newTestRandUtMetadata(size), //mallicious
		newTestRandUtMetadata(size), //mallicious
		nil,                         //honest
	}
	testMetadataExtensionMallicious(t, uts)
}

//len(uts)-1 seeders try to send fake metadata and one honest seeder tries to send
//the right ones.
func testMetadataExtensionMallicious(t *testing.T, uts []*utMetadata) {
	seeders := make([]*Client, len(uts))
	for i, ut := range uts {
		seeders[i] = startClientCustomMetadata(t, ut)
	}
	cfg := testingConfig()
	cfg.BaseDir += "/leecher"
	defer os.RemoveAll(cfg.BaseDir)
	leecher, err := NewClient(cfg)
	require.NoError(t, err)
	ihash := seeders[0].Torrents()[0].mi.Info.Hash
	leecherTr, err := leecher.AddFromParser(&metainfo.InfoHashParser{ihash})
	require.NoError(t, err)
	addrs := addrsFromClients(seeders)
	honest := addrs[len(addrs)-1]
	mallicious := addrs[:len(addrs)-1]
	require.NoError(t, leecherTr.AddPeers(addrsToPeers(mallicious)...))
	//connect to the honest seeder after a delay, we want to give the mallicious seeders
	//time to forward their metadata.
	//TODO: find a way to identify fake metadata and wait until we have len(uts) - 1 of them.
	time.AfterFunc(2*time.Second, func() {
		require.NoError(t, leecherTr.AddPeers(addrToPeer(honest, SourceIncoming)))
	})
	<-leecherTr.InfoC
	require.NoError(t, leecherTr.StartDataTransfer())
	<-leecherTr.DownloadedDataC
}

func newTestUtMetadata(bytes []byte) *utMetadata {
	ut := newUtMetadata(len(bytes))
	ut.infoBytes = bytes
	return ut
}

func newTestRandUtMetadata(size int) *utMetadata {
	b := make([]byte, size)
	rand.Read(b)
	return newTestUtMetadata(b)
}

func startClientCustomMetadata(t *testing.T, ut *utMetadata) *Client {
	cl, err := NewClient(testingConfig())
	require.NoError(t, err)
	seederTr, err := cl.AddFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	if ut != nil {
		metadata := seederTr.utmetadata
		metadata.m = make(map[int64]*utMetadata)
		metadata.m[int64(len(ut.infoBytes))] = ut
		metadata.correctSize = int64(len(ut.infoBytes))
	}
	err = seederTr.StartDataTransfer()
	require.NoError(t, err)
	return cl
}
