package torrent

import (
	"math/rand"
	"net"
	"strconv"
	"time"

	"github.com/lkslts64/charo-torrent/peer_wire"
)

const clientID = "CH"
const version = "0001"

//Client manages multiple torrents
type Client struct {
	peerID [20]byte
	//torrents []Torrent
	//conns    []net.Conn
	torrents []*Torrent
	//torrents map[[20]byte]Torrent
	//a set of info_hashes that clients is
	//responsible for - easy access
	//this will be set in initialaztion
	//no  mutex needed
	//TODO:it is dupicate
	infoHashes map[[20]byte]struct{}
	//extensionsSupported map[string]int

	port int16
}

func NewClient(sources []string) (*Client, error) {
	var err error
	var peerID [20]byte
	rand.Read(peerID[:])
	torrents := make([]*Torrent, len(sources))
	for i, s := range sources {
		torrents[i], err = NewTorrent(s)
		if err != nil {
			return nil, err
		}
	}
	infoHashes := make(map[[20]byte]struct{})
	for _, t := range torrents {
		infoHashes[t.tm.meta.Info.Hash] = struct{}{}
	}
	return &Client{
		peerID:     peerID,
		torrents:   torrents,
		infoHashes: infoHashes,
	}, nil

}

//6881-6889
func (c *Client) listen() error {
	var i int16
	var err error
	var ln net.Listener
	for i = 6881; i < 6890; i++ {
		ln, err = net.Listen("tcp", ":"+strconv.Itoa(int(i)))
		if err == nil {
			break
		}
	}
	c.port = i
	for {
		conn, err := ln.Accept()
		if err != nil {
			//TODO: handle different
			return err
		}
		go c.handleConn(conn)
	}
}

func (c *Client) handleConn(conn net.Conn) {
	hs := &peer_wire.HandShake{
		Reserved: [8]byte{5: 0x01}, //support extension proto
		//InfoHash: [20]byte{},
		PeerID: c.peerID,
	}
	err := hs.Receipt(conn,c.infoHashes)
	if err != nil {
		conn.Close()
	}
	var index int
	for i, t := range c.torrents {
		if t.tm.meta.Info.Hash == hs.InfoHash {
			index = i
			break
		}
	}

}
