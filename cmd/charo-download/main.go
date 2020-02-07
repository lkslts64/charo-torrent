package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gosuri/uilive"
	"github.com/lkslts64/charo-torrent/torrent"
)

func main() {
	args := os.Args[1:]
	if len(args) != 1 {
		log.Fatal("wrong # of args")
	}
	cfg := torrent.DefaultConfig()
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	t, err := cl.NewTorrentFromFile(args[0])
	if err != nil {
		log.Fatal(err)
	}
	w := uilive.New()
	w.Start()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	finish := t.Download()
loop:
	for {
		select {
		case <-finish:
			break loop
		case <-ticker.C:
			t.WriteStatus(w)
		}
	}
	fmt.Println("downloaded torrent")
}
