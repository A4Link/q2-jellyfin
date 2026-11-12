package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	plain := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	plain.do("albums")
	if _, err := os.Stat(filepath.Join(plain.tmp, "q2-jellyfin.art")); err == nil {
		t.Fatal("covers without the firmware's art")
	}
}

// The key is FNV-1a 32 of URI, tab, context, from FNV_SEED: as patch/addon.c art_file hashes them
// (the value worked out separately, byte by byte).
func TestArtKey(t *testing.T) {
	if got := artKey("album", "album:al0"); got != "a20cd32b" {
		t.Fatalf("%q", got)
	}
}
