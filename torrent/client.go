package torrent

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/torrent/storage"
	"github.com/lkslts64/charo-torrent/tracker"
)

const clientID = "CH"
const version = "0001"
const logFileName = "charo.log"

//Client manages multiple torrents
type Client struct {
	config   *Config
	peerID   [20]byte
	logger   *log.Logger
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
	//when this channel closes, all Torrents and conns that the client is managing will close.
	//close                   chan chan struct{}
	trackerAnnouncer *trackerAnnouncer
	dhtServer        *dht.Server
	//the reserved bytes we'll send at every handshake
	reserved                peer_wire.Reserved
	trackerAnnouncerCloseCh chan chan struct{}
	port                    int
}

//Config provides configuration for a Client.
type Config struct {
	//max outstanding requests per connection we allow for a peer to have
	MaxOnFlightReqs     int
	MaxEstablishedConns int
	//This option disables DHT also.
	RejectIncomingConnections bool
	DisableTrackers           bool
	DisableDHT                bool
	//directory to store the data
	BaseDir     string
	OpenStorage storage.Open
}

//NewClient creats a fresh new Client with the provided configuration.
//Use `NewClient(nil)` for the default configuration.
func NewClient(cfg *Config) (*Client, error) {
	var err error
	if cfg == nil {
		cfg, err = DefaultConfig()
		if err != nil {
			return nil, err
		}
	}
	logFile, err := os.Create(path.Join(os.TempDir(), logFileName))
	if err != nil {
		return nil, err
	}
	cl := &Client{
		peerID:     newPeerID(),
		config:     cfg,
		infoHashes: make(map[[20]byte]struct{}),
		torrents:   make(map[[20]byte]*Torrent),
	}
	logPrefix := fmt.Sprintf("client%x ", cl.peerID[14:len(cl.peerID)]) //last 6 bytes of peerID
	cl.logger = log.New(logFile, logPrefix, log.LstdFlags)
	if !cl.config.RejectIncomingConnections {
		go func() {
			log.Fatal(cl.listenAndAccept())
		}()
	} else {
		cl.config.DisableDHT = true
	}
	if !cl.config.DisableTrackers {
		cl.trackerAnnouncer = &trackerAnnouncer{
			cl:                            cl,
			trackerAnnouncerSubmitEventCh: make(chan trackerAnnouncerEvent, 5),
			trackers:                      make(map[string]tracker.TrackerURL),
		}
		go cl.trackerAnnouncer.run()
	}
	if !cl.config.DisableDHT {
		cl.reserved.SetDHT()
		if cl.dhtServer, err = dht.NewServer(nil); err == nil {
			go func() {
				ts, err := cl.dhtServer.Bootstrap()
				if err != nil {
					cl.logger.Printf("error bootstrapping dht: %s", err)
				}
				cl.logger.Printf("dht bootstrap complete with stats %v", ts)
			}()
		} else {
			cl.logger.Fatalf("error creating dht server: %s", err)
		}
	}
	return cl, nil
}

//Close drops all Torrents that the client manages
func (cl *Client) Close() {
	torrents := cl.Torrents()
	chanArr := make([]chan struct{}, len(torrents))
	for i := 0; i < len(chanArr); i++ {
		chanArr[i] = make(chan struct{}, 1)
	}
	//signal all torents to close
	for i, t := range torrents {
		t.close <- chanArr[i]
	}
	//wait until all torrents actually close
	for _, ch := range chanArr {
		<-ch
	}
}

//Returns the default configuration for a client
func DefaultConfig() (*Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &Config{
		MaxOnFlightReqs:     250,
		MaxEstablishedConns: 55,
		BaseDir:             dir,
		OpenStorage:         storage.OpenFileStorage,
	}, nil
}

//AddFromFile creates a torrent based on the contents of filename.
//The Torrent returned maybe be in seeding mode if all the data is already downloaded.
func (cl *Client) AddFromFile(filename string) (*Torrent, error) {
	t := newTorrent(cl)
	var err error
	if t.mi, err = metainfo.LoadMetainfoFile(filename); err != nil {
		return nil, err
	}
	cl.addToMaps(t)
	t.gotInfo()
	return t, nil
}

