package metainfo

import (
	"fmt"
	_ "fmt"
	"io/ioutil"
	"strings"

	"github.com/lkslts64/charo-torrent/bencode"
)

type AnnounceURL string

type MetaInfo struct {
	Announce     AnnounceURL `bencode:"announce"`
	AnnounceList [][]string  `bencode:"announce-list" empty:"omit"`
	Comment      string      `bencode:"comment" empty:"omit"`
	Created      string      `bencode:"created by" empty:"omit"`
	CreationDate int         `bencode:"creation date" empty:"omit"`
	Encoding     string      `bencode:"encoding" empty:"omit"`
	Info         InfoDict    `bencode:"info"`
	//URLList      []string    `bencode:"url-list" empty:"omit"`
}

func LoadTorrentFile(fileName string) (*MetaInfo, error) {
	data, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("load torrent:cant read .torrent file with err: %w", err)
	}
	var meta MetaInfo
	err = bencode.Decode(data, &meta)
	if err != nil {
		return nil, fmt.Errorf("load torrent:: %w", err)
	}
	err = meta.Parse()
	if err != nil {
		return nil, fmt.Errorf("load torrent: %w", err)
	}
	err = meta.Info.SetInfoHash(data)
	if err != nil {
		return nil, fmt.Errorf("load torrent: %w", err)
	}
	return &meta, nil
}

//Parse makes some checks based on a torrent file.
//Maybe further checks should be made beyond these.
func (m *MetaInfo) Parse() error {
	err := m.Info.Parse()
	if err != nil {
		return fmt.Errorf("metainfo parse: %w", err)
	}
	//currently support only http(s) trackers
	//TODO: uncomment when we start supporting UDP trackers.
	/*if !strings.HasPrefix(string(m.Announce), "http") {
		return errors.New("metainfo parse:this is not an http tracker")
	}*/
	return nil
}

func (m *MetaInfo) CreateTorrentFile(fileName string) error {
	data, err := bencode.Encode(m)
	if err != nil {
		fmt.Errorf("create torrent: %w", err)
	}
	err = ioutil.WriteFile(fileName, data, 0777)
	if err != nil {
		return fmt.Errorf("create torrent: %w", err)
	}
	return nil
}

func (a AnnounceURL) Scrape() string {
	const s = "announce"
	var i int
	if i = strings.LastIndexByte(string(a), '/'); i < 0 {
		return ""
	}
	if len(a) < i+1+len(s) {
		return ""
	}
	if a[i+1:i+len(s)+1] != s {
		return ""
	}
	return string(a[:i+1]) + "scrape" + string(a[i+len(s)+1:])
}
