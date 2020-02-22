package main

import (
	"flag"
	"fmt"
	"log"
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
	if err != nil {
		log.Fatal(err)
	}
	t, err := cl.AddFromFile(*torrentFile)
	if err != nil {
		log.Fatal(err)
	}
	w := uilive.New()
	w.Start()
	ticker := time.NewTicker(2 * time.Second)
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
