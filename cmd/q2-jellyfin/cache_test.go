package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// putCache puts a song of n bytes in the playback cache, as an earlier version left it.
func putCache(t *testing.T, d *tdaemon, name string, n int) string {
	t.Helper()
	p := filepath.Join(d.card, ".jellyfin", "cache", name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCacheClear(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "urls", true)
	putCache(t, d, "a-original.flac", 100)
	putCache(t, d, "b-original.flac", 50)
	if n := d.clearCache(); n != 150 || d.cacheSize() != 0 {
		t.Fatalf("cleared %d, left %d", n, d.cacheSize())
	}
}

// Clear Playback Cache asks first, as Q2 Pod's Clear All Downloads does, and still empties the
// songs an earlier version cached; nothing sets the cache's size any more.
func TestCacheSettings(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "urls", true)
	if r := d.do("cache"); r.Status != q2spot.Unsupported {
		t.Fatalf("Playback Cache's size: %+v", r)
	}
	if m := d.do("clearcache").Message; m != "The cache is empty" {
		t.Fatalf("%q", m)
	}
	p := putCache(t, d, "s-original.flac", 3e6)
	if m := d.do("clearcache").Message; m != "Press again to remove 3 MB of cached songs" || !exists(p) {
		t.Fatalf("first press %q", m)
	}
	d.do("storage") // Q2 Pod asks the page again after the toast: the first press stays
	if m := d.do("clearcache").Message; m != "Removed 3 MB of cached songs" || exists(p) {
		t.Fatalf("second press %q", m)
	}
	// Another press in between drops the first: nothing is removed without asking again.
	putCache(t, d, "s-original.flac", 3e6) // the clear pruned the empty folder
	d.do("clearcache")
	d.do("settings")
	if m := d.do("clearcache").Message; m != "Press again to remove 3 MB of cached songs" || !exists(p) {
		t.Fatalf("after another press %q", m)
	}
}

// Quality and Storage has no Playback Cache size; Clear Playback Cache shows only while the cache
// holds songs an earlier version left. Swipe on or off, the same.
func TestPlaybackCacheRows(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "urls", true)
	for _, swipe := range []string{"on", "off"} {
		rows := func() (size, clear bool) {
			r := d.do("storage")
			_, size = find(r, "Playback Cache: ")
			_, clear = find(r, "Clear Playback Cache")
			return size, clear
		}
		if size, clear := rows(); size || clear {
			t.Errorf("swipe %s, empty cache: size row %v, clear row %v", swipe, size, clear)
		}
		p := putCache(t, d, "s-original.flac", 1000)
		if size, clear := rows(); size || !clear {
			t.Errorf("swipe %s, songs cached: size row %v, clear row %v", swipe, size, clear)
		}
		os.Remove(p)
		d.do("swipe")
	}
}

// A sync no longer takes a song from the playback cache: it downloads the server's file, and the
// cached copy stays for Clear Playback Cache.
func TestSyncLeavesCache(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "urls", true)
	p := putCache(t, d, "al0.0-original.flac", 1000) // the server's 1000 bytes, as 0.8.0 took them
	os.WriteFile(p, []byte(strings.Repeat("c", 1000)), 0o644)
	d.do("toggle", "album:al0")
	if _, ok := find(d.preview(t), "Start Sync"); !ok {
		t.Fatalf("no Start Sync: %q", titles(d.preview(t)))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	a := d.manifest().Songs["al0.0"]
	if a == nil {
		t.Fatal("not synced")
	}
	if got, _ := os.ReadFile(a.Path); string(got) != strings.Repeat("x", 1000) {
		t.Fatalf("al0.0 is %q: the cached copy was used", got[:min(len(got), 8)])
	}
	if !exists(p) {
		t.Fatal("the sync took the cached copy")
	}
}
