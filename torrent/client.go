package torrent

import (
	"errors"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/tracker"
)

const clientID = "CH"
const version = "0001"

var reserved [8]byte

//Client manages multiple torrents
type Client struct {
	config *Config
	peerID [20]byte
	logger *log.Logger
	//torrents []Torrent
	//conns    []net.Conn
	torrents map[[20]byte]*Torrent
	//torrents map[[20]byte]Torrent
	//a set of info_hashes that clients is
	//responsible for - easy access
	//this will be set in initialaztion
	//no  mutex needed
	//TODO:it is dupicate
	infoHashes map[[20]byte]struct{}
	//extensionsSupported map[string]int
	listener net.Listener
	port     int16
}

type Config struct {
	maxOnFlightReqs int //max outstanding requests we allowed for a peer to have
	maxConns        int
	maxUploadSlots  int
	optimisticSlots int
	//directory to store the data
	baseDir string
}

func NewClient(cfg *Config) (*Client, error) {
	var err error
	if cfg == nil {
		cfg, err = defaultConfig()
		if err != nil {
			return nil, err
		}
	}
	cl := &Client{
		peerID:     newPeerID(),
		config:     cfg,
		logger:     log.New(os.Stdout, "client", log.LstdFlags),
		infoHashes: make(map[[20]byte]struct{}),
		torrents:   make(map[[20]byte]*Torrent),
	}
	cl.listen()
	go cl.accept()
	return cl, nil
}

func defaultConfig() (*Config, error) {
	tdir, err := ioutil.TempDir(os.TempDir(), "")
	if err != nil {
		return nil, err
	}
	return &Config{
		maxOnFlightReqs: 250,
		maxConns:        55,
		maxUploadSlots:  4,
		optimisticSlots: 1,
		baseDir:         tdir,
	}, nil
}

//NewTorrentFromFile creates a torrent based on a .torrent file
func (cl *Client) NewTorrentFromFile(filename string) (*Torrent, error) {
	t := newTorrent(cl)
	var err error
	if t.mi, err = metainfo.LoadMetainfoFile(filename); err != nil {
		return nil, err
	}
	cl.infoHashes[t.mi.Info.Hash] = struct{}{}
	cl.torrents[t.mi.Info.Hash] = t
	//TODO: find another way of getting the info bytes, it is expensive
	//to read and decode the file twice
	if t.infoRaw, err = t.mi.Info.Bytes(filename); err != nil {
		return nil, err
	}
	t.gotInfo()
	if t.trackerURL, err = tracker.NewTrackerURL(t.mi.Announce); err != nil {
		return nil, err
	}
	go t.mainLoop()
	return t, nil
}

/*func NewClient(sources []string) (*Client, error) {
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
		infoHashes[t.tm.mi.Info.Hash] = struct{}{}
	}
	return &Client{
		peerID:     peerID,
		torrents:   torrents,
		infoHashes: infoHashes,
	}, nil

}
*/

//6881-6889
func (cl *Client) listen() error {
	var i int16
	var err error
	//var ln net.Listener
	for i = 6881; i < 6890; i++ {
		cl.listener, err = net.Listen("tcp", ":"+strconv.Itoa(int(i)))
		if err == nil {
			cl.port = i
			return nil
		}
	}
	return errors.New("could not find port to listen")
}

func (cl *Client) accept() error {
	for {
		conn, err := cl.listener.Accept()
		if err != nil {
			//TODO: handle different
			cl.logger.Printf("error accepting conn")
			continue
		}
		go cl.handleConn(conn)
	}
}

func (cl *Client) handleConn(tcpConn net.Conn) {
	hs := &peer_wire.HandShake{
		Reserved: reserved,
		PeerID:   cl.peerID,
	}
	err := cl.handshake(tcpConn, hs)
	if err != nil {
		return
	}
	var (
		t  *Torrent
		ok bool
	)
	if t, ok = cl.torrents[hs.InfoHash]; !ok {
		panic("we checked that we have this torrent")
	}
	cl.startConn(t, tcpConn)
}

func (cl *Client) connectToPeer(address string, t *Torrent) {
	tcpConn, err := net.Dial("tcp", address)
	if err != nil {
		cl.logger.Printf("cannot dial peer: %s", err)
	}
	err = cl.handshake(tcpConn, &peer_wire.HandShake{
		Reserved: reserved,
		PeerID:   cl.peerID,
		InfoHash: t.mi.Info.Hash,
	})
	if err != nil {
		return
	}
	tcpConn.SetDeadline(time.Time{})
	cl.startConn(t, tcpConn)
}

func (cl *Client) startConn(t *Torrent, tcpConn net.Conn) {
	newConn(t, tcpConn, cl.peerID[:]).mainLoop()
}

func (cl *Client) connectToPeers(t *Torrent, peers ...tracker.Peer) {
	for _, peer := range peers {
		go cl.connectToPeer(peer.String(), t)
	}
}

func (cl *Client) addr() string {
	return cl.listener.Addr().String()
}

func (cl *Client) handshake(tcpConn net.Conn, hs *peer_wire.HandShake) error {
	//dont wait more than 30 secs for handshake
	tcpConn.SetDeadline(time.Now().Add(30 * time.Second))
	err := hs.Do(tcpConn, cl.infoHashes)
	if err != nil {
		cl.logger.Println(err)
		tcpConn.Close()
	}
	return err
}
