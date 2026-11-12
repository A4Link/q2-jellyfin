package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/library"
)

// press is one record of Q2 Pod's fav.log: a line break first and last.
func press(kind, state, key string) string { return "\n" + kind + "\t" + state + "\t" + key + "\n" }

// favLines writes what the firmware appended to fav.log.
func (d *tdaemon) favLines(t *testing.T, s string) {
	t.Helper()
	f, err := os.OpenFile(d.favPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}

// take takes fav.log as two ticks favSettle apart do.
func (d *tdaemon) take() {
	now := time.Now()
	d.takeFavs(now)
	d.takeFavs(now.Add(favSettle))
}

func favs(rs []report) []string {
	var out []string
	for _, r := range rs {
		if r.Kind == favKind {
			out = append(out, r.Item+map[bool]string{true: " on", false: " off"}[r.On])
		}
	}
	return out
}

// U30: fav.log's records, each starting and ending with a line break, so a whole one is followed
// by an empty line or the end. A synced song's path is found in the manifest (exactly); only each
// song's last state is kept, in the order of its last press. A line cut short (no break after it,
// or the next press right after it), of the wrong shape, or a path not synced is dropped.
func TestFavPresses(t *testing.T) {
	card := "/mnt/mmc"
	m, _ := reportSongs(card)
	blue := m.Songs["blue1"].Path
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	b := press("item", "1", "al0.0") +
		press("path", "1", blue) +
		press("item", "0", "al0.1") +
		press("item", "0", "al0.0") + // al0.0 again: its last state, after al0.1
		press("item", "1", "al0.2") +
		press("item", "2", "al0.3") + // not 0 or 1
		press("song", "1", "al0.4") + // no such kind
		press("item", "1", "") + // no key
		"\nitem\t1\tal0.5\textra\n" + // four fields
		press("path", "1", strings.ToUpper(blue)) + // not the manifest's path
		press("path", "1", "/mnt/mmc/Music/other.flac") + // another folder
		"\nitem\t1\tal0" + press("item", "0", "al0.7") + // a write cut short, then a whole press
		"\nitem\t1\tal0.6" // being written, or cut short
	got := favs(favPresses([]byte(b), m, logf))
	want := []string{"blue1 on", "al0.1 off", "al0.0 off", "al0.2 on", "al0.7 off"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q\nwant %q", got, want)
	}
	if len(logged) != 2 {
		t.Errorf("logged %q", logged)
	}
	for _, empty := range []string{"", "\n", "\n\n", "item\t1\tal0.0"} {
		if rs := favPresses([]byte(empty), &library.Manifest{}, logf); len(rs) != 0 {
			t.Errorf("%q: %+v", empty, rs)
		}
	}
}

// U30: fav.log is renamed aside and read only a tick favSettle later (a press written as it was
// renamed lands in the renamed file), its presses go to the outbox and the file is deleted; the
// server then gets each song's state, POST for a heart and DELETE for none, whatever it has, and
// Favourites is fetched again. An empty file leaves nothing.
func TestFavLogTaken(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, favs: 1}
	d := newTestDaemon(t, n, "urls fav", true)
	m, _ := reportSongs(d.card)
	m.Save(d.card, filepath.Join(d.dir, "manifest.json"))
	aside := d.favPath() + ".taking"
	d.favLines(t, press("item", "1", "al0.1")+press("path", "1", m.Songs["blue1"].Path))
	now := time.Now()
	d.takeFavs(now)
	if exists(d.favPath()) || !exists(aside) || len(d.outbox()) != 0 {
		t.Fatalf("fav.log %v, aside %v, outbox %+v", exists(d.favPath()), exists(aside), d.outbox())
	}
	f, _ := os.OpenFile(aside, os.O_APPEND|os.O_WRONLY, 0) // a press written as it was renamed
	f.WriteString(press("item", "1", "al0.0") + press("item", "0", "al0.1"))
	f.Close()
	d.favLines(t, press("item", "1", "al0.2")) // a press after: the next take's
	d.takeFavs(now.Add(favSettle / 2))
	if len(d.outbox()) != 0 {
		t.Fatalf("read before favSettle: %+v", d.outbox())
	}
	d.do("favorites", favKey) // Favourites, kept as resolved
	d.takeFavs(now.Add(favSettle))
	if got := favs(d.outbox()); !slices.Equal(got, []string{"blue1 on", "al0.0 on", "al0.1 off"}) || exists(aside) {
		t.Fatalf("outbox %q, aside %v", got, exists(aside))
	}
	if h := d.hearts(); !h["al0.0"] || h["al0.1"] {
		t.Fatalf("hearts %v", h)
	}
	d.mu.Lock()
	_, kept := d.resolved[favKey]
	d.mu.Unlock()
	if !kept {
		t.Fatal("Favourites wasn't resolved")
	}
	d.sendNow(t) // al0.2's file is renamed aside here
	if got := n.reported(); !slices.Equal(got, []string{"fav blue1 1", "fav al0.0 1", "fav al0.1 0"}) || len(d.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, d.outbox())
	}
	d.mu.Lock()
	_, kept = d.resolved[favKey]
	d.mu.Unlock()
	if kept {
		t.Fatal("Favourites kept after its hearts changed")
	}
	if !n.favourite("al0.0") || n.favourite("al0.1") {
		t.Fatal("the server's hearts")
	}
	d.take()
	d.sendNow(t)
	if got := n.reported(); got[len(got)-1] != "fav al0.2 1" {
		t.Fatalf("%q", got)
	}
	d.favLines(t, "")
	d.take()
	if exists(d.favPath()) || exists(aside) || len(d.outbox()) != 0 {
		t.Fatalf("empty: fav.log %v, aside %v, outbox %+v", exists(d.favPath()), exists(aside), d.outbox())
	}
}

