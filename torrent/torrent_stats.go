package torrent

import (
	"fmt"

	"go.uber.org/atomic"
)

//these are cancels that peers send to us but it was too late because
//we had already processed the request
var latecomerCancels atomic.Uint32

//Stats contains statistics about a Torrent
type Stats struct {
	BlocksDownloaded int
	BlocksUploaded   int
	//Remainings bytes to download
	BytesLeft int
	//Bytes we have downloaded and verified
	BytesDownloaded int
	//Bytes we have uploaded
	BytesUploaded int
}

func (s *Stats) blockDownloaded(bytes int) {
	s.BytesDownloaded += bytes
	s.BytesLeft -= bytes
	s.BlocksDownloaded++
}

func (s *Stats) blockUploaded(bytes int) {
	s.BytesUploaded += bytes
	s.BlocksUploaded++
}

func (s *Stats) String() string {
	return fmt.Sprintf(`blocks downloaded: %d,blocks uploaded: %d\n,
	bytes left: %d,bytes downloaded %d,bytes uploaded: %d\n`, s.BlocksDownloaded,
		s.BlocksUploaded, s.BytesLeft, s.BytesDownloaded, s.BytesUploaded)

}
