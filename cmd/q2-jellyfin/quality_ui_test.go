package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

var wantChoices = []string{"Original (no transcoding)", "Opus 320 kbps", "Opus 288 kbps", "Opus 256 kbps", "Opus 224 kbps",
	"Opus 192 kbps", "Opus 160 kbps", "Opus 128 kbps", "Opus 96 kbps", "Opus 64 kbps"}

// Sync Quality is Original (the server's file, untouched); its page offers Opus at each of the
// nine bitrates, highest first, and marks the one chosen.
func TestSyncQualityPage(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 1}, "", true)
	row, _ := find(d.do("storage"), "Sync Quality: Original")
	if row.Kind != "list" || row.URI != "defq" || row.Subtitle != "For every synced song" {
		t.Fatalf("settings row %+v", row)
	}
	r := d.do("defq")
	if strings.Join(titles(r), "|") != strings.Join(wantChoices, "|") || r.Rows[0].Subtitle != "Selected" || r.Message != "refresh" {
		t.Fatalf("page %q %+v", titles(r), r.Rows[0])
	}
	opus, _ := find(r, "Opus 96 kbps")
	if m := d.do(opus.URI, opus.Context).Message; m != "Sync Quality: Opus 96 kbps" {
		t.Fatalf("toast %q", m)
	}
	if row, _ := find(d.do("defq"), "Opus 96 kbps"); row.Subtitle != "Selected" {
		t.Fatalf("mark %+v", row)
	}
	if _, ok := find(d.do("storage"), "Sync Quality: Opus 96 kbps"); !ok {
		t.Fatalf("storage %q", titles(d.do("storage")))
	}
}

// Every item syncs at Sync Quality, whatever quality was saved with it (each item had its own up
// to 0.6): a change of Sync Quality brings the songs on the card again at the new one, and the
// next save of the selection writes it over the old.
func TestEveryItemAtSyncQuality(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 1}, "", true)
	sel := selection.Set{Version: 1}
	sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al0", Name: "Album 000", Quality: "opus-64"})
	sel.Save(filepath.Join(d.dir, "selection.json"))
	d.syncOK(t)
	for _, e := range d.manifest().Songs {
		if e.Quality != "original" {
			t.Fatalf("synced at %q, not Sync Quality", e.Quality)
		}
	}
	d.do("setdefq", "opus-128")
	if row, _ := find(d.do("albums"), "Sync Now"); row.Subtitle != "1 waiting" {
		t.Fatalf("Sync Now %+v", row)
	}
	d.syncOK(t)
	for _, e := range d.manifest().Songs {
		if e.Quality != "opus-128" {
			t.Fatalf("synced again at %q", e.Quality)
		}
	}
	d.do("toggle", "album:al1")
	for _, it := range d.selected(t) {
		if it.Quality != "opus-128" {
			t.Fatalf("saved %+v", it)
		}
	}
}

// Streaming Quality offers the same choices; a setting saved as "Same as Default" (empty) is Sync
// Quality's, and keeps that value when Sync Quality changes.
func TestStreamQualityPage(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	os.WriteFile(filepath.Join(d.dir, "settings.json"), []byte(`{"defaultQuality":"opus-224"}`), 0o644) // as 0.6 left it
	if _, ok := find(d.do("storage"), "Streaming Quality: Opus 224 kbps"); !ok {
		t.Fatalf("%q", titles(d.do("storage")))
	}
	r := d.do("streamq")
	if strings.Join(titles(r), "|") != strings.Join(wantChoices, "|") || r.Message != "refresh" {
		t.Fatalf("%q", titles(r))
	}
	if row, _ := find(r, "Opus 224 kbps"); row.Subtitle != "Selected" {
		t.Fatalf("mark %+v", row)
	}
	d.do("setdefq", "original")
	if d.settings().stream() != "opus-224" {
		t.Fatalf("followed Sync Quality: %q", d.settings().stream())
	}
	opus, _ := find(r, "Opus 160 kbps")
	if m := d.do(opus.URI, opus.Context).Message; m != "Streaming Quality: Opus 160 kbps" || d.settings().stream() != "opus-160" {
		t.Fatalf("%q %q", m, d.settings().stream())
	}
}

