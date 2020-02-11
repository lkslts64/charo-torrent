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
	trackers map[string]tracker.TrackerURL
	close    chan chan struct{}
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
		//TODO:consider not requesting every time 50 peers
		Numwant: 200,
		Port:    t.cl.port,
	}
	url := te.t.mi.Announce
	if _, ok := t.trackers[url]; !ok {
		trackerURL, err := tracker.NewTrackerURL(url)
		if err != nil {
			return nil, err
		}
		t.trackers[url] = trackerURL
	}
	return t.trackers[url].Announce(ctx, req)
}
