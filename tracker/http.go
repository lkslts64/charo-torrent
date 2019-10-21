package tracker

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lkslts64/charo-torrent/bencode"
)

type httpAnnounceResponse struct {
	Fail        string     `bencode:"failure reason" empty:"omit"`
	Warning     string     `bencode:"warning message" empty:"omit"`
	Interval    int32      `bencode:"interval"`
	MinInterval int32      `bencode:"min interval" empty:"omit"`
	TrackerID   []byte     `bencode:"tracker id" empty:"omit"`
	Complete    int32      `bencode:"complete" empty:"omit"`
	Incomplete  int32      `bencode:"incomplete" empty:"omit"`
	Peers       []Peer     `bencode:"peers" empty:"omit"`
	CheapPeers  cheapPeers `bencode:"peers" empty:"omit"`
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
	} else if r.CheapPeers != nil {
		r.Peers, err = r.CheapPeers.peers()
		if err != nil {
			return err
		}
		/*var numPeers int
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
			r.Peers[j].Port = port*/
	} else {
		return errors.New("Both Peers and CheapPeers fields are empty")
	}
	//err may only be nil or warning
	return err
}

func (r *httpAnnounceResponse) announceResp() *AnnounceResp {
	return &AnnounceResp{r.Interval, r.Incomplete, r.Complete, r.Peers, r.MinInterval}
}

var events = map[event]string{
	completed: "completed",
	started:   "started",
	stopped:   "stopped",
}

func (t *HTTPTrackerURL) Announce(ctx context.Context, r AnnounceReq) (*AnnounceResp, error) {
	HTTPresp, err := t.announce(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("http announce: %w", err)
	}
	//assign trackerID if its the first time we get it - it was nil before,
	//OR if we already have it and we got a different one from HTTPresponse.
	if id := HTTPresp.TrackerID; id != nil && (t.id == nil || t.id != nil && string(id) != string(t.id)) {
		t.id = id
	}
	return HTTPresp.announceResp(), nil
}

func (t *HTTPTrackerURL) announce(ctx context.Context, r AnnounceReq) (*httpAnnounceResponse, error) {
	//maybe I can make a simple http.Get but leave this here for now.
	//--------------------------------
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := r.buildURL(t.url)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	//--------------------------------
	benData, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var res httpAnnounceResponse
	err = bencode.Decode(benData, &res)
	if err != nil {
		return nil, err
	}
	err = res.parse()
	if err != nil {
		return nil, err
	}
	//HTTP tracker should fil TrackerID field afther return of this func.
	return &res, nil
}

func (r AnnounceReq) buildURL(turl trackerURL) (*url.URL, error) {
	u, err := url.Parse(string(turl))
	if err != nil {
		return nil, err
	}
	u.RawQuery = r.queryValues()
	return u, nil
}

func (r AnnounceReq) queryValues() string {
	v := url.Values{}
	v.Set("info_hash", string(r.InfoHash[:]))
	v.Set("peer_id", string(r.PeerID[:]))
	v.Set("port", strconv.Itoa(int(r.Port)))
	v.Set("uploaded", strconv.Itoa(int(r.Uploaded)))
	v.Set("downloaded", strconv.Itoa(int(r.Downloaded)))
	v.Set("left", strconv.Itoa(int(r.Left)))
	v.Set("compact", "1")
	v.Set("no_peer_id", "1")
	if r.Event != 0 {
		v.Set("event", events[r.Event])
	}
	if r.Numwant != 0 {
		v.Set("numwant", strconv.Itoa(int(r.Numwant)))
	}
	if r.Key != 0 {
		v.Set("key", strconv.Itoa(int(r.Key)))
	}
	return v.Encode()
}

//Same as ScrapeResp but with the addition of Fail.
type httpScrapeResp struct {
	//TODO:string -> [20]byte (must support byte arrays in bencode)
	Files map[string]TorrentInfo `bencode:"files"`
	Fail  string                 `bencode:"failure_reason" empty:"omit"`
}

func (t *HTTPTrackerURL) Scrape(ctx context.Context, infos ...[20]byte) (*ScrapeResp, error) {
	HTTPresp, err := t.scrape(ctx, infos...)
	if err != nil {
		return nil, fmt.Errorf("http scrape: %w", err)
	}
	return HTTPresp.scrapeResponse(), nil
}

func (t *HTTPTrackerURL) scrape(ctx context.Context, infoHashes ...[20]byte) (*httpScrapeResp, error) {
	var s string
	if s = t.url.ScrapeURL(); s == "" {
		//return
	}
	u, err := url.Parse(string(s))
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	for _, info := range infoHashes {
		v.Set("info_hash", string(info[:]))
	}
	u.RawQuery = v.Encode()
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	benData, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var sr httpScrapeResp
	err = bencode.Decode(benData, &sr)
	if err != nil {
		return nil, err
	}
	if err = sr.parse(); err != nil {
		return nil, err
	}
	return &sr, nil
}

func (sr *httpScrapeResp) parse() error {
	if sr.Fail != "" {
		return errors.New("tracker responsed with failure reason: " + sr.Fail)
	}
	for ihash := range sr.Files {
		if len(ihash) != 20 {
			return errors.New("info hash " + ihash + " isn't 20 bytes")
		}
	}
	return nil
}

func (sr *httpScrapeResp) scrapeResponse() *ScrapeResp {
	return &ScrapeResp{sr.Files}
}
