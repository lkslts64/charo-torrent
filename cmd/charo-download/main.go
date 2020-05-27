package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/gosuri/uilive"
	"github.com/lkslts64/charo-torrent/torrent"
)

var torrentFile = flag.String("torrentfile", "", "read the contents of the torrent `file`")

func main() {
	flag.Parse()
	//runtime.SetBlockProfileRate(1)
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	cfg, err := torrent.DefaultConfig()
	if err != nil {
		log.Fatal(err)
	}
	cl, err := torrent.NewClient(cfg)
	defer cl.Close()
	if err != nil {
		log.Fatal(err)
	}
	t, err := cl.AddFromFile(*torrentFile)
	if err != nil {
		log.Fatal(err)
	}
	err = t.StartDataTransfer()
	if err != nil {
		log.Fatal(err)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var seedC <-chan time.Time
	downloadC := t.DownloadedDataC
	w := uilive.New()
	w.Start()
loop:
	for {
		select {
		case <-downloadC:
			fmt.Println("Downloaded torrent. Will be seeding for 1 hour...")
			seedC = time.NewTimer(time.Hour).C
			downloadC = nil
		case <-ticker.C:
			t.WriteStatus(w)
		case <-t.ClosedC:
			log.Fatal("Torrent closed abnormally")
		case <-seedC:
			break loop
		}
	}
}
