package torrent

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/anacrolix/torrent"
	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/lkslts64/charo-torrent/tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTorrentNewConnection(t *testing.T) {
	cl, err := NewClient(nil)
	defer os.RemoveAll(cl.config.BaseDir)
	require.NoError(t, err)
	tr, err := cl.NewTorrentFromFile("../metainfo/testdata/oliver-koletszki-Schneeweiss11.torrent")
	tr.pieces.ownedPieces.Set(0, true)
	require.NoError(t, err)
	go tr.mainLoop()
	for i := 0; i < maxConns; i++ {
		cc := &connChansInfo{
			eventCh:   make(chan interface{}, eventChSize),
			commandCh: make(chan interface{}, commandChSize),
			dropped:   make(chan struct{}),
		}
		tr.newConnCh <- cc
		v := <-cc.commandCh
		switch v.(type) {
		case haveInfo:
		default:
			t.Fail()
		}
		v = <-cc.commandCh
		switch v.(type) {
		case bitmap.Bitmap:
		default:
			t.Fail()
		}
	}
	assert.Equal(t, maxConns, len(tr.conns))
}

func testingConfig() *Config {
	return &Config{
		MaxOnFlightReqs: 250,
		MaxConns:        55,
		BaseDir:         "./testdata/",
		DisableTrackers: true,
	}
}

var helloWorldTorrentFile = "./testdata/hello_world.torrent"
var helloWorldContents = "Hello World\n"

func TestLoadCompleteTorrent(t *testing.T) {
	cl, err := NewClient(testingConfig())
	require.NoError(t, err)
	tr, err := cl.NewTorrentFromFile(helloWorldTorrentFile)
	require.NoError(t, err)
	assert.Equal(t, true, tr.seeding)
	data := make([]byte, tr.pieceLen(0))
	tr.readBlock(data, 0, 0)
	assert.EqualValues(t, 12, len(data))
	assert.EqualValues(t, helloWorldContents, string(data))
}

func TestSingleFileTorrentTransfer(t *testing.T) {
	testDataTransfer(t, "./testdata/hello_world.torrent", 1)
}

func TestMultiFileTorrentTransfer(t *testing.T) {
	testDataTransfer(t, "./testdata/blockchain.torrent", 5)
}

func testDataTransfer(t *testing.T, torrentFile string, numLeechers int) {
	seeder, err := NewClient(testingConfig())
	require.NoError(t, err)
	seederTr, err := seeder.NewTorrentFromFile(torrentFile)
	require.NoError(t, err)
	assert.True(t, seederTr.seeding)
	require.Equal(t, struct{}{}, <-seederTr.Download())
	dataSeeder := make([]byte, seederTr.length)
	//read whole contents
	err = seederTr.readBlock(dataSeeder, 0, 0)
	require.NoError(t, err)
	//create leechers
	leechers := make([]*Client, numLeechers)
	for i := range leechers {
		leecher, err := NewClient(testingConfig())
		defer leecher.Close()
		leecher.config.BaseDir = "./testdata/leecher" + strconv.Itoa(i)
		defer os.RemoveAll(leecher.config.BaseDir)
		_, err = leecher.NewTorrentFromFile(torrentFile)
		require.NoError(t, err)
		leechers[i] = leecher
	}
	addrs := make([]string, len(leechers))
	for i := range addrs {
		addrs[i] = leechers[i].addr()
	}
	for i, leecher := range leechers {
		leecher.connectToPeers(leecher.Torrents()[0], append(addrs[i+1:], seeder.addr())...)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for _, leecher := range leechers {
		leecherTr := leecher.Torrents()[0]
		<-leecherTr.Download()
		//<-leecherTr.downloadedAll
		/*loop:
		for {
			select {
			case <-leecherTr.downloadedAll:
				fmt.Println("omg", leecherTr.eventsReceived, leecherTr.commandsSent)
				break loop
			case <-ticker.C:
				for _, lchr := range leechers {
					lchr.Torrents()[0].DisplayStats()
					//fmt.Println("omg", nonUsefulRequestReads.Load())
				}
			}
		}*/
		assert.True(t, leecherTr.seeding)
		testContents(t, dataSeeder, leecherTr)
	}
	fmt.Println("nonUsefulRequestReads", nonUsefulRequestReads.Load())
	fmt.Println("lostBlocksDueToSync", lostBlocksDueToSync.Load())
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
		t.Skip("skip test with third party torrent libriaries (anacrolix)")
	}
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
	//
	leecher, err := NewClient(testingConfig())
	require.NoError(t, err)
	leecher.config.BaseDir = "./testdata/leecher"
	defer os.RemoveAll(leecher.config.BaseDir)
	leecherTr, err := leecher.NewTorrentFromFile(torrentFile)
	go leecher.connectToPeer(seeder.ListenAddrs()[0].String(), leecherTr)
	<-leecherTr.Download()
	assert.True(t, leecherTr.seeding)
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
		t.Log("third party reader returned wrong length with err:", err)
	}
	assert.EqualValues(t, n, len(dataSeeder))
	err = leecherTr.readBlock(dataLeecher, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, dataSeeder, dataLeecher)
}

