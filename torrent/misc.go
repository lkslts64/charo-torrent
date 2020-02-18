package torrent

import (
	"log"
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

// Get preferred outbound ip of this machine
func GetOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80") //never write to this conn
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}
