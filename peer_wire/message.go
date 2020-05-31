package peer_wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/lkslts64/charo-torrent/bencode"
)

const (
	Proto        = "BitTorrent protocol"
	maxMsgLength = (1 << 10) * 256 // 256KiB
)

type MessageKind int8

const (
	Choke MessageKind = iota
	Unchoke
	Interested
	NotInterested
	Have
	Bitfield
	Request
	Piece
	Cancel
	Port
	//KeepAlive doesn't have an ID at spec but we define one
	KeepAlive
	Extended = 20
)

func (id MessageKind) String() string {
	switch id {
	case Choke:
		return "Choke"
	case Unchoke:
		return "Unchoke"
	case Interested:
		return "Interested"
	case NotInterested:
		return "NotInterested"
	case Have:
		return "Have"
	case Bitfield:
		return "Bitfield"
	case Request:
		return "Request"
	case Piece:
		return "Piece"
	case Cancel:
		return "Cancel"
	case Port:
		return "Port"
	case KeepAlive:
		return "Keepalive"
	case Extended:
		return "Extended"
	default:
		return "Unknown"
	}
}

//Msg represents a BitTorrent message to be sent over wire.
type Msg struct {
	Kind        MessageKind
	Index       uint32
	Begin       uint32
	Len         uint32
	Bf          BitField
	Block       []byte
	Port        uint16
	ExtendedID  ExtensionID
	ExtendedMsg interface{}
}

//Encode m as BitTorrent protocol specifies.
func (m *Msg) Encode() []byte {
	try := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	var b bytes.Buffer
	switch m.Kind {
	case KeepAlive:
	case Choke, Unchoke, Interested, NotInterested:
		try(writeBinary(&b, m.Kind))
	case Have:
		try(writeBinary(&b, m.Kind, m.Index))
	case Bitfield:
		try(writeBinary(&b, m.Kind, m.Bf))
	case Request, Cancel:
		try(writeBinary(&b, m.Kind, m.Index, m.Begin, m.Len))
	case Piece:
		try(writeBinary(&b, m.Kind, m.Index, m.Begin, m.Block))
	case Extended:
		try(writeBinary(&b, m.Kind, m.ExtendedID, writeExtension(m)))
	case Port:
		try(writeBinary(&b, m.Kind, m.Port))
	default:
		panic("Unknown kind of msg to send")
	}
	var msgLen [4]byte
	binary.BigEndian.PutUint32(msgLen[:], uint32(b.Len()))
	buf := append(msgLen[:], b.Bytes()...)
	return buf
}

//Decode reads from r an encoded BitTorrent message and decode it as the protocol specifies.
func Decode(r io.Reader) (*Msg, error) {
	msg := new(Msg)
	checkRead := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	msgLen := make([]byte, 4)
	_, err := io.ReadFull(r, msgLen)
	if err != nil {
		return nil, err
	}
	_msgLen := binary.BigEndian.Uint32(msgLen)
	if _msgLen > maxMsgLength {
		return nil, errors.New("peer wire: too long msg")
	}
	if _msgLen == 0 {
		msg.Kind = KeepAlive
		return msg, nil
	}
	buf := make([]byte, _msgLen)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return nil, err
	}
	b := bytes.NewBuffer(buf)
	checkRead(readFromBinary(b, &msg.Kind))
	switch msg.Kind {
	case Choke, Unchoke, Interested, NotInterested:
	case Have:
		checkRead(readFromBinary(b, &msg.Index))
	case Bitfield:
		msg.Bf = b.Bytes()
	case Request, Cancel:
		checkRead(readFromBinary(b, &msg.Index, &msg.Begin, &msg.Len))
	case Piece:
		checkRead(readFromBinary(b, &msg.Index, &msg.Begin))
		msg.Block = b.Bytes()
	case Port:
		checkRead(readFromBinary(b, &msg.Port))
	case Extended:
		checkRead(readFromBinary(b, &msg.ExtendedID))
		err = readExtension(msg.ExtendedID, b.Bytes(), msg)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unknown kind of msg")
	}
	return msg, nil
}

//decodes msg.ExtendedMsg
func writeExtension(msg *Msg) (b []byte) {
	var err error
	switch msg.ExtendedID {
	//TODO: Really? change this for specific extension number (peer chooses them)
	//add a field at msg struct to specify extended msg id
	//currently we suupoort only metadata ext so this is sufficient but not safe and secure.
	default:
		b, err = bencode.Encode(&msg.ExtendedMsg)
		if err != nil {
			panic(err)
		}
		if msg.ExtendedID == 0 {
			return
		}
		emsg := msg.ExtendedMsg.(MetadataExtMsg)
		if emsg.Data == nil {
			return
		}
		b = append(b, emsg.Data...)
		/*default:
		panic("unknown extension id")
		*/
	}
	return
}

//sets msg.ExtendedMsg
func readExtension(extKind ExtensionID, payload []byte, msg *Msg) error {
	var err error
	switch extKind {
	case 0:
		msg.ExtendedMsg, err = decodeExtHandshakeMsg(payload)
		if err != nil {
			return err
		}
	case ExtMetadataID:
		var metaExt MetadataExtMsg
		_payload := make([]byte, len(payload))
		copy(_payload, payload)
		err = bencode.Decode(payload, &metaExt)
		if err != nil {
			//we expect binary data after so we discard this err
			var errBuf *bencode.LargeBufferErr
			if !errors.As(err, &errBuf) || metaExt.Kind != MetadataDataID {
				return err
			}
			metaExt.Data = _payload[len(_payload)-errBuf.RemainingLen:]
		} else {
			if metaExt.Kind == MetadataDataID {
				return errors.New("metadata ext: expected binary data")
			}
		}
		msg.ExtendedMsg = metaExt
	default:
		return errors.New("unknown extension id")
	}
	return nil
}

//Given a message (m) of kind 'piece', Request
//returns the message of kind 'request' that
//gives the equivelant m as response.It panics if
//m's kind is not 'piece'.
func (m *Msg) Request() *Msg {
	if m.Kind != Piece {
		panic(m.Kind)
	}
	return &Msg{
		Kind:  Request,
		Index: m.Index,
		Begin: m.Begin,
		Len:   uint32(len(m.Block)),
	}
}

//This is not used till now. If we implement readBitfield then
//maybe.
//TODO:make []bool and move marshaling into peer_wire?
func writeBinaryBitField(w io.Writer, bytefield []bool) error {
	bitfield := make([]byte, int(math.Ceil(float64(len(bytefield))/8.0)))
	for i, v := range bytefield {
		if !v {
			continue
		}
		ind := i / 8
		mask := byte(0x01 << uint(7-i%8))
		bitfield[ind] |= mask
	}
	return binary.Write(w, binary.BigEndian, bitfield)
}

func readFromBinary(r io.Reader, data ...interface{}) error {
	var err error
	for _, d := range data {
		err = binary.Read(r, binary.BigEndian, d)
		if err != nil {
			return fmt.Errorf("read binary: %w", err)
		}
	}
	return nil
}

func writeBinary(w io.Writer, data ...interface{}) error {
	var err error
	for _, d := range data {
		err = binary.Write(w, binary.BigEndian, d)
		if err != nil {
			return fmt.Errorf("write binary: %w", err)
		}
	}
	return nil
}
