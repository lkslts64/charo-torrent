package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"reflect"
	"strconv"
	"time"
)

//TODO: support IPv6
var connectionIdExpiredErr = errors.New("connectionID expired")

const (
	actionConnect int32 = iota
	actionAnnounce
	actionScrape
	actionError

	protoID int64 = 0x41727101980
)

type respHeader struct {
	Action int32
	TxID   int32
}

type reqHeader struct {
	ConnID int64
	Action int32
	TxID   int32
}

type connectReq struct {
	ProtoID int64
	Action  int32
	TxID    int32
}

type udpScrapeInfo struct {
	Seeders   int32
	Completed int32
	Leechers  int32
}

type errResp struct {
	action int32
	txID   int32
	msg    string
}

func (t *UDPTracker) Announce(r AnnounceReq) (*AnnounceResp, error) {
	resp, err := t.announce(r)
	for errors.Is(err, connectionIdExpiredErr) {
		//connect again
		resp, err = t.announce(r)
	}
	if err != nil {
		return nil, fmt.Errorf("udp announce: %w", err)
	}
	return resp, nil
}

func (t *UDPTracker) Scrape(ihashes ...[20]byte) (*ScrapeResp, error) {
	resp, err := t.scrape(ihashes...)
	for errors.Is(err, connectionIdExpiredErr) {
		//connect again
		resp, err = t.scrape(ihashes...)
	}
	if err != nil {
		return nil, fmt.Errorf("udp scrape: %w", err)
	}
	return resp, nil

}

func (t *UDPTracker) connect() error {
	var err error
	if t.isConnected() {
		return nil
	}
	t.conn, err = net.Dial("udp", t.host)
	if err != nil {
		return errors.New("fail to dial connection with tracker")
	}
	txID := rand.Int31()
	err = t.writeRequest(connectReq{protoID, actionConnect, txID})
	if err != nil {
		return err
	}
	buf, err := t.readResponse(respHeader{actionConnect, txID})
	if err != nil {
		return err
	}
	var connID int64
	err = readFromBinary(buf, &connID)
	if err != nil {
		return err
	}
	t.connID = connID
	t.lastConncted = time.Now()
	return nil
}

func (t *UDPTracker) announce(req AnnounceReq) (*AnnounceResp, error) {
	err := t.connect()
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	txID := rand.Int31()
	err = t.writeRequest(struct {
		req  reqHeader
		data AnnounceReq
	}{
		reqHeader{t.connID, actionAnnounce, txID},
		req,
	})
	if err != nil {
		return nil, err
	}
	buf, err := t.readResponse(respHeader{actionAnnounce, txID})
	if err != nil {
		return nil, err
	}
	fixedData := struct {
		Interval int32
		Leechers int32
		Seeders  int32
	}{}
	var cheapPeers [][6]byte
	err = readFromBinary(buf, &fixedData, &cheapPeers)
	if err != nil {
		return nil, err
	}
	//transform fixed and cheapPeers to announceResp
	var ip net.IP
	peers := make([]Peer, len(cheapPeers))
	for i, pair := range cheapPeers {
		if ip = net.IP([]byte(pair[:4])).To16(); ip == nil {
			return nil, errors.New("cheapPeers ip parse")
		}
		port, err := strconv.Atoi(string(pair[4:]))
		if err != nil {
			return nil, fmt.Errorf("cheapPeers port parse: %w", err)
		}
		peers[i].IP = ip
		peers[i].Port = port
	}
	return &AnnounceResp{fixedData.Interval, fixedData.Leechers, fixedData.Seeders, peers, 0}, nil
}

func (t *UDPTracker) scrape(ihashes ...[20]byte) (*ScrapeResp, error) {
	err := t.connect()
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	txID := rand.Int31()
	err = t.writeRequest(reqHeader{t.connID, actionScrape, txID}, ihashes)
	if err != nil {
		return nil, err
	}
	buf, err := t.readResponse(respHeader{actionScrape, txID})
	if err != nil {
		return nil, err
	}
	var scrapeInfos []udpScrapeInfo
	err = readFromBinary(buf, &scrapeInfos)
	if err != nil {
		return nil, err
	}
	if len(scrapeInfos) != len(ihashes) {
		errors.New("number of info hashes requested is different from the ones in response")
	}
	torrents := make(map[string]TorrentInfo)
	for i, ihash := range ihashes {
		torrents[string(ihash[:])] = TorrentInfo{scrapeInfos[i].Seeders, scrapeInfos[i].Completed, scrapeInfos[i].Leechers, ""}
	}
	return &ScrapeResp{torrents}, nil
}

