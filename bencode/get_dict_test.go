package bencode

import (
	"fmt"
	"io/ioutil"
	"testing"

	"github.com/stretchr/testify/require"
)

func testFile(t *testing.T, fileName string) {
	t.Logf("Testing file %s\n", fileName)
	data, err := ioutil.ReadFile(fileName)
	require.NoError(t, err)
	value, ok, err := Get(data, "info")
	require.NoError(t, err)
	if ok && err == nil {
		fmt.Println(string(value))
	}
}

func TestFiles(t *testing.T) {
	testFile(t, "test/alice.torrent")
	testFile(t, "test/a.torrent")
}
