package torrent

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/anacrolix/torrent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTorrentNewConnection(t *testing.T) {
	cl, err := NewClient(nil)
	defer os.RemoveAll(cl.config.baseDir)
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
}

func testingConfig() *Config {
	return &Config{
		maxOnFlightReqs: 250,
		maxConns:        55,
		maxUploadSlots:  4,
		optimisticSlots: 1,
		baseDir:         "./testdata/",
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
	/*go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	runtime.SetBlockProfileRate(1)*/
	testDataTransfer(t, "./testdata/blockchain.torrent", 30)
}

func testDataTransfer(t *testing.T, torrentFile string, numLeechers int) {
	seeder, err := NewClient(testingConfig())
	require.NoError(t, err)
	seederTr, err := seeder.NewTorrentFromFile(torrentFile)
	fmt.Println(seederTr.numPieces())
	require.NoError(t, err)
	assert.Equal(t, true, seederTr.seeding)
	dataSeeder := make([]byte, seederTr.length)
	//read whole contents
	err = seederTr.readBlock(dataSeeder, 0, 0)
	require.NoError(t, err)
	//create leechers
	leechers := make([]*Client, numLeechers)
	for i := range leechers {
		leecher, err := NewClient(nil)
		defer leecher.Close()
		leecher.config.baseDir = "./testdata/leecher" + strconv.Itoa(i)
		defer os.RemoveAll(leecher.config.baseDir)
		_, err = leecher.NewTorrentFromFile(torrentFile)
		require.NoError(t, err)
		leechers[i] = leecher
	}
	addrs := make([]string, len(leechers))
	for i := range addrs {
		addrs[i] = leechers[i].addr()
	}
	for i, leecher := range leechers {
		leecher.connectToPeers(leecher.Torrents()[0], append(append(addrs[:i], addrs[i+1:]...), seeder.addr())...)
	}
	for _, leecher := range leechers {
		leecherTr := leecher.Torrents()[0]
		<-leecherTr.downloadedAll
		leecherTr.DisplayStats()
		assert.True(t, leecherTr.seeding)
		testContents(t, dataSeeder, leecherTr)
	}
	seederTr.DisplayStats()
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
	leecher, err := NewClient(nil)
	require.NoError(t, err)
	leecher.config.baseDir = "./testdata/leecher"
	defer os.RemoveAll(leecher.config.baseDir)
	leecherTr, err := leecher.NewTorrentFromFile(torrentFile)
	go leecher.connectToPeer(seeder.ListenAddrs()[0].String(), leecherTr)
	<-leecherTr.downloadedAll
	seeder.WriteStatus(os.Stdout)
	leecherTr.DisplayStats()
	assert.True(t, leecherTr.seeding)
	testContentsThirdParty(t, seederTr, leecherTr)
}

func testContentsThirdParty(t *testing.T, seederTr *torrent.Torrent, leecherTr *Torrent) {
	assert.EqualValues(t, seederTr.Length(), leecherTr.length)
	dataSeeder := make([]byte, seederTr.Length())
	dataLeecher := make([]byte, leecherTr.length)
	r := seederTr.NewReader()
	n, err := r.Read(dataSeeder)
	if err == io.EOF {
		err = nil
	}
	if n != len(dataSeeder) {
		t.Log("third party reader returned wrong length with err:", err)
	}
	require.NoError(t, err)
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

//TODO:test piece verification failure by mocking storage (specifically ReadBlock())
//mock storage by giving user to options to provide its own OpenStorage impl in client
//config.
