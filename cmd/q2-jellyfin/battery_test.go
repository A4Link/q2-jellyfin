package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/library"
)

// Battery audit 2026-11-09 (B1-B12): the work the app saves.

// B2: twelve songs of one album cost one cover download, and the rows each get their file; drawn
// again, nothing is fetched.
func TestCoverFetchedOncePerItem(t *testing.T) {
	n := &nav{albums: 1, songsPer: 12, favs: 12, songArt: true}
	d := newTestDaemon(t, n, "art", true)
	d.do("favorites", favKey)
	if got := n.images.Load(); got != 1 {
		t.Fatalf("%d cover requests for 12 songs of one album", got)
	}
	rows, _ := filepath.Glob(filepath.Join(d.artDir(), "[0-9a-f]*.jpg"))
	if len(rows) < 12 {
		t.Fatalf("%d row files, want one per song", len(rows))
	}
	d.do("favorites", favKey)
	if got := n.images.Load(); got != 1 {
		t.Errorf("drawn again: %d cover requests", got)
	}
}

// B11: a cover that failed (a 5xx) isn't asked again on the next refresh, within a minute.
func TestFailedCoverNotRetriedAtOnce(t *testing.T) {
	n := &nav{albums: 3, songsPer: 1, coverStatus: 500}
	d := newTestDaemon(t, n, "art", true)
	d.do("albums")
	first := n.images.Load()
	if first == 0 {
		t.Fatal("no cover asked")
	}
	d.do("albums")
	if got := n.images.Load(); got != first {
		t.Errorf("asked again at once: %d then %d", first, got)
	}
}

// B3: Play on an album just opened uses the list the page fetched, not another request.
func TestPlayReusesThePagesList(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3}
	d := newTestDaemon(t, n, "", true)
	d.do("album", "album:al1")
	before := n.albumReqs.Load()
	if r := d.do("play", "album:al1 1"); r.Status != "" {
		t.Fatalf("%+v", r)
	}
	if got := n.albumReqs.Load(); got != before {
		t.Errorf("Play fetched the album again: %d requests, then %d", before, got)
	}
}

// B10: Newly Added's second reply reuses its first's list.
func TestNewlyAddedSecondPageReuses(t *testing.T) {
	n := &nav{albums: 60, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	r := d.do("latest")
	if r.Cursor == "" {
		t.Fatalf("one reply only: %d rows", len(r.Rows))
	}
	before := n.albumLists.Load()
	d.do("latest", r.Cursor)
	if got := n.albumLists.Load(); got != before {
		t.Errorf("second page asked again: %d then %d", before, got)
	}
}

// B9: the Settings line follows the latest real request, with no ping: nothing asked yet shows only
// the sync, a list that came says Connected, and a proxy answering 502 (Jellyfin down behind it)
// says Not connected.
func TestConnectedFollowsTheLastRequest(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	sub := func() string { row, _ := find(d.do("settings"), "Sync"); return row.Subtitle }
	if got := sub(); strings.HasPrefix(got, "Connected") || strings.HasPrefix(got, "Not connected") {
		t.Errorf("before any request: %q", got)
	}
	d.do("albums")
	if got := sub(); !strings.HasPrefix(got, "Connected") {
		t.Errorf("after a list: %q", got)
	}
	n.rawStatus.Store(502)
	d.do("album", "album:al0") // Albums again would come from memory (listFor), asking nothing
	if got := sub(); !strings.HasPrefix(got, "Not connected") {
		t.Errorf("after a 502: %q", got)
	}
	n.rawStatus.Store(0)
	d.do("album", "album:al0")
	if got := sub(); !strings.HasPrefix(got, "Connected") {
		t.Errorf("after the server came back: %q", got)
	}
	if p := n.pings.Load(); p != 0 {
		t.Errorf("%d pings sent for the status", p)
	}
}

// S14: an album the album pages missed (the list shifted during a scan on the server) keeps its
// songs in Entire Library's plan, rather than the sync deleting them from the card.
func TestEntireLibraryKeepsSongsOfAMissedAlbum(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, hideAlbum: "al1"}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	ids := map[string]bool{}
	for _, w := range d.daemon.preview.plan.Add {
		ids[w.Song.ID] = true
	}
	if len(ids) != 6 || !ids["al1.0"] || !ids["al1.1"] {
		t.Errorf("planned %v, want all 6 songs including al1's", ids)
	}
}

// countLyrics counts lyrics requests, answering each as lyricSrc does.
type countLyrics struct {
	urlSrc
	n *atomic.Int32
}

