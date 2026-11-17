package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// view is what status subtitles are read from: the selection, the manifest and the songs fetched,
// indexed in one pass per request, so a list of thousands of items never walks the manifest per
// row (I17).
// ponytail: built again per request (twice on a first page with Sync Now), about 25 ms per 40k songs on a laptop;
// cache it by manifest, selection and resolve if pages of a huge library are slow on the Q2.
type view struct {
	sel     selection.Set
	m       *library.Manifest
	res     map[string][]jellyfin.Song
	artists map[string][]jellyfin.Album // artist id -> its albums, as last fetched
	keyed   map[string][]string         // item key -> the songs on the card recorded for it
	albumOn map[string]map[string]bool  // album id -> its fetched songs that are on the card
	// pending: item key -> a sync is due though no song of it is missing: the item was taken out
	// and its songs are still on the card, or they are there at another quality (M35).
	pending map[string]bool
	held    map[string]bool // chosen item key -> it is held: it keeps its songs at the quality on the card
	lib     bool            // Entire Library is chosen: so are the playlists it takes in
	libHeld bool            // Entire Library is held: so are the playlists it takes in
	covers  *sync.Map       // song id -> its cover's item (art.go), recorded as song rows are made
}

func (d *daemon) view() view {
	sel, _ := d.selection()
	m := d.manifest()
	d.mu.Lock()
	res, artists := maps.Clone(d.resolved), maps.Clone(d.artistAlbums)
	d.mu.Unlock()
	v := view{sel: sel, m: m, res: res, artists: artists, keyed: map[string][]string{}, albumOn: map[string]map[string]bool{},
		pending: map[string]bool{}, held: map[string]bool{}, covers: &d.art.songs}
	set := d.settings()
	syncQ, reencode := set.def(), set.reencode() // every chosen item's, but a held one's: as on the card
	for _, it := range sel.Items {
		v.held[selection.Key(it.Kind, it.ID)] = it.Held
		if it.Kind == selection.KindLibrary {
			v.lib, v.libHeld = true, it.Held
		}
	}
	song := map[string]*jellyfin.Song{} // the fetched songs on the card; pointers, not 100-byte copies
	for _, songs := range res {
		for i := range songs {
			s := &songs[i]
			if m.Songs[s.ID] == nil {
				continue
			}
			if v.albumOn[s.AlbumID] == nil {
				v.albumOn[s.AlbumID] = map[string]bool{}
			}
			v.albumOn[s.AlbumID][s.ID], song[s.ID] = true, s
		}
	}
	for id, e := range m.Songs {
		// A song not fetched since the start: an original on the card is of its file's format.
		var sg jellyfin.Song
		p, known := song[id]
		if known {
			sg = *p
		}
		if !known && quality.IsOriginal(e.Quality) {
			sg.Suffix = strings.ToLower(strings.TrimPrefix(filepath.Ext(e.Path), "."))
		}
		want := "" // the best quality among the chosen items holding it, as the plan picks
		for _, k := range e.Items {
			v.keyed[k] = append(v.keyed[k], id)
			if !v.chosen(k) {
				v.pending[k] = true // taken out: its songs go at the next sync
				continue
			}
			q, re := syncQ, reencode
			// held, or a playlist a held Entire Library takes in: as on the card
			if held, own := v.held[k]; held || !own && v.libHeld {
				q, re = e.Quality, true
			}
			// An original the Q2 can't play syncs as quality.Fallback, a lossy song under a transcode as
			// it is (library.Quality), as the plan does per item.
			if q = library.Quality(q, sg, re); quality.Better(q, want) {
				want = q
			}
		}
		// An original .m4a or .wma not fetched may be lossless (library.Lossy): not known here, so no change.
		if want == "" || e.Quality == want || !known && (sg.Suffix == "m4a" || sg.Suffix == "wma") {
			continue
		}
		for _, k := range e.Items {
			v.pending[k] = true
		}
	}
	return v
}

// chosen reports whether the item with key k is chosen: one in the selection, or a playlist Entire
// Library takes in.
func (v view) chosen(k string) bool {
	_, ok := v.held[k]
	return ok || v.lib && strings.HasPrefix(k, string(selection.KindPlaylist)+":")
}

