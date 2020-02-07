package torrent

import "time"

func newExpiredTimer() *time.Timer {
	timer := time.NewTimer(time.Second) //arbitrary duration
	if !timer.Stop() {
		<-timer.C
	}
	return timer
}
