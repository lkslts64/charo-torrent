package peer_wire

import (
	"errors"

	"github.com/lkslts64/charo-torrent/bencode"
)

type ExtensionID int8

const (
	ExtHandshakeID ExtensionID = iota
	ExtMetadataID
)

type ExtensionName string

const (
	ExtMetadataName ExtensionName = "ut_metadata"
)

type Extensions map[ExtensionName]ExtensionID

type ExtHandshakeDict map[string]interface{}

func decodeExtHandshakeMsg(msg []byte) (d ExtHandshakeDict, err error) {
	err = bencode.Decode([]byte(msg), &d)
	return
}

//return 'm' dict contents. we received d from a peer
func (d ExtHandshakeDict) Extensions() (Extensions, error) {
	var peerExt Extensions
	var ok bool
	var _m interface{}
	if _m, ok = d["m"]; !ok {
		return nil, errors.New("ext hanshake doesn't contain 'm' dict")
	}
	if _m != nil {
		var m map[string]interface{}
		if m, ok = _m.(map[string]interface{}); !ok {
			return nil, errors.New("ext handshake's 'm' isnt dict")
		}
		var id int64
		for k, v := range m {
			if id, ok = v.(int64); !ok {
				return nil, errors.New("value of 'm' dict isn't an integer")
			}
			//TODO: Maybe discard the extensions that we dont't care about?
			peerExt[ExtensionName(k)] = ExtensionID(id)
		}
	}
	return peerExt, nil
}

func (d ExtHandshakeDict) MetadataSize() (msize int64, ok bool) {
	var v interface{}
	if v, ok = d["metadata_size"]; !ok {
		return
	}
	msize, ok = v.(int64)
	return
}

const (
	MetadataReqID ExtensionID = iota
	MetadataDataID
	MetadataRejID
)

type MetadataExtMsg struct {
	Kind    ExtensionID `bencode:"msg_type"`
	Piece   int         `bencode:"piece"`
	TotalSz int         `bencode:"total_size" empty:"omit"`
	Data    []byte      `bencode:"-"`
}
