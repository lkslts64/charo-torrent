package tracker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

type Event int32

const (
	None Event = iota
	Completed
	Started
	Stopped
)

type AnnounceReq struct {
	InfoHash   [20]byte
	PeerID     [20]byte
	Downloaded int64
	Left       int64
	Uploaded   int64
	Event      Event
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
	Seeders    int32  `bencode:"complete"`
	Downloaded int32  `bencode:"downloaded"`
	Leechers   int32  `bencode:"incomplete"`
	Name       string `bencode:"name" empty:"omit"`
}

type Peer struct {
	ID   []byte `bencode:"peer id"`
	IP   net.IP `bencode:"ip"`
	Port uint16 `bencode:"port"`
}

type cheapPeers []byte

func (cheap cheapPeers) peers() ([]Peer, error) {
	var numPeers int
	var ip net.IP
	if numPeers = len(cheap); numPeers%6 != 0 {
		return nil, fmt.Errorf("cheapPeers length is not divided exactly by 6.Instead it has length %d", numPeers)
	}
	peers := make([]Peer, numPeers/6)
	j := 0
	for i := 0; i < numPeers; i += 6 {
		j = i / 6
		if ip = net.IP([]byte(cheap[i : i+4])).To16(); ip == nil {
			return nil, errors.New("cheapPeers ip parse")
		}
		peers[j].Port = binary.BigEndian.Uint16(cheap[i+4 : i+6])
		peers[j].IP = ip
	}
	return peers, nil
}

type trackerURL string

func (u trackerURL) ScrapeURL() string {
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

type TrackerURL interface {
	Announce(context.Context, AnnounceReq) (*AnnounceResp, error)
	Scrape(context.Context, ...[20]byte) (*ScrapeResp, error)
}

func NewTrackerURL(tURL string) (TrackerURL, error) {
	u, err := url.Parse(tURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https":
		return &HTTPTrackerURL{url: trackerURL(tURL)}, nil
	case "udp":
		return &UDPTrackerURL{url: trackerURL(tURL), host: addPortMaybe(u.Host)}, nil
	default:
		return nil, errors.New("err bad scheme")
	}
}

type HTTPTrackerURL struct {
	url trackerURL
	id  []byte
}

type UDPTrackerURL struct {
	url          trackerURL
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
