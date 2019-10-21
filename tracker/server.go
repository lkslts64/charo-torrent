package tracker

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
)

type portIP struct {
	IP   int32
	port int16
}

type torrent struct {
	stats udpScrapeInfo
	Peers []portIP
}

type server struct {
	pc    net.PacketConn
	conns map[int64]struct{}
	t     map[[20]byte]torrent
}

func marshal(parts ...interface{}) (ret []byte, err error) {
	var buf bytes.Buffer
	for _, p := range parts {
		err = binary.Write(&buf, binary.BigEndian, p)
		if err != nil {
			return
		}
	}
	ret = buf.Bytes()
	return
}

func (s *server) respond(addr net.Addr, rh respHeader, parts ...interface{}) (err error) {
	b, err := marshal(append([]interface{}{rh}, parts...)...)
	if err != nil {
		return
	}
	_, err = s.pc.WriteTo(b, addr)
	return
}

func (s *server) newConn() (ret int64) {
	ret = rand.Int63()
	if s.conns == nil {
		s.conns = make(map[int64]struct{})
	}
	s.conns[ret] = struct{}{}
	return
}

func (s *server) serveOne() error {
	b := make([]byte, 0x10000)
	n, addr, err := s.pc.ReadFrom(b)
	if err != nil {
		return err
	}
	r := bytes.NewReader(b[:n])
	var h reqHeader
	err = readFromBinary(r, &h)
	if err != nil {
		return err
	}
	switch h.Action {
	case actionConnect:
		if h.ConnID != protoID {
			return err
		}
		connID := s.newConn()
		err = s.respond(addr, respHeader{
			actionConnect,
			h.TxID,
		}, connID)
		return err
	case actionAnnounce:
		if err = s.checkConnectionID(h, addr); err != nil {
			return err
		}
		var ar AnnounceReq
		err = readFromBinary(r, &ar)
		if err != nil {
			return err
		}

		var t torrent
		var ok bool
		if t, ok = s.t[ar.InfoHash]; ok != true {
			return fmt.Errorf("announce: ihash %v doesnt exist in tracker db", ar.InfoHash)
		}
		err = s.respond(addr, respHeader{
			actionAnnounce,
			h.TxID,
		}, announceFixed{
			900,
			t.stats.Leechers,
			t.stats.Seeders,
		}, t.Peers)
		return err
	case actionScrape:
		if err = s.checkConnectionID(h, addr); err != nil {
			return err
		}
		ihashSz := 20
		if r.Len()%ihashSz != 0 {
			return fmt.Errorf("scrape: remaining scrape request is not divided exactly with %d", ihashSz)
		}
		numScrapeReq := r.Len() / ihashSz
		//BEP specifies a limit
		if numScrapeReq > 74 {
			return fmt.Errorf("scrape: cannot process more than %d scrape requests", numScrapeReq)
		}
		scrapeReq := make([][20]byte, numScrapeReq)
		err = readFromBinary(r, &scrapeReq)
		if err != nil {
			return err
		}
		scrapeResp := make([]udpScrapeInfo, numScrapeReq)
		var t torrent
		var ok bool
		for i, ihash := range scrapeReq {
			if t, ok = s.t[ihash]; ok != true {
				return fmt.Errorf("scrape: ihash %v doesnt exist in tracker db", ihash)
			}
			scrapeResp[i] = t.stats
		}
		err = s.respond(addr, respHeader{
			actionScrape,
			h.TxID,
		}, scrapeResp)
		return err
	default:
		s.respond(addr, respHeader{
			h.TxID,
			actionError,
		}, []byte("unhandled action"))
		return fmt.Errorf("unhandled action: %d", h.Action)
	}
}

func (s *server) checkConnectionID(h reqHeader, addr net.Addr) error {
	if _, ok := s.conns[h.ConnID]; !ok {
		s.respond(addr, respHeader{
			TxID:   h.TxID,
			Action: actionError,
		}, []byte("not connected"))
		return fmt.Errorf("client %s requested without being connected", addr.String())
	}
	return nil
}
