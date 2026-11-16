package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Covers in lists, for Q2 Pod's "art" (Q2 Pod 1.7.3, patch/addon.c art_rows): a row's cover is a
// JPEG at <tmp>/q2-jellyfin.art/<key>.jpg, key the row's URI, a tab and its context hashed FNV-1a
// 32 as 8 hex digits. With one there, the firmware draws the list as Tidal's: 78 px rows, the cover
// at the left. The covers are fetched before the reply (a few at a time, within artBudget), as the
// firmware reads them when it draws the list and never again: a cover that lands later shows the
// next time the list is drawn.

const (
	artPX      = 64              // patch/addon.c ART_PX
	artBudget  = 4 * time.Second // the most a reply waits for its covers
	artWorkers = 6
	artKeep    = 1500             // covers kept in /tmp (RAM), the oldest dropped past it
	artBytes   = 8 << 20          // nor 8 MB of them: a server that can't resize sends whole images
	artMax     = 1 << 20          // a cover bigger than this isn't kept (audit 13)
	artPrune   = 100              // covers dropped at a time, the oldest first
	artMissFor = 10 * time.Minute // an item without a cover isn't asked again for this long
	artFailFor = time.Minute      // nor one whose cover failed otherwise (a timeout, a 5xx; audit B11)
)

// artOps are the lists drawn with covers. An album's own songs aren't: they'd all be the same one.
var artOps = map[string]bool{"latest": true, "albums": true, "artists": true, "playlists": true,
	"artist": true, "playlist": true, "favorites": true, "search": true}

type artState struct {
	mu    sync.Mutex
	miss  map[string]time.Time // cover id -> until when it isn't asked again (none, or it failed)
	songs sync.Map             // song id -> its cover's item id (view.song records them)
}

func artKey(uri, context string) string {
	h := fnv.New32a()
	h.Write([]byte(uri + "\t" + context))
	return fmt.Sprintf("%08x", h.Sum32())
}

func (d *daemon) artDir() string { return filepath.Join(d.tmp, "q2-jellyfin.art") }

// coverOf is the item whose image is row's cover; "" for none.
func (d *daemon) coverOf(row q2spot.Row) string {
	kind, id := parseKey(row.Context)
	switch {
	case id == "":
		return ""
	case row.Kind == "list" && (kind == "album" || kind == "artist" || kind == "playlist"):
		return id
	case kind == "song":
		if c, ok := d.art.songs.Load(id); ok {
			return c.(string)
		}
	}
	return ""
}

// withArt fetches the covers r's rows lack, within until (and artBudget), and returns r unchanged.
// Each cover is fetched once per item, to id-<item>.jpg, then linked to every row's file that shows
// it (/tmp is RAM): a playlist's 12 songs of one album cost one download, and the same song in
// another list or place costs none (audit B2).
func (d *daemon) withArt(cl *jellyfin.Client, r q2spot.Reply, until time.Time) q2spot.Reply {
	if !d.caps.Art || cl == nil {
		return r
	}
	dir := d.artDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r
	}
	want := map[string][]string{} // cover id -> the row files still to make
	var ids []string              // in the rows' order
	now := time.Now()
	d.art.mu.Lock()
	for _, row := range r.Rows {
		id := d.coverOf(row)
		if id == "" {
			continue
		}
		if until, ok := d.art.miss[id]; ok && now.Before(until) {
			continue
		}
		p := filepath.Join(dir, artKey(row.URI, row.Context)+".jpg")
		if exists(p) {
			continue
		}
		if linkArt(d.artFile(id), p) { // fetched for another row already
			continue
		}
		if want[id] == nil {
			ids = append(ids, id)
		}
		want[id] = append(want[id], p)
	}
	d.art.mu.Unlock()
	if len(ids) == 0 {
		return r
	}
	deadline := now.Add(artBudget)
	if until.Before(deadline) {
		deadline = until
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ch := make(chan string)
	var wg sync.WaitGroup
	for range min(artWorkers, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				if d.fetchArt(ctx, cl, id, d.artFile(id)) {
					for _, p := range want[id] {
						linkArt(d.artFile(id), p)
					}
				}
			}
		}()
	}
	for _, id := range ids {
		select {
		case ch <- id:
		case <-ctx.Done():
		}
	}
	close(ch)
	wg.Wait()
	d.pruneArt()
	return r
}

// artFile is where item id's cover is kept once fetched.
func (d *daemon) artFile(id string) string {
	return filepath.Join(d.artDir(), "id-"+artKey("id", id)+".jpg")
}

// linkArt makes p another name of the cover at from; whether p is there now.
func linkArt(from, p string) bool {
	if !exists(from) {
		return false
	}
	if os.Link(from, p) == nil || exists(p) {
		return true
	}
	b, err := os.ReadFile(from) // no hard links here: a copy
	return err == nil && os.WriteFile(p, b, 0o644) == nil
}

// fetchArt saves id's cover to path, aside and renamed into place, so the firmware never reads half
// of one; whether it did. A 404 or a cover over artMax isn't asked again for artMissFor, another
// failure for artFailFor; a fetch the app gave up on (its budget ran out) is asked again next time.
func (d *daemon) fetchArt(ctx context.Context, cl *jellyfin.Client, id, path string) bool {
	if ctx.Err() != nil {
		return false
	}
	resp, err := cl.Get(ctx, cl.ThumbURL(id, artPX), 0)
	if err != nil {
		if ctx.Err() == nil {
			wait := artFailFor
			var ae *jellyfin.APIError
			if errors.As(err, &ae) && ae.HTTP == 404 {
				wait = artMissFor
			}
			d.missArt(id, wait)
		}
		return false
	}
	defer resp.Body.Close()
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, artMax+1)) // one byte more: a cut cover shows
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > artMax {
		d.missArt(id, artMissFor) // the whole image, not a thumbnail: as for none
		err = errors.New("cover too big")
	}
	if err != nil || os.Rename(tmp, path) != nil {
		os.Remove(tmp)
		return false
	}
	return true
}

// missArt keeps id's cover from being asked again for wait.
func (d *daemon) missArt(id string, wait time.Duration) {
	d.art.mu.Lock()
	if d.art.miss == nil {
		d.art.miss = map[string]time.Time{}
	}
	d.art.miss[id] = time.Now().Add(wait)
	d.art.mu.Unlock()
}

// pruneArt drops the oldest covers, artPrune at a time, until there are artKeep or fewer and they
// add up to less than artBytes. Each row's file counts toward artKeep, links too; toward artBytes a
// cover counts once, however many rows link it, as it takes its room once.
func (d *daemon) pruneArt() {
	ents, err := os.ReadDir(d.artDir())
	if err != nil {
		return
	}
	type f struct {
		name string
		at   time.Time
		size int64
		ino  uint64
	}
	var fs []f
	var total int64
	links := map[uint64]int{} // a cover's names still here, by inode
	for _, e := range ents {
		if info, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".jpg") {
			ino := info.Sys().(*syscall.Stat_t).Ino
			fs = append(fs, f{e.Name(), info.ModTime(), info.Size(), ino})
			if links[ino] == 0 {
				total += info.Size()
			}
			links[ino]++
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].at.Before(fs[j].at) })
	for len(fs) > artKeep || total >= artBytes {
		for _, x := range fs[:min(artPrune, len(fs))] {
			os.Remove(filepath.Join(d.artDir(), x.name))
			links[x.ino]--
			if links[x.ino] == 0 { // its last name: its room is free
				total -= x.size
			}
		}
		fs = fs[min(artPrune, len(fs)):]
	}
}
