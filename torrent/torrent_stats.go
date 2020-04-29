package torrent

import (
	"fmt"
)

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
