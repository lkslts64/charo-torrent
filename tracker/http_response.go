package tracker

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

type httpAnnounceResponse struct {
	Fail        string `bencode:"failure reason" empty:"omit"`
	Warning     string `bencode:"warning message" empty:"omit"`
	Interval    int32  `bencode:"interval"`
	MinInterval int32  `bencode:"min interval" empty:"omit"`
	TrackerID   []byte `bencode:"tracker id" empty:"omit"`
	Complete    int32  `bencode:"complete" empty:"omit"`
	Incomplete  int32  `bencode:"incomplete" empty:"omit"`
	Peers       []Peer `bencode:"peers" empty:"omit"`
	CheapPeers  string `bencode:"peers" empty:"omit"`
}

//Parse checks if the tracker's response contained
//any errors or if it's valid. If CheapPeers is set,
//then Peers struct is filled with the data from
//CheapPeers.So, at the end, Peers field shall not be
//empty.
func (r *httpAnnounceResponse) parse() error {
	var err error
	if r.Fail != "" {
		return fmt.Errorf("tracker response fail: %w", errors.New(r.Fail))
	}
	if r.Warning != "" {
		return fmt.Errorf("tracker response warning: %w", errors.New(r.Warning))
		//err = fmt.Errorf("tracker warning: %w", errors.New(r.Warning))
		//TODO: Create a warning message - caller should be able to determine if err is warining.
		//or Println the warning.
	}
	if r.Peers != nil {
		var ip net.IP
		for i := range r.Peers {
			if len(r.Peers[i].ID) != 20 {
				return errors.New("one of the peer's IDs is not 20 exactly bytes long")
			}
			if ip = net.ParseIP(string(r.Peers[i].IP)); ip == nil {
				var ips []net.IP
				if ips, err = net.LookupIP(string(r.Peers[i].IP)); err != nil {
					return fmt.Errorf("IP parse error (neither an IPv4/6 nor a DNS name) at peer with ID %s : %w", string(r.Peers[i].ID), err)
				}
				r.Peers[i].IP = ips[0]
			} else {
				r.Peers[i].IP = ip
			}
		}
	} else if r.CheapPeers != "" {
		var numPeers int
		var ip net.IP
		if numPeers = len(r.CheapPeers); numPeers%6 != 0 {
			return errors.New(fmt.Sprintf("cheapPeers length is not divided exactly by 6.Instead it has length %d", numPeers))
		}
		r.Peers = make([]Peer, numPeers/6)
		j := 0
		for i := 0; i < numPeers; i += 6 {
			j = i / 6
			if ip = net.IP([]byte(r.CheapPeers[i : i+4])).To16(); ip == nil {
				return errors.New("cheapPeers ip parse")
			}
			r.Peers[j].IP = ip
			port, err := strconv.Atoi(r.CheapPeers[i+4 : i+6])
			if err != nil {
				return fmt.Errorf("cheapPeers port parse: %w", err)
			}
			r.Peers[j].Port = port
		}
	} else {
		return errors.New("Both Peers and CheapPeers fields are empty")
	}
	//err may only be nil or warning
	return err
}

func (r *httpAnnounceResponse) announceResp() *AnnounceResp {
	return &AnnounceResp{r.Interval, r.Incomplete, r.Complete, r.Peers, r.MinInterval}
}
