package torrent

import (
	"math/rand"
)

//type PeerID [20]byte

var peerID [20]byte

//this can be the inside the init func
func newPeerID() (id [20]byte) {
	rbytes := make([]byte, 12)
	rand.Read(rbytes)
	copy(id[:], append([]byte("-"+clientID+version+"-"), rbytes...))
	peerID = id
	return
}
