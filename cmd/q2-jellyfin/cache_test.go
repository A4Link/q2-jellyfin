package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
)

func (e *env) cached(id, q, ext string) string {
	return filepath.Join(e.root, ".jellyfin", "cache", id+"-"+q+"."+ext)
}

// Songs played past the window go to the cache, and so does the rest of the list when another
// starts; a song played again at the same quality comes from the cache, with no request.
func TestCacheKeepsAndReplays(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.cacheLimit = func() int64 { return 1 << 30 }
	e.s.start(songs(8, "flac"), 0)
	until(t, "0-2", func() bool { return exists(e.stream(2, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(5, "flac")), 0o644)
	until(t, "0-2 retired", func() bool {
		return !exists(e.stream(0, "flac")) && exists(e.cached("s0", "original", "flac")) && exists(e.cached("s2", "original", "flac"))
	})
	before := e.src.tries("s0")
	e.s.start(songs(3, "flac"), 0) // s0 again
	until(t, "s0 back", func() bool { return exists(e.stream(0, "flac")) })
	if n := e.src.tries("s0"); n != before {
		t.Fatalf("s0 fetched again: %d requests, was %d", n, before)
	}
	if exists(e.cached("s0", "original", "flac")) {
		t.Fatal("s0 both in the cache and playing")
	}
	if b, _ := os.ReadFile(e.stream(0, "flac")); string(b) != "data-p:s0:original" {
		t.Fatalf("cached copy %q", b)
	}
}

// A different quality is a different file: the original in the cache doesn't stand in for MP3 320.
func TestCacheKeyedByQuality(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.cacheLimit = func() int64 { return 1 << 30 }
	os.MkdirAll(filepath.Dir(e.cached("s0", "original", "flac")), 0o755)
	os.WriteFile(e.cached("s0", "original", "flac"), []byte("orig"), 0o644)
	e.s.quality = func() string { return "mp3-320" }
	e.s.start(songs(1, "flac"), 0)
	until(t, "s0", func() bool { return exists(e.stream(0, "mp3")) })
	if e.src.tries("s0") != 1 || !exists(e.cached("s0", "original", "flac")) {
		t.Fatalf("tries %d", e.src.tries("s0"))
	}
}

// Off (the default for a streamer without a cap), played songs are deleted as before.
func TestCacheOffDeletes(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.start(songs(8, "flac"), 0)
	until(t, "0-2", func() bool { return exists(e.stream(2, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(5, "flac")), 0o644)
	until(t, "trim", func() bool { return !exists(e.stream(0, "flac")) })
	if _, err := os.Stat(filepath.Join(e.root, ".jellyfin", "cache")); !os.IsNotExist(err) {
		t.Fatal("cache folder made while off")
	}
}

// putCache writes a cached song of n bytes, played at.
func putCache(t *testing.T, e *env, id string, n int, at time.Time) string {
	t.Helper()
	p := e.cached(id, "original", "flac")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(strings.Repeat("x", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, at, at)
	return p
}

// Over its cap, the cache loses the least recently played first.
func TestCacheTrimsOldest(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	now := time.Now()
	old := putCache(t, e, "old", 100, now.Add(-3*time.Hour))
	mid := putCache(t, e, "mid", 100, now.Add(-2*time.Hour))
	recent := putCache(t, e, "new", 100, now.Add(-time.Hour))
	e.s.trim(250)
	if exists(old) || !exists(mid) || !exists(recent) || e.s.cacheSize() != 200 {
		t.Fatalf("old %v mid %v new %v size %d", exists(old), exists(mid), exists(recent), e.s.cacheSize())
	}
}

// A sync comes first: the cache gives up its oldest songs until the card has the room asked for.
func TestCacheEvictForSync(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	now := time.Now()
	a := putCache(t, e, "a", 100, now.Add(-2*time.Hour))
	b := putCache(t, e, "b", 100, now.Add(-time.Hour))
	free := func() int64 { return 300 - e.s.cacheSize() } // a 300-byte card
	e.s.evictFor(200, free, nil)
	if exists(a) || !exists(b) || free() != 200 {
		t.Fatalf("a %v b %v free %d", exists(a), exists(b), free())
	}
}

func TestCacheClear(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	putCache(t, e, "a", 100, time.Now())
	putCache(t, e, "b", 50, time.Now())
	if n := e.s.clearCache(); n != 150 || e.s.cacheSize() != 0 {
		t.Fatalf("cleared %d, left %d", n, e.s.cacheSize())
	}
}

// Quality and Storage: Playback Cache steps Off, 512 MB … 8 GB (2 GB by default), and Clear Playback Cache
// asks first, as Q2 Pod's Clear All Downloads does.
func TestCacheSettings(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	r := d.do("storage")
	if _, ok := find(r, "Playback Cache: 2 GB"); !ok {
		t.Fatalf("%q", titles(r))
	}
	for _, want := range []string{"4 GB", "8 GB", "Off", "512 MB", "1 GB", "2 GB"} {
		if m := d.do("cache").Message; m != "Playback Cache: "+want {
			t.Fatalf("got %q, want %s", m, want)
		}
	}
	if m := d.do("clearcache").Message; m != "The cache is empty" {
		t.Fatalf("%q", m)
	}
	p := filepath.Join(d.card, ".jellyfin", "cache", "s-original.flac")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, make([]byte, 3e6), 0o644)
	if m := d.do("clearcache").Message; m != "Press again to remove 3 MB of cached songs" || !exists(p) {
		t.Fatalf("first press %q", m)
	}
	if m := d.do("clearcache").Message; m != "Removed 3 MB of cached songs" || exists(p) {
		t.Fatalf("second press %q", m)
	}
	// Another press in between drops the first: nothing is removed without asking again.
	os.MkdirAll(filepath.Dir(p), 0o755) // the clear pruned the empty folder
	os.WriteFile(p, make([]byte, 3e6), 0o644)
	d.do("clearcache")
	d.do("settings")
	if m := d.do("clearcache").Message; m != "Press again to remove 3 MB of cached songs" || !exists(p) {
		t.Fatalf("after another press %q", m)
	}
}

// The preview counts the cache as room, and Start Sync frees what the sync needs from it.
func TestCacheGivesWayToSync(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	p := filepath.Join(d.card, ".jellyfin", "cache", "s-original.flac")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, 20<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	d.free = func(string) int64 { return 512e6 + 21<<20 - d.stream.cacheSize() } // at Space to Keep Free but for the cache
	d.do("toggle", "album:al0")
	if _, ok := find(d.preview(t), "Start Sync"); !ok {
		t.Fatalf("no Start Sync: %q", titles(d.preview(t)))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if exists(p) {
		t.Fatal("the cache kept its song though the sync needed the room")
	}
}

// Stepping Playback Cache through Off to another size never empties the cache: the size applies
// only once the presses stop, as it then stands.
func TestCacheSettingPassesOff(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	p := filepath.Join(d.card, ".jellyfin", "cache", "s-original.flac")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, make([]byte, 1000), 0o644)
	for i := 0; i < 3; i++ { // 2 GB -> 4 GB -> 8 GB -> Off
		d.do("cache")
	}
	d.do("cache") // -> 512 MB
	time.Sleep(trimDelay + time.Second)
	if !exists(p) {
		t.Fatal("passing Off emptied the cache")
	}
}

// U32: a sync takes a song already in the playback cache at Sync Quality, whole, by a rename with
// its lyrics, instead of downloading it again. An original cached at another size than the
// server's (replaced on the server since) is downloaded.
func TestSyncTakesFromCache(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	dir := filepath.Join(d.card, ".jellyfin", "cache")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "al0.0-original.flac"), []byte(strings.Repeat("c", 1000)), 0o644) // the server's 1000 bytes
	os.WriteFile(filepath.Join(dir, "al0.0-original.lrc"), []byte("[00:01.00]cached"), 0o644)
	os.WriteFile(filepath.Join(dir, "al0.1-original.flac"), []byte(strings.Repeat("c", 999)), 0o644) // an older file
	d.do("toggle", "album:al0")
	if _, ok := find(d.preview(t), "Start Sync"); !ok {
		t.Fatalf("no Start Sync: %q", titles(d.preview(t)))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	m := d.manifest()
	a, b := m.Songs["al0.0"], m.Songs["al0.1"]
	if a == nil || b == nil {
		t.Fatalf("manifest %+v", m.Songs)
	}
	if got, _ := os.ReadFile(a.Path); string(got) != strings.Repeat("c", 1000) || a.Size != 1000 {
		t.Fatalf("al0.0 is %q (%d bytes recorded): not the cached copy", got[:min(len(got), 8)], a.Size)
	}
	if got, _ := os.ReadFile(library.LyricsPath(a.Path)); string(got) != "[00:01.00]cached" {
		t.Fatalf("lyrics %q", got)
	}
	if got, _ := os.ReadFile(b.Path); string(got) != strings.Repeat("x", 1000) {
		t.Fatalf("al0.1 is %q: the cached copy of another size was used", got[:min(len(got), 8)])
	}
	if exists(filepath.Join(dir, "al0.0-original.flac")) || !exists(filepath.Join(dir, "al0.1-original.flac")) {
		t.Fatal("the cache should have given up al0.0 only")
	}
}

// A trim (a song played, or a smaller Playback Cache) keeps the cached songs the sync under way is
// to take, until it ends.
func TestCacheTrimKeepsSyncs(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	now := time.Now()
	want := putCache(t, e, "a", 100, now.Add(-2*time.Hour))
	other := putCache(t, e, "b", 100, now.Add(-time.Hour))
	running := true
	e.s.syncing = func() bool { return running }
	e.s.keepForSync(map[string]bool{want: true})
	e.s.trim(100)
	if !exists(want) || exists(other) {
		t.Fatalf("a %v b %v", exists(want), exists(other))
	}
	running = false // the sync ended, whatever way
	e.s.trim(0)
	if exists(want) {
		t.Fatal("kept after the sync")
	}
}

// Start Sync short of room keeps in the cache the songs the sync takes from it (they need no
// room), giving up the others first.
func TestCacheKeepsWhatSyncTakes(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	now := time.Now()
	want := putCache(t, e, "a", 100, now.Add(-2*time.Hour)) // the oldest, but the sync's
	other := putCache(t, e, "b", 100, now.Add(-time.Hour))
	w := plan.Want{Song: jellyfin.Song{ID: "a", Size: 100}, Quality: "original", Path: filepath.Join(e.root, "Jellyfin", "x", "a.flac"), Bytes: 100}
	keep, n := e.s.cachedFor([]plan.Want{w})
	if !keep[want] || n != 100 {
		t.Fatalf("keep %v, %d bytes", keep, n)
	}
	free := func() int64 { return 300 - e.s.cacheSize() }
	e.s.evictFor(250, free, keep)
	if !exists(want) || exists(other) {
		t.Fatalf("a %v b %v", exists(want), exists(other))
	}
}

// A cancelled sync no longer holds its cached songs: a trim may delete them again.
func TestCacheTrimAfterCancel(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	c := filepath.Join(d.card, ".jellyfin", "cache", "al0.1-original.flac")
	os.MkdirAll(filepath.Dir(c), 0o755)
	os.WriteFile(c, []byte(strings.Repeat("c", 1000)), 0o644)
	d.do("toggle", "album:al0")
	if _, ok := find(d.preview(t), "Start Sync"); !ok {
		t.Fatalf("no Start Sync: %q", titles(d.preview(t)))
	}
	d.do("start") // al0.0 downloads first, and waits
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "running" })
	d.stream.trim(0)
	if !exists(c) {
		t.Fatal("trimmed while the sync is to take it")
	}
	d.runner.Cancel()
	d.stream.trim(0)
	if exists(c) {
		t.Fatal("kept after the sync was cancelled")
	}
	close(n.block)
}
