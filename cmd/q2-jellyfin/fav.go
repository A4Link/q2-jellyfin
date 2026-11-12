package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Hearts (U30; Q2 Pod's "fav", docs/librespot.md Hearts): Now Playing's heart marks a song a
// favourite on the server, or not.
//
//   - A streamed song's queue line carries its item and heart (stream_urls.go); the firmware flips
//     the heart at once and appends "item<TAB><0|1><TAB><item id>" to .jellyfin/fav.log.
//   - A synced song (under favdir=Jellyfin, contrib/shanlingq2/q2app) keeps the Q2's own heart (My
//     Fav); each change appends "path<TAB><0|1><TAB><full path>", found here through the manifest.
//
// The app takes the file by renaming it aside (fav.log.taking), and a tick at least favSettle
// later, when a press written as it was renamed is in it, reads it and puts each song's last state
// in the outbox (report.go), then deletes it. A record starts and ends with a line break: a line
// cut short (no break at its end, or another line right after it), and one that isn't three
// fields of item or path, 0 or 1 and a key, are dropped. A manifest that can't be read leaves the
// file aside for a later tick. What is in the outbox goes as reports do: when the server is in reach, retried
// with a growing wait, never on a page's time; the state is sent whatever the server's is
// (POST or DELETE /UserFavoriteItems), and only the last state of a song waits. A song the server
// no longer has (404) is dropped; one it refuses (400, 403) is dropped too, logged, and the root
// (Settings with sections) says so for the rest of the app's run (favRows).
//
// Signed out, the file waits for a sign-in; Sign Out drops it with the outbox: the presses were the
// account's. Favourites and Sync Favourites go by the server's list, as before: a song un-hearted
// leaves the card at the next sync.

const (
	favKind   = "favourite"
	favLog    = "fav.log"
	favSettle = time.Second // a press being written as the file was renamed lands in it by then
)

func (d *daemon) favPath() string { return filepath.Join(d.dir, favLog) }

// takeFavs, from tick, takes Q2 Pod's heart presses into the outbox: fav.log is renamed aside,
// then read at a tick favSettle on. One the last run left aside waits favSettle too.
func (d *daemon) takeFavs(now time.Time) {
	log, aside := d.favPath(), d.favPath()+".taking"
	cl := d.client() // signed out: the presses wait for a sign-in
	r := &d.rep
	r.mu.Lock()
	at := r.favAt
	r.mu.Unlock()
	if !exists(aside) {
		if !exists(log) || cl == nil || os.Rename(log, aside) != nil {
			return
		}
		at = time.Time{}
	}
	if at.IsZero() {
		r.mu.Lock()
		r.favAt = now
		r.mu.Unlock()
		return
	}
	if now.Sub(at) < favSettle || cl == nil {
		return
	}
	man, err := d.manifestErr()
	if err != nil {
		return // nothing is known of what is synced: the presses wait aside, as Q2 Pod's log does
	}
	b, err := os.ReadFile(aside)
	if err != nil {
		d.log.Printf("hearts: %v", err)
		return
	}
	rs := favPresses(b, man, d.log.Printf)
	if err := r.box.add(rs...); err != nil {
		d.log.Printf("hearts: outbox: %v", err) // the file stays for the next tick
		return
	}
	os.Remove(aside)
	if d.client() != cl { // signed out meanwhile: the presses were that account's
		if err := r.box.drop(favKind); err != nil {
			d.log.Printf("hearts: outbox: %v", err)
		}
		return
	}
	r.mu.Lock()
	r.favAt = time.Time{}
	if r.favNow == nil {
		r.favNow = map[string]bool{}
	}
	for _, x := range rs {
		r.favNow[x.Item] = x.On
	}
	r.mu.Unlock()
	if len(rs) > 0 {
		d.log.Printf("hearts: %d taken", len(rs))
		d.wantReports(false)
	}
}

// favPresses is fav.log's presses as favourite reports, the last state of each song, in the order
// of its last press. A synced song's path is its manifest entry's (exact); one not in the
// manifest is dropped, logged.
func favPresses(b []byte, man *library.Manifest, logf func(string, ...any)) []report {
	var byPath map[string]string
	var rs []report
	// A whole record is followed by an empty line (the next record's first break) or the end; a
	// line followed by another was cut short, and the last has no break after it.
	lines := strings.Split(string(b), "\n")
	for i, line := range lines[:len(lines)-1] {
		if lines[i+1] != "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[1] != "0" && f[1] != "1" || f[2] == "" {
			continue // the empty line a record starts with too
		}
		id := ""
		switch f[0] {
		case "item":
			id = f[2]
		case "path":
			if byPath == nil {
				byPath = map[string]string{}
				for k, e := range man.Songs {
					byPath[e.Path] = k
				}
			}
			if id = byPath[f[2]]; id == "" {
				logf("hearts: %s is no synced song", filepath.Base(f[2]))
				continue
			}
		default:
			continue
		}
		for i := range rs {
			if rs[i].Item == id {
				rs = append(rs[:i], rs[i+1:]...)
				break
			}
		}
		rs = append(rs, report{Kind: favKind, Item: id, On: f[1] == "1"})
	}
	return rs
}

// sendFav sends a heart's state.
func (d *daemon) sendFav(cl *jellyfin.Client, r report) error {
	ctx, cancel := context.WithTimeout(context.Background(), reportWait)
	defer cancel()
	return cl.SetFavorite(ctx, r.Item, r.On)
}

// httpStatus is err's HTTP status, 0 when it has none (no answer). For a favourite, 403 (not allowed)
// is the server's final word as Refused's 400 and 404 are; 404 is a song gone, said to no one.
func httpStatus(err error) int {
	var ae *jellyfin.APIError
	if errors.As(err, &ae) {
		return ae.HTTP
	}
	return 0
}

// hearts are the hearts the Q2 has that the songs as fetched may not: the presses waiting in the
// outbox, and those of this run.
func (d *daemon) hearts() map[string]bool {
	m := map[string]bool{}
	for _, r := range d.rep.box.pending() {
		if r.Kind == favKind {
			m[r.Item] = r.On
		}
	}
	d.rep.mu.Lock()
	for k, v := range d.rep.favNow {
		m[k] = v
	}
	d.rep.mu.Unlock()
	return m
}

// favsChanged forgets the Favourites list kept since it was fetched: it is fetched again.
func (d *daemon) favsChanged() {
	d.mu.Lock()
	delete(d.resolved, favKey)
	d.mu.Unlock()
	c := &d.lists
	c.mu.Lock()
	if l, ok := c.vals["item"].(itemList); ok && l.key == favKey {
		delete(c.vals, "item")
	}
	c.mu.Unlock()
}

// favRows says, once a favourite was refused for good in this run, that the Q2's heart and the
// server's may differ.
func (d *daemon) favRows() []q2spot.Row {
	d.rep.mu.Lock()
	defer d.rep.mu.Unlock()
	if !d.rep.refused {
		return nil
	}
	return []q2spot.Row{{Kind: "info", Title: "A favourite wasn't saved on the server", Subtitle: "The server refused it"}}
}

// dropFavs is Sign Out: the presses not taken yet were the account's.
func (d *daemon) dropFavs() {
	os.Remove(d.favPath())
	os.Remove(d.favPath() + ".taking")
	d.rep.mu.Lock()
	d.rep.favAt, d.rep.favNow, d.rep.refused = time.Time{}, nil, false
	d.rep.mu.Unlock()
}
