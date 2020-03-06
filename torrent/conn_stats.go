package torrent

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
)

type connStats struct {
	uploadUsefulBytes   int //TODO:uint64
	downloadUsefulBytes int //TODO:uint64
	blocksDownloaded    int
	blocksUploaded      int
	//how many pieces this conn has verified (we send verifications jobs to conns).
	verifications int
	//initally it holds the time that we first got in `downloading` state
	lastReceivedPieceMsg time.Time
	//last time we were interested and peer was unchoking
	lastStartedDownloading time.Time
	//last time we were unchoking and peer was interested
	lastStartedUploading time.Time
	//duration we are in downloading state
	sumDownloading time.Duration
	//duration we are in uploading state
	sumUploading            time.Duration
	snubbed                 bool
	badPiecesContributions  int
	goodPiecesContributions int
}

func (cs *connStats) stopDownloading() {
	cs.sumDownloading += time.Since(cs.lastStartedDownloading)
}

func (cs *connStats) stopUploading() {
	cs.sumUploading += time.Since(cs.lastStartedUploading)
}

func (cs *connStats) onBlockDownload(len int) {
	cs.downloadUsefulBytes += len
	cs.blocksDownloaded++
	cs.lastReceivedPieceMsg = time.Now()
}

func (cs *connStats) onBlockUpload(len int) {
	cs.blocksUploaded++
	cs.uploadUsefulBytes += len
}

func (cs *connStats) onPieceHashed() {
	cs.verifications++
}

//Deprecated
func (cs *connStats) uploadLimitsReached() bool {
	//if we have uploaded 200KiB more than downloaded (anacrolix has 100)
	return cs.uploadUsefulBytes-cs.downloadUsefulBytes > (1<<10)*200
}

func (cs *connStats) isSnubbed() bool {
	if cs.snubbed {
		return true
	}
	cs.snubbed = (!cs.lastReceivedPieceMsg.IsZero() && time.Since(cs.lastReceivedPieceMsg) >= time.Minute)
	return cs.snubbed
}

func (cs *connStats) malliciousness() int {
	return cs.badPiecesContributions - cs.goodPiecesContributions
}

func (cs *connStats) String() string {
	return fmt.Sprintf(`bytes downloaded: %s
	bytes uploaded: %s
	blocks downloaded: %d 
	blocks uploaded: %d
	#verifications: %d`, humanize.Bytes(uint64(cs.downloadUsefulBytes)),
		humanize.Bytes(uint64(cs.uploadUsefulBytes)),
		cs.blocksDownloaded,
		cs.blocksUploaded,
		cs.verifications)
}
