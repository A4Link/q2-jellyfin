package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Each section's search box searches that section: Albums finds albums and songs, Artists
// artists, Playlists playlists.
func TestSectionSearch(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 1, playlists: 2}, "", true)

	r := d.do("albums")
	if r.Rows[0].Kind != "search" || r.Rows[0].Title != "Search Albums" {
		t.Fatalf("albums %+v", r.Rows[0])
	}
	got := strings.Join(titles(d.do("search", "x")), "|")
	if !strings.Contains(got, "Album 000") || !strings.Contains(got, "Song 0") || strings.Contains(got, "Artist") {
		t.Fatalf("album search %q", got)
	}

	if r := d.do("artists"); r.Rows[0].Kind != "search" || r.Rows[0].Title != "Search Artists" {
		t.Fatalf("artists %+v", r.Rows[0])
	}
	got = strings.Join(titles(d.do("search", "Artist")), "|")
	if got != "Artist" {
		t.Fatalf("artist search %q", got)
	}

	if r := d.do("playlists"); r.Rows[0].Kind != "search" || r.Rows[0].Title != "Search Playlists" {
		t.Fatalf("playlists %+v", r.Rows[0])
	}
	if got := strings.Join(titles(d.do("search", "mix 2")), "|"); got != "Mix 2" {
		t.Fatalf("playlist search %q", got)
	}
	if got := strings.Join(titles(d.do("search", "nothing")), "|"); got != "No playlists called that" {
		t.Fatalf("no match %q", got)
	}
}

// A search typed on Albums searches Albums even when an Artists list the user swiped away from,
// still loading, finishes after Albums answered: Q2 Pod asks for one page at a time, so the box is
// the page asked for last, not the one answered last.
func TestSearchAfterSlowSectionLeft(t *testing.T) {
	n := &nav{albums: 2, songsPer: 1, holdArtists: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	done := make(chan struct{})
	go func() { d.do("artists"); close(done) }()
	eventually(t, "the artist list asked for", func() bool { return n.artistReqs.Load() > 0 })
	d.do("albums")
	close(n.holdArtists)
	<-done
	got := strings.Join(titles(d.do("search", "x")), "|")
	if !strings.Contains(got, "Album 000") || !strings.Contains(got, "Song 0") || strings.Contains(got, "Artist") {
		t.Fatalf("search from Albums after a slow Artists %q", got)
	}
}

// Newly Added lists albums, newest first as the server orders them, under a search of everything.
func TestNewlyAdded(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 1}, "", true)
	r := d.do("latest")
	if got := strings.Join(titles(r), "|"); got != "Search|Album 000|Album 001|Album 002" {
		t.Fatalf("%q", got)
	}
	if row := r.Rows[1]; r.Rows[0].Kind != "search" || row.Kind != "list" || row.URI != "album" {
		t.Fatalf("%+v", r.Rows[:2])
	}
	if got := titles(d.do("search", "Album")); len(got) < 2 || got[0] != "Album 000" {
		t.Fatalf("search from Newly Added %q", got)
	}
	if got := titles(newTestDaemon(t, &nav{empty: true}, "", true).do("latest")); len(got) != 2 || got[1] != "Nothing added yet" {
		t.Fatalf("empty %q", got)
	}
}

// Newly Added's search box and 50 albums are more than a reply holds: every album comes once over
// the pages, with or without Sync Now first.
func TestNewlyAddedPages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 50, songsPer: 1}, "", true)
	for _, syncNow := range []bool{false, true} {
		if syncNow {
			d.do("toggle", "album:al0")
		}
		seen := map[string]int{}
		for _, row := range walkWindow(t, d, "latest", "") {
			seen[row.Title]++
		}
		if (seen["Sync Now"] == 1) != syncNow {
			t.Errorf("sync now %v: %d Sync Now rows", syncNow, seen["Sync Now"])
		}
		for i := 0; i < 50; i++ {
			if a := fmt.Sprintf("Album %03d", i); seen[a] != 1 {
				t.Errorf("sync now %v: %s shown %d times", syncNow, a, seen[a])
			}
		}
	}
}

// With Q2 Pod's "sections", the first page is the five sections, for it to show as pages a swipe
// moves between; each is a list row whose URI fills it. Without, the same rows are the main list.
func TestSectionsForFirmware(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "sections", true)
	r := d.do("menu")
	if r.Message != "sections" || strings.Join(titles(r), "|") != "Newly Added|Albums|Artists|Playlists|Settings" {
		t.Fatalf("%q %q", r.Message, titles(r))
	}
	for i, op := range []string{"latest", "albums", "artists", "playlists", "settings"} {
		if row := r.Rows[i]; row.Kind != "list" || row.URI != op {
			t.Errorf("%+v", row)
		}
	}
	if r := newTestDaemon(t, &nav{}, "", true).do("menu"); r.Message != "refresh" {
		t.Fatalf("without sections: %q", r.Message)
	}
}

// With sections there is no main list to show a stream's failure: Settings shows it, first.
func TestSectionsStreamErrorInSettings(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2, mediaErr: 70}, "sections", true)
	d.stream.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	d.do("menu") // the firmware shows sections
	d.do("play", "album:al0 0")
	var r q2spot.Reply
	eventually(t, "the stream error", func() bool { r = d.do("settings"); _, ok := find(r, "Couldn't stream Song 0"); return ok })
	if r.Rows[0].Title != "Couldn't stream Song 0" || r.Rows[1].Title != "Details" || r.Rows[2].Title != "Sync" {
		t.Fatalf("%q", titles(r))
	}
}
