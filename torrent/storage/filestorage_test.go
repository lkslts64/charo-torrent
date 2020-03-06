package storage

import (
	"crypto/sha1"
	"io/ioutil"
	"log"
	"os"
	"testing"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorage(t *testing.T) {
	info := &metainfo.InfoDict{
		Name: "test_storage", //trying to be malicious
		//all pieces have the same length (500)
		Files: []metainfo.File{
			{
				Len:  998,
				Path: []string{"first/test"},
			},
			{
				Len:  5002,
				Path: []string{"second/test"},
			},
		},
		PieceLen: 500,
		Pieces:   make([]byte, 20*((998+5002)/500)),
	}
	td, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(td)
	blocks := make([]int, len(info.Pieces))
	//four blocks per piece
	blockSize := 125
	for i := range blocks {
		blocks[i] = info.PieceLen / blockSize
	}
	s, seeding := OpenFileStorage(&metainfo.MetaInfo{
		Info: info,
	}, td, blocks, log.New(os.Stdout, "storage", log.LstdFlags))
	fs := s.(*FileStorage)
	assert.False(t, seeding)
	piece := 1
	testParallelWrites(t, fs, blockSize, piece)
	//try to read one block before verification
	b := make([]byte, blockSize)
	n, err := fs.ReadBlock(b, int64(piece*fs.mi.Info.PieceLen))
	assert.Equal(t, ErrReadNonVerified, err)
	assert.Equal(t, 0, n)
	testHashPiece(t, fs, piece)
	//try to read after verification
	//
	//fill the buffer with something else than zeros because we expect to be
	//filled with zeros after reading from storage.
	for i := 0; i < len(b); i++ {
		b[i] = byte(i + 1)
	}
	n, err = fs.ReadBlock(b, int64(piece*fs.mi.Info.PieceLen))
	assert.NoError(t, err)
	assert.Equal(t, len(b), n)
	assert.Equal(t, byte(0), b[0])
}

func testParallelWrites(t *testing.T, s *FileStorage, blockSize, piece int) {
	var (
		err2 error
		n2   int
	)
	ch1, ch2 := make(chan struct{}), make(chan struct{})
	//write at 5th piece at its first block
	off := int64(piece * s.mi.Info.PieceLen)
	block := make([]byte, blockSize)
	//check that if we write at the same offset, then only one write can succeed.
	go func() {
		n2, err2 = s.WriteBlock(block, int64(off))
		close(ch1)
	}()
	//but writing in different offset should succeed
	go func() {
		//write all blocks of piece except of the first one
		for i := 1; i < s.pieces[s.pieceIndex(off)].blocks; i++ {
			n3, err3 := s.WriteBlock(block, off+int64(i*blockSize))
			assert.Equal(t, n3, len(block))
			assert.NoError(t, err3)
		}
		close(ch2)
	}()
	n, err := s.WriteBlock(block, int64(off))
	<-ch1
	<-ch2
	switch {
	case n == 0 && n2 == len(block):
		assert.NoError(t, err2)
		assert.Equal(t, ErrAlreadyWritten, err)
	case n2 == 0 && n == len(block):
		assert.NoError(t, err)
		assert.Equal(t, ErrAlreadyWritten, err2)
	default:
		t.Fail()
	}
}

func testHashPiece(t *testing.T, s *FileStorage, piece int) {
	//find the actual hash of the piece and set it
	_hash := sha1.Sum(make([]byte, s.mi.Info.PieceLen))
	hash := _hash[:]
	for i := piece * 20; i < piece*20+20; i++ {
		s.mi.Info.Pieces[i] = hash[i-piece*20]
	}
	s.HashPiece(piece, s.mi.Info.PieceLen)
	assert.Equal(t, true, s.pieces[piece].verified)
	assert.Equal(t, 0, len(s.pieces[piece].dirtyBlocks))
}