func (c countLyrics) Lyrics(_ context.Context, id string) (string, error) {
	c.n.Add(1)
	return "[00:01.00]" + id, nil
}

// B1: a second Play of the same songs fetches no lyrics again: they're kept by song between Plays.
func TestLyricsKeptBetweenPlays(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	var n atomic.Int32
	e.s.src = countLyrics{urlSrc: urlSrc{e.src}, n: &n}
	ss := songs(5, "flac")
	for i := range ss {
		ss[i].HasLyrics = true
	}
	dir := filepath.Join(filepath.Dir(e.play), "q2-jellyfin.lyrics")
	lines := func() int { m, _ := filepath.Glob(dir + "/[0-9]*.lrc"); return len(m) }
	e.s.start(ss, 0)
	eventually(t, "every line's lyrics", func() bool { return lines() == 5 })
	first := n.Load()
	e.s.start(ss, 2)
	eventually(t, "every line's lyrics again", func() bool { return lines() == 5 })
	if got := n.Load(); got != first {
		t.Errorf("lyrics fetched again: %d then %d", first, got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "0.lrc"))
	if string(b) != "[00:01.00]s0" {
		t.Errorf("line 0: %q", b)
	}
}

// B1: a song already on the card takes its line's lyrics from the .lrc beside it, with no request;
// the others come from the server.
func TestLyricsOfASyncedSongComeFromTheCard(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	var n atomic.Int32
	e.s.src = countLyrics{urlSrc: urlSrc{e.src}, n: &n}
	ss := songs(3, "flac")
	for i := range ss {
		ss[i].HasLyrics = true
	}
	song := filepath.Join(e.root, "Jellyfin", "A", "s0.flac")
	os.MkdirAll(filepath.Dir(song), 0o755)
	os.WriteFile(song, []byte("flac"), 0o644)
	os.WriteFile(library.LyricsPath(song), []byte("[00:02.00]from the card"), 0o644)
	e.man.Songs["s0"] = &library.Entry{ID: "s0", Path: song, Quality: "original", Size: 4}
	dir := filepath.Join(filepath.Dir(e.play), "q2-jellyfin.lyrics")
	e.s.start(ss, 0)
	eventually(t, "every line's lyrics", func() bool { m, _ := filepath.Glob(dir + "/[0-9]*.lrc"); return len(m) == 3 })
	if b, _ := os.ReadFile(filepath.Join(dir, "0.lrc")); string(b) != "[00:02.00]from the card" {
		t.Errorf("line 0: %q", b)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("%d lyrics requests, want 2 (not the song on the card)", got)
	}
}

// Going back to Newly Added, Albums or Artists within listFor asks the server nothing; a new sort
// fetches the list afresh, the same sort chosen again doesn't, and a kept list goes after listFor.
func TestListsKeptForComingBack(t *testing.T) {
	n := &nav{albums: 3, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	d.do("latest")
	d.do("albums")
	d.do("artists")
	albums, artists := n.albumLists.Load(), n.artistReqs.Load()
	d.do("artists")
	d.do("albums")
	d.do("latest")
	if n.albumLists.Load() != albums || n.artistReqs.Load() != artists {
		t.Fatalf("asked again: albums %d then %d, artists %d then %d", albums, n.albumLists.Load(), artists, n.artistReqs.Load())
	}
	d.do("setorder", "albums turned")
	d.do("albums")
	if n.albumLists.Load() != albums+1 || n.albumSort.Load() != "SortName Descending" {
		t.Fatalf("new order: %d lists, sort %v", n.albumLists.Load()-albums, n.albumSort.Load())
	}
	d.do("setorder", "albums turned") // the current order again
	d.do("albums")
	if n.albumLists.Load() != albums+1 {
		t.Fatal("the same order fetched again")
	}
	d.lists.mu.Lock()
	for k := range d.lists.at {
		d.lists.at[k] = d.lists.at[k].Add(-listFor)
	}
	d.lists.mu.Unlock()
	d.do("artists")
	if n.artistReqs.Load() != artists+1 {
		t.Fatal("kept past listFor")
	}
}

// "Last synced" is the last sync run to its end, not the manifest file's time, which previews and
// imports change too (audit 2026-11-10 #10).
func TestLastSyncedOnlySync(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	sub := func() string { row, _ := find(d.do("settings"), "Sync"); return row.Subtitle }
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil { // as a preview's save
		t.Fatal(err)
	}
	if got := sub(); strings.Contains(got, "Last synced") {
		t.Fatalf("with no sync: %q", got)
	}
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if got := sub(); !strings.Contains(got, "Last synced today") {
		t.Fatalf("after a sync: %q", got)
	}
}
