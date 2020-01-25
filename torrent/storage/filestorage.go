package storage

//Some methods were copied verbatim from anacrolix's torrent package, so
//credits to him.

import (
	"crypto/sha1"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/lkslts64/charo-torrent/metainfo"
)

//Storage is the interface every storage should adhere to
type Storage interface {
	ReadBlock(b []byte, off int64) (n int, err error)
	WriteBlock(b []byte, off int64) (n int, err error)
	HashPiece(pieceIndex int, len int) (correct bool)
}

//FileStorage is a file-based storage for torrent data
type FileStorage struct {
	logger *log.Logger
	//fts    *fileTorrentImpl
	dir    string
	mi     *metainfo.MetaInfo
	pieces []*piece
}

//OpenFileStorage initializes the storage.`blocks` is a slice containing how many
//blocks each piece has. if returns true it means we are ready for seeding
func OpenFileStorage(mi *metainfo.MetaInfo, baseDir string, blocks []int, logger *log.Logger) (fs *FileStorage, seed bool) {
	//Surely we can find `blocks` from metainfo but is expensive.
	pieces := make([]*piece, mi.Info.NumPieces())
	for i := 0; i < len(pieces); i++ {
		pieces[i] = &piece{
			blocks:      blocks[i],
			dirtyBlocks: make(map[int64]struct{}),
		}
	}
	fs = &FileStorage{
		logger: logger,
		mi:     mi,
		dir:    baseDir,
		pieces: pieces,
	}
	seed = fs.dataComplete() && fs.dataVerified()
	return
}

//check that all files have the required size
func (s *FileStorage) dataComplete() bool {
	for _, fi := range s.mi.Info.FilesInfo() {
		s, err := os.Stat(s.fileInfoName(fi))
		if err != nil || s.Size() < int64(fi.Len) {
			return false
		}
	}
	return true
}

//check that all pieces are verified
func (s *FileStorage) dataVerified() bool {
	//TODO:maybe do this concurently
	for i := range s.pieces {
		if !s.hashPiece(i, s.mi.Info.PieceLength(i)) {
			return false
		}
	}
	return true
}

// Returns EOF on short or missing file.
func (s *FileStorage) readFileAt(fi metainfo.File, b []byte, off int64) (n int, err error) {
	f, err := os.Open(s.fileInfoName(fi))
	if os.IsNotExist(err) {
		// File missing is treated the same as a short file.
		err = io.EOF
		return
	}
	if err != nil {
		return
	}
	defer f.Close()
	flen := int64(fi.Len)
	// Limit the read to within the expected bounds of this file.
	if int64(len(b)) > flen-off {
		b = b[:flen-off]
	}
	for off < flen && len(b) != 0 {
		n1, err1 := f.ReadAt(b, off)
		b = b[n1:]
		n += n1
		off += int64(n1)
		if n1 == 0 {
			err = err1
			break
		}
	}
	return
}

//TODO: these meths should not have receiver (be functions)

//returns the piece index that off corresponds to.
func (s *FileStorage) pieceIndex(off int64) int {
	return int(off / int64(s.mi.Info.PieceLen))
}

//returns the offset that `pieceIndex` starts.
func (s *FileStorage) pieceOff(pieceIndex int) int64 {
	return int64(pieceIndex * s.mi.Info.PieceLen)
}

var ErrReadNonVerified = errors.New("storage: trying to read non verified piece")

//ReadBlock is like ReadAt but fails if the piece to be read is not verified
func (s *FileStorage) ReadBlock(b []byte, off int64) (n int, err error) {
	piece := s.pieces[s.pieceIndex(off)]
	if !piece.isVerified() {
		err = ErrReadNonVerified
		return
	}
	return s.ReadAt(b, off)
}

// Only returns EOF at the end of the torrent. Premature EOF is ErrUnexpectedEOF.
func (s *FileStorage) ReadAt(b []byte, off int64) (n int, err error) {
	for _, fi := range s.mi.Info.FilesInfo() {
		flen := int64(fi.Len)
		for off < flen {
			n1, err1 := s.readFileAt(fi, b, off)
			n += n1
			off += int64(n1)
			b = b[n1:]
			if len(b) == 0 {
				// Got what we need.
				return
			}
			if n1 != 0 {
				// Made progress.
				continue
			}
			err = err1
			if err == io.EOF {
				// Lies.
				err = io.ErrUnexpectedEOF
			}
			return
		}
		off -= flen
	}
	err = io.EOF
	return
}

var ErrAlreadyWritten = errors.New("storage: trying to write at already written block")

//WriteBlock behaves like WriteAt but it fails if another write has occured
//at the same offset
func (s *FileStorage) WriteBlock(p []byte, off int64) (n int, err error) {
	piece := s.pieces[s.pieceIndex(off)]
	if !piece.reserveOffset(off) {
		err = ErrAlreadyWritten
		return
	}
	return s.WriteAt(p, off)
}

func (s *FileStorage) WriteAt(p []byte, off int64) (n int, err error) {
	for _, fi := range s.mi.Info.FilesInfo() {
		flen := int64(fi.Len)
		if off >= flen {
			off -= flen
			continue
		}
		n1 := len(p)
		if int64(n1) > flen-off {
			n1 = int(flen - off)
		}
		name := s.fileInfoName(fi)
		os.MkdirAll(filepath.Dir(name), 0777)
		var f *os.File
		f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			return
		}
		n1, err = f.WriteAt(p[:n1], off)
		// TODO: On some systems, write errors can be delayed until the Close.
		f.Close()
		if err != nil {
			return
		}
		n += n1
		off = 0
		p = p[n1:]
		if len(p) == 0 {
			break
		}
	}
	return
}

func (s *FileStorage) fileInfoName(fi metainfo.File) string {
	return filepath.Join(append([]string{s.dir, s.mi.Info.Name}, fi.Path...)...)
}

var ErrNotReadyForVerification = errors.New("storage: not all piece's blocks are written")

//HashPiece hashes `pieceIndex` whose length is `len` and returns if
//the hash was the expected.
func (s *FileStorage) HashPiece(pieceIndex, len int) (correct bool) {
	piece := s.pieces[pieceIndex]
	if piece.isVerified() {
		s.logger.Fatal("storage: piece already verified")
	}
	if !piece.readyForVerification() {
		s.logger.Fatal(ErrNotReadyForVerification)
	}
	return s.hashPiece(pieceIndex, len)
}

func (s *FileStorage) hashPiece(pieceIndex, len int) (correct bool) {
	defer func() {
		piece := s.pieces[pieceIndex]
		if correct {
			piece.markComplete()
		} else {
			piece.markNotComplete()
		}
	}()
	hasher := sha1.New()
	_len := int64(len)
	n, err := io.Copy(hasher, io.NewSectionReader(s, s.pieceOff(pieceIndex), _len))
	if n == _len {
		hash := hasher.Sum(nil)
		actualHash := s.mi.Info.PieceHash(pieceIndex)
		correct = compareHashes(hash, actualHash)
		return
	}
	if err != nil {
		s.logger.Printf("error hasing piece %d\n", pieceIndex)
	}
	return

}

func compareHashes(a, b []byte) bool {
	if a == nil || b == nil {
		panic("expecting non nil hash")
	}
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
