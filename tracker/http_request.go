package tracker

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lkslts64/charo-torrent/bencode"
)

var events = map[event]string{
	completed: "completed",
	started:   "started",
	stopped:   "stopped",
}

func (t *HTTPTracker) Announce(r AnnounceReq) (*AnnounceResp, error) {
	HTTPresp, err := t.announce(r)
	if err != nil {
		return nil, fmt.Errorf("http announce: %w", err)
	}
	//assign trackerID if its the first time we get it - it was nil before,
	//OR if we already have it and we got a different one from HTTPresponse.
	if id := HTTPresp.TrackerID; id != nil && (t.ID == nil || t.ID != nil && string(id) != string(t.ID)) {
		t.ID = id
	}
	return HTTPresp.announceResp(), nil
}

func (t *HTTPTracker) announce(r AnnounceReq) (*httpAnnounceResponse, error) {
	//maybe I can make a simple http.Get but leave this here for now.
	//--------------------------------
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, err := r.buildURL(t.URL)
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

func (r AnnounceReq) buildURL(trackerURL TrackerURL) (*url.URL, error) {
	u, err := url.Parse(string(trackerURL))
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
	v.Set("port", strconv.Itoa(r.Port))
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
