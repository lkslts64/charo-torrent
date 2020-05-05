package torrent

import (
	"io"
	"log"
	"net"
	"os"
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
	cl, err := NewClient(testingConfig())
	require.NoError(t, err)
	tr := newTorrent(cl)
	cn := newConn(tr, r, Peer{})
	go cn.mainLoop()
	numPieces := 100
	bf := peer_wire.NewBitField(numPieces)
	bf.SetPiece(7)
	bf.SetPiece(91)
	w.Write((&peer_wire.Msg{
		Kind: peer_wire.Bitfield,
		Bf:   bf,
	}).Encode())
	//get events from eventCH
	e := <-cn.sendC
	bm := e.(bitmap.Bitmap)
	assert.Equal(t, 2, bm.Len())
	assert.Equal(t, bm.Get(7), true)
	assert.Equal(t, bm.Get(91), true)
	for i := 0; i < 30*2; i += 2 {
		w.Write((&peer_wire.Msg{
			Kind:  peer_wire.Have,
			Index: uint32(i),
		}).Encode())
	}
	for i := 0; i < 30*2; i += 2 {
		e := <-cn.sendC
		msg := e.(*peer_wire.Msg)
		assert.Equal(t, peer_wire.Have, msg.Kind)
		assert.EqualValues(t, i, msg.Index)
	}
	assert.Equal(t, 30+2, cn.peerBf.Len())
}

func TestConnState(t *testing.T) {
	w, r := net.Pipe()
	cl, err := NewClient(testingConfig())
	require.NoError(t, err)
	tr := newTorrent(cl)
	cn := newConn(tr, r, Peer{})
	go cn.mainLoop()
	go readForever(w)
	//we dont expect conn to send an event since state didn't change
	w.Write((&peer_wire.Msg{
		Kind: peer_wire.NotInterested,
	}).Encode())
	//now we expect
	w.Write((&peer_wire.Msg{
		Kind: peer_wire.Unchoke,
	}).Encode())
	cn.recvC <- &peer_wire.Msg{
		Kind: peer_wire.Interested,
	}
	e := <-cn.sendC
	msg := e.(*peer_wire.Msg)
	assert.Equal(t, peer_wire.Unchoke, msg.Kind)
}

type dummyStorage struct{}

func (ds dummyStorage) ReadBlock(b []byte, off int64) (n int, err error) {
	n = len(b)
	return
}

func (ds dummyStorage) WriteBlock(b []byte, off int64) (n int, err error) {
	n = len(b)
	return
}

func (ds dummyStorage) HashPiece(pieceIndex int, len int) (correct bool) {
	correct = true
	return
}

//The last asertion may fail but no fuss because it just tests how efficient
//is our cancellation mechanism.
func TestPeerRequestAndCancel(t *testing.T) {
	w, r := net.Pipe()
	//we need to read the Piece msgs that r produces (see net.Pipe docs)
	go readForever(w)
	cn, tr, err := loadTorrentFile(t, w, r, "../metainfo/testdata/archlinux-2011.08.19-netinstall-i686.iso.torrent")
	require.NoError(t, err)
	tr.storage = dummyStorage{}
	allowUpload(cn, w)
	numPieces := 200
	//If a piece is uploaded we ll get notified at sendC.But if we send
	//a Cancel for a Request, then we dont know if the conn will upload the
	//block or it will se the Cancel first and ignore it. So, we are not sure
	//about how many values sendC will send (which is not wanted).As a
	//workaround we send a Cancel for a piece that we don't own so a value
	//won't be send to sendC in all cases.
	for i := 0; i < numPieces; i++ {
		if i == numPieces-2 { //skip this piece, we 'll send Cancel for it
			continue
		}
		cn.myBf.Set(i, true)
	}
	count := 0
	ch := make(chan struct{})
	go func() {
		for e := range cn.sendC {
			switch e.(type) {
			case uploadedBlock:
			default:
				t.Fail()
			}
			count++
			if count >= numPieces-2 {
				close(ch)
				return
			}
		}
	}()
	for i := 0; i < numPieces-1; i++ {
		w.Write((&peer_wire.Msg{
			Kind:  peer_wire.Request,
			Index: uint32(i),
			Len:   1 << 14,
		}).Encode())
		//send cancel for the piece we dont have
		if i == numPieces-2 {
			w.Write((&peer_wire.Msg{
				Kind:  peer_wire.Cancel,
				Index: uint32(i),
				Len:   1 << 14,
			}).Encode())
		}
	}
	<-ch
	assert.Nil(t, tr.cl.counters.Get("latecomerCancels"))
}

func BenchmarkPeerPieceMsg(b *testing.B) {
	w, r := net.Pipe()
	cn, tr, err := loadTorrentFile(b, w, r, "testdata/blockchain.torrent")
	tr.storage = dummyStorage{}
	tr.blockRequestSize = tr.blockSize()
	tr.pieces = newPieces(tr)
	require.NoError(b, err)
	cn.state.isChoking, cn.state.amInterested = false, true
	msg := &peer_wire.Msg{
		Kind:  peer_wire.Piece,
		Block: make([]byte, 1<<14),
	}
	msgBytes := msg.Encode()
	require.NoError(b, err)
	b.SetBytes(int64(len(msg.Block)))
	var n int
	//send unexpected blocks to conn
	for i := 0; i < b.N; i++ {
		n, err = w.Write(msgBytes)
		require.NoError(b, err)
		assert.Equal(b, n, len(msgBytes))
		<-cn.sendC
	}
}

func readForever(r io.Reader) {
	b := make([]byte, 1000)
	for {
		_, err := r.Read(b)
		if err == io.EOF {
			break
		}
	}
}

func loadTorrentFile(t testing.TB, w, r net.Conn, filename string) (*conn, *Torrent, error) {
	cfg := testingConfig()
	cfg.RejectIncomingConnections = true
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	tr := newTorrent(cl)
	cn := newConn(tr, r, Peer{})
	tr.mi, err = metainfo.LoadMetainfoFile(filename)
	require.NoError(t, err)
	tr.logger = log.New(os.Stdout, "test_logger", log.LstdFlags)
	cn.recvC <- haveInfo{}
	go func() {
		require.NoError(t, cn.mainLoop())
	}()
	return cn, tr, nil
}

func allowUpload(cn *conn, w net.Conn) {
	w.Write((&peer_wire.Msg{
		Kind: peer_wire.Interested,
	}).Encode())
	cn.recvC <- &peer_wire.Msg{
		Kind: peer_wire.Unchoke,
	}
	<-cn.sendC
}

func allowDownload(cn *conn, w net.Conn) {
	w.Write((&peer_wire.Msg{
		Kind: peer_wire.Unchoke,
	}).Encode())
	cn.recvC <- &peer_wire.Msg{
		Kind: peer_wire.Interested,
	}
	w.Read(make([]byte, 50))
	<-cn.sendC
}
