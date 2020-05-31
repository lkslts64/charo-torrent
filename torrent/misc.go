package torrent

import (
	"log"
	"math/rand"
	"net"
	"strconv"
	"time"
)

func newExpiredTimer() *time.Timer {
	timer := time.NewTimer(time.Second) //arbitrary duration
	if !timer.Stop() {
		<-timer.C
	}
	return timer
}

// Get machine's preferred outbound ip
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80") //never write to this conn
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP
}

func flipCoin() bool {
	return rand.Intn(2) == 0
}

type addrPort struct {
	ip   net.IP
	port uint16
}

//works only if ch is a close only channel
func isChanClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func parseAddr(address string) (*addrPort, error) {
	shost, sport, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(sport, 10, 16)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(shost)
	if ip == nil {
		return nil, err
	}
	return &addrPort{
		ip:   ip.To4(),
		port: uint16(port),
	}, nil
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func contains(s []int, v int) bool {
	for _, num := range s {
		if num == v {
			return true
		}
	}
	return false
}
