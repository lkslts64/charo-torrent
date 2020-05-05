package torrent

import (
	"github.com/lkslts64/charo-torrent/tracker"
)

//Torrent sends this when info is available
type haveInfo struct{}

//Torrent sends this to signal a conn to be dropped
type drop struct{}

//Torrent broadcasts this to conns when it need to signal conns that they should
// try to request some blocks
type requestsAvailable struct{}

//obsolete?
type downloadPieces struct{}

type trackerAnnouncerEvent struct {
	//which Torrent submited the event
	t     *Torrent
	event tracker.Event
	stats Stats
}

type trackerAnnouncerResponse struct {
	resp *tracker.AnnounceResp
	err  error
}

//conn sends this struct to Torrent as the first message to bootstrap
//the communication between the two.Initially, master doesn't know about the conn's channs.
type connChansInfo struct {
	sendC    chan interface{}
	recvC    chan interface{}
	droppedC chan struct{}
}

// Torrent receives messages with this type. val is the actual value of the
// message and conn is the connection that sent it
type msgWithConn struct {
	//which conn produced this msg
	conn *connInfo
	//the actal msg
	val interface{}
}

//pieceHasher sends this when a piece was hashed
type pieceHashed struct {
	pieceIndex int
	//succesfully or not
	ok bool
}

//conn sends this to signal that a block was downloaded
type downloadedBlock block

//conn sends this to signal that a block was uploaded
type uploadedBlock block

//conn sends this to signal that a conn was dropped
type connDroped struct{}

//when a conn discards requests,it sends this message to notify other conns that
//some blocks are available for requesting.
type discardedRequests struct{}
