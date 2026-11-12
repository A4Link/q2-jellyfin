package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

// cardOf makes d's card hold room bytes above Space to Keep Free (512 MB by default), less what the
// sync has put in Jellyfin/: free space shrinks as songs arrive, as on a real card.
func cardOf(d *tdaemon, room int64) {
	d.free = func(string) int64 {
		var used int64
		filepath.WalkDir(filepath.Join(d.card, "Jellyfin"), func(_ string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if fi, err := e.Info(); err == nil {
					used += fi.Size()
				}
			}
			return nil
		})
		return 512e6 + room - used
	}
}

// A sync stops before a song would leave the card under Space to Keep Free: the songs before it
// are on the card and in their playlist, and the Sync page says why and offers Resume.
func TestSyncStopsAtReserve(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 3}, "", true) // three 1000-byte songs
	cardOf(d, 2500)                                                        // room for two
	d.do("toggle", favKey)
	d.preview(t)
	d.do("start")
	eventually(t, "the stop", func() bool { return d.runner.Snapshot().State == "paused" })
	s := d.runner.Snapshot()
	var sl *syncer.SpaceLowError
	if !errors.As(s.Err, &sl) || s.Done != 2 || sl.Reserve != 512e6 {
		t.Fatalf("%+v", s)
	}
	got := strings.Join(titles(d.do("sync")), "|")
	for _, w := range []string{"Stopped: the card is nearly full", "keeping 512 MB free", "Free some space or sync less, then Resume", "Resume"} {
		if !strings.Contains(got, w) {
			t.Errorf("Sync page lacks %q: %q", w, got)
		}
	}
	b, err := os.ReadFile(filepath.Join(d.card, "_explaylist_data", "Favourites.m3u"))
	if err != nil || strings.Count(string(b), "Song ") != 2 {
		t.Fatalf("playlist of what arrived: %q %v", b, err)
	}
	// Space freed: Resume finishes the sync.
	cardOf(d, 1<<30)
	d.do("resume")
	eventually(t, "the rest", func() bool { return d.runner.Snapshot().State == "done" })
	if b, _ := os.ReadFile(filepath.Join(d.card, "_explaylist_data", "Favourites.m3u")); strings.Count(string(b), "Song ") != 3 {
		t.Fatalf("after Resume %q", b)
	}
}

// Sync Favourites: one press puts Favourites in Sync (at Sync Quality) and starts the sync.
func TestSyncFavouritesOnePress(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
	d.do("setdefq", "opus-160")
	r := d.do("sync")
	if row := r.Rows[2]; row.Kind != "action" || row.URI != "syncfav" || row.Title != "Sync Favourites" {
		t.Fatalf("Sync page %+v", r.Rows)
	}
	if m := d.do("syncfav").Message; m != "Added to Sync · 2 songs · starting" {
		t.Fatalf("%q", m)
	}
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if it := d.selected(t); len(it) != 1 || it[0].Kind != "favorites" || it[0].Quality != "opus-160" {
		t.Fatalf("%+v", it)
	}
	m, _ := filepath.Glob(filepath.Join(d.card, "Jellyfin", "*", "*", "*.opus"))
	if len(m) != 2 {
		t.Fatalf("synced %v", m)
	}
	if row, _ := find(d.do("sync"), "Sync Favourites"); row.Subtitle != "In Sync · press to sync now" {
		t.Fatalf("%+v", row)
	}
	if m := d.do("syncfav").Message; m != "Syncing Favourites · starting" { // in Sync already: just sync
		t.Fatalf("again %q", m)
	}
}

// Choose Playlists lists the playlists, each pressed in and out of Sync.
func TestChoosePlaylists(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2, playlists: 2}, "", true)
	r := d.do("syncpl")
	if r.Message != "refresh" || strings.Join(titles(r), "|") != "Mix|Mix 2" {
		t.Fatalf("%q", titles(r))
	}
	row := r.Rows[1]
	if row.Kind != "action" || row.URI != "toggle" || row.Context != "playlist:pl2" || row.Subtitle != "2 songs" {
		t.Fatalf("%+v", row)
	}
	if m := d.do(row.URI, row.Context).Message; m != "Added to Sync · 2 songs" {
		t.Fatalf("%q", m)
	}
	if row, _ := find(d.do("syncpl"), "Mix 2"); row.Subtitle != "In Sync · 2 songs" {
		t.Fatalf("%+v", row)
	}
	if row, _ := find(d.do("sync"), "Choose Playlists"); row.Subtitle != "1 playlist in Sync" || row.Kind != "list" {
		t.Fatalf("%+v", row)
	}
}

