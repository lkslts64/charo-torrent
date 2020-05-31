package torrent

import (
	"errors"
	"net"
	"strconv"

	"github.com/lkslts64/charo-torrent/peer_wire"
)

type dialer struct {
	cl   *Client
	t    *Torrent
	peer Peer
}

func maybeSendExtHandshake(c *conn, t *Torrent) error {
	if !c.reserved.SupportExtended() || !c.cl.reserved.SupportExtended() {
		return nil
	}
	var infoSize int64
	if ut := t.utmetadata.correct(); ut != nil {
		infoSize = int64(len(ut.infoBytes))
	}
	return c.sendMsgToPeer(extensionHandshakeMsg(infoSize), true)
}

func (d *dialer) dial() (*conn, error) {
	var err error
	defer d.t.removeHalfOpen(d.peer.P.String())
	tcpConn, err := net.DialTimeout("tcp", d.peer.P.String(), d.cl.config.DialTimeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tcpConn.Close()
		}
	}()
	hs, err := d.cl.handshake(tcpConn, &peer_wire.HandShake{
		Reserved: d.cl.reserved,
		PeerID:   d.cl.peerID,
		InfoHash: d.t.mi.Info.Hash,
	}, d.peer)
	if err != nil {
		return nil, err
	}
	c := newConn(d.t, tcpConn, d.peer, hs.Reserved)
	return c, maybeSendExtHandshake(c, d.t)
}

type listener interface {
	Accept() (*conn, error)
	Close() error
	Addr() net.Addr
}

type btListener struct {
	l  net.Listener
	cl *Client
}

func listen(cl *Client) (*btListener, error) {
	l := &btListener{
		cl: cl,
	}
	var err error
	//try ports 6881-6889 first
	for i := 6881; i < 6890; i++ {
		//we dont support IPv6
		l.l, err = net.Listen("tcp4", ":"+strconv.Itoa(int(i)))
		if err == nil {
			l.cl.port = i
			return l, nil
		}
	}
	//if none of the above ports were avaialable, try other ones.
	if l.l, err = net.Listen("tcp4", ":"); err != nil {
		return nil, errors.New("could not find port to listen")
	}
	ap, err := parseAddr(l.l.Addr().String())
	if err != nil {
		return nil, err
	}
	l.cl.port = int(ap.port)
	return l, nil
}

func (btl *btListener) Close() error {
	return btl.l.Close()
}

func (btl *btListener) Addr() net.Addr {
	return btl.l.Addr()
}

func (btl *btListener) Accept() (*conn, error) {
	var err error
	tcpConn, err := btl.l.Accept()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tcpConn.Close()
		}
	}()
	peer := addrToPeer(tcpConn.RemoteAddr().String(), SourceIncoming)
	hs, err := btl.cl.handshake(tcpConn, &peer_wire.HandShake{
		Reserved: btl.cl.reserved,
		PeerID:   btl.cl.peerID,
	}, peer)
	if err != nil {
		return nil, err
	}
	btl.cl.mu.Lock()
	t, ok := btl.cl.torrents[hs.InfoHash]
	btl.cl.mu.Unlock()
	if !ok {
		return nil, errors.New("peer handshake contain infohash that client doesn't manage")
	}
	c := newConn(t, tcpConn, peer, hs.Reserved)
	return c, maybeSendExtHandshake(c, t)
}
