package torrent

import "github.com/lkslts64/charo-torrent/tracker"

//Which source informed us about that peer
type PeerSource byte

const (
	//It was an incoming connection
	SourceIncoming PeerSource = iota
	//The peer was give to us by DHT
	SourceDHT
	//The peer was give to us by a tracker
	SourceTracker
	//The user manually added this peer
	SourceUser
)

//Holds basic information about a peer
type Peer struct {
	tp     tracker.Peer
	source PeerSource
}