// on counts ids and those of keyed[key] not among them: songs on the card through two indexes.
func (v view) on(key string, ids map[string]bool) int {
	n := len(ids)
	for _, sid := range v.keyed[key] {
		if !ids[sid] {
			n++
		}
	}
	return n
}

// status is an item's sync status: On Q2, Waiting to sync, X of Y songs on Q2, or "" when none of it
// is on the Q2 and it is not selected. total is Y when its songs were never fetched.
func (v view) status(kind selection.Kind, id string, total int) string {
	key := selection.Key(kind, id)
	_, selected := v.held[key]
	songs, resolved := v.res[key]
	x := 0
	switch {
	case resolved: // its own songs, counted by id: a playlist may hold a song twice (I13), and a song
		// it no longer holds doesn't stand in for one it gained (review G3, Minor 4)
		on, all := map[string]bool{}, map[string]bool{}
		for _, s := range songs {
			all[s.ID] = true
			if v.m.Songs[s.ID] != nil {
				on[s.ID] = true
			}
		}
		x, total = len(on), len(all)
	case kind == selection.KindAlbum:
		x = v.on(key, v.albumOn[id])
	case kind == selection.KindArtist && !selected && v.artists[id] != nil: // through its albums (M36)
		total = 0
		for _, a := range v.artists[id] {
			total += a.SongCount
			x += v.on(selection.Key(selection.KindAlbum, a.ID), v.albumOn[a.ID])
		}
	default:
		x = len(v.keyed[key])
		if kind == selection.KindPlaylist && selected {
			total = 0 // the server's count holds a song twice when the playlist does (I13)
		}
	}
	switch {
	case v.pending[key]:
		return "Waiting to sync"
	case selected && resolved && total == 0:
		return "On Q2" // chosen and empty: nothing to wait for (I13)
	case x == 0 && selected:
		return "Waiting to sync"
	case x == 0:
		return ""
	case total == 0 || x >= total:
		return "On Q2"
	}
	return fmt.Sprintf("%d of %d songs on Q2", x, total)
}

// jobStatus is a chosen item's status for Sync Jobs: "Not checked yet" instead of On Q2 while its
// songs weren't fetched (since the start, say), as the songs recorded for it can't tell a whole
// playlist from one whose sync stopped part way. Sync Now keeps On Q2, so it doesn't show after
// every start; opening Sync checks.
func (v view) jobStatus(kind selection.Kind, id string) string {
	st := v.status(kind, id, 0)
	if _, ok := v.res[selection.Key(kind, id)]; !ok && st == "On Q2" {
		return "Not checked yet"
	}
	return st
}

func (v view) item(kind selection.Kind, id, name, own string, total int) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: string(kind), Title: name, Subtitle: join(own, v.status(kind, id, total)),
		Context: selection.Key(kind, id), Position: "toggle"}
}

func (v view) song(s jellyfin.Song, listKey string, i int) q2spot.Row {
	if s.CoverArt != "" && v.covers != nil {
		v.covers.Store(s.ID, s.CoverArt)
	}
	st := ""
	if v.m.Songs[s.ID] != nil {
		st = "On Q2"
	} else if _, ok := v.held[selection.Key(selection.KindSong, s.ID)]; ok {
		st = "Waiting to sync"
	}
	return q2spot.Row{Kind: "action", URI: "play", Title: s.Title, Subtitle: join(s.Artist, st),
		Context: fmt.Sprintf("song:%s %s %d", s.ID, listKey, i), Position: "toggle"}
}

// syncNowRow is the shortcut to Sync, present while the card differs from the selection: a chosen
// item not all on the Q2 or at another quality, one taken out whose songs are still there (M35), or
// files an earlier sync left listed to delete (Stale).
// Cheap: read from the selection, manifest and cached songs, never the network.
func (d *daemon) syncNowRow() (q2spot.Row, bool) {
	v := d.view()
	n := 0
	for _, it := range v.sel.Items {
		if !it.Held && v.status(it.Kind, it.ID, 0) != "On Q2" {
			n++
		}
	}
	for k := range v.pending {
		if !v.chosen(k) {
			n++
		}
	}
	if len(v.m.Stale) > 0 {
		n++
	}
	if n == 0 {
		return q2spot.Row{}, false
	}
	return list("sync", "Sync Now", fmt.Sprintf("%d waiting", n), ""), true
}
