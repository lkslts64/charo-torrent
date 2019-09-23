package tracker

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

type Response struct {
	Fail        string `bencode:"failure reason" empty:"omit"`
	Warning     string `bencode:"warning message" empty:"omit"`
	Interval    int    `bencode:"interval"`
	MinInterval int    `bencode:"min interval" empty:"omit"`
	TrackerID   string `bencode:"tracker id" empty:"omit"`
	Complete    int    `bencode:"complete" empty:"omit"`
	Incomplete  int    `bencode:"incomplete" empty:"omit"`
	Peers       []Peer `bencode:"peers" empty:"omit"`
	CheapPeers  string `bencode:"peers" empty:"omit"`
}

type Peer struct {
	ID   []byte `bencode:"peer id"`
	IP   net.IP `bencode:"ip"`
	Port int    `bencode:"port"`
}

//Parse checks if the tracker's response contained
//any errors or if it's valid. If CheapPeers is set,
//then Peers struct is filled with the data from
//CheapPeers.So, at the end, Peers field shall not be
//empty.
func (r *Response) Parse() error {
	var err error
	if r.Fail != "" {
		return fmt.Errorf("tracker failure: %w", errors.New(r.Fail))
	}
	if r.Warning != "" {
		return fmt.Errorf("tracker warning: %w", errors.New(r.Warning))
	}
	if r.Peers != nil {
		var ip net.IP
		for _, peer := range r.Peers {
			if len(peer.ID) != 20 {
				return errors.New("one of the peer's IDs is not 20 exactly bytes long")
			}
			if ip = net.ParseIP(string(peer.IP)); ip == nil {
				var ips []net.IP
				if ips, err = net.LookupIP(string(peer.IP)); err != nil {
					return fmt.Errorf("IP parse error (neither an IPv4/6 nor a DNS name) at peer with ID %s : %w", string(peer.ID), err)
				}
				peer.IP = ips[0]
			} else {
				peer.IP = ip
			}
		}
	} else if r.CheapPeers != "" {
		var numPeers int
		if numPeers = len(r.CheapPeers); numPeers%6 != 0 {
			return errors.New(fmt.Sprintf("cheapPeers length is not divided exactly by 6.Instead it has length %d", numPeers))
		}
		r.Peers = make([]Peer, numPeers/6)
		j := 0
		for i := 0; i < numPeers; i += 6 {
			j = i / 6
			r.Peers[j].IP = []byte(r.CheapPeers[i : i+4])
			port, err := strconv.Atoi(r.CheapPeers[i+4 : i+6])
			if err != nil {
				return fmt.Errorf("cheapPeers port parse: %w", err)
			}
			r.Peers[j].Port = port
		}
	} else {
		return errors.New("Both Peers and CheapPeers fields are empty")
	}
	return nil
}
