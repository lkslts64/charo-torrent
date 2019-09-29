package tracker

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lkslts64/charo-torrent/bencode"
)

//Omit IP param
type HTTPAnnounceReqParams struct {
	InfoHash [20]byte
	//TODO: pointer to global const peerID.
	PeerID     [20]byte
	Port       int
	Uploaded   int
	Downloaded int
	Left       int
	Compact    bool
	NoPeerID   bool
	Event      string
	Numwant    int
	Key        []byte
	TrackerID  []byte
}

func (r HTTPAnnounceReqParams) RequestTracker(announceURL string) error {
	//maybe I can make a simple http.Get but leave this here for now.
	//--------------------------------
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, err := r.buildURL(announceURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	//--------------------------------
	benData, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var res HTTPAnnounceResponse
	err = bencode.Decode(benData, &res)
	if err != nil {
		return err
	}
	return nil
}

func (r HTTPAnnounceReqParams) buildURL(announceURL string) (*url.URL, error) {
	u, err := url.Parse(announceURL)
	if err != nil {
		return nil, err
	}
	u.RawQuery = r.QueryValues()
	return u, nil
}

func (r HTTPAnnounceReqParams) QueryValues() string {
	v := url.Values{}
	v.Set("info_hash", string(r.InfoHash[:]))
	v.Set("peer_id", string(r.PeerID[:]))
	v.Set("port", strconv.Itoa(r.Port))
	v.Set("uploaded", strconv.Itoa(r.Uploaded))
	v.Set("downloaded", strconv.Itoa(r.Downloaded))
	v.Set("left", strconv.Itoa(r.Left))
	v.Set("compact", strconv.FormatBool(r.Compact))
	v.Set("no_peer_id", strconv.FormatBool(r.NoPeerID))
	if r.Event != "" {
		v.Set("event", r.Event)
	}
	if r.Numwant != 0 {
		v.Set("numwant", strconv.Itoa(r.Numwant))
	}
	if r.Key != nil {
		v.Set("key", string(r.Key))
	}
	if r.TrackerID != nil {
		v.Set("trackerid", string(r.TrackerID))
	}
	return v.Encode()
}
