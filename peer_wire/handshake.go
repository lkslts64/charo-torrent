package peer_wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

const protoLen byte = 19

var proto = [...]byte{
	'B', 'i', 't', 'T', 'o', 'r', 'r', 'e', 'n', 't',
	' ', 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l',
}

type HandShake struct {
	Reserved [8]byte
	InfoHash [20]byte
	PeerID   [20]byte
}

//If Do returns error connection should be closed
//h.InfoHash should be zero if are going to be recipient.
func (h *HandShake) Do(
	conn net.Conn, ihashes map[[20]byte]struct{}) error {
	var err error
	//check if we are initiator or recipients
	switch {
	case h.InfoHash != [20]byte{}:
		err = h.Receipt(conn, ihashes)
	default:
		err = h.Initiate(conn)
	}
	return fmt.Errorf("handshake: %w", err)
}

//Initiate should be called when a client wants to initiate
//a handshake .InfoHash field should not be empty.
func (h *HandShake) Initiate(conn io.ReadWriter) error {
	var err error
	if err = h.write(conn); err != nil {
		return fmt.Errorf("hs initiate: %w", err)
	}
	var _h *HandShake
	if _h, err = readHs(conn); err != nil {
		return fmt.Errorf("hs initiate: %w", err)
	}
	if h.InfoHash != _h.InfoHash {
		return fmt.Errorf("hs initiate:info_hash response of peer  doesn't match the client's")
	}
	return nil
}

//Receipt should be called when client is the recipient
//of a handshake. InfoHash field must be zero - will be
//filled inside.
func (h *HandShake) Receipt(conn io.ReadWriter, ihashes map[[20]byte]struct{}) error {
	var err error
	var _h *HandShake
	if _h, err = readHs(conn); err != nil {
		return fmt.Errorf("hs receipt: %w", err)
	}
	if _, ok := ihashes[_h.InfoHash]; !ok {
		return errors.New("hs receipt: client doesn't manage this info_hash")
	}
	h.InfoHash = _h.InfoHash
	if err = h.write(conn); err != nil {
		return fmt.Errorf("hs receipt: %w", err)
	}
	return nil
}

//write sends write the bytes of h to conn.
func (h *HandShake) write(conn io.Writer) error {
	var b bytes.Buffer
	if err := writeBinary(&b, protoLen, proto, h); err != nil {
		panic(err)
	}
	_, err := conn.Write(b.Bytes())
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

//readHs writes response to the receiver
func readHs(conn io.Reader) (*HandShake, error) {
	h := new(HandShake)
	pstrLenSlc := make([]byte, 20)
	var err error
	_, err = io.ReadFull(conn, pstrLenSlc)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if pstrLenSlc[0] != 19 && bytes.Equal(pstrLenSlc[1:], proto[:]) {
		return nil, errors.New("proto or protoLen are not the right one(s)")
	}
	buf := make([]byte, 48)
	_, err = io.ReadFull(conn, buf)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	b := bytes.NewBuffer(buf)
	err = binary.Read(b, binary.BigEndian, h)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return h, nil
}
