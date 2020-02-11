package storage

import (
	"log"

	"github.com/lkslts64/charo-torrent/metainfo"
)

//Open returns a Storage object
type Open func(mi *metainfo.MetaInfo, baseDir string, blocks []int, logger *log.Logger) (s Storage, seed bool)

//Storage is the interface every storage should adhere to
type Storage interface {
	ReadBlock(b []byte, off int64) (n int, err error)
	WriteBlock(b []byte, off int64) (n int, err error)
	HashPiece(pieceIndex int, len int) (correct bool)
}
