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
var magnet = flag.String("magnet", "", "read the contents of the magnet")

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
	var t *torrent.Torrent
	if *torrentFile != "" {
		t, err = cl.AddFromFile(*torrentFile)
	} else if *magnet != "" {
		t, err = cl.AddFromMagnet(*magnet)
	} else {
		log.Fatal("please provide file or magnet")
	}
	if err != nil {
		log.Fatal(err)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var seedC <-chan time.Time
	downloadC := t.DownloadedDataC
	infoC := t.InfoC
	w := uilive.New()
	w.Start()
loop:
	for {
		select {
		case <-infoC:
			err = t.StartDataTransfer()
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println("downloaded info!")
			infoC = nil
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
