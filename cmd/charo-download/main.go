package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/gosuri/uilive"
	"github.com/lkslts64/charo-torrent/torrent"
)

var torrentFile = flag.String("torrentfile", "", "read the contents of the torrent `file`")
var cpuprofile = flag.String("cpuprof", "", "write cpu profile to `file`")
var memprofile = flag.String("memprof", "", "write memory profile to `file`")

func main() {
	flag.Parse()
	//runtime.SetBlockProfileRate(1)
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}
	//
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
	w := uilive.New()
	w.Start()
	err = t.StartDataTransfer()
	if err != nil {
		log.Fatal(err)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var seedC <-chan time.Time
loop:
	for {
		select {
		case <-t.DownloadedDataC:
			fmt.Println("Downloaded torrent. Will be seeding for 1 hour...")
			seedC = time.NewTimer(time.Hour).C
		case <-ticker.C:
			t.WriteStatus(w)
		case <-t.ClosedC:
			log.Fatal("Torrent closed abnormally")
		case <-seedC:
			break loop
		}
	}
	//
	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal("could not create memory profile: ", err)
		}
		defer f.Close()
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
	}
}
