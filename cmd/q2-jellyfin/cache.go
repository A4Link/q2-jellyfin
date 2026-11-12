package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// The playback cache, as Q2 Pod's Tidal cache: a song streamed whole is kept on the card in
// .jellyfin/cache instead of being deleted once played, and a later stream of the same song at the
// same quality plays from it without the network. Its size is capped (Settings → Quality and
// Storage → Playback Cache, Off or 512 MB to 8 GB, 2 GB by default) and the least recently played
// go first. It is separate from Sync: synced songs are the library's and never evicted; cached
// ones are temporary, live in a hidden folder the Q2's library doesn't list, and give way to a
// sync that needs their room.
//
// Moving a song in or out is a rename on the card, so it costs no copy.

// cacheSizes are the Playback Cache setting's steps; "" is the default.
var cacheSizes = []string{"off", "512M", "1G", "2G", "4G", "8G"}

const defaultCache = "2G"

func cacheBytes(v string) int64 {
	switch v {
	case "off":
		return 0
	case "512M":
		return 512 << 20
	case "1G":
		return 1 << 30
	case "4G":
		return 4 << 30
	case "8G":
		return 8 << 30
	}
	return 2 << 30
}

func cacheName(v string) string {
	switch v {
	case "off":
		return "Off"
	case "512M":
		return "512 MB"
	case "":
		v = defaultCache
	}
	return strings.TrimSuffix(v, "G") + " GB"
}

func nextCache(v string) string {
	if v == "" {
		v = defaultCache
	}
	for i, s := range cacheSizes {
		if s == v {
			return cacheSizes[(i+1)%len(cacheSizes)]
		}
	}
	return defaultCache
}

// cacheKey names a song at a quality: what a cached file stands for.
type cacheKey struct{ id, quality, ext string }

func (s *streamer) cacheDir() string { return filepath.Join(s.root, ".jellyfin", "cache") }

func (s *streamer) cachePath(k cacheKey) string {
	return filepath.Join(s.cacheDir(), library.SafeName(k.id)+"-"+k.quality+"."+k.ext)
}

func (s *streamer) cacheCap() int64 {
	if s.cacheLimit == nil {
		return 0
	}
	return s.cacheLimit()
}

// fromCache moves the cached copy of k to path, reporting whether there was one.
func (s *streamer) fromCache(k cacheKey, path string) bool {
	if s.cacheCap() <= 0 || !s.take(s.cachePath(k), path) {
		return false
	}
	s.logf("stream: %s from the cache", k.id)
	return true
}

// take moves the cached file c, and its lyrics, to path, under cacheMu: no trim deletes it half
// moved, and of a stream and a sync after the same song only one gets it.
func (s *streamer) take(c, path string) bool {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !exists(c) || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return false
	}
	os.Rename(library.LyricsPath(c), library.LyricsPath(path)) // its lyrics first, when it has any
	return os.Rename(c, path) == nil
}

// forSync is the syncer's FromCache: a sync takes a song played before from the cache by a rename,
// instead of downloading it again, when it is the same file the sync would fetch. A cached file is
// whole (only a song fetched whole is kept: retire) and is the server's answer to the same request
// at the same quality: a transcode comes from the same URL as a sync's, tags and all, and an
// original is the server's file either way (/Audio/{id}/stream?static=true and /Items/{id}/Download
// send the same bytes). An original must still be the server's size, so a file replaced on the
// server since (a re-rip) is downloaded again. The cache's size setting doesn't matter: a cached
// song is used whether or not the cache is still on.
func (s *streamer) forSync(w plan.Want) (int64, bool) {
	c, n := s.syncCopy(w)
	if n == 0 || !s.take(c, w.Path) {
		return 0, false
	}
	return n, true
}

// syncCopy is where w's song would be in the cache, and its size when it is there as the sync wants
// it (forSync); 0 when it isn't.
func (s *streamer) syncCopy(w plan.Want) (string, int64) {
	c := s.cachePath(cacheKey{w.Song.ID, w.Quality, strings.TrimPrefix(filepath.Ext(w.Path), ".")})
	fi, err := os.Stat(c)
	if err != nil || fi.Size() == 0 || quality.IsOriginal(w.Quality) && fi.Size() != w.Song.Size {
		return c, 0
	}
	return c, fi.Size()
}

