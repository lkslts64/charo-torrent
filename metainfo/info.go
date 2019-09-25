package metainfo

import (
	"crypto/sha1"
	"errors"
	"fmt"

	"github.com/lkslts64/charo-torrent/bencode"
)

//InfoDict contains all the basic information about
//about the files that the .torrent file is mentioning.
type InfoDict struct {
	Files    []File `bencode:"files" empty:"omit"`
	Len      int    `bencode:"length" empty:"omit"`
	Md5      []byte `bencode:"md5sum" empty:"omit"`
	Name     string `bencode:"name" empty:"omit"`
	PieceLen int    `bencode:"piece length"`
	Pieces   []byte `bencode:"pieces"`
	Private  int    `bencode:"private" empty:"omit"`
	//store info hash - we dont want to compute it every time
	Hash [20]byte `bencode:"-"`
}

//File contains information about a specific file
//in a .torrent file.
type File struct {
	Len  int      `bencode:"length"`
	Md5  []byte   `bencode:"md5sum" empty:"omit"`
	Path []string `bencode:"path"`
}

func (info *InfoDict) Parse() error {
	if len(info.Pieces)%20 != 0 {
		return errors.New("info parse: SHA-1 hash of pieces has not the right length")
	}
	return nil
}

func (info *InfoDict) SetInfoHash(data []byte) error {
	const key = "info"
	infoBenc, ok, err := bencode.Get(data, key)
	if !ok {
		return fmt.Errorf("set info hash: key %s doesn't exist in dict", key)
	}
	if err != nil {
		return fmt.Errorf("set info hash: %w", err)
	}
	h := sha1.Sum(infoBenc)
	info.Hash = h
	return nil
}

/*func (info *InfoDict) SetInfoHash() error {
	data, err := bencode.Encode(info)
	if err != nil {
		return fmt.Errorf("info hash: %w", err)
	}
	h := sha1.Sum(data)
	info.Hash = h
	return nil
	//return h[:], nil
}*/
func (info *InfoDict) PiecesHash() ([][]byte, error) {
	if len(info.Pieces)%20 != 0 {
		return nil, errors.New("hashes of pieces is not divided exctly with 20")
	}
	h := [][]byte{}
	for i := 0; i < len(info.Pieces); i += 20 {
		h = append(h, info.Pieces[i:i+20])
	}
	return h, nil
}

//maybe discard this and use path from stdlib
func (f File) PathToDir() string {
	var dir string
	for _, v := range f.Path {
		dir += v + "/"
	}
	return dir[:len(dir)-1]
}