func TestThirdPartySingleFileDataTransfer(t *testing.T) {
	testThirdPartyDataTransfer(t, "./testdata/hello_world.torrent")
}

func TestThirdPartyMultiFileDataTransfer(t *testing.T) {
	testThirdPartyDataTransfer(t, "./testdata/blockchain.torrent")
}

//dummyTracker always responds to announces with the same ip/port pair
type dummyTracker struct {
	peers  []tracker.Peer
	close  chan struct{}
	myAddr string
}

func (dt *dummyTracker) addr() string {
	return "http://" + dt.myAddr + "/announce"
}

type httpAnnounceResponse struct {
	Interval int32          `bencode:"interval"`
	Peers    []tracker.Peer `bencode:"peers" empty:"omit"`
}

func (dt *dummyTracker) announceHandler(w http.ResponseWriter, r *http.Request) {
	bytes, _ := bencode.Encode(httpAnnounceResponse{
		Interval: 1,
		Peers:    dt.peers,
	})
	w.Write(bytes)
}

func (dt *dummyTracker) serve() {
	port := strings.Split(dt.myAddr, ":")[1]
	http.HandleFunc("/announce", dt.announceHandler)
	go func() {
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}()
}

var localhost = "127.0.0.1"

//Test the interaction with a tracker.Check that the announce goes as expected
//and that we dont connect to the same peer multiple times.
func TestTrackerAnnouncer(t *testing.T) {
	cfg := testingConfig()
	cfg.DisableTrackers = false
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	addrSplit := strings.Split(cl.addr(), ":")
	port, err := strconv.Atoi(addrSplit[1])
	require.NoError(t, err)
	dt := &dummyTracker{
		peers: []tracker.Peer{
			//dummy tracker will always respond to announces with the requester's ip/port pair
			tracker.Peer{
				ID:   cl.peerID[:],
				IP:   []byte(localhost),
				Port: uint16(port),
			},
		},
		myAddr: localhost + ":8081",
	}
	dt.serve()
	tr, err := cl.NewTorrentFromFile("./testdata/hello_world.torrent")
	require.NoError(t, err)
	tr.mi.Announce = dt.addr()
	tr.Download()
	//we want to announce multiple times so sleep for a bit
	time.Sleep(20 * time.Second)
	/*time.Sleep(10 * time.Second)
	fmt.Println(tr.numAnnounces)*/
	cl.Close()
	//assert that we filtered duplicate ip/port pairs
	//We should have established only 2 connections (in essence 1 because we have connected
	//to ourselves so there are 2 - one becaused we dialed in `connectToPeer` and that
	//triggered us to accept another one in `handleConn`.)
	assert.Equal(t, 2, len(tr.conns))
}

/*func TestWithOpenTracker(t *testing.T) {
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
//config.
