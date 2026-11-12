package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// Sync Jobs lists what is in Sync and not yet on the Q2; a press holds an item, so the sync adds
// only the rest, and a held item stays listed (and isn't "waiting") until it is resumed.
func TestSyncJobsHold(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3, favs: 2}, "", true)
	d.do("toggle", favKey)
	d.do("toggle", "album:al1")
	if got := titles(d.do("jobs")); len(got) != 2 || got[0] != "Favourites" || got[1] != "Album 001" {
		t.Fatalf("jobs %q", got)
	}
	if m := d.do("hold", favKey).Message; m != "Held" {
		t.Fatalf("hold %q", m)
	}
	if row, _ := find(d.do("jobs"), "Favourites"); !strings.HasPrefix(row.Subtitle, "Held") {
		t.Fatalf("held row %+v", row)
	}
	d.do("check")
	if _, ok := find(d.preview(t), "+3 songs · 0 MB"); !ok {
		t.Fatalf("preview %q", titles(d.preview(t)))
	}
	if got := titles(d.do("added")); len(got) != 3 || got[0] != "Song 0" {
		t.Fatalf("added %q", got)
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if got := titles(d.do("jobs")); len(got) != 1 || got[0] != "Favourites" {
		t.Fatalf("jobs after %q", got)
	}
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Fatal("a held item counted as waiting")
	}
	if m := d.do("hold", favKey).Message; m != "Resumed" {
		t.Fatalf("resume %q", m)
	}
	d.do("check")
	if _, ok := find(d.preview(t), "+2 songs · 0 MB"); !ok {
		t.Fatalf("preview after resume %q", titles(d.preview(t)))
	}
}

// After a restart an item's songs aren't fetched yet: one with songs on the card is "Not checked
// yet" in Sync Jobs, not On Q2; Sync Now stays away (it would show after every start).
func TestSyncJobsNotChecked(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 3}, "", true)
	d.do("toggle", favKey)
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	d.mu.Lock()
	clear(d.resolved) // as after a restart
	d.mu.Unlock()
	if row, _ := find(d.do("jobs"), "Favourites"); row.Subtitle != "Not checked yet" {
		t.Fatalf("jobs %+v", d.do("jobs").Rows)
	}
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Fatal("Sync Now before a preview")
	}
	d.do("check")
	d.preview(t)
	if got := titles(d.do("jobs")); len(got) != 1 || got[0] != "Nothing waiting to sync" {
		t.Fatalf("jobs after the preview %q", got)
	}
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Fatal("Sync Now after the preview")
	}
}

// Past 50 items, Sync Jobs pages, so every one can be held.
func TestSyncJobsPages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 60, songsPer: 1}, "", true)
	sel := selection.Set{Version: 1}
	for i := 0; i < 60; i++ {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: fmt.Sprintf("al%d", i), Name: fmt.Sprintf("Album %03d", i), Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	rows := walkWindow(t, d, "jobs", "")
	if len(rows) != 60 || rows[59].Context != "album:al59" {
		t.Fatalf("%d rows, last %+v", len(rows), rows[len(rows)-1])
	}
}

// Holding while a sync runs starts it again without the held item.
func TestSyncJobsHoldRestarts(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, favs: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", favKey)
	d.do("toggle", "album:al1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", func() bool { return d.runner.Snapshot().State == "running" })
	if m := d.do("hold", favKey).Message; m != "Held · sync restarting" {
		t.Fatalf("hold %q", m)
	}
	close(n.block)
	eventually(t, "the restarted sync", func() bool {
		s := d.runner.Snapshot()
		return s.State == "done" && s.Total == 3
	})
}

// Holding while a sync is paused stops it; it starts again only from the Sync page's Start Sync.
func TestSyncJobsHoldWhilePaused(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, favs: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", favKey)
	d.do("toggle", "album:al1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", func() bool { return d.runner.Snapshot().State == "running" })
	d.do("pause")
	eventually(t, "the sync to pause", func() bool { return d.runner.Snapshot().State == "paused" })
	if m := d.do("hold", favKey).Message; m != "Held · press Start Sync to continue" {
		t.Fatalf("hold %q", m)
	}
	close(n.block)
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "idle" })
	if _, ok := find(d.preview(t), "Start Sync"); !ok {
		t.Fatal("no Start Sync")
	}
	d.do("start")
	eventually(t, "the sync without the held item", func() bool {
		s := d.runner.Snapshot()
		return s.State == "done" && s.Total == 3
	})
}
