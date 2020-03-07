package torrent

import (
	"context"
	"time"

	"github.com/lkslts64/charo-torrent/tracker"
)

type trackerAnnouncer struct {
	//Every trackerAnnouncer is associated with a Client - maybe not mandatory
	cl *Client
	//All Torrents send events to tracker via this chan
	trackerAnnouncerSubmitEventCh chan trackerAnnouncerEvent
	//the trackerURLs that we maintain
	trackers map[string]*trackerURL
	close    chan chan struct{}
}

//wrapper for tracker.TrackerURL
type trackerURL struct {
	tu           tracker.TrackerURL
	numAnnounces int
}

func newTrackerURL(url string) (*trackerURL, error) {
	tu, err := tracker.NewTrackerURL(url)
	if err != nil {
		return nil, err
	}
	return &trackerURL{
		tu: tu,
	}, nil
}

func (tu *trackerURL) Announce(ctx context.Context, req tracker.AnnounceReq) (*tracker.AnnounceResp, error) {
	if req.Event == tracker.None && tu.numAnnounces == 0 {
		req.Event = tracker.Started
	}
	tu.numAnnounces++
	return tu.tu.Announce(ctx, req)
}

//The logic is simple but if we start supporting AnnounceList extension then it 'll be more
//interesting.
func (t *trackerAnnouncer) run() {
	for {
		select {
		case te := <-t.trackerAnnouncerSubmitEventCh:
			resp, err := t.announce(te)
			te.t.trackerAnnouncerResponseCh <- trackerAnnouncerResponse{
				resp: resp,
				err:  err,
			}
		case <-t.cl.close:
			//TODO:send stopped msgs to trackers?
			return
		}
	}
}

func (t *trackerAnnouncer) announce(te trackerAnnouncerEvent) (*tracker.AnnounceResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := tracker.AnnounceReq{
		InfoHash:   te.t.mi.Info.Hash,
		PeerID:     t.cl.peerID,
		Downloaded: int64(te.stats.BytesDownloaded),
		Left:       int64(te.stats.BytesLeft),
		Uploaded:   int64(te.stats.BytesUploaded),
		Event:      te.event,
		Numwant:    200,
		Port:       int16(t.cl.port),
	}
	url := te.t.mi.Announce
	if _, ok := t.trackers[url]; !ok {
		trackerURL, err := newTrackerURL(url)
		if err != nil {
			return nil, err
		}
		t.trackers[url] = trackerURL
	}
	return t.trackers[url].Announce(ctx, req)
}
