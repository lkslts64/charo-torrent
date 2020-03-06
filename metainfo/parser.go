package metainfo

import (
	"io"
	"io/ioutil"
)

//Parser parses an input source (e.g file,uri) into a MetaInfo struct
type Parser interface {
	Parse() (*MetaInfo, error)
}

//FileParser parses a file with name Filename
type FileParser struct {
	Filename string
}

func (fp *FileParser) Parse() (*MetaInfo, error) {
	return LoadMetainfoFile(fp.Filename)
}

//MagnetParser parses an URI
type MagnetParser struct {
	URI string
}

func (mp *MagnetParser) Parse() (*MetaInfo, error) {
	return nil, nil
}

type ReaderParser struct {
	R io.Reader
}

//Parse reads until EOF from R and then parses the read bytes
func (rp *ReaderParser) Parse() (*MetaInfo, error) {
	data, err := ioutil.ReadAll(rp.R)
	if err != nil {
		return nil, err
	}
	return loadMetainfoFromBytes(data)
}

type InfoHashParser struct {
	InfoHash [20]byte
}

func (ip *InfoHashParser) Parse() (*MetaInfo, error) {
	return &MetaInfo{
		Info: &InfoDict{
			Hash: ip.InfoHash,
		},
	}, nil
}
