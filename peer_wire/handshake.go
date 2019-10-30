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

func (h *HandShake) Initiate(conn net.Conn) error {
	var err error
	if err = h.write(conn); err != nil {
		return fmt.Errorf("initiate: %w", err)
	}
	var _h *HandShake
	if _h, err = readHs(conn); err != nil {
		return fmt.Errorf("initiate: %w", err)
	}
	if h.InfoHash != _h.InfoHash {
		return fmt.Errorf("initiate:info_hash response of peer %v doesn't match the client's", conn.RemoteAddr())
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
		return err
	}
	if _, ok := ihashes[_h.InfoHash]; !ok {
		return errors.New("receipt: client doesn't manage this info_hash")
	}
	h.InfoHash = _h.InfoHash
	if err = h.write(conn); err != nil {
		return fmt.Errorf("receipt: %w", err)
	}
	return nil
}

/*Read reads a peer's handshake message
func (h *HandShake) Read(conn io.ReadWriter) error {
	if err := h.readHs(conn); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	return nil
}*/

//write sends write the bytes of h to conn.
func (h *HandShake) write(conn io.ReadWriter) error {
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
func readHs(conn io.ReadWriter) (*HandShake, error) {
	h := new(HandShake)
	pstrLenSlc := make([]byte, 20)
	var err error
	//err = readConnFull(conn, pstrLenSlc)
	_, err = io.ReadFull(conn, pstrLenSlc)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if pstrLenSlc[0] != 19 && bytes.Equal(pstrLenSlc[1:], proto[:]) {
		return nil, errors.New("proto or protoLen are not the right one(s)")
	}
	//pstrLen := pstrLenSlc[0]
	//dataLen := int(pstrLen) + 48
	buf := make([]byte, 48)
	//err = readConnFull(conn, buf)
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

/*
//reads from conn until all len(buf) bytes are written to buf
//or timeout expires
func readConnFull(conn net.Conn, buf []byte) error {
	var err error
	n, err := io.ReadFull(conn, buf)
	var nErr net.Error
	if err != nil {
		if ok := errors.As(err, &nErr); ok && nErr.Timeout() {
			return fmt.Errorf("readConnFull: timeout: %w", nErr)
		}
		return fmt.Errorf("readConnFull :%w", err)
	}
	if n != len(buf) {
		return fmt.Errorf("readConnFull: read less than %d bytes", len(buf))
	}
	return nil
}

//reads from conn until all len(buf) bytes are written to buf
//or timeout expires
func readConnAtLeastV2(conn net.Conn, buf []byte) error {
	var err error
	var n int
	total := 0
	dataLen := len(buf)
	for {
		n, err = conn.Read(buf[total:])
		var nErr *net.Error
		if err != nil {
			if ok := errors.As(err, &nErr); ok && (*nErr).Timeout() {
				return errors.New("readConn: timeout")
			}
			return err
		}
		total += n
		if total >= dataLen {
			break
		}
	}
	return nil
}
*/