// Only a song's last state waits in the outbox, whatever came before; a press taken while its
// earlier state is being sent stays for the next send.
func TestFavCollapse(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true}, report{Kind: "played", Item: "al0.0", At: 5})
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: false})
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true})
	if got := favs(d.outbox()); !slices.Equal(got, []string{"al0.0 on"}) || len(d.outbox()) != 2 {
		t.Fatalf("%+v", d.outbox())
	}
	d.rep.box.drop("played")
	n.onReport = func() {
		if len(n.reported()) == 0 { // pressed again as the heart goes
			d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: false})
		}
	}
	d.sendNow(t)
	if got := favs(d.outbox()); !slices.Equal(got, []string{"al0.0 off"}) {
		t.Fatalf("the press after: %q", got)
	}
	d.sendNow(t)
	if got := n.reported(); !slices.Equal(got, []string{"fav al0.0 1", "fav al0.0 0"}) || len(d.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, d.outbox())
	}
}

// A favourite waits while the server can't be reached and goes with a growing wait; one the
// server no longer has (404) is dropped quietly; one it refuses (403, 400) is dropped, logged, and
// the root says so once, whatever the number.
func TestFavRetryAndRefused(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true})
	n.favStatus.Store(503)
	d.sendNow(t)
	if len(d.outbox()) != 1 {
		t.Fatalf("outbox %+v", d.outbox())
	}
	n.favStatus.Store(0)
	d.tick() // backing off
	d.bg.Wait()
	if len(d.outbox()) != 1 {
		t.Fatal("sent while backing off")
	}
	n.favStatus.Store(404)
	d.sendNow(t)
	if len(d.outbox()) != 0 || len(d.favRows()) != 0 {
		t.Fatalf("404: outbox %+v, rows %v", d.outbox(), d.favRows())
	}
	n.favStatus.Store(403)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true}, report{Kind: favKind, Item: "al0.1", On: false})
	d.sendNow(t)
	if len(d.outbox()) != 0 {
		t.Fatalf("403: outbox %+v", d.outbox())
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt")); !strings.Contains(string(b), "reports: favourite al0.1 refused") {
		t.Fatalf("log %s", b)
	}
	rows := titles(d.do("menu"))
	if c := slices.Index(rows, "A favourite wasn't saved on the server"); c < 0 || slices.Index(rows[c+1:], rows[c]) >= 0 {
		t.Fatalf("%q", rows)
	}
}

