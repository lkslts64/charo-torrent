package metainfo

import (
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
	//t.Log("Piece hashes:")
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
	_, err = bencode.Encode(info)
	require.NoError(t, err)
}

//we dont test the other files because they contain
//fields that our MetaInfo struct is not aware of.
var files = []string{
	"testdata/archlinux-2011.08.19-netinstall-i686.iso.torrent",
	"testdata/a.torrent",
	"testdata/Parquet-Goodies-2018.torrent",
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

func TestInfoHash(t *testing.T) {
	meta, err := LoadMetainfoFile("testdata/Parquet-Goodies-2018.torrent")
	assert.NoError(t, err)
	//Infohash from Deluge: c27727f12f2469ca4643f759ac2736a18de545b5
	assert.Equal(t, []byte{0xc2, 0x77, 0x27, 0xf1, 0x2f, 0x24, 0x69, 0xca, 0x46, 0x43, 0xf7, 0x59, 0xac, 0x27, 0x36, 0xa1, 0x8d, 0xe5, 0x45, 0xb5}, meta.Info.Hash[:])
	meta, err = LoadMetainfoFile("testdata/oliver-koletszki-Schneeweiss11.torrent")
	assert.NoError(t, err)
	//Infohash from Deluge: 800ca2bcc78d4946f640586e4b789654782c8ae5
	assert.Equal(t, []byte{0x80, 0x0c, 0xa2, 0xbc, 0xc7, 0x8d, 0x49, 0x46, 0xf6, 0x40, 0x58, 0x6e, 0x4b, 0x78, 0x96, 0x54, 0x78, 0x2c, 0x8a, 0xe5}, meta.Info.Hash[:])
}
