package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

func swipeRowOf(t *testing.T, d *tdaemon) q2spot.Row {
	t.Helper()
	for _, row := range d.do("settings").Rows {
		if strings.HasPrefix(row.Title, "Swipe Between Sections") {
			return row
		}
	}
	t.Fatal("no Swipe Between Sections row")
	return q2spot.Row{}
}

// Swipe Between Sections: on by default where the firmware offers sections; off on request; a
// card moved to firmware without them turns it off, and it stays off on firmware with them again.
func TestSwipeSetting(t *testing.T) {
	card := t.TempDir()
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1}, "sections", true)
	if r := d.do("menu"); r.Message != "sections" {
		t.Fatalf("default: %q", r.Message)
	}
	if d.daemon.settings().Swipe != "on" {
		t.Fatalf("the default is stored: %q", d.daemon.settings().Swipe)
	}
	if row := swipeRowOf(t, d); row.Kind != "action" || row.Title != "Swipe Between Sections: On" {
		t.Fatalf("%+v", row)
	}
	if r := d.do("swipe"); !strings.Contains(r.Message, "Swipe off") {
		t.Fatalf("%q", r.Message)
	}
	if r := d.do("menu"); r.Message != "refresh" {
		t.Fatalf("off: %q", r.Message)
	}
	if row := swipeRowOf(t, d); row.Title != "Swipe Between Sections: Off" {
		t.Fatalf("%+v", row)
	}
	d.do("swipe")
	if r := d.do("menu"); r.Message != "sections" {
		t.Fatalf("back on: %q", r.Message)
	}

	// the same card on firmware without sections: the old list, the setting off, its row inert
	old := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1}, "progress", true)
	if r := old.do("menu"); r.Message != "refresh" || len(r.Rows) < 5 {
		t.Fatalf("old firmware: %q %q", r.Message, titles(r))
	}
	if old.daemon.settings().Swipe != "off" {
		t.Fatalf("not turned off: %q", old.daemon.settings().Swipe)
	}
	if row := swipeRowOf(t, old); row.Kind != "info" || !strings.Contains(row.Title, "Unavailable") {
		t.Fatalf("%+v", row)
	}
	if r := old.do("swipe"); !strings.Contains(r.Message, "can't") {
		t.Fatalf("%q", r.Message)
	}
	// and back on firmware with sections: off until turned on
	again := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1}, "sections", true)
	if r := again.do("menu"); r.Message != "refresh" {
		t.Fatalf("stays off: %q", r.Message)
	}

	// a fresh card on old firmware leaves the default alone: later firmware with sections swipes
	fresh := t.TempDir()
	if r := newTestDaemonOn(t, fresh, &nav{}, "", true).do("menu"); r.Message != "refresh" {
		t.Fatalf("%q", r.Message)
	}
	if r := newTestDaemonOn(t, fresh, &nav{}, "sections", true).do("menu"); r.Message != "sections" {
		t.Fatalf("fresh, upgraded: %q", r.Message)
	}
}

