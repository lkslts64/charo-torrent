package torrent

import (
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/tracker"
)

//messages passed to channels
type msgID int

//jobs can be sent by master to peer conns
//take a look at conn.go at parseJob func to see
//what values a command can hold.
//
//TODO:maybe change this struct to and a `type` field that
//identifies the command type (e.g block,bitmap,Msg)
/*type command struct {
	val interface{}
}*/

type haveInfo struct{}

type seeding struct{}

type drop struct{}

type verifyPiece int

type jobID int8

const (
	//TODO: some of them are not necessary- we can unify them in one?
	amChoking       jobID = jobID(peer_wire.Choke)
	amUnchoking           = jobID(peer_wire.Unchoke)
	amInterested          = jobID(peer_wire.Interested)
	amNotInterested       = jobID(peer_wire.NotInterested)
	have                  = jobID(peer_wire.Have)
	bitfield              = jobID(peer_wire.Bitfield)
	//an action with request length == pieceLength begin == 0
	//will be interpreted as we want to download whole piece
	request     = jobID(peer_wire.Request)
	cancel      = jobID(peer_wire.Cancel)
	metadataExt = iota + 1
	gotMetadata
)

type eventID int8

const (
	isChoking       eventID = eventID(peer_wire.Choke)
	isUnchoking             = eventID(peer_wire.Unchoke)
	isInterested            = eventID(peer_wire.Interested)
	isNotInterested         = eventID(peer_wire.NotInterested)
	//an action with request length == pieceLength begin == 0
	//will be interpreted as we want to download whole piece
)

const (
	//events - typically workers send them
	bitfield2 eventID = iota
	haveRcv
	piece2
)

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

//a wrapper for the channels a conn has.
//
//Conn sends this struct to master at a special channel as the first message to bootstrap
//the communication between the two.Initially, master doesn't know about the conn's channs.
type connChansInfo struct {
	commandCh chan interface{}
	eventCh   chan interface{}
	dropped   chan struct{}
}

type event struct {
	//which conn produced this event
	conn *connInfo
	val  interface{}
}

type pieceHashed struct {
	pieceIndex int
	ok         bool
}

//type discardedRequests []block

//signals that a block was downloaded
type downloadedBlock block

//signals that a block was uploaded
type uploadedBlock block

//signals that a conn was dropped
type connDroped struct{}

//signals that a conn has space to request some blokcs i.e request queue length
//is not full.
type wantBlocks struct{}

//when a conn discards requests, we use this event to notify other conns that
//some blocks are available for requesting.
type requestsAvailable struct{}

type discardedRequests struct{}
