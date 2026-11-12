// q2-jellyfin is a Q2 Pod card app (contrib/shanlingq2/README): a Jellyfin client for
// the Shanling Q2 that syncs and streams. It runs from the card's .jellyfin folder (copied to /tmp
// first, so the card is never kept busy) and draws the Q2's Jellyfin page over Q2SPOT/1.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/A4Link/q2-jellyfin/internal/q2"
)

const version = "q2-v0.7.0"

func main() {
	dir := flag.String("dir", "/mnt/mmc/.jellyfin", "the add-on's folder on the card")
	card := flag.String("card", "/mnt/mmc", "the card's root")
	tmp := flag.String("tmp", "/tmp", "where the firmware's files and the socket live")
	flag.Parse()
	// The Q2 has about 67 MB beside demo and no swap: the GC works harder near this instead of the
	// kernel's OOM killer picking the app, or demo (I16).
	debug.SetMemoryLimit(48 << 20)
	q2.SetupLog("q2-jellyfin")
	q2.Card = *card
	log.Print("start ", version)
	d := newDaemon(*dir, *card, *tmp)
	d.log.Printf("start %s", version)
	d.signInFromFile() // C4, before the first request; without Wi-Fi yet, the root takes it later
	sock := filepath.Join(*tmp, "q2-jellyfin.browse.sock")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; d.close(sock); os.Exit(0) }()
	log.Fatal(d.serve(sock))
}
