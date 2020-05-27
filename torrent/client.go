package torrent

import (
	"bytes"
	"errors"
	"expvar"
	"fmt"
	"log"
	"net"
	"os"
	"path"
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

	listener         listener
	trackerAnnouncer *trackerAnnouncer
	dhtServer        *dht.Server
	//the reserved bytes we'll send at every handshake
	reserved               peer_wire.Reserved
	trackerAnnouncerCloseC chan chan struct{}
	port                   int
	counters               *expvar.Map
	mu                     sync.Mutex //guards following
	blackList              []net.IP
}

//Config provides configuration for a Client.
type Config struct {
	//Returns a new PieceSelector instantiated for each torrent the client manages
	SelectorF func() PieceSelector
	//Max outstanding requests per connection we allow for a peer to have
	MaxOnFlightReqs int
	//Max active/established connections per torrent
	MaxEstablishedConns int
	//This option disables DHT also.
	RejectIncomingConnections bool
	DisableTrackers           bool
	DisableDHT                bool
	//Directory to store the data
	BaseDir string
	//Function to open the storage.Provide your own for a custom storage implementation
	OpenStorage storage.Open
	//Dials for new connections will fail after this duration
	DialTimeout time.Duration
	//BitTorrent handshakes will fail after this duration
	HandshakeTiemout time.Duration
}

