package main

import (
	"slices"
	"strings"
	"testing"

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

// Swipe Between Sections: on by default; off on request, and on again. An earlier version's
// stored setting is kept: "on" swipes, "off" (also what a card moved to firmware without sections
// was turned to) stays off until turned on.
func TestSwipeSetting(t *testing.T) {
	card := t.TempDir()
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1}, "urls", true)
	if r := d.do("menu"); r.Message != "sections" {
		t.Fatalf("default: %q", r.Message)
	}
	if row := swipeRowOf(t, d); row.Kind != "action" || row.Title != "Swipe Between Sections: On" {
		t.Fatalf("%+v", row)
	}
	if r := d.do("swipe"); !strings.Contains(r.Message, "Swipe off") {
		t.Fatalf("%q", r.Message)
	}
	if r := d.do("menu"); r.Message != "refresh" || len(r.Rows) < 5 {
		t.Fatalf("off: %q %q", r.Message, titles(r))
	}
	if row := swipeRowOf(t, d); row.Title != "Swipe Between Sections: Off" {
		t.Fatalf("%+v", row)
	}
	if again := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1}, "urls", true); again.do("menu").Message != "refresh" {
		t.Fatal("off not kept")
	}
	if r := d.do("swipe"); !strings.Contains(r.Message, "Swipe on") {
		t.Fatalf("%q", r.Message)
	}
	if r := d.do("menu"); r.Message != "sections" {
		t.Fatalf("back on: %q", r.Message)
	}
	s := d.daemon.settings()
	s.Swipe = "on" // as an earlier version stored the default
	d.saveSettings(s)
	if r := d.do("menu"); r.Message != "sections" {
		t.Fatalf("stored on: %q", r.Message)
	}
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

// A restarted app is asked for Settings, not the menu (Q2 Pod takes the sections once): the main
// list's notices are in Settings with Swipe Between Sections on, and not with it off, where the
// main list ends with them (review A7a-P1-8).
func TestNoticesAfterRestart(t *testing.T) {
	card := t.TempDir()
	n := &nav{albums: 1, songsPer: 1}
	notice := "A favourite wasn't saved on the server"
	for _, off := range []bool{false, true} {
		d := newTestDaemonOn(t, card, n, "urls", true)
		d.rep.refused = true
		rows := titles(d.do("settings"))
		if c := slices.Index(rows, notice); (c < 0) != off || c >= 0 && slices.Index(rows[c+1:], notice) >= 0 {
			t.Fatalf("swipe off %v: settings %q", off, rows)
		}
		if !off {
			d.do("swipe") // off, for the next run
		}
	}
}
