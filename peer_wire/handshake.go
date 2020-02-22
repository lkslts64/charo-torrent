package peer_wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const protoLen byte = 19

var proto = [...]byte{
	'B', 'i', 't', 'T', 'o', 'r', 'r', 'e', 'n', 't',
	' ', 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l',
}

type HandShake struct {
	Reserved Reserved
	InfoHash [20]byte
	PeerID   [20]byte
}

//h.InfoHash should be zero if are going to be recipient.
func (h *HandShake) Do(rw io.ReadWriter) (*HandShake, error) {
	//check if we are initiator or recipients
	switch {
	case h.InfoHash != [20]byte{}:
		return h.initiate(rw)
	default:
		return h.receipt(rw)
	}
}

//initiate should be called when a client wants to initiate
//a handshake .InfoHash field should not be empty.
func (h *HandShake) initiate(rw io.ReadWriter) (*HandShake, error) {
	var err error
	if err = h.write(rw); err != nil {
		return nil, fmt.Errorf("hs initiate: %s", err)
	}
	var _h *HandShake
	if _h, err = readHs(rw); err != nil {
		return nil, fmt.Errorf("hs initiate: %s", err)
	}
	if h.InfoHash != _h.InfoHash {
		return nil, fmt.Errorf("hs initiate:info_hash response of peer  doesn't match the client's")
	}
	return _h, nil
}

//receipt should be called when client is the recipient
//of a handshake. InfoHash field must be zero - will be
//filled inside.
func (h *HandShake) receipt(rw io.ReadWriter) (*HandShake, error) {
	var err error
	var _h *HandShake
	if _h, err = readHsWithoutPeerID(rw); err != nil {
		return nil, fmt.Errorf("hs receipt: %s", err)
	}
	h.InfoHash = _h.InfoHash
	if err = h.write(rw); err != nil {
		return nil, fmt.Errorf("hs receipt: %s", err)
	}
	if err = _h.readPeerID(rw); err != nil {
		return nil, err
	}
	return _h, nil
}

//write sends write the bytes of h to conn.
func (h *HandShake) write(conn io.Writer) error {
	var b bytes.Buffer
	if err := writeBinary(&b, protoLen, proto, h); err != nil {
		panic(err)
	}
	_, err := conn.Write(b.Bytes())
	if err != nil {
		return fmt.Errorf("write: %s", err)
	}
	return nil
}

func readHs(conn io.Reader) (*HandShake, error) {
	hs, err := readHsWithoutPeerID(conn)
	if err != nil {
		return nil, err
	}
	if err = hs.readPeerID(conn); err != nil {
		return nil, err
	}
	return hs, nil
}

func readHsWithoutPeerID(conn io.Reader) (*HandShake, error) {
	h := new(HandShake)
	pstrLenSlc := make([]byte, 20)
	var err error
	_, err = io.ReadFull(conn, pstrLenSlc)
	if err != nil {
		return nil, err
	}
	if pstrLenSlc[0] != 19 && bytes.Equal(pstrLenSlc[1:], proto[:]) {
		return nil, errors.New("proto or protoLen are not the right one(s)")
	}
	buf := make([]byte, 28)
	_, err = io.ReadFull(conn, buf)
	if err != nil {
		return nil, err
	}
	b := bytes.NewBuffer(buf)
	err = binary.Read(b, binary.BigEndian, &h.Reserved)
	if err != nil {
		return nil, err
	}
	err = binary.Read(b, binary.BigEndian, &h.InfoHash)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (h *HandShake) readPeerID(conn io.Reader) error {
	buf := make([]byte, 20)
	_, err := io.ReadFull(conn, buf)
	if err != nil {
		return err
	}
	b := bytes.NewBuffer(buf)
	return binary.Read(b, binary.BigEndian, &h.PeerID)
}
