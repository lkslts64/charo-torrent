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
	"time"
)

//TODO: support IPv6
//var connectionIdExpiredErr = errors.New("connectionID expired")
var requestTimeoutErr = errors.New("request timeout out")

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

type announceFixed struct {
	Interval int32
	Leechers int32
	Seeders  int32
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

func (t *UDPTrackerURL) Announce(ctx context.Context, r AnnounceReq) (*AnnounceResp, error) {
	resp, err := t.announce(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("udp announce: %w", err)
	}
	return resp, nil
}

func (t *UDPTrackerURL) Scrape(ctx context.Context, ihashes ...[20]byte) (*ScrapeResp, error) {
	var scrapeURL string
	if scrapeURL = t.url.ScrapeURL(); scrapeURL == "" {
		return nil, errors.New("tracker doesn't support scrape")
	}
	resp, err := t.scrape(ctx, ihashes...)
	if err != nil {
		return nil, fmt.Errorf("udp scrape: %w", err)
	}
	return resp, nil
}

func (t *UDPTrackerURL) connect(ctx context.Context) error {
	var err error
	if t.isConnected() {
		return nil
	}
	t.conn, err = net.Dial("udp", t.host)
	if err != nil {
		return errors.New("fail to dial connection with tracker")
	}
	txID := rand.Int31()
	buf, err := t.request(ctx, respHeader{actionConnect, txID}, reqHeader{protoID, actionConnect, txID})
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

func (t *UDPTrackerURL) announce(ctx context.Context, req AnnounceReq) (*AnnounceResp, error) {
	err := t.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	txID := rand.Int31()
	buf, err := t.request(ctx, respHeader{actionAnnounce, txID}, struct {
		req  reqHeader
		data AnnounceReq
	}{
		reqHeader{t.connID, actionAnnounce, txID},
		req,
	})
	if err != nil {
		return nil, err
	}
	var fixed announceFixed
	err = readFromBinary(buf, &fixed)
	if err != nil {
		return nil, err
	}
	var cheap cheapPeers = buf.Bytes()
	peers, err := cheap.peers()
	if err != nil {
		return nil, err
	}
	return &AnnounceResp{fixed.Interval, fixed.Leechers, fixed.Seeders, peers, 0}, nil
}

func (t *UDPTrackerURL) scrape(ctx context.Context, ihashes ...[20]byte) (*ScrapeResp, error) {
	err := t.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	txID := rand.Int31()
	buf, err := t.request(ctx, respHeader{actionScrape, txID}, reqHeader{t.connID, actionScrape, txID}, ihashes)
	if err != nil {
		return nil, err
	}
	//var scrapeInfos []udpScrapeInfo
	scrapeInfos := make([]udpScrapeInfo, len(ihashes))
	err = readFromBinary(buf, &scrapeInfos)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) && errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("probably not all infohashes are available: %w", err)
		}
		return nil, err
	}
	torrents := make(map[string]TorrentInfo)
	for i, ihash := range ihashes {
		torrents[string(ihash[:])] = TorrentInfo{scrapeInfos[i].Seeders, scrapeInfos[i].Completed, scrapeInfos[i].Leechers, ""}
	}
	return &ScrapeResp{torrents}, nil
}

func (t *UDPTrackerURL) request(ctx context.Context, respHead respHeader, req ...interface{}) (buf *bytes.Buffer, err error) {
	b := make([]byte, 0x100) //4KB
	for {
		//ensure we are conncted in case of a timeout
		if respHead.Action != actionConnect {
			err = t.connect(ctx)
			if err != nil {
				return
			}
		}
		err = t.writeRequest(req...)
		if err != nil {
			return
		}
		buf, err = t.readResponse(ctx, respHead, b)
		if err != nil && errors.Is(err, requestTimeoutErr) {
			continue
		}
		return
	}
}

func (t *UDPTrackerURL) readResponse(ctx context.Context, header respHeader, buf []byte) (*bytes.Buffer, error) {
	var readErr, err error
	var n int
	dur := t.timeoutTime()
	if dur < 0 {
		return nil, errors.New("waited tracker to much time for response (3840 secs)")
	}
	err = t.conn.SetReadDeadline(time.Now().Add(dur))
	if err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		//read a whole packet. all data should fit in
		//acording to BEP 15
		//maybe conn.Read reads a whole packet too.
		udpConn := t.conn.(*net.UDPConn)
		n, _, readErr = udpConn.ReadFrom(buf)
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ch:
		switch tp := readErr.(type) {
		case *net.OpError:
			if readErr.(*net.OpError).Timeout() {
				t.consecutiveTimeouts++
				return nil, requestTimeoutErr
			}
			return nil, fmt.Errorf("udp tracker read conn net.OpError with no timeout: %w", readErr)
		case nil:
		default:
			return nil, fmt.Errorf("udp tracker read conn err of type %T : %w", tp, readErr)
		}
		b := bytes.NewBuffer(buf[:n])
		err = checkRespHeader(b, header)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
}

func (t *UDPTrackerURL) writeRequest(data ...interface{}) error {
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
	return nil
}

func (t *UDPTrackerURL) isConnected() bool {
	return !t.lastConncted.IsZero() && time.Now().Sub(t.lastConncted) < time.Minute
}

//do some check as BEP specifies
func checkRespHeader(buf *bytes.Buffer, expectedHeader respHeader) error {
	var header respHeader
	err := errors.New("response is too small")
	switch expectedHeader.Action {
	case actionAnnounce:
		if buf.Len() < 20 {
			return err
		}
	case actionConnect:
		if buf.Len() < 16 {
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

func (t *UDPTrackerURL) timeoutTime() time.Duration {
	if t.consecutiveTimeouts >= 8 {
		return -1
	}
	return time.Duration(int(math.Pow(2.0, float64(t.consecutiveTimeouts)))) * 15 * time.Second
}

func readFromBinary(r io.Reader, data ...interface{}) error {
	var err error
	for _, d := range data {
		err = binary.Read(r, binary.BigEndian, d)
		if err != nil {
			return fmt.Errorf("read binary: %w", err)
		}
	}
	return nil
}

//actually no variadic is needed here
func writeBinary(w io.Writer, data ...interface{}) error {
	var err error
	for _, d := range data {
		err = binary.Write(w, binary.BigEndian, d)
		if err != nil {
			return fmt.Errorf("write binary: %w", err)
		}
	}
	return nil
}
