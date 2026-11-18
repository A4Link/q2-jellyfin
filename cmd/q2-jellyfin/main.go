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
	"time"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

const version = "q2-v0.8.1"

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
	sock := filepath.Join(*tmp, "q2-jellyfin.browse.sock")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if !fw.ReadCaps(filepath.Join(*tmp, "q2-addon.caps")).URLs { // before anything else: only creds.Load runs there
		go func() { <-sig; os.Remove(sock); os.Exit(0) }()
		creds.Load(*dir) // C3 here too: a sign-in an older version saved plain is encrypted now
		log.Fatal(serveOld(sock))
	}
	d := newDaemon(*dir, *card, *tmp)
	d.log.Printf("start %s", version)
	d.startRelay(relayAddr) // at once, and before close can run (sig): the firmware's player streams through it
	d.signInFromFile()      // C4, before the first request; without Wi-Fi yet, the root takes it later
	go func() { <-sig; d.close(sock); os.Exit(0) }()
	log.Fatal(d.serve(sock))
}

// serveOld is the app on Q2 Pod before 1.7.3 (no "urls" in its caps): it streams only through Q2
// Pod's own player, so there it signs in, browses, syncs and streams nothing. Every request gets
// one page asking for an update, a plain info row, which any Q2 Pod with card apps draws. It exits
// 0 once no request came for idleAfter.
func serveOld(sock string) error {
	page := q2spot.Reply{Rows: []q2spot.Row{{Kind: "info", Title: oldFirmware}}}
	idle := time.AfterFunc(idleAfter, func() { os.Remove(sock); os.Exit(0) })
	return q2spot.Serve(sock, func(q2spot.Request) q2spot.Reply {
		idle.Reset(idleAfter)
		return page
	})
}

// oldFirmware is the update page's one row (U45).
const oldFirmware = "Needs Q2 Pod 1.7.3 or later. Update Q2 Pod to use Jellyfin."
