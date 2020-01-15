package torrent

import "go.uber.org/atomic"

//these are cancels that peers send to us but it was too late because
//we had already processed the request
var latecomerCancels atomic.Uint32

//TorrentStats contains statistics about a Torrent
type TorrentStats struct {
	//Remainings bytes to download
	Left int
	//Bytes we have downloaded and verified
	Downloaded int
	//Bytes we have uploaded
	Uploaded int
}

func (ts *TorrentStats) addDownloaded(bytes int) {
	ts.Downloaded += bytes
	ts.Left -= bytes
}

func (ts *TorrentStats) addUploaded(bytes int) {
	ts.Uploaded += bytes
}
