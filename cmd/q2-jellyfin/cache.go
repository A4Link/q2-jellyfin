package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/library"
)

// The playback cache, .jellyfin/cache: songs that earlier versions of the app kept there once
// played on Q2 Pod before 1.7.3. Nothing adds to it now and a sync doesn't use it; Quality and
// Storage's Clear Playback Cache, shown while it holds songs, removes them.

func (d *daemon) cacheDir() string { return filepath.Join(d.card, ".jellyfin", "cache") }

type cached struct {
	path string
	size int64
}

// cacheFiles lists the cache's songs; part files and anything else are left out.
func (d *daemon) cacheFiles() []cached {
	ents, _ := os.ReadDir(d.cacheDir())
	var out []cached
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") || strings.HasSuffix(e.Name(), ".tmp") ||
			strings.HasSuffix(e.Name(), ".lrc") { // a song's lyrics go with it (library.Remove)
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, cached{filepath.Join(d.cacheDir(), e.Name()), fi.Size()})
		}
	}
	return out
}

// cacheSize is the bytes the cache holds.
func (d *daemon) cacheSize() int64 {
	var n int64
	for _, c := range d.cacheFiles() {
		n += c.size
	}
	return n
}

// clearCache empties the cache and reports the bytes it held.
func (d *daemon) clearCache() int64 {
	var n int64
	for _, c := range d.cacheFiles() {
		if library.Remove(d.card, c.path) == nil {
			n += c.size
		}
	}
	return n
}
