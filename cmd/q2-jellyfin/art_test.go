package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
)

// With "art", a list's rows get their covers before the reply, named by the row's URI and context
// as the firmware looks for them; an item without one isn't asked again; no "art", no covers.
func TestListCovers(t *testing.T) {
	n := &nav{albums: 3, songsPer: 1}
	d := newTestDaemon(t, n, "art", true)
	r := d.do("albums")
	var row = r.Rows[2] // after the search and sort rows
	if row.Context != "album:al0" {
		t.Fatalf("row %+v", row)
	}
	p := filepath.Join(d.tmp, "q2-jellyfin.art", artKey(row.URI, row.Context)+".jpg")
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "/Items/al0/Images/Primary 64") {
		t.Fatalf("cover %q %v", b, err)
	}
	got := n.images.Load()
	d.do("albums")
	if n.images.Load() != got {
		t.Fatal("covers fetched again")
	}
	d.do("artists") // the artist has no cover: asked once
	missed := n.images.Load()
	d.do("artists")
	if n.images.Load() != missed {
		t.Fatal("a missing cover asked again")
	}
}

// The key is FNV-1a 32 of URI, tab, context, from FNV_SEED: as patch/addon.c art_file hashes them
// (the value worked out separately, byte by byte).
func TestArtKey(t *testing.T) {
	if got := artKey("album", "album:al0"); got != "a20cd32b" {
		t.Fatalf("%q", got)
	}
}

// artFiles makes n covers of size bytes in d's art dir, cover 0 the oldest; their names.
func artFiles(t *testing.T, d *tdaemon, n, size int) []string {
	t.Helper()
	os.MkdirAll(d.artDir(), 0o755)
	old := time.Now().Add(-time.Hour)
	var names []string
	for i := range n {
		p := filepath.Join(d.artDir(), fmt.Sprintf("%08x.jpg", i))
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old.Add(time.Duration(i)*time.Second))
		names = append(names, p)
	}
	return names
}

// keptArt fails unless the oldest dropped of names are gone and the rest still there.
func keptArt(t *testing.T, names []string, dropped int) {
	t.Helper()
	for i, p := range names {
		if exists(p) == (i < dropped) {
			t.Fatalf("cover %d of %d: there %v, want the oldest %d dropped", i, len(names), exists(p), dropped)
		}
	}
}

// Past artKeep covers, the oldest go, artPrune at a time: 1501 leave 1401.
func TestArtPrunedByCount(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "art", true)
	names := artFiles(t, d, artKeep+1, 10)
	d.pruneArt()
	keptArt(t, names, artPrune)
}

// Covers adding up to artBytes or more: the oldest go, artPrune at a time, until under it.
func TestArtPrunedBySize(t *testing.T) {
	for _, c := range []struct{ n, dropped int }{
		{256, 100}, // exactly 8 MB: over, 156 left
		{400, 200}, // 12.5 MB: 9.4 MB still over after one round, 6.25 MB after two
	} {
		d := newTestDaemon(t, &nav{}, "art", true)
		names := artFiles(t, d, c.n, artBytes/256)
		d.pruneArt()
		keptArt(t, names, c.dropped)
	}
}

// A cover linked to several rows counts once toward artBytes: 40 covers of 150 KB (6 MB), each
// shown by 4 rows (24 MB by their names), are all kept.
func TestArtLinksCountOnce(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "art", true)
	names := artFiles(t, d, 40, 150<<10)
	for i, p := range names {
		for j := range 3 {
			if err := os.Link(p, filepath.Join(d.artDir(), fmt.Sprintf("row-%d-%d.jpg", i, j))); err != nil {
				t.Fatal(err)
			}
		}
	}
	d.pruneArt()
	keptArt(t, names, 0)
}

// A cover fetch cut short by an app stop leaves its *.tmp: a prune drops one over a minute old (a
// live fetch lasts at most artBudget), not a fresh one.
func TestArtPruneDropsOldTmp(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "art", true)
	os.MkdirAll(d.artDir(), 0o755)
	old, fresh := filepath.Join(d.artDir(), "111.tmp"), filepath.Join(d.artDir(), "222.tmp")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, make([]byte, 10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-2 * time.Minute)
	os.Chtimes(old, at, at)
	d.pruneArt()
	if exists(old) || !exists(fresh) {
		t.Fatalf("old .tmp there %v, fresh .tmp there %v; want gone, kept", exists(old), exists(fresh))
	}
}

// A single reply that fetched a cover prunes, not one in a hundred.
func TestArtPrunedAfterOneReply(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "art", true)
	names := artFiles(t, d, artKeep+1, 10)
	d.do("albums")
	if exists(names[0]) {
		t.Fatal("not pruned after a reply that fetched")
	}
}

// A cover over artMax (a server sending the whole image) isn't kept cut off, nor asked again.
func TestBigCoverNotKept(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1, coverSize: artMax + 1}
	d := newTestDaemon(t, n, "art", true)
	d.do("albums")
	if jpgs, _ := filepath.Glob(filepath.Join(d.artDir(), "*")); len(jpgs) != 0 {
		t.Fatalf("kept %q", jpgs)
	}
	asked := n.images.Load()
	d.do("albums")
	if n.images.Load() != asked {
		t.Fatal("a cover too big asked again")
	}

	n = &nav{albums: 1, songsPer: 1, coverSize: artMax}
	d = newTestDaemon(t, n, "art", true)
	d.do("albums")
	if b, err := os.ReadFile(d.artFile("al0")); err != nil || len(b) != artMax {
		t.Fatalf("a cover of artMax bytes: %d %v", len(b), err)
	}
}

// Two fetches of one cover at once (a page asked again while its covers come) each write their own
// file aside: the cover renamed into place is whole, never one with the other's truncation in it.
func TestArtFetchedTwiceAtOnce(t *testing.T) {
	cover := bytes.Repeat([]byte("j"), 2000)
	var reqs atomic.Int32
	half, rest := make(chan struct{}), make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		if reqs.Add(1) == 1 { // the first: half, then the rest once the second has begun
			w.Write(cover[:1000])
			w.(http.Flusher).Flush()
			close(half)
			<-rest
			w.Write(cover[1000:])
			return
		}
		w.(http.Flusher).Flush() // the second: begun, never finished
		<-r.Context().Done()
	}))
	defer srv.Close()
	cl, err := jellyfin.New(jellyfin.Creds{Server: srv.URL, User: "u", Token: "tok", UserID: "u1"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "art", true)
	os.MkdirAll(d.artDir(), 0o755)
	path := d.artFile("al0")
	tmpSizes := func() (sizes []int64) {
		m, _ := filepath.Glob(filepath.Join(d.artDir(), "*.tmp"))
		for _, f := range m {
			if fi, err := os.Stat(f); err == nil {
				sizes = append(sizes, fi.Size())
			}
		}
		return sizes
	}
	first := make(chan bool)
	go func() { first <- d.fetchArt(context.Background(), cl, "al0", path) }()
	<-half
	eventually(t, "the first half on the card", func() bool { s := tmpSizes(); return len(s) == 1 && s[0] == 1000 })
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan bool)
	go func() { second <- d.fetchArt(ctx, cl, "al0", path) }()
	eventually(t, "the second fetch's file", func() bool { return slices.Contains(tmpSizes(), 0) })
	close(rest)
	if !<-first {
		t.Fatal("the first fetch failed")
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, cover) {
		t.Errorf("cover of %d bytes, %d of them zero", len(b), bytes.Count(b, []byte{0}))
	}
	cancel()
	<-second
	if s := tmpSizes(); len(s) != 0 {
		t.Errorf("left aside: %v", s)
	}
}