// Pressing a song that must arrive first toasts "Buffering…", and the list it was pressed in turns
// live, the song's row saying what the stream does, from the poll cache (no new server requests);
// once the song plays the list is plain again.
func TestBufferingInList(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	d := newTestDaemon(t, n, "progress playlist", true)
	var once sync.Once
	unblock := func() { once.Do(func() { close(n.block) }) }
	t.Cleanup(unblock)
	page := d.do("album", "album:al0")
	if page.Message == "refresh" {
		t.Fatal("a plain list is live")
	}
	var row q2spot.Row
	for _, r := range page.Rows {
		if r.Title == "Song 1" {
			row = r
		}
	}
	if row.Kind != "action" || row.URI != "play" {
		t.Fatalf("song row %+v", row)
	}
	if r := d.do("play", row.Context); r.Message != "Buffering Song 1…" {
		t.Fatalf("toast %q", r.Message)
	}
	reqs := n.albumReqs.Load()
	var live q2spot.Reply
	eventually(t, "the waiting row", func() bool {
		live = d.do("album", "album:al0")
		r, _ := find(live, "Song 1")
		return live.Message == "refresh" && (strings.HasPrefix(r.Subtitle, "Connecting") || strings.Contains(r.Subtitle, "%"))
	})
	if r, _ := find(live, "Song 0"); strings.Contains(r.Subtitle, "%") || strings.Contains(r.Subtitle, "Connecting") {
		t.Fatalf("another row marked: %+v", r)
	}
	if got := n.albumReqs.Load(); got != reqs {
		t.Fatalf("the polls asked the server again: %d -> %d", reqs, got)
	}

	unblock()
	var path string
	eventually(t, "the song on the card", func() bool {
		m, _ := filepath.Glob(filepath.Join(d.card, ".jellyfin", "stream", "*Song 1*.flac"))
		if len(m) == 1 {
			path = m[0]
		}
		return path != ""
	})
	os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.playing"), []byte(path+"\n"), 0o644) // the firmware plays it
	eventually(t, "a plain list again", func() bool {
		r := d.do("album", "album:al0")
		s, _ := find(r, "Song 1")
		return r.Message != "refresh" && !strings.Contains(s.Subtitle, "%")
	})
}

func TestPhaseText(t *testing.T) {
	for _, c := range []struct {
		st   streamStatus
		want string
	}{
		{streamStatus{}, "Connecting…"},
		{streamStatus{phase: phaseBuffering, got: 50, want: 100, total: 1000}, "Buffering 50 %"},
		{streamStatus{phase: phaseBuffering, got: 100, want: 600, total: 1000}, "Slow connection · buffering 16 %"},
		{streamStatus{phase: phaseWhole, got: 30, want: -1, total: 100}, "Downloading 30 %"},
		{streamStatus{phase: phaseReady}, "Starting…"},
	} {
		if got := phaseText(c.st); got != c.want {
			t.Errorf("%+v: %q, want %q", c.st, got, c.want)
		}
	}
}

// A song after the first lands as early as its link allows, before it is whole, so the firmware
// queues it and Next can go to it while the one before still plays.
func TestStreamNextSongEarly(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	e.src.body = func(u string) (io.Reader, int64) {
		if strings.Contains(u, "s1") {
			return &heldBody{head: make([]byte, 900_000), rest: make([]byte, 100_000), release: release}, 1_000_000
		}
		return strings.NewReader(strings.Repeat("x", 1_000_000)), 1_000_000
	}
	ss := songs(2, "flac")
	for i := range ss {
		ss[i].Duration, ss[i].Size = 600, 1_000_000 // s0 plays for 10 minutes
	}
	e.s.start(ss, 0)
	until(t, "s0", func() bool { return exists(e.stream(0, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(0, "flac")), 0o644) // the player on s0
	deadline := time.Now().Add(5 * time.Second)
	for !exists(e.stream(1, "flac")) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fi, err := os.Stat(e.stream(1, "flac")); err != nil || fi.Size() >= 1_000_000 {
		t.Fatalf("s1 not there before it was whole: %v", err)
	}
	free()
	until(t, "s1 whole", func() bool { fi, err := os.Stat(e.stream(1, "flac")); return err == nil && fi.Size() == 1_000_000 })
}

// A song that plays at once (a URL queue, or a song on the card) answers Play with no message, so
// Q2 Pod opens Now Playing straight away instead of a "Playing …" toast first.
func TestPlayAtOnceNoToast(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "progress playlist urls", true)
	row, _ := find(d.do("album", "album:al0"), "Song 1")
	if row.URI != "play" {
		t.Fatalf("song row %+v", row)
	}
	if r := d.do("play", row.Context); r.Message != "" || r.Status != "" {
		t.Fatalf("play reply %+v", r)
	}
}