// A held item keeps its songs at the quality on the card, as the plan does: a song it shares with
// another item, after Sync Quality is lowered, stays as it is and nothing waits for a sync.
func TestHeldItemKeepsQualityOnCard(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "playlist:pl1") // al0's songs
	d.syncOK(t)
	d.do("hold", "album:al0")
	d.do("setdefq", "opus-128")
	d.syncOK(t)
	for _, e := range d.manifest().Songs {
		if e.Quality != "original" {
			t.Fatalf("a held item's song synced again at %q", e.Quality)
		}
	}
	r := d.do("playlists")
	if row, ok := find(r, "Sync Now"); ok {
		t.Fatalf("Sync Now %+v in %q", row, titles(r))
	}
}

// A held Entire Library holds the playlists it takes in too: a change of Sync Quality plans nothing
// for them and shows no Sync Now.
func TestHeldLibraryHoldsItsPlaylists(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true) // pl1 holds al0's songs
	d.do("setdefq", "opus-128")
	d.do("toggle", "library:all")
	d.syncOK(t)
	d.do("hold", "library:all")
	d.do("setdefq", "original")
	d.preview(t)
	d.mu.Lock()
	pl := d.daemon.preview.plan
	d.mu.Unlock()
	if pl.Changes() {
		t.Fatalf("plan %+v", *pl)
	}
	if row, ok := find(d.do("playlists"), "Sync Now"); ok {
		t.Fatalf("Sync Now %+v", row)
	}
}

// A change of Sync Quality while a sync runs starts it again at the new quality, as a hold does:
// the songs it hadn't fetched yet don't come at the old one.
func TestSyncQualityChangeRestartsSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", func() bool { return d.runner.Snapshot().State == "running" })
	if m := d.do("setdefq", "opus-128").Message; m != "Sync Quality: Opus 128 kbps · checking the sync again" {
		t.Fatalf("toast %q", m)
	}
	close(n.block)
	eventually(t, "the restarted sync", func() bool {
		s := d.runner.Snapshot()
		return s.State == "done" && s.Total == 3
	})
	for _, e := range d.manifest().Songs {
		if e.Quality != "opus-128" {
			t.Fatalf("synced at %q", e.Quality)
		}
	}
}

// After a finished sync, a change of Sync Quality gives the Sync page's "Sync complete" way to the
// new preview, which offers to bring the songs again.
func TestSyncQualityChangeAfterSyncPreviews(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	d.do("setdefq", "opus-128")
	if r := d.preview(t); !slices.Contains(titles(r), "Start Sync") {
		t.Fatalf("Sync page %q", titles(r))
	}
}

// runningSync starts a sync of album al0 whose downloads wait for n.block.
func runningSync(t *testing.T, n *nav) *tdaemon {
	t.Helper()
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", func() bool { return d.runner.Snapshot().State == "running" })
	return d
}

// A sync paused when Sync Quality changes is stopped, not started again by itself: the Sync page
// offers the new preview's Start Sync.
func TestPausedSyncQualityChangeWaitsForStart(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	d := runningSync(t, n)
	d.do("pause")
	eventually(t, "the sync to pause", func() bool { return d.runner.Snapshot().State == "paused" })
	if m := d.do("setdefq", "opus-128").Message; m != "Sync Quality: Opus 128 kbps · press Start Sync to continue" {
		t.Fatalf("toast %q", m)
	}
	close(n.block)
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "idle" })
	r := d.preview(t)
	time.Sleep(200 * time.Millisecond) // the preview done: nothing starts it
	if st := d.runner.Snapshot().State; st != "idle" || !slices.Contains(titles(r), "Start Sync") {
		t.Fatalf("state %s, Sync page %q", st, titles(r))
	}
}

// A sync stopped by a change of Sync Quality doesn't start again by itself when the new preview
// can't read the selection: the preview fails.
func TestRestartWithDamagedSelection(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	d := runningSync(t, n)
	os.WriteFile(filepath.Join(d.dir, "selection.json"), []byte(`{"items":[{"kind":`), 0o644)
	d.do("setdefq", "opus-128")
	close(n.block)
	d.bg.Wait()
	d.mu.Lock()
	p, auto := d.daemon.preview, d.autoStart
	d.mu.Unlock()
	if p == nil || !p.done || p.err == nil || auto {
		t.Fatalf("preview %+v, autoStart %v", p, auto)
	}
	if st := d.runner.Snapshot().State; st == "running" {
		t.Fatalf("a sync started: %s", st)
	}
}

