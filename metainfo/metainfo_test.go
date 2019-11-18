package metainfo

import (
	"fmt"
	"io/ioutil"
	"path"
	"testing"

	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFile(t *testing.T, fileName string) {
	meta, err := LoadMetainfoFile(fileName)
	require.NoError(t, err)
	info := meta.Info
	hp := info.PiecesHash()
	require.NoError(t, err)
	t.Log("Piece hashes:")
	for i, hash := range hp {
		t.Logf("%dth hash: %s\n", i, string(hash))
	}
	if len(info.Files) > 0 {
		t.Logf("Multiple files: %s\n", info.Name)
		for _, f := range info.Files {
			t.Logf(" - %s (length: %d)\n", path.Join(f.Path...), f.Len)
		}
	} else {
		t.Logf("Single file: %s (length: %d)\n", info.Name, info.Len)
	}
	for _, group := range meta.AnnounceList {
		for _, tracker := range group {
			t.Logf("Tracker: %s\n", tracker)
		}
	}
	//fmt.Printf("trackers scrape URL: %s\n", meta.Announce.Scrape())
	fmt.Printf("info hash: %x\n", info.Hash)
	_infoBenc, err := bencode.Encode(info)
	require.NoError(t, err)
	benData, err := ioutil.ReadFile(fileName)
	infoBenc, ok, err := bencode.Get(benData, "info")
	if !ok {
		t.Fail()
	}
	require.NoError(t, err)
	fmt.Println(len(_infoBenc))
	fmt.Println(len(infoBenc))
	assert.EqualValues(t, string(_infoBenc), string(infoBenc))
}

//we dont test the other files because they contain
//fields that our MetaInfo struct is not aware of.
var files = []string{
	"testdata/archlinux-2011.08.19-netinstall-i686.iso.torrent",
	"testdata/a.torrent",
}

func TestFile(t *testing.T) {
	for _, f := range files {
		testFile(t, f)
	}
}

func testUnmarshal(t *testing.T, input string, expected *MetaInfo) {
	var actual MetaInfo
	err := bencode.Decode([]byte(input), &actual)
	if expected == nil {
		assert.Error(t, err)
		return
	}
	assert.NoError(t, err)
	assert.EqualValues(t, *expected, actual)
}

func TestUnmarshal(t *testing.T) {
	testUnmarshal(t, "d4:infoe", nil)
	testUnmarshal(t, "d4:infoabce", nil)
	testUnmarshal(t, "d8:announce3:url4:infod12:piece lengthi5e6:pieces3:omgee",
		&MetaInfo{
			Announce: "url",
			Info: InfoDict{
				PieceLen: 5,
				Pieces:   []byte("omg"),
			},
		})
}
