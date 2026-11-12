package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

func TestQ2AppName(t *testing.T) { // package.py zips this file as it is
	b, err := os.ReadFile("../../contrib/shanlingq2/q2app")
	if err != nil {
		t.Fatal(err)
	}
	// stop=0: no Stop Jellyfin row on its Loading… and error pages (Cancel instead); more=: Now
	// Playing's ⋯ on Jellyfin's queue opens its Play Queue (Q2 Pod's card-app-stop and -more).
	// favdir (U30): the folder the sync fills, whose songs' hearts Q2 Pod reports in fav.log.
	for _, w := range []string{"name=Jellyfin\n", "search=Search Jellyfin\n", "stop=0\n",
		"more=npqueue Play Queue\n", "favdir=Jellyfin\n"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("q2app lacks %q", w)
		}
	}
	// ⋯'s op, asked with no arguments as a first page, is the play queue (playqueue_test.go).
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	if r := d.do("npqueue"); r.Status != "" && r.Status != q2spot.OK || len(r.Rows) == 0 {
		t.Fatalf("play queue %+v", r)
	}
	s := jellyfin.Song{Title: "T", Album: "A", AlbumArtist: "B"}
	if p := library.SongPath("/mnt/mmc", s, "original", false); !strings.HasPrefix(p, "/mnt/mmc/Jellyfin/") {
		t.Errorf("the sync's folder: %s", p)
	}
}

// The release builds with a Go of its own choosing, not go.mod's minimum (M44).
func TestReleaseGoVersion(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "go-version-file") || !strings.Contains(string(b), "go-version:") {
		t.Error("release.yml builds with go.mod's minimum Go")
	}
}

// No other server's name shows anywhere in the app's pages.
func TestNoOtherServerNamesInUI(t *testing.T) {
	check := func(what string, r q2spot.Reply) {
		t.Helper()
		all := append(titles(r), r.Message)
		for _, row := range r.Rows {
			all = append(all, row.Subtitle)
		}
		for _, s := range all {
			if strings.Contains(s, "Navidrome") || strings.Contains(s, "Subsonic") {
				t.Errorf("%s shows %q", what, s)
			}
		}
	}
	for _, n := range []*nav{{albums: 3, songsPer: 2}, {empty: true}, {errCode: 40}} {
		d := newTestDaemon(t, n, "library", true)
		d.do("toggle", "album:al0")
		for _, op := range [][]string{{"menu"}, {"playlists"}, {"albums"}, {"artists"}, {"artist", "artist:ar1"}, {"search", "x"},
			{"playlist", "playlist:pl1"}, {"album", "album:al0"}, {"settings"}, {"account"}, {"onq2"}, {"library"},
			{"details", "Items|0|0|0|"}} {
			check(strings.Join(op, " "), d.do(op[0], op[1:]...))
		}
		check("sync", d.do("sync"))
		check("sync", d.preview(t))
	}
	check("signed out", newTestDaemon(t, &nav{}, "", false).do("menu"))
}

// Folders another app owns (q2-subsonic's Subsonic/ and its older Navidrome/) are never moved,
// read or removed: this app has no migration, and a sync touches only Jellyfin/.
func TestOtherAppFoldersUntouched(t *testing.T) {
	card := t.TempDir()
	theirs := []string{filepath.Join(card, "Navidrome", "A", "song.flac"), filepath.Join(card, "Subsonic", "B", "song.flac")}
	for _, p := range theirs {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("theirs"), 0o644)
	}
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	for _, p := range theirs {
		if b, err := os.ReadFile(p); err != nil || string(b) != "theirs" {
			t.Errorf("%s changed: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(card, "Jellyfin")); err != nil {
		t.Error("nothing synced to Jellyfin/:", err)
	}
}
