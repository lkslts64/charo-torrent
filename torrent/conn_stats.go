package torrent

import "time"

type connStats struct {
	uploadUseful   int
	downloadUseful int
	//initally it holds the time that we first got in `downloading` state
	lastPieceMsg time.Time
	//last time we were interested and peer was unchoking
	lastStartedDownloading time.Time
	//last time we were unchoking and peer was interested
	lastStartedUploading time.Time
	//duration we are in downloading state
	sumDownloading time.Duration
	//duration we are in uploading state
	sumUploading            time.Duration
	badPiecesContributions  int
	goodPiecesContributions int
}

func newConnStats() *connStats {
	return &connStats{
		lastPieceMsg: time.Now(),
	}
}

func (cs *connStats) stopDownloading() {
	cs.sumDownloading += time.Since(cs.lastStartedDownloading)
}

func (cs *connStats) stopUploading() {
	cs.sumUploading += time.Since(cs.lastStartedUploading)
}

func (cs *connStats) onBlockDownload(len int) {
	cs.downloadUseful += len
	cs.lastPieceMsg = time.Now()
}

func (cs *connStats) onBlockUpload(len int) {
	cs.uploadUseful += len
}

func (cs *connStats) uploadLimitsReached() bool {
	//if we have uploaded 200KiB more than downloaded (anacrolix has 100)
	return cs.uploadUseful-cs.downloadUseful > (1<<10)*200
}

func (cs *connStats) isSnubbed() bool {
	return cs.uploadLimitsReached() || time.Since(cs.lastPieceMsg) >= time.Minute
}

func (cs *connStats) malliciousness() int {
	return cs.badPiecesContributions - cs.goodPiecesContributions
}
