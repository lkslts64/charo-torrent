package torrent

import "github.com/lkslts64/charo-torrent/peer_wire"

//messages passed to channels
type msgID int

//jobs can be sent by orchestrator to peer conns
type job struct {
	id  jobID
	val interface{}
}

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
	piece
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

type event struct {
	id  eventID
	val interface{}
}