// fetched records that path holds k whole, so retire can keep it.
func (s *streamer) fetched(path string, k cacheKey) {
	s.keptMu.Lock()
	defer s.keptMu.Unlock()
	if s.kept == nil {
		s.kept = map[string]cacheKey{}
	}
	s.kept[path] = k
}

// retire is a played song leaving the stream folder: kept in the cache when it was fetched whole
// and the cache is on, else deleted.
func (s *streamer) retire(path string) {
	s.keptMu.Lock()
	k, ok := s.kept[path]
	delete(s.kept, path)
	s.keptMu.Unlock()
	if ok && s.cacheCap() > 0 && exists(path) && os.MkdirAll(s.cacheDir(), 0o755) == nil {
		c := s.cachePath(k)
		if os.Rename(path, c) == nil {
			os.Rename(library.LyricsPath(path), library.LyricsPath(c)) // its lyrics stay with it
			now := time.Now()
			os.Chtimes(c, now, now) // the most recently played goes last
			s.trim(s.cacheCap())
			return
		}
	}
	library.Remove(s.root, path)
}

type cached struct {
	path string
	size int64
	at   time.Time
}

// cacheFiles lists the cache, oldest first; part files and anything else are left out.
func (s *streamer) cacheFiles() []cached {
	ents, _ := os.ReadDir(s.cacheDir())
	var out []cached
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") || strings.HasSuffix(e.Name(), ".tmp") ||
			strings.HasSuffix(e.Name(), ".lrc") { // a song's lyrics go with it (library.Remove)
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, cached{filepath.Join(s.cacheDir(), e.Name()), fi.Size(), fi.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// cacheSize is the bytes the cache holds.
func (s *streamer) cacheSize() int64 {
	var n int64
	for _, c := range s.cacheFiles() {
		n += c.size
	}
	return n
}

// trim deletes the least recently played songs until the cache holds at most limit bytes.
func (s *streamer) trim(limit int64) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.syncing == nil || !s.syncing() {
		s.syncKeep = nil // the sync ended, however: done, stopped, cancelled or failed
	}
	files := s.cacheFiles()
	var total int64
	for _, c := range files {
		total += c.size
	}
	for _, c := range files {
		if total <= limit {
			return
		}
		if s.syncKeep[c.path] {
			continue // the sync under way takes it (cachedFor)
		}
		if library.Remove(s.root, c.path) == nil {
			total -= c.size
		}
	}
}

// evictFor frees cache until free() reports at least need bytes, or the cache is empty: a sync
// comes before the cache. Songs in keep stay: the sync takes them (cachedFor).
func (s *streamer) evictFor(need int64, free func() int64, keep map[string]bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for _, c := range s.cacheFiles() {
		if free() >= need {
			return
		}
		if keep[c.path] {
			continue
		}
		library.Remove(s.root, c.path)
	}
}

// keepForSync makes trim keep the cached files a sync is to take, while it runs or is paused
// (syncing).
func (s *streamer) keepForSync(keep map[string]bool) {
	s.cacheMu.Lock()
	s.syncKeep = keep
	s.cacheMu.Unlock()
}

// cachedFor is the cached files a sync of ws will take (forSync), and the bytes the plan counted
// for those songs: a rename on the card, they need no room.
func (s *streamer) cachedFor(ws []plan.Want) (map[string]bool, int64) {
	keep, n := map[string]bool{}, int64(0)
	for _, w := range ws {
		if c, size := s.syncCopy(w); size > 0 {
			keep[c] = true
			n += w.Bytes
		}
	}
	return keep, n
}

// clearCache empties the cache and reports the bytes it held.
func (s *streamer) clearCache() int64 {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	var n int64
	for _, c := range s.cacheFiles() {
		if library.Remove(s.root, c.path) == nil {
			n += c.size
		}
	}
	return n
}

// trimDelay is how long after the last press of Playback Cache its new size applies: the setting
// steps through Off and the smaller sizes on the way to the one wanted, and passing them must not
// empty the cache.
const trimDelay = 5 * time.Second

// trimSoon trims the cache to the setting as it stands trimDelay after the last call.
func (d *daemon) trimSoon() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.trimTimer != nil {
		d.trimTimer.Stop()
	}
	d.trimTimer = time.AfterFunc(trimDelay, func() { d.stream.trim(cacheBytes(d.settings().PlaybackCache)) })
}