//AddFromMagnet creates a torrent based on the magnet link provided
func (cl *Client) AddFromMagnet(uri string) (*Torrent, error) {
	t := newTorrent(cl)
	//TODO:change with parseMagnet
	cl.addToMaps(t)
	return t, nil
}

//AddFromInfoHash creates a torrent based on it's infohash.
func (cl *Client) AddFromInfoHash(infohash [20]byte) (*Torrent, error) {
	t := newTorrent(cl)
	t.mi = &metainfo.MetaInfo{
		Info: &metainfo.InfoDict{
			Hash: infohash,
		},
	}
	cl.addToMaps(t)
	return t, nil
}

func (cl *Client) addToMaps(t *Torrent) error {
	ihash := t.mi.Info.Hash
	if _, ok := cl.infoHashes[ihash]; ok {
		return errors.New("torrent already exists")
	}
	cl.infoHashes[ihash] = struct{}{}
	cl.torrents[ihash] = t
	return nil
}

//Torrents returns all torrents that the client manages.
func (cl *Client) Torrents() []*Torrent {
	ts := []*Torrent{}
	for _, t := range cl.torrents {
		ts = append(ts, t)
	}
	return ts
}

func (cl *Client) dhtPort() uint16 {
	_, _port, err := net.SplitHostPort(cl.dhtServer.Addr().String())
	if err != nil {
		panic(err)
	}
	port, err := strconv.ParseUint(_port, 10, 16)
	if err != nil {
		panic(err)
	}
	return uint16(port)
}

func (cl *Client) listenAndAccept() error {
	var err error
	//try ports 6881-6889 first
	for i := 6881; i < 6890; i++ {
		//we dont support IPv6
		cl.listener, err = net.Listen("tcp4", ":"+strconv.Itoa(int(i)))
		if err == nil {
			cl.port = i
			return nil
		}
	}
	//if none of the above ports were avaialable, try other ones.
	if cl.listener, err = net.Listen("tcp4", ":"); err != nil {
		return errors.New("could not find port to listen")
	}
	_, port, err := net.SplitHostPort(cl.listener.Addr().String())
	if err != nil {
		return err
	}
	cl.port, err = strconv.Atoi(port)
	if err != nil {
		return err
	}
	for {
		conn, err := cl.listener.Accept()
		if err != nil {
			return err
		}
		go cl.handleConn(conn)
	}
}

func (cl *Client) handleConn(tcpConn net.Conn) {
	hs := &peer_wire.HandShake{
		Reserved: cl.reserved,
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
	err = newConn(t, tcpConn, cl.peerID[:]).mainLoop()
	if err != nil {
		cl.logger.Println(err)
	}
}

//TODO: store the remote addr and pop when finish
func (cl *Client) connectToPeer(address string, t *Torrent) {
	//cl.logger.Printf("new conn at connectPeer with address %s\n", address)
	tcpConn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		cl.logger.Printf("cannot dial peer: %s", err)
		return
	}
	defer tcpConn.Close()
	err = cl.handshake(tcpConn, &peer_wire.HandShake{
		Reserved: cl.reserved,
		PeerID:   cl.peerID,
		InfoHash: t.mi.Info.Hash,
	})
	if err != nil {
		cl.logger.Printf("handshake: %s", err)
		return
	}
	err = newConn(t, tcpConn, cl.peerID[:]).mainLoop()
	if err != nil {
		cl.logger.Println(err)
	}
}

func (cl *Client) connectToPeers(t *Torrent, addresses ...string) {
	for _, addr := range addresses {
		go cl.connectToPeer(addr, t)
	}
}

func (cl *Client) addr() string {
	return cl.listener.Addr().String()
}

func (cl *Client) handshake(tcpConn net.Conn, hs *peer_wire.HandShake) error {
	//dont wait more than 5 secs for handshake
	tcpConn.SetDeadline(time.Now().Add(5 * time.Second))
	defer tcpConn.SetDeadline(time.Time{})
	return hs.Do(tcpConn, cl.infoHashes)
}
