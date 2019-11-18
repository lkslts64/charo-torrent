package torrent

import (
	"net"
	"testing"
	"time"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//TODO: do not test extensively here, test when master is ready

func initPeerConn() *PeerConn {
	tr := &Torrent{
		tm: &TorrentMeta{
			mi: &metainfo.MetaInfo{},
		},
	}
	pc := &PeerConn{
		t:  tr,
		cl: &Client{},
	}
	tr.tm.setInfoDict(&metainfo.InfoDict{
		PieceLen: 1 << 20,
	})
	pc.init()
	return pc
}

func testSimpleJob(t *testing.T, pc *PeerConn, r net.Conn, msgSend peer_wire.Msg) {
	pc.jobCh <- job{&msgSend}
	msgRecv, err := peer_wire.Read(r)
	require.NoError(t, err)
	assert.Equal(t, msgRecv, &msgSend)
}

func TestRecvJob(t *testing.T) {
	pc := initPeerConn()
	w, r := net.Pipe()
	pc.conn = w
	go pc.mainLoop()
	testSimpleJob(t, pc, r, peer_wire.Msg{
		Kind: peer_wire.Interested,
	})
	//send a job and check if was written to conn
	testSimpleJob(t, pc, r, peer_wire.Msg{
		Kind: peer_wire.NotInterested,
	})
	//since we have not set the 'peerInfo.amInterested'
	//bit, we expect to get an unsatified request at eventCh
	msg := &peer_wire.Msg{
		Kind: peer_wire.Request,
		Len:  blockSz,
	}
	pc.jobCh <- job{msg}
	select {
	case req := <-pc.eventCh:
		assert.EqualValues(t, req.val.(*peer_wire.Msg), msg)
	case <-time.After(10 * time.Second):
		t.Fail()
	}
	//set peerInfo.amInterested
	testSimpleJob(t, pc, r, peer_wire.Msg{
		Kind: peer_wire.Interested,
	})
	extra := 4
	sendRequestJobs(pc, requestQueueChSize+extra)
	for i := 0; i < requestQueueChSize; i++ {
		_, err := peer_wire.Read(r)
		require.NoError(t, err)
	}
	//ensure last request is processed by mainLoop
	//by adding another dummy Read after it
	testSimpleJob(t, pc, r, peer_wire.Msg{
		Kind: peer_wire.Interested,
	})
	assert.Equal(t, extra, len(pc.reqQueue))
	assert.Equal(t, requestQueueChSize+extra, len(pc.pendingRequests))
}

func sendRequestJobs(pc *PeerConn, numReqs int) {
	for i := 0; i < numReqs; i++ {
		pc.jobCh <- job{&peer_wire.Msg{
			Kind: peer_wire.Request,
			Len:  blockSz,
		}}
	}
}

func TestRecvMsg(t *testing.T) {
	pc := initPeerConn()
	r, w := net.Pipe()
	pc.conn = r
	pc.info.amInterested = true
	go pc.mainLoop()
	//send msg to client - simulating a remote peer
	testSimpleRecvMsg(t, pc, isInterested, w, &peer_wire.Msg{
		Kind: peer_wire.Interested,
	})
	/*sendRequestJobs(pc, 1)
	_, err := peer_wire.Read(r)
	require.NoError(t, err)
	testSimpleRecvMsg(t, pc, isChoking, w, &peer_wire.Msg{
		Kind: peer_wire.Choke,
	})*/
}

func testSimpleRecvMsg(t *testing.T, pc *PeerConn, expect eventID, w net.Conn,
	msg *peer_wire.Msg) {
	msg.Write(w)
	v := <-pc.eventCh
	var actual eventID
	var ok bool
	if actual, ok = v.val.(eventID); !ok {
		t.Fail()
	} else {
		assert.Equal(t, expect, actual)
	}
}
