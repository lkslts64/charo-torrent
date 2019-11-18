package peer_wire

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteHs(t *testing.T) {
	var b bytes.Buffer
	(&HandShake{
		Reserved: [8]byte{2: 1},
		InfoHash: [20]byte{5: 23},
		PeerID:   [20]byte{10: 10},
	}).write(&b)
	protoSlc := make([]byte, 20)
	b.Read(protoSlc)
	assert.EqualValues(t, append([]byte{19}, proto[:]...), protoSlc)
	res := [8]byte{2: 1}
	ihash := [20]byte{5: 23}
	peerID := [20]byte{10: 10}
	assert.EqualValues(t, append(res[:], append(ihash[:], peerID[:]...)...), b.Bytes())
}

func TestReadHs(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	go func() {
		defer w.Close()
		b := append([]byte{19}, proto[:]...)
		payload := [48]byte{4: 4, 8: 8, 28: 28}
		b = append(b, payload[:]...)
		w.Write(b)
	}()
	hs, err := readHs(r)
	require.NoError(t, err)
	assert.EqualValues(t, &HandShake{
		Reserved: [8]byte{4: 4},
		InfoHash: [20]byte{0: 8},
		PeerID:   [20]byte{0: 28},
	}, hs)
}
