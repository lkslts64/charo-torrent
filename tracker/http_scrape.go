package tracker

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/lkslts64/charo-torrent/bencode"
)

//Same as ScrapeResp but with the addition of Fail.
type httpScrapeResp struct {
	//TODO:string -> [20]byte (must support byte arrays in bencode)
	Files map[string]TorrentInfo `bencode:"files"`
	Fail  string                 `bencode:"failure_reason" empty:"omit"`
}

func (t *HTTPTracker) Scrape(infos ...[]byte) (*ScrapeResp, error) {
	HTTPresp, err := t.scrape(infos...)
	if err != nil {
		return nil, fmt.Errorf("http scrape: %w", err)
	}
	return HTTPresp.scrapeResponse(), nil
}

func (t *HTTPTracker) scrape(infoHashes ...[]byte) (*httpScrapeResp, error) {
	var s string
	if s = t.URL.Scrape(); s == "" {
		//return
	}
	u, err := url.Parse(string(s))
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	for _, info := range infoHashes {
		v.Set("info_hash", string(info))
	}
	u.RawQuery = v.Encode()
	resp, err := http.Get(u.String())
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