// Choose Playlists that fails stays live, so it shows the playlists once the server answers.
func TestChoosePlaylistsErrorRefreshes(t *testing.T) {
	d := newTestDaemon(t, &nav{errCode: 70}, "", true)
	r := d.do("syncpl")
	if _, ok := find(r, "Details"); !ok || r.Message != "refresh" {
		t.Fatalf("%q %q", r.Message, titles(r))
	}
}

// Past 50 playlists Choose Playlists pages, every page live, so the 60th can be chosen too.
func TestChoosePlaylistsPages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, playlists: 60}, "", true)
	var rows []string
	for cursor, i := "", 0; i == 0 || cursor != ""; i++ {
		r := d.do("syncpl", cursor)
		if r.Message != "refresh" || len(r.Rows) > 50 || i > 5 {
			t.Fatalf("page %d: %q, %d rows", i, r.Message, len(r.Rows))
		}
		rows, cursor = append(rows, titles(r)...), r.Cursor
	}
	if len(rows) != 60 || rows[59] != "Mix 60" {
		t.Fatalf("%d rows: %q", len(rows), rows)
	}
}

func TestKeepFreeSetting(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	if _, ok := find(d.do("storage"), "Space to Keep Free: 512 MB"); !ok {
		t.Fatalf("%q", titles(d.do("storage")))
	}
	for _, want := range []string{"1 GB", "2 GB", "256 MB", "512 MB"} {
		if m := d.do("keepfree").Message; m != "Space to Keep Free: "+want {
			t.Fatalf("got %q, want %s", m, want)
		}
	}
	d.do("keepfree") // 1 GB
	if d.runner.Reserve() != 1e9 {
		t.Fatalf("reserve %d", d.runner.Reserve())
	}
}

// A Sync Favourites (or Hold) whose preview hadn't finished never starts a sync after Sign Out: the
// next account's preview waits for Start, as any other (audit 2026-11-10 #1).
func TestSignOutDropsAutoStart(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
	d.do("toggle", favKey) // in Sync, not started
	d.mu.Lock()
	d.autoStart = true // as Sync Favourites leaves it while its preview computes
	d.mu.Unlock()
	srv := d.server
	if err := d.signOut(); err != nil {
		t.Fatal(err)
	}
	d.bg.Wait()
	if err := creds.Save(d.dir, jellyfin.Creds{Server: srv, User: "u", Token: "tok", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	d.do("sync")
	d.bg.Wait() // the preview
	if st := d.runner.Snapshot().State; st != "idle" {
		t.Fatalf("sync %s with no Start", st)
	}
}

// A sync that takes one album out and puts another in, on a card with room only once the first is
// gone, removes first and finishes, rather than stopping at the same song every time (audit
// 2026-11-10 #2). The preview counts the removals as freed.
func TestSyncRemovesFirstWhenShort(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "", true) // 1000-byte songs
	cardOf(d, 1<<30)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })
	used := 512e6 + 1<<30 - d.free("")
	cardOf(d, used+500) // room for no new song until al0 goes
	d.do("toggle", "album:al0")
	d.do("toggle", "album:al1")
	d.preview(t) // its "fits" counts the removals as freed: plan's TestFitsCountsRemovals
	d.do("start")
	eventually(t, "the second sync", func() bool { st := d.runner.Snapshot().State; return st == "done" || st == "paused" })
	if s := d.runner.Snapshot(); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	m, _ := filepath.Glob(filepath.Join(d.card, "Jellyfin", "*", "*", "*"))
	var songs []string
	for _, p := range m {
		if !strings.HasSuffix(p, "cover.jpg") {
			songs = append(songs, filepath.Base(filepath.Dir(p))+"/"+filepath.Base(p))
		}
	}
	if len(songs) != 3 || strings.Contains(strings.Join(songs, " "), "Album 000") {
		t.Fatalf("on the card: %q", songs)
	}
}
