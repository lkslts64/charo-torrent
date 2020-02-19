package torrent

import (
	"log"
	"math/rand"
	"net"
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