func (t *UDPTracker) readResponse(header respHeader) (*bytes.Buffer, error) {
	var readErr, err error
	var n int
	buf := make([]byte, 0x100) //4KB
	consecutiveTimeouts := 0
	for {
		dur := time.Duration(timeoutTime(consecutiveTimeouts))
		if dur < 0 {
			return nil, errors.New("waited tracker to much time for response (3840 secs)")
		}
		err = t.conn.SetReadDeadline(time.Now().Add(time.Second * dur))
		if err != nil {
			return nil, fmt.Errorf("set read deadline: %w", err)
		}
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			//read a packet
			udpConn := t.conn.(*net.UDPConn)
			n, _, readErr = udpConn.ReadFrom(buf)
		}()
		if t.ctx == nil {
			t.ctx = context.Background()
		}
		select {
		case <-t.ctx.Done():
			return nil, t.ctx.Err()
		case <-ch:
			switch tp := readErr.(type) {
			case *net.OpError:
				if readErr.(*net.OpError).Timeout() {
					consecutiveTimeouts++
					//periodically check if we are still connected
					//before we retransmit
					if header.Action != actionConnect && !t.isConnected() {
						return nil, connectionIdExpiredErr
					}
					t.rewriteLastRequest()
					continue
				}
				return nil, fmt.Errorf("udp tracker read conn net.OpError with no timeout: %w", readErr)
			case nil:
			default:
				return nil, fmt.Errorf("udp tracker read conn of type %T : %w", tp, readErr)
			}
			b := bytes.NewBuffer(buf[:n])
			err = checkRespHeader(b, header)
			if err != nil {
				return nil, err
			}
			return b, nil
		}
	}
}

func (t *UDPTracker) writeRequest(data ...interface{}) error {
	//serialize
	var b bytes.Buffer
	var err error
	for _, d := range data {
		err = writeBinary(&b, d)
		if err != nil {
			return err
		}
	}
	len := b.Len()
	n, err := t.conn.Write(b.Bytes())
	if err != nil {
		return fmt.Errorf("conn write: %w", err)
	}
	if len != n {
		return errors.New("didnt wrote all bytes to socket")

	}
	t.lastReq = b.Bytes()
	return nil

}

func (t *UDPTracker) rewriteLastRequest() error {
	len := len(t.lastReq)
	n, err := t.conn.Write(t.lastReq)
	if err != nil {
		return fmt.Errorf("conn write: %w", err)
	}
	if len != n {
		return errors.New("didnt wrote all bytes to socket")
	}
	return nil
}

func (t *UDPTracker) isConnected() bool {
	return !t.lastConncted.IsZero() && time.Now().Sub(t.lastConncted) < time.Minute
}

//do some check as BEP specifies
func checkRespHeader(buf *bytes.Buffer, expectedHeader respHeader) error {
	var header respHeader
	err := errors.New("response is too small")
	switch expectedHeader.Action {
	case actionAnnounce:
		if buf.Len() < 16 {
			return err
		}
	case actionConnect:
		if buf.Len() < 20 {
			return err
		}
	case actionScrape, actionError:
		if buf.Len() < 8 {
			return err
		}
	default:
		return errors.New("unknown action number")
	}
	err = readFromBinary(buf, &header)
	if err != nil {
		return err
	}
	if header.TxID != expectedHeader.TxID {
		return errors.New("transactionID is not the same")
	}
	if header.Action == actionError {
		return errors.New("tracker responded with err message: " + string(buf.Bytes()))
	}
	if header.Action != expectedHeader.Action {
		return errors.New("Actions don't match")
	}
	return nil
}

func timeoutTime(consecutiveTimeouts int) int {
	if consecutiveTimeouts >= 8 {
		return -1
	}
	return int(math.Pow(2.0, float64(consecutiveTimeouts))) * 15
}

func readFromBinary(r io.Reader, data ...interface{}) error {
	var err error
	for _, d := range data {
		v := reflect.ValueOf(d)
		switch t := v.Type(); v.Kind() {
		case reflect.Slice:
			e := reflect.New(t.Elem()).Elem()
			for i := 0; i < v.Len(); i++ {
				err = binary.Read(r, binary.BigEndian, e.Interface())
				if err != nil {
					break
				}
				v = reflect.Append(v, e)
			}
		default:
			err = binary.Read(r, binary.BigEndian, d)
			if err != nil {
				return fmt.Errorf("read binary: %w", err)
			}
		}
	}
	return nil
}

//actually no variadic is needed here
func writeBinary(w io.Writer, data ...interface{}) error {
	var err error
	for _, d := range data {
		v := reflect.ValueOf(d)
		switch v.Type().Kind() {
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				err = binary.Write(w, binary.BigEndian, v.Index(i).Interface())
				if err != nil {
					return err
				}
			}
		default:
			err := binary.Write(w, binary.BigEndian, d)
			if err != nil {
				return fmt.Errorf("write binary: %w", err)
			}
		}
	}
	return nil
}
