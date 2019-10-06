package tracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type event int32

const (
	none event = iota
	completed
	started
	stopped
)

//Omit IP field
type AnnounceReq struct {
	InfoHash [20]byte
	//TODO: pointer to global const peerID.
	PeerID     [20]byte
	Downloaded int64
	Left       int64
	Uploaded   int64
	Event      event
	IP         int32
	Key        int32
	Numwant    int32
	Port       int16
}

type AnnounceResp struct {
	Interval int32
	Leechers int32
	Seeders  int32
	Peers    []Peer
	//if udp tracker then always zero.
	MinInterval int32
}

type ScrapeResp struct {
	//TODO:string -> [20]byte (must support byte arrays in bencode)
	Torrents map[string]TorrentInfo
}

type TorrentInfo struct {
	Complete   int32  `bencode:"complete"`
	Downloaded int32  `bencode:"downloaded"`
	Incomplete int32  `bencode:"incomplete"`
	Name       string `bencode:"name" empty:"omit"`
}

type Peer struct {
	ID   []byte `bencode:"peer id"`
	IP   net.IP `bencode:"ip"`
	Port int    `bencode:"port"`
}

type cheapPeers []byte

func (cheap cheapPeers) peers() ([]Peer, error) {
	var numPeers int
	var ip net.IP
	if numPeers = len(cheap); numPeers%6 != 0 {
		return nil, errors.New(fmt.Sprintf("cheapPeers length is not divided exactly by 6.Instead it has length %d", numPeers))
	}
	peers := make([]Peer, numPeers/6)
	j := 0
	for i := 0; i < numPeers; i += 6 {
		j = i / 6
		if ip = net.IP([]byte(cheap[i : i+4])).To16(); ip == nil {
			return nil, errors.New("cheapPeers ip parse")
		}
		port, err := strconv.Atoi(string(cheap[i+4 : i+6]))
		if err != nil {
			return nil, fmt.Errorf("cheapPeers port parse: %w", err)
		}
		peers[j].IP = ip
		peers[j].Port = port
	}
	return peers, nil
}

type TrackerURL string

func (u TrackerURL) Scrape() string {
	const s = "announce"
	var i int
	if i = strings.LastIndexByte(string(u), '/'); i < 0 {
		return ""
	}
	if len(u) < i+1+len(s) {
		return ""
	}
	if u[i+1:i+len(s)+1] != s {
		return ""
	}
	return string(u[:i+1]) + "scrape" + string(u[i+len(s)+1:])
}

type Tracker interface {
	Announce(context.Context, AnnounceReq) (*AnnounceResp, error)
	Scrape(context.Context, ...[20]byte) (*ScrapeResp, error)
}

func NewTracker(trackerURL string) (Tracker, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https":
		return &HTTPTracker{URL: TrackerURL(trackerURL)}, nil
	case "udp":
		return &UDPTracker{URL: TrackerURL(trackerURL), host: addPortMaybe(u.Host)}, nil
	default:
		return nil, errors.New("err bad scheme")
	}
}

type HTTPTracker struct {
	URL TrackerURL
	ID  []byte
}

type UDPTracker struct {
	URL          TrackerURL
	host         string
	conn         net.Conn
	connID       int64
	lastConncted time.Time
	//ctx                 context.Context
	//interval            time.Duration
	consecutiveTimeouts int
}

func addPortMaybe(host string) string {
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	return host

}
