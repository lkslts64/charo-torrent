package torrent

import (
	"net"
	"testing"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//clients like Deluge dont send all pieces that they have in Bitfield message.
//Instead, they send a portion of them in Bitfield and the remaining ones are sent
//via Have messages.
func TestConnBitfieldThenHaveBombardism(t *testing.T) {
	w, r := net.Pipe()
	tr := newTorrent(&Client{})
	cn := newConn(tr, r, make([]byte, 20), 250)
	//make eventCh too big so we dont block
	go cn.mainLoop()
	numPieces := 100
	bf := peer_wire.NewBitField(numPieces)
	bf.SetPiece(7)
	bf.SetPiece(91)
	(&peer_wire.Msg{
		Kind: peer_wire.Bitfield,
		Bf:   bf,
	}).Write(w)
	//get events from eventCH
	e := <-cn.eventCh
	bm := e.(bitmap.Bitmap)
	assert.Equal(t, 2, bm.Len())
	assert.Equal(t, bm.Get(7), true)
	assert.Equal(t, bm.Get(91), true)
	for i := 0; i < 30*2; i += 2 {
		(&peer_wire.Msg{
			Kind:  peer_wire.Have,
			Index: uint32(i),
		}).Write(w)
	}
	for i := 0; i < 30*2; i += 2 {
		e := <-cn.eventCh
		msg := e.(*peer_wire.Msg)
		assert.Equal(t, peer_wire.Have, msg.Kind)
		assert.EqualValues(t, i, msg.Index)
	}
	assert.Equal(t, 30+2, cn.peerBf.Len())
}

func TestConnState(t *testing.T) {
	w, r := net.Pipe()
	tr := newTorrent(&Client{})
	cn := newConn(tr, r, make([]byte, 20), 250)
	go cn.mainLoop()
	//we dont expect conn to send an event since state didn't change
	(&peer_wire.Msg{
		Kind: peer_wire.NotInterested,
	}).Write(w)
	//now we expect
	(&peer_wire.Msg{
		Kind: peer_wire.Unchoke,
	}).Write(w)
	cn.jobCh.In() <- job{&peer_wire.Msg{
		Kind: peer_wire.Interested,
	}}
	//read here is mandatory, see net.Pipe() docs
	w.Read(make([]byte, 100))
	e := <-cn.eventCh
	msg := e.(*peer_wire.Msg)
	assert.Equal(t, peer_wire.Unchoke, msg.Kind)
	e = <-cn.eventCh
	switch e.(type) {
	case wantBlocks:
	default:
		t.Fail()
	}
}

//load a torrent file and change conn's state to uploadable
//TODO:implement storage for this
func BenchmarkPeerRequest(b *testing.B) {
	w, r := net.Pipe()
	cn, tr, err := loadFileThenPrepareUpload(w, r, "../metainfo/testdata/archlinux-2011.08.19-netinstall-i686.iso.torrent")
	require.NoError(b, err)
	for i := 0; i < tr.mi.Info.NumPieces(); i++ {
		cn.myBf.Set(i, true)
	}
	//conn will produce an event from every request so if we write all requests
	//without interploation of rcvEvents eventChan will block.
	for i := 0; i < b.N; i++ {
		start := 0
		for start+eventChSize < tr.mi.Info.NumPieces() {
			writeRequests(w, start, eventChSize)
			if !rcvEvents(cn.eventCh, start, eventChSize) {
				b.Fail()
			}
			start += eventChSize
		}
	}
}

//just ensure cancelation doesn't have any obvious problems
func TestRequestAndCancel(t *testing.T) {
	w, r := net.Pipe()
	cn, tr, err := loadFileThenPrepareUpload(w, r, "../metainfo/testdata/oliver-koletszki-Schneeweiss11.torrent")
	require.NoError(t, err)
	for i := 0; i < tr.mi.Info.NumPieces(); i++ {
		cn.myBf.Set(i, true)
	}
	start := 0
	writeRequestsThenCancel(w, start, eventChSize)
}

func loadFileThenPrepareUpload(w, r net.Conn, filename string) (*conn, *Torrent, error) {
	tr := newTorrent(&Client{})
	cn := newConn(tr, r, make([]byte, 20), 250)
	var err error
	tr.mi, err = metainfo.LoadMetainfoFile(filename)
	if err != nil {
		return nil, nil, err
	}
	//signal conn that we have the info
	close(cn.haveInfoCh)
	go cn.mainLoop()
	allowUpload := func() {
		(&peer_wire.Msg{
			Kind: peer_wire.Interested,
		}).Write(w)
		cn.jobCh.In() <- job{&peer_wire.Msg{
			Kind: peer_wire.Unchoke,
		}}
		w.Read(make([]byte, 100))
		<-cn.eventCh
	}
	allowUpload()
	return cn, tr, nil
}

func writeRequestsThenCancel(w net.Conn, start, n int) {
	writeRequests(w, start, n)
	cancelRequests(w, start, n)
}

func writeRequests(w net.Conn, start, n int) {
	for i := start; i < start+n; i++ {
		(&peer_wire.Msg{
			Kind:  peer_wire.Request,
			Index: uint32(i),
			Len:   1 << 14,
			Begin: 0,
		}).Write(w)
	}
}

func cancelRequests(w net.Conn, start, n int) {
	for i := start; i < start+n; i++ {
		(&peer_wire.Msg{
			Kind:  peer_wire.Cancel,
			Index: uint32(start + n - 1),
			Len:   1 << 14,
			Begin: 0,
		}).Write(w)
	}
}

//returns false on failure
func rcvEvents(ch chan interface{}, start, n int) bool {
	for i := start; i < start+n; i++ {
		e := <-ch
		switch e.(type) {
		case uploadedBlock:
		default:
			return false
		}
	}
	return true
}

//TODO: test piece msgs reading
