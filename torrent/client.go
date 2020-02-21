package torrent

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"sync"
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
	close    chan struct{}
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
	MaxOnFlightReqs int
	//TODO: move to torrent
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
		close:      make(chan struct{}),
		infoHashes: make(map[[20]byte]struct{}),
		torrents:   make(map[[20]byte]*Torrent),
	}
	logPrefix := fmt.Sprintf("client%x ", cl.peerID[14:len(cl.peerID)]) //last 6 bytes of peerID
	cl.logger = log.New(logFile, logPrefix, log.LstdFlags)
	if !cl.config.RejectIncomingConnections {
		if err = cl.listen(); err != nil {
			cl.logger.Fatal(err)
		}
		go func() {
			log.Fatal(cl.acceptForEver())
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

//Close calls Remove for all the torrents managed by the client.
func (cl *Client) Close() {
	close(cl.close)
	if cl.dhtServer != nil {
		cl.dhtServer.Close()
	}
	wg := sync.WaitGroup{}
	wg.Add(len(cl.torrents))
	for ihash := range cl.torrents {
		go func(ihash [20]byte) {
			defer wg.Done()
			if err := cl.Remove(ihash); err != nil {
				panic(err)
			}
		}(ihash)
	}
	wg.Wait()
}

//DefaultConfig Returns the default configuration for a client
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

//Remove removes the Torrent the  torrent with infohash `infohash`.
func (cl *Client) Remove(infohash [20]byte) error {
	var (
		t  *Torrent
		ok bool
	)
	if t, ok = cl.torrents[infohash]; !ok {
		return errors.New("torrent doesn't exist")
	}
	ch := make(chan struct{})
	t.close <- ch
	<-ch
	delete(cl.torrents, infohash)
	return nil
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
	ap, err := parseAddr(cl.dhtServer.Addr().String())
	if err != nil {
		panic(err)
	}
	return ap.port
}

func (cl *Client) listen() error {
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
	ap, err := parseAddr(cl.listener.Addr().String())
	if err != nil {
		return err
	}
	cl.port = int(ap.port)
	return nil
}

func (cl *Client) acceptForEver() error {
	for {
		conn, err := cl.listener.Accept()
		if err != nil {
			return err
		}
		go cl.handleIncomingConnection(conn)
	}
}

func addrToPeer(address string, source PeerSource) Peer {
	ap, err := parseAddr(address)
	if err != nil {
		panic(err)
	}
	return Peer{
		tp: tracker.Peer{
			IP:   ap.ip,
			Port: ap.port,
		},
		source: source,
	}
}

func (cl *Client) handleIncomingConnection(tcpConn net.Conn) {
	defer tcpConn.Close()
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
	p := addrToPeer(tcpConn.RemoteAddr().String(), SourceIncoming)
	runConnection(t, tcpConn, p)
	if err != nil {
		cl.logger.Println(err)
	}
}

//TODO: store the remote addr and pop when finish
func (cl *Client) makeOutgoingConnection(t *Torrent, peer Peer) {
	tcpConn, err := net.DialTimeout("tcp", peer.tp.String(), 5*time.Second)
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
	//TODO:check if IDs match?
	runConnection(t, tcpConn, peer)
	if err != nil {
		cl.logger.Println(err)
	}
}

func runConnection(t *Torrent, conn net.Conn, peer Peer) error {
	return newConn(t, conn, peer).mainLoop()
}

func (cl *Client) makeOutgoingConnections(t *Torrent, peers ...Peer) {
	for _, peer := range peers {
		go cl.makeOutgoingConnection(t, peer)
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