// Pending favourites outlast the app: a new run on the same card sends the outbox, and takes a
// fav.log the firmware wrote while no app ran, and one the last run left aside.
func TestFavSurviveRestart(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3}
	card := t.TempDir()
	d := newTestDaemonOn(t, card, n, "", true)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true})
	n.favStatus.Store(503)
	d.sendNow(t)
	d.favLines(t, press("item", "1", "al0.1"))
	d.takeFavs(time.Now()) // aside, then the app ends
	d.favLines(t, press("item", "1", "al0.2"))
	n.favStatus.Store(0)

	e := newTestDaemonOn(t, card, n, "", false) // the same card: signed in still
	e.take()
	e.sendNow(t)
	e.take()
	e.sendNow(t)
	got := n.reported()
	slices.Sort(got)
	if !slices.Equal(got, []string{"fav al0.0 1", "fav al0.1 1", "fav al0.2 1"}) || len(e.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, e.outbox())
	}
}

// Report Plays to Server Off forgets plays, not favourites, which still go.
func TestFavReportsOff(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true}, report{Kind: "played", Item: "al0.1", At: 7})
	n.favStatus.Store(503)
	d.do("reportplays") // Off
	if got := favs(d.outbox()); !slices.Equal(got, []string{"al0.0 on"}) || len(d.outbox()) != 1 {
		t.Fatalf("%+v", d.outbox())
	}
	n.favStatus.Store(0)
	d.favLines(t, press("item", "0", "al0.1"))
	d.take()
	d.sendNow(t)
	if got := n.reported(); !slices.Equal(got, []string{"fav al0.0 1", "fav al0.1 0"}) || len(d.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, d.outbox())
	}
}

// Sign Out drops the favourites waiting and the presses not taken yet: they were the account's.
// Signed out, a new fav.log waits for the next sign-in.
func TestFavSignOut(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true})
	d.favLines(t, press("item", "1", "al0.1"))
	d.takeFavs(time.Now())
	d.favLines(t, press("item", "1", "al0.0"))
	d.do("signout")
	d.bg.Wait()
	if len(d.outbox()) != 0 || exists(d.favPath()) || exists(d.favPath()+".taking") {
		t.Fatalf("outbox %+v, fav.log %v", d.outbox(), exists(d.favPath()))
	}
	d.favLines(t, press("item", "1", "al0.1"))
	d.take()
	if !exists(d.favPath()) || len(d.outbox()) != 0 {
		t.Fatalf("taken signed out: outbox %+v", d.outbox())
	}
}

// A manifest that can't be read leaves the presses aside for a later tick, as Q2 Pod's play log
// waits (readPlays): a synced song's path would otherwise be dropped as no synced song.
func TestFavManifestUnreadable(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	m, _ := reportSongs(d.card)
	man := filepath.Join(d.dir, "manifest.json")
	os.WriteFile(man, []byte("{"), 0o644)
	d.favLines(t, press("path", "1", m.Songs["blue1"].Path))
	d.take()
	if !exists(d.favPath()+".taking") || len(d.outbox()) != 0 {
		t.Fatalf("aside %v, outbox %+v", exists(d.favPath()+".taking"), d.outbox())
	}
	m.Save(d.card, man)
	d.take()
	if got := favs(d.outbox()); !slices.Equal(got, []string{"blue1 on"}) || exists(d.favPath()+".taking") {
		t.Fatalf("outbox %q", got)
	}
}
