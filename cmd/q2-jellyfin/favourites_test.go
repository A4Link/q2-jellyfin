package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Favourites is the first of the playlists, after Playlists' search box, as Liked Songs and My
// Collection are elsewhere. Its row is a playlist's: held to toggle Sync, with its status (audit
// 8), the same with Swipe Between Sections on and off.
func TestFavouritesInPlaylists(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2, favs: 1}, "urls now sections window", true)
	got := strings.Join(titles(d.do("playlists")), "|")
	if !strings.HasPrefix(got, "Search Playlists|Sort|Favourites|Mix") {
		t.Fatalf("playlists %q", got)
	}
	row, _ := find(d.do("playlists"), "Favourites")
	if row.Kind != "list" || row.URI != "favorites" || row.Context != favKey || row.Position != "toggle" || row.Subtitle != "" {
		t.Fatalf("row %+v", row)
	}
	d.do("toggle", favKey)
	if row, _ := find(d.do("playlists"), "Favourites"); row.Subtitle != "Waiting to sync" {
		t.Fatalf("chosen %+v", row)
	}
	d.syncOK(t)
	on, _ := find(d.do("playlists"), "Favourites")
	d.do("swipe")
	if d.swipeOn() {
		t.Fatal("swipe stayed on")
	}
	if off, _ := find(d.do("playlists"), "Favourites"); on.Subtitle != "On Q2" || off != on {
		t.Fatalf("swipe on %+v, off %+v", on, off)
	}
}

// Its page is a list's: Add to Sync, Play, then the songs.
func TestFavouritesPage(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
	r := d.do("favorites", favKey)
	got := titles(r)
	if len(got) != 4 || got[0] != "Add to Sync" || got[1] != "Play" || got[2] != "Song 0" || got[3] != "Song 1" {
		t.Fatalf("page %q", got)
	}
	if m := d.do("play", favKey+" 1").Message; m != "Buffering Song 1…" {
		t.Fatalf("play %q", m)
	}
}

func TestFavouritesEmpty(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	if _, ok := find(d.do("favorites", favKey), "No favourites yet"); !ok {
		t.Fatalf("%q", titles(d.do("favorites", favKey)))
	}
}

// Synced, Favourites is a playlist on the Q2: its songs land in Jellyfin/ and "Favourites.m3u"
// lists them in the server's order.
func TestFavouritesSync(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
	if m := d.do("toggle", favKey).Message; m != "Added to Sync · 2 songs" {
		t.Fatalf("toggle %q", m)
	}
	if _, ok := find(d.do("favorites", favKey), "Remove from Sync"); !ok {
		t.Fatalf("page after toggle %q", titles(d.do("favorites", favKey)))
	}
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	b, err := os.ReadFile(filepath.Join(d.card, "_explaylist_data", "Favourites.m3u"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var songs []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "#") {
			songs = append(songs, l)
		}
	}
	if len(songs) != 2 || !strings.Contains(songs[0], "Song 0") || !strings.Contains(songs[1], "Song 1") {
		t.Fatalf("m3u %q", b)
	}
}

// A song already in Favourites, when Favourites is in Sync, says so instead of being added twice.
func TestSongIncludedViaFavourites(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
	d.do("toggle", favKey)
	if m := d.do("toggle", "song:al0.1").Message; m != "Included via Favourites" {
		t.Fatalf("%q", m)
	}
}

// With Entire Library on, Favourites is still a playlist of its own (audit 7): Sync Favourites
// adds it, its songs are downloaded once, Favourites.m3u lists them, and a press takes it out.
func TestFavouritesUnderLibrary(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
	d.do("toggle", "library:all")
	if row, _ := find(d.do("sync"), "Sync Favourites"); row.Subtitle != "Add your favourites to Sync" {
		t.Fatalf("Sync page %+v", row)
	}
	if row, _ := find(d.do("favorites", favKey), "Add to Sync"); row.Kind != "action" {
		t.Fatalf("page %q", titles(d.do("favorites", favKey)))
	}
	if m := d.do("syncfav").Message; m != "Added to Sync · 2 songs" {
		t.Fatalf("%q", m)
	}
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if n := len(d.manifest().Songs); n != 3 {
		t.Fatalf("%d songs on the card", n)
	}
	if b, _ := os.ReadFile(filepath.Join(d.card, "_explaylist_data", "Favourites.m3u")); strings.Count(string(b), "Song ") != 2 {
		t.Fatalf("m3u %q", b)
	}
	if row, _ := find(d.do("sync"), "Sync Favourites"); row.Subtitle != "In Sync · press to check for new hearts" {
		t.Fatalf("after %+v", row)
	}
	if m := d.do("toggle", favKey).Message; m != "Removed from Sync" {
		t.Fatalf("remove %q", m)
	}
	if it := d.selected(t); len(it) != 1 || it[0].Kind != "library" {
		t.Fatalf("%+v", it)
	}
}
