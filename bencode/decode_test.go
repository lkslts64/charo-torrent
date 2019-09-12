package bencode

import (
	"testing"

	"fmt"
	"io/ioutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type random_decode_test struct {
	data     string
	expected interface{}
}

var random_decode_tests = []random_decode_test{
	{"i57e", int64(57)},
	{"i-9223372036854775808e", int64(-9223372036854775808)},
	{"5:hello", "hello"},
	{"29:unicode test проверка", "unicode test проверка"},
	{"d1:ai5e1:b5:helloe", map[string]interface{}{"a": int64(5), "b": "hello"}},
	{"li5ei10ei15ei20e7:bencodee",
		[]interface{}{int64(5), int64(10), int64(15), int64(20), "bencode"}},
	{"ldedee", []interface{}{map[string]interface{}{}, map[string]interface{}{}}},
	{"le", []interface{}{}},
	/*{"d1:rd6:\xd4/\xe2F\x00\x01e1:t3:\x9a\x87\x011:v4:TR%=1:y1:re", map[string]interface{}{
		"r": map[string]interface{}{},
		"t": "\x9a\x87\x01",
		"v": "TR%=",
		"y": "r",
	}},*/
	{"d1:rde1:t3:\x9a\x87\x011:v4:TR%=1:y1:re", map[string]interface{}{
		"r": map[string]interface{}{},
		"t": "\x9a\x87\x01",
		"v": "TR%=",
		"y": "r",
	}},
}

func TestRandomDecode(t *testing.T) {
	for _, test := range random_decode_tests {
		var value interface{}
		err := Decode([]byte(test.data), &value)
		if err != nil {
			t.Error(err, test.data)
			continue
		}
		assert.EqualValues(t, test.expected, value)
		fmt.Println(value, value)

	}
}

func TestSimpleDict(t *testing.T) {
	s := struct {
		Ignore int
		Normal int
	}{}

	err := Decode([]byte("d6:Ignorei5454e5:helloi54534ee"), &s)
	if err != nil {
		fmt.Println(err)
	}
	fmt.Println(s)
}

func TestDecodeCustomSlice(t *testing.T) {
	type flag byte
	var fs3 []flag
	// We do a longer slice then a shorter slice to see if the buffers are
	// shared.
	require.NoError(t, Decode([]byte("3:\x01\x10\xff"), &fs3))
	//require.NoError(t, Decode([]byte("3:\x01\x10\xff2:\x04\x0f"), &fs2))
	assert.EqualValues(t, []flag{1, 16, 255}, fs3)
	//assert.EqualValues(t, []flag{4, 15}, fs2)
}

//Test a real torrent file.
func TestTorrentFile(t *testing.T) {
	data, err := ioutil.ReadFile("test/alice.torrent")
	if err != nil {
		fmt.Println(err)
	}
	var i interface{}
	err = Decode(data, &i)
	if err != nil {
		fmt.Println(err)
	}
	fmt.Println(i)

}
