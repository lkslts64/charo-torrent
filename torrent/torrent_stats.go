package torrent

import (
	"fmt"

	"go.uber.org/atomic"
)

//these are cancels that peers send to us but it was too late because
//we had already processed the request
var latecomerCancels atomic.Uint32

var duplicateBlocksReceived atomic.Uint32

var duplicateRequestsReceived atomic.Uint32

var nonUsefulRequestReads atomic.Uint64

var sendRequestWhenChoked atomic.Uint64

var lostBlocksDueToSync atomic.Uint64

//Stats contains statistics about a Torrent
type Stats struct {
	//Number of blocks/chunks downloaded (not necessarily verified)
	BlocksDownloaded int
	//Number of blocks/chunks uploaded
	BlocksUploaded int
	//Remainings bytes to download (bytes that are downloaded but not verified are not included)
	BytesLeft int
	//Number of verified bytes we have downloaded
	BytesDownloaded int
	//Number of bytes we have uploaded
	BytesUploaded int
}

func (s *Stats) blockDownloaded(bytes int) {
	s.BlocksDownloaded++
}

func (s *Stats) blockUploaded(bytes int) {
	s.BytesUploaded += bytes
	s.BlocksUploaded++
}

func (s *Stats) onPieceDownload(bytes int) {
	s.BytesDownloaded += bytes
	s.BytesLeft -= bytes
}

func (s *Stats) String() string {
	return fmt.Sprintf(`blocks downloaded: %d,blocks uploaded: %d\n,
	bytes left: %d,bytes downloaded %d,bytes uploaded: %d\n`, s.BlocksDownloaded,
		s.BlocksUploaded, s.BytesLeft, s.BytesDownloaded, s.BytesUploaded)

}
