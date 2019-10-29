package peer_wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const Proto = "BitTorrent protocol"

type MessageID int8

const (
	Choke MessageID = iota
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
)

type Msg struct {
	Kind     MessageID
	Index    uint32
	Begin    uint32
	Len      uint32
	Bitfield []byte
	Block    []byte
}

func (m *Msg) writeV2(conn net.Conn) (err error) {
	checkWrite := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	var b bytes.Buffer
	if m.Kind == KeepAlive {
		checkWrite(writeBinary(&b, 0))
		_, err = conn.Write(b.Bytes())
		return
	}
	kind := byte(m.Kind)
	checkWrite(writeBinary(&b, &kind))
	if m.Index != 0 {
		checkWrite(writeBinary(&b, &(m.Index)))
	}
	if m.Begin != 0 {
		checkWrite(writeBinary(&b, &(m.Begin)))
	}
	if m.Len != 0 {
		checkWrite(writeBinary(&b, &(m.Len)))
	}
	if m.Bitfield != nil {
		checkWrite(writeBinary(&b, &(m.Bitfield)))
	}
	if m.Block != nil {
		checkWrite(writeBinary(&b, &(m.Block)))
	}
	var msgLen [4]byte
	binary.BigEndian.PutUint32(msgLen[:], uint32(b.Len()))
	_, err = conn.Write(append(msgLen[:], b.Bytes()...))
	return
}

func (m *Msg) Write(conn net.Conn) (err error) {
	checkWrite := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	var b bytes.Buffer
	if m.Kind == KeepAlive {
		checkWrite(writeBinary(&b, 0))
		_, err = conn.Write(b.Bytes())
		return
	}
	switch m.Kind {
	case Port:
	case KeepAlive:
	case Choke, Unchoke, Interested, NotInterested:
		checkWrite(writeBinary(&b, byte(m.Kind)))
	case Have:
		checkWrite(writeBinary(&b, byte(m.Kind), m.Index))
	case Bitfield:
		checkWrite(writeBinary(&b, byte(m.Kind), m.Bitfield))
	case Request, Cancel:
		checkWrite(writeBinary(&b, byte(m.Kind), m.Index, m.Begin, m.Len))
	case Piece:
		checkWrite(writeBinary(&b, byte(m.Kind), m.Index, m.Begin, m.Block))
	default:
		panic("Unkonwn kind of msg to send")
	}
	var msgLen [4]byte
	binary.BigEndian.PutUint32(msgLen[:], uint32(b.Len()))
	_, err = conn.Write(append(msgLen[:], b.Bytes()...))
	return
}

func Read(conn net.Conn) (*Msg, error) {
	msg := new(Msg)
	checkRead := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	var msgLen [4]byte
	_, err := io.ReadFull(conn, msgLen[:])
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	var _msgLen uint32
	checkRead(readFromBinary(&b, &_msgLen))
	if _msgLen == 0 {
		msg.Kind = MsgKeepAlive
		return msg, nil
	}
	buf := make([]byte, _msgLen)
	_, err = io.ReadFull(conn, buf)
	if err != nil {
		return nil, err
	}
	b.Write(buf)
	var kind MessageID
	checkRead(readFromBinary(&b, &kind))
	msg.Kind = kind
	switch msg.Kind {

	}

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
