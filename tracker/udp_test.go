package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var udpTrackers = []string{
	"udp://tracker.coppersurfer.tk:6969",
	"udp://tracker.leechers-paradise.org:6969",
}

var ihash = [20]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

func TestWriteBinary(t *testing.T) {
	var buf bytes.Buffer
	req := AnnounceReq{ihash, [20]byte{}, 6881, 0, 0, 2, 0, 89789, 48, 6981}
	var i int32 = 5
	err := writeBinary(&buf, req, i)
	require.NoError(t, err)
	assert.EqualValues(t, buf.Len(), binary.Size(req)+4)
	ihashBytes := make([]byte, 20)
	buf.Read(ihashBytes)
	assert.EqualValues(t, ihashBytes, ihash[:])
	//test slice
	buf.Reset()
	var ihashes = [][20]byte{ihash, [20]byte{2: 1, 4: 5}}
	err = writeBinary(&buf, ihashes)
	require.NoError(t, err)
	assert.EqualValues(t, buf.Len(), 40)
	assert.EqualValues(t, buf.Bytes(), append(ihashes[0][:], ihashes[1][:]...))
}

func TestReadBinary(t *testing.T) {
	fixedData := udpScrapeInfo{}
	buf := bytes.NewBuffer([]byte{0x0, 0x0, 0x0, 0x0, 0x1, 0x1, 0x1, 0x7, 0x2, 0x2, 0x2, 0x2})
	err := readFromBinary(buf, &fixedData)
	require.NoError(t, err)
	assert.EqualValues(t, fixedData, udpScrapeInfo{0x0, 0x01010107, 0x02020202})
	//test slice
	fixedData = udpScrapeInfo{1, 2, 3}
	slc := []byte{0x0, 0x0, 0x0, 0x1, 0x0, 0x2, 0x0, 0x3, 0x0, 0x4, 0x0, 0x5}
	slc = append(slc, slc...)
	buf = bytes.NewBuffer(slc)
	slcFixedData := make([]udpScrapeInfo, 2)
	err = readFromBinary(buf, &slcFixedData)
	require.NoError(t, err)
	assert.EqualValues(t, slcFixedData, []udpScrapeInfo{{0x1, 0x020003, 0x040005}, {0x1, 0x020003, 0x040005}})
}

func TestTimeoutTime(t *testing.T) {
	tr, err := NewTrackerURL("udp://lol.omg")
	require.NoError(t, err)
	udpTr := tr.(*UDPTrackerURL)
	assert.EqualValues(t, udpTr.timeoutTime(), 15*time.Second)
	udpTr.consecutiveTimeouts = 5
	assert.EqualValues(t, udpTr.timeoutTime(), time.Duration(math.Pow(2, 5))*15*time.Second)
	udpTr.consecutiveTimeouts = 10
	assert.EqualValues(t, udpTr.timeoutTime(), -1)
}

func TestAnnounceLocalhost(t *testing.T) {
	t.Parallel()
	srv := server{
		t: map[[20]byte]torrent{
			{0xa3, 0x56, 0x41, 0x43, 0x74, 0x23, 0xe6, 0x26, 0xd9, 0x38, 0x25, 0x4a, 0x6b, 0x80, 0x49, 0x10, 0xa6, 0x67, 0xa, 0xc1}: {
				stats: udpScrapeInfo{
					10, 20, 5,
				},
				Peers: []portIP{
					{1, 2},
					{3, 4},
					{5, 6},
				},
			},
			{0xf3, 0x56, 0x41, 0x43, 0x74, 0x23, 0xe6, 0x26, 0xd9, 0x38, 0x25, 0x4a, 0x6b, 0x80, 0x49, 0x10, 0xa6, 0x67, 0xa, 0xc1}: {
				stats: udpScrapeInfo{
					3429, 9482, 555,
				},
				Peers: []portIP{
					{423423, 4329},
					{8, 9994},
					{9, 9982},
				},
			},
		},
	}
	var err error
	srv.pc, err = net.ListenPacket("udp", ":0")
	require.NoError(t, err)
	defer srv.pc.Close()
	go func() {
		require.NoError(t, srv.serveOne())
	}()
	req := AnnounceReq{
		Numwant: -1,
		Event:   Started,
	}
	rand.Read(req.PeerID[:])
	copy(req.InfoHash[:], []byte{0xa3, 0x56, 0x41, 0x43, 0x74, 0x23, 0xe6, 0x26, 0xd9, 0x38, 0x25, 0x4a, 0x6b, 0x80, 0x49, 0x10, 0xa6, 0x67, 0xa, 0xc1})
	go func() {
		require.NoError(t, srv.serveOne())
	}()
	tr, err := NewTrackerURL(fmt.Sprintf("udp://%s/announce", srv.pc.LocalAddr().String()))
	require.NoError(t, err)
	aresp, err := tr.Announce(context.Background(), req)
	require.NoError(t, err)
	assert.EqualValues(t, 5, aresp.Leechers)
	assert.EqualValues(t, 10, aresp.Seeders)
	assert.EqualValues(t, 3, len(aresp.Peers))
	go func() {
		require.NoError(t, srv.serveOne())
	}()
	ihashes := [][20]byte{
		{0xa3, 0x56, 0x41, 0x43, 0x74, 0x23, 0xe6, 0x26, 0xd9, 0x38, 0x25, 0x4a, 0x6b, 0x80, 0x49, 0x10, 0xa6, 0x67, 0xa, 0xc1},
		{0xf3, 0x56, 0x41, 0x43, 0x74, 0x23, 0xe6, 0x26, 0xd9, 0x38, 0x25, 0x4a, 0x6b, 0x80, 0x49, 0x10, 0xa6, 0x67, 0xa, 0xc1},
	}
	sresp, err := tr.Scrape(context.Background(), ihashes...)
	require.NoError(t, err)
	assert.EqualValues(t, 2, len(sresp.Torrents))
	if _, ok := sresp.Torrents[string(ihashes[0][:])]; ok != true {
		t.Fail()
	}
	info, ok := sresp.Torrents[string(ihashes[1][:])]
	if ok != true {
		t.Fail()
	}
	assert.EqualValues(t, 3429, info.Seeders)
	assert.EqualValues(t, 555, info.Leechers)
	assert.EqualValues(t, 9482, info.Downloaded)
}

func TestAnnounceRandomInfoHashThirdParty(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		// This test involves contacting third party servers that may have
		// unpredictable results.
		t.SkipNow()
	}
	req := AnnounceReq{
		Event: Stopped,
	}
	rand.Read(req.PeerID[:])
	rand.Read(req.InfoHash[:])
	wg := sync.WaitGroup{}
	//ctx, cancel := context.WithCancel(context.Background())
	for _, url := range udpTrackers {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			tr, err := NewTrackerURL(url)
			require.NoError(t, err)
			resp, err := tr.Announce(context.Background(), req)
			if err != nil {
				t.Logf("error announcing to %s: %s", url, err)
				return
			}
			if resp.Leechers != 0 || resp.Seeders != 0 || len(resp.Peers) != 0 {
				// The info hash we generated was random in 2^160 space. If we
				// get a hit, something is weird.
				t.Fatal(resp)
			}
			t.Logf("announced to %s", url)
		}(url)
	}
	wg.Wait()
}
