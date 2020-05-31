package metainfo

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strings"
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
	const xtPrefix = "urn:btih:"
	u, err := url.Parse(mp.URI)
	if err != nil {
		err = fmt.Errorf("error parsing uri: %s", err)
		return nil, nil
	}
	if u.Scheme != "magnet" {
		err = fmt.Errorf("unexpected scheme: %q", u.Scheme)
		return nil, nil
	}
	xt := u.Query().Get("xt")
	if !strings.HasPrefix(xt, xtPrefix) {
		err = fmt.Errorf("bad xt parameter")
		return nil, nil
	}
	infoHash := xt[len(xtPrefix):]
	// BTIH hash can be in HEX or BASE32 encoding
	// will assign appropriate func judging from symbol length
	var decode func(dst, src []byte) (int, error)
	switch len(infoHash) {
	case 40:
		decode = hex.Decode
	case 32:
		decode = base32.StdEncoding.Decode
	}
	if decode == nil {
		err = fmt.Errorf("unhandled xt parameter encoding: encoded length %d", len(infoHash))
		return nil, nil
	}
	var (
		ihash [20]byte
	)
	n, err := decode(ihash[:], []byte(infoHash))
	if err != nil {
		err = fmt.Errorf("error decoding xt: %s", err)
		return nil, nil
	}
	if n != 20 {
		panic(n)
	}
	//m.DisplayName = u.Query().Get("dn")
	//m.Trackers = u.Query()["tr"]
	return &MetaInfo{
		Info: &InfoDict{
			Hash: ihash,
		},
	}, nil
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