//NewClient creates a new Client with the provided configuration.
//Use `NewClient(nil)` for the default configuration.
func NewClient(cfg *Config) (*Client, error) {
	var err error
	if cfg == nil {
		cfg, err = DefaultConfig()
		if err != nil {
			return nil, err
		}
	}
	cl := &Client{
		peerID:    newPeerID(),
		config:    cfg,
		close:     make(chan struct{}),
		torrents:  make(map[[20]byte]*Torrent),
		blackList: make([]net.IP, 0),
	}
	cl.counters = expvar.NewMap("counters" + string(cl.peerID[:]))
	logPrefix := fmt.Sprintf("client%x ", cl.peerID[14:]) //last 6 bytes of peerID
	logFile, err := os.Create(path.Join(os.TempDir(), logFileName+logPrefix))
	if err != nil {
		return nil, err
	}
	cl.logger = log.New(logFile, logPrefix, log.LstdFlags)
	if !cl.config.RejectIncomingConnections {
		if cl.listener, err = listen(cl); err != nil {
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
			trackers:                      make(map[string]*trackerURL),
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

//Close calls torrent.Close for all the torrents managed by the client.
func (cl *Client) Close() {
	close(cl.close)
	if cl.dhtServer != nil {
		cl.dhtServer.Close()
	}
	wg := sync.WaitGroup{}
	wg.Add(len(cl.torrents))
	for _, t := range cl.Torrents() {
		go func(t *Torrent) {
			defer wg.Done()
			t.Close()
		}(t)
	}
	wg.Wait()
}

//DefaultConfig returns the default configuration for a client
func DefaultConfig() (*Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &Config{
		SelectorF:           NewDefaultPieceSelector,
		MaxOnFlightReqs:     250,
		MaxEstablishedConns: 55,
		BaseDir:             dir,
		OpenStorage:         storage.OpenFileStorage,
		DialTimeout:         5 * time.Second,
		HandshakeTiemout:    4 * time.Second,
	}, nil
}

//AddFromFile creates a torrent based on the contents of filename.
//The Torrent returned maybe be in seeding mode if all the data is already downloaded.
func (cl *Client) AddFromFile(filename string) (*Torrent, error) {
	t, err := cl.add(&metainfo.FileParser{
		Filename: filename,
	})
	if err != nil {
		return nil, err
	}
	t.gotInfo()
	go t.mainLoop()
	return t, nil
}

//AddFromMagnet creates a torrent based on the magnet link provided (not implemented yet)
func (cl *Client) AddFromMagnet(uri string) (*Torrent, error) {
	t, err := cl.add(&metainfo.MagnetParser{
		URI: uri,
	})
	if err != nil {
		return nil, err
	}
	go t.mainLoop()
	if err = <-t.InfoC; err != nil {
		return nil, err
	}
	return t, nil
}

//AddFromInfoHash creates a torrent based on it's infohash (not implemented yet)
func (cl *Client) AddFromInfoHash(infohash [20]byte) (*Torrent, error) {
	t, err := cl.add(&metainfo.InfoHashParser{
		InfoHash: infohash,
	})
	if err != nil {
		return nil, err
	}
	go t.mainLoop()
	if err = <-t.InfoC; err != nil {
		return nil, err
	}
	return t, nil
}

func (cl *Client) add(p metainfo.Parser) (*Torrent, error) {
	var err error
	t := newTorrent(cl)
	t.mi, err = p.Parse()
	if err != nil {
		return nil, err
	}
	t.gotInfoHash()
	ihash := t.mi.Info.Hash
	if _, ok := cl.torrents[ihash]; ok {
		return nil, errors.New("torrent already exists")
	}
	cl.torrents[ihash] = t
	return t, nil
}

//dropTorrent removes the Torrent the  torrent with infohash `infohash`.
func (cl *Client) dropTorrent(infohash [20]byte) error {
	if _, ok := cl.torrents[infohash]; !ok {
		return errors.New("torrent doesn't exist")
	}
	delete(cl.torrents, infohash)
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

//ListenPort returns the port that the client listens for new connections
func (cl *Client) ListenPort() int {
	return cl.port
}

//ID returns the Client's random ID
func (cl *Client) ID() []byte {
	return cl.peerID[:]
}

func (cl *Client) addTorrent(t *Torrent) error {
	ihash := t.mi.Info.Hash
	if _, ok := cl.torrents[ihash]; ok {
		return errors.New("torrent already exists")
	}
	cl.torrents[ihash] = t
	return nil
}

func (cl *Client) dhtPort() uint16 {
	ap, err := parseAddr(cl.dhtServer.Addr().String())
	if err != nil {
		panic(err)
	}
	return ap.port
}

//TODO: don't leak this goroutine on client.Close
func (cl *Client) acceptForEver() error {
	for {
		conn, err := cl.listener.Accept()
		if err != nil {
			cl.logger.Println(err)
			continue
		}
		go cl.runConnection(conn)
	}
}

func addrToPeer(address string, source PeerSource) Peer {
	ap, err := parseAddr(address)
	if err != nil {
		panic(err)
	}
	return Peer{
		P: tracker.Peer{
			IP:   ap.ip,
			Port: ap.port,
		},
		Source: source,
	}
}

//TODO: store the remote addr and pop when finish
func (cl *Client) runConnection(c *conn) {
	var err error
	defer func() {
		if err != nil {
			cl.logger.Println(err)
		}
		c.cn.Close()
		cl.counters.Add("closed connections", 1)
	}()
	if err = c.informTorrent(); err != nil {
		return
	}
	err = c.mainLoop()
	if c.ban {
		cl.banIP(c.peer.P.IP)
	}
}

func (cl *Client) banIP(ip net.IP) {
	cl.counters.Add("banned IPs", 1)
	cl.logger.Printf("banned ip %s", ip.String())
	cl.mu.Lock()
	cl.blackList = append(cl.blackList, ip)
	cl.mu.Unlock()
}

func (cl *Client) makeOutgoingConnection(t *Torrent, peer Peer) {
	c, err := (&dialer{
		cl:   cl,
		t:    t,
		peer: peer,
	}).dial()
	if err != nil {
		cl.counters.Add("could not dial", 1)
		return
	}
	cl.runConnection(c)
}

func (cl *Client) addr() string {
	return cl.listener.Addr().String()
}

func (cl *Client) handshake(tcpConn net.Conn, hs *peer_wire.HandShake, peer Peer) (*peer_wire.HandShake, error) {
	tcpConn.SetDeadline(time.Now().Add(cl.config.HandshakeTiemout))
	defer tcpConn.SetDeadline(time.Time{})
	hs, err := hs.Do(tcpConn)
	if err != nil {
		return nil, err
	}
	if peer.P.ID != nil && !bytes.Equal(peer.P.ID, hs.PeerID[:]) {
		return nil, errors.New("peer ID not compatible with the one tracker gave us")
	}
	return hs, nil
}