// Sync Quality chosen again as it is changes nothing: a sync under way goes on, its download kept.
func TestSyncQualitySameKeepsSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	d := runningSync(t, n)
	d.mu.Lock()
	gen := d.gen
	d.mu.Unlock()
	if m := d.do("setdefq", "original").Message; m != "Sync Quality: Original" {
		t.Fatalf("toast %q", m)
	}
	d.bg.Wait()
	d.mu.Lock()
	again := d.gen
	d.mu.Unlock()
	if st := d.runner.Snapshot().State; st != "running" || again != gen {
		t.Fatalf("state %s, gen %d -> %d", st, gen, again)
	}
	close(n.block)
}

// Always Re-encode under an Original Sync Quality changes no synced song, only streaming: a sync
// under way goes on, its download kept. Under an Opus one it starts the sync again.
func TestReencodeRestartsOnlyUnderOpus(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	defer close(n.block)
	d := runningSync(t, n)
	d.mu.Lock()
	gen := d.gen
	d.mu.Unlock()
	if m := d.do("reencode").Message; m != "Always Re-encode: On" {
		t.Fatalf("toast %q", m)
	}
	d.bg.Wait()
	d.mu.Lock()
	again := d.gen
	d.mu.Unlock()
	if st := d.runner.Snapshot().State; st != "running" || again != gen {
		t.Fatalf("state %s, gen %d -> %d", st, gen, again)
	}
	d.do("setdefq", "opus-128")
	d.bg.Wait()
	eventually(t, "the sync to run again", func() bool { return d.runner.Snapshot().State == "running" })
	if m := d.do("reencode").Message; m != "Always Re-encode: Off · checking the sync again" {
		t.Fatalf("toast under Opus %q", m)
	}
}

// Two changes of Sync Quality in a row mid-sync, the second while the first's preview is still at
// work (the sync stopped): the sync runs again at the second.
func TestSyncQualityChangedTwiceRestartsAtLast(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{}), holdAlbum: make(chan struct{})}
	var held atomic.Bool // getAlbum waits while it is set
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			if !held.Load() {
				select {
				case n.holdAlbum <- struct{}{}:
				case <-time.After(time.Millisecond):
				}
			}
		}
	}()
	d := runningSync(t, n)
	held.Store(true)
	time.Sleep(20 * time.Millisecond)
	d.do("setdefq", "opus-128")
	eventually(t, "the sync to stop", func() bool {
		d.mu.Lock()
		p := d.daemon.preview
		d.mu.Unlock()
		return d.runner.Snapshot().State == "idle" && p != nil
	})
	d.do("setdefq", "opus-64")
	held.Store(false)
	close(n.block)
	eventually(t, "the restarted sync", func() bool {
		s := d.runner.Snapshot()
		return s.State == "done" && s.Total == 3
	})
	for _, e := range d.manifest().Songs {
		if e.Quality != "opus-64" {
			t.Fatalf("synced at %q", e.Quality)
		}
	}
}

// A setting that can't be saved says so, and isn't shown as chosen; a sync under way goes on.
func TestSettingNotSaved(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, block: make(chan struct{})}
	d := runningSync(t, n)
	if err := os.Mkdir(filepath.Join(d.dir, "settings.json"), 0o755); err != nil { // a file can't be written there
		t.Fatal(err)
	}
	if r := d.do("setstreamq", "opus-160"); r.Status != q2spot.Error || r.Message != settingsDamaged { // nor read
		t.Fatalf("reply %+v", r)
	}
	if r := d.do("setdefq", "opus-128"); r.Status != q2spot.Error || r.Message != "Couldn't save the setting" {
		t.Fatalf("setdefq reply %+v", r)
	}
	if r := d.do("swipe"); r.Status != q2spot.Error || r.Message != settingsDamaged { // unreadable: nothing written (A5-P2-2)
		t.Fatalf("swipe reply %+v", r)
	}
	d.bg.Wait()
	if st := d.runner.Snapshot().State; st != "running" {
		t.Fatalf("state %s", st)
	}
	close(n.block)
}

// Swipe Between Sections with settings.json readable but the folder not writable (a full or
// read-only card) says it wasn't saved, as every other setting does (review A7a-P1-1).
func TestSwipeNotSaved(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	d := newTestDaemon(t, &nav{}, "", true)
	if err := d.saveSettings(settings{}); err != nil {
		t.Fatal(err)
	}
	os.Chmod(d.dir, 0o555)
	t.Cleanup(func() { os.Chmod(d.dir, 0o755) })
	on := d.swipeOn()
	if r := d.do("swipe"); r.Status != q2spot.Error || r.Message != "Couldn't save the setting" {
		t.Fatalf("swipe reply %+v", r)
	}
	if d.swipeOn() != on {
		t.Fatal("swipe changed though it wasn't saved")
	}
}
