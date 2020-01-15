package bencode

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io/ioutil"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFile(t *testing.T, fileName string) {
	t.Logf("Testing file %s\n", fileName)
	data, err := ioutil.ReadFile(fileName)
	require.NoError(t, err)
	value, ok, err := Get(data, "info")
	require.NoError(t, err)
	if ok && err == nil {
		hash := sha1.Sum(value)
		fmt.Printf("%x\n", hash)
	}
}

func TestFiles(t *testing.T) {
	testFile(t, "test/alice.torrent")
	testFile(t, "test/a.torrent")
	testFile(t, "test/archlinux-2011.08.19-netinstall-i686.iso.torrent")
}

func TestGet(t *testing.T) {
	data := []string{
		"omg this is not a dict",
		"d8:announce3:lol4:memale7:instantd5:aaaaa4:bbbb1:ai3333eee",
		"dbad_inpute",
	}

	_, ok, err := Get([]byte(data[0]), "garbage")
	if ok {
		t.Fatal("it was in dict unexpectedly")
	}

	require.EqualValues(t, fmt.Errorf("bencode get: %w", ErrNoDict), err)
	value, ok, err := Get([]byte(data[1]), "instant")
	if !ok {
		t.Fatal("instant wasn't in dict")
	}
	assert.EqualValues(t, string(value), "d5:aaaaa4:bbbb1:ai3333ee")
	_, ok, err = Get([]byte(data[2]), "garbage")
	if ok {
		t.Fatal("it was in dict unexpectedly")
	}
	var uv *UnknownValueError
	if !errors.As(err, &uv) {
		t.Fatal("no unknown error value")
	}
}
