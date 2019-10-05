package tracker

import (
	"context"
	"net"
	"net/url"
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
	Port       int
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
	Announce(AnnounceReq) (*AnnounceResp, error)
	Scrape(...[20]byte) (*ScrapeResp, error)
}

type HTTPTracker struct {
	URL TrackerURL
	ID  []byte
}

func NewHTTPTracker(trackerURL string) *HTTPTracker {
	return &HTTPTracker{URL: TrackerURL(trackerURL)}
}

type UDPTracker struct {
	URL          TrackerURL
	host         string
	conn         net.Conn
	connID       int64
	lastConncted time.Time
	ctx          context.Context
	interval     time.Duration
	lastReq      []byte
}

func NewUDPTracker(ctx context.Context, trackerURL string) (*UDPTracker, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(u.Host, ":") {
		host += ":80"
	}
	return &UDPTracker{URL: TrackerURL(trackerURL), host: host, ctx: ctx}, nil
}
