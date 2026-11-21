package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// view is what status subtitles are read from: the selection, the card's record, the songs fetched
// and what the sync does to each song, indexed once per request, so a list of thousands of items
// never walks the manifest per row (I17).
//
// What the sync does is planned here from what is at hand, by the plan's own rules (plan.Wanted,
// plan.Fetches): each chosen item's songs as fetched (a preview fetches every one), else as the
// card records them for it (its last sync or preview), else an artist's from its albums'. An item
// chosen since and not fetched holds songs not known: a song on the card it may hold (an album's
// own, an artist's albums', any for another kind) is not taken as removed.
// ponytail: built again per request (twice on a first page with Sync Now), one pass over the fetched
// songs and two over the manifest; cache it by manifest, selection and resolve if pages of a huge
// library are slow on the Q2.
type view struct {
	sel     selection.Set
	m       *library.Manifest
	res     map[string][]jellyfin.Song
	artists map[string][]jellyfin.Album // artist id -> its albums, as last fetched
	keyed   map[string][]string         // item key -> the songs on the card recorded for it
	albumOn map[string]map[string]bool  // album id -> its fetched songs that are on the card
	derived map[string][]string         // chosen artist not fetched -> its songs, from its albums' (each known)
	unknown map[string]bool             // chosen item whose songs aren't known: not fetched, none recorded
	touched map[string]bool             // song id -> the sync fetches, replaces or removes it
	pending map[string]bool             // item key -> the sync touches a song of it
	held    map[string]bool             // chosen item key -> it is held
	lib     bool                        // Entire Library is chosen: so are the playlists it takes in
	libHeld bool                        // Entire Library is chosen and held
	covers  *sync.Map                   // song id -> its cover's item (art.go), recorded as song rows are made
}

func (d *daemon) view() view {
	sel, _ := d.selection()
	m := d.manifest()
	set := d.settings()
	d.mu.Lock()
	res, artists := maps.Clone(d.resolved), maps.Clone(d.artistAlbums)
	d.mu.Unlock()
	v := view{sel: sel, m: m, res: res, artists: artists, keyed: map[string][]string{}, albumOn: map[string]map[string]bool{},
		derived: map[string][]string{}, unknown: map[string]bool{}, touched: map[string]bool{}, pending: map[string]bool{},
		held: map[string]bool{}, covers: &d.art.songs}
	for id, e := range m.Songs {
		for _, k := range e.Items {
			v.keyed[k] = append(v.keyed[k], id)
		}
	}
	known := map[string]*jellyfin.Song{} // the fetched songs on the card; pointers, not 200-byte copies
	for _, songs := range res {
		for i := range songs {
			if s := &songs[i]; m.Songs[s.ID] != nil {
				if v.albumOn[s.AlbumID] == nil {
					v.albumOn[s.AlbumID] = map[string]bool{}
				}
				v.albumOn[s.AlbumID][s.ID], known[s.ID] = true, s
			}
		}
	}
	// The chosen items as the plan has them: Entire Library takes in the playlists not chosen on
	// their own, held with it (resolveItems).
	items := slices.Clone(sel.Items)
	for _, it := range sel.Items {
		v.held[selection.Key(it.Kind, it.ID)] = it.Held
		if it.Kind == selection.KindLibrary {
			v.lib, v.libHeld = true, it.Held
		}
	}
	if v.lib {
		taken := map[string]bool{}
		take := func(k string) {
			if kind, id := parseKey(k); kind == selection.KindPlaylist && !v.chosenAlone(k) && !taken[k] {
				taken[k] = true
				items = append(items, selection.Item{Kind: kind, ID: id, Held: v.libHeld})
			}
		}
		for k := range v.keyed {
			take(k)
		}
		for k := range res {
			take(k)
		}
	}
	syncQ, reencode := set.def(), set.reencode()
	want := map[string]string{}        // song id -> the best quality the chosen items want it at
	of := make([][]string, len(items)) // each item's songs, for the pending pass below
	for i, it := range items {
		key := selection.Key(it.Kind, it.ID)
		_, fetched := res[key]
		all := v.songsOf(it, known, func(s jellyfin.Song) {
			of[i] = append(of[i], s.ID)
			if q, ok := plan.Wanted(it.Held, s, m.Songs[s.ID], syncQ, reencode); ok {
				if w, seen := want[s.ID]; !seen || quality.Better(q, w) {
					want[s.ID] = q
				}
			}
		})
		switch {
		case !all:
			v.unknown[key] = true
		case !fetched && len(v.keyed[key]) == 0 && it.Kind == selection.KindArtist:
			v.derived[key] = of[i]
		}
	}
	for id, q := range want {
		if e := m.Songs[id]; plan.Fetches(e, standIn(id, e, known), q) {
			v.touched[id] = true
		}
	}
	// A song on the card no chosen item holds is removed, unless one whose songs aren't known may
	// hold it (maybe): an album its own songs, an artist its listed albums', another kind any song.
	openAlbum, openAny := map[string]bool{}, false
	for _, it := range items {
		switch {
		case !v.unknown[selection.Key(it.Kind, it.ID)]:
		case it.Kind == selection.KindAlbum:
			openAlbum[it.ID] = true
		case it.Kind == selection.KindArtist && artists[it.ID] != nil:
			for _, a := range artists[it.ID] {
				openAlbum[a.ID] = true
			}
		default:
			openAny = true
		}
	}
	maybe := map[string]bool{}
	for id, e := range m.Songs {
		if _, ok := want[id]; ok {
			continue
		}
		may := openAny
		if !may && len(openAlbum) > 0 {
			al, ok := albumOf(e, known[id])
			may = !ok || openAlbum[al]
		}
		if may {
			maybe[id] = true
		} else {
			v.touched[id] = true
		}
	}
	// The sync touches an item when it touches a song recorded for it (taken out of Sync or not), or
	// one a chosen item has in the plan (held or not). One taken out is due while a song of it may
	// go (M35).
	for k, ids := range v.keyed {
		out := !v.chosen(k)
		if slices.ContainsFunc(ids, func(id string) bool { return v.touched[id] || out && maybe[id] }) {
			v.pending[k] = true
		}
	}
	for i, it := range items {
		if slices.ContainsFunc(of[i], func(id string) bool { return v.touched[id] }) {
			v.pending[selection.Key(it.Kind, it.ID)] = true
		}
	}
	return v
}

// songsOf calls f with each song of chosen item it as the plan has it: as fetched, else as the card
// records it for it, else (an artist) its albums' as fetched or recorded for them. It reports
// whether every song of it is known.
func (v view) songsOf(it selection.Item, known map[string]*jellyfin.Song, f func(jellyfin.Song)) bool {
	list := func(k string) bool {
		if songs, ok := v.res[k]; ok {
			for _, s := range songs {
				f(s)
			}
			return true
		}
		for _, id := range v.keyed[k] {
			f(standIn(id, v.m.Songs[id], known))
		}
		return len(v.keyed[k]) > 0
	}
	switch {
	case list(selection.Key(it.Kind, it.ID)):
		return true
	case it.Kind == selection.KindSong: // its one song, maybe on the card through another item
		f(standIn(it.ID, v.m.Songs[it.ID], known))
		return true
	case it.Kind == selection.KindArtist && v.artists[it.ID] != nil:
		all := true
		for _, a := range v.artists[it.ID] {
			all = list(selection.Key(selection.KindAlbum, a.ID)) && all
		}
		return all
	}
	return false
}

// standIn is song id as the plan would have it when no list fetched holds it: what the card records
// of it (e). An original's format is its file's; a transcode's source format isn't recorded. An
// original .m4a or .wma counts as lossy: the server tells ALAC or WMA Lossless only once fetched.
func standIn(id string, e *library.Entry, known map[string]*jellyfin.Song) jellyfin.Song {
	if s := known[id]; s != nil {
		return *s
	}
	s := jellyfin.Song{ID: id}
	if e != nil && quality.IsOriginal(e.Quality) {
		s.Suffix = strings.ToLower(strings.TrimPrefix(filepath.Ext(e.Path), "."))
		s.Codec = map[string]string{"m4a": "aac", "wma": "wmav2"}[s.Suffix]
	}
	return s
}

// albumOf is the album of a song on the card: s's when fetched, else the one recorded for it (e);
// false when not known.
func albumOf(e *library.Entry, s *jellyfin.Song) (string, bool) {
	if s != nil {
		return s.AlbumID, true
	}
	for _, k := range e.Items {
		if kind, id := parseKey(k); kind == selection.KindAlbum {
			return id, true
		}
	}
	return "", false
}

// chosenAlone reports whether the item with key k is in the selection itself.
func (v view) chosenAlone(k string) bool {
	_, ok := v.held[k]
	return ok
}

// chosen reports whether the item with key k is chosen: one in the selection, or a playlist Entire
// Library takes in.
func (v view) chosen(k string) bool {
	return v.chosenAlone(k) || v.lib && strings.HasPrefix(k, string(selection.KindPlaylist)+":")
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

// status is an item's sync status: Waiting to sync when the sync touches a song of it (or it is
// chosen and its songs aren't known yet), else Held, On Q2, X of Y songs on Q2, or "" when none of
// it is on the Q2 and it is not selected. total is Y when its songs were never fetched. Under Entire
// Library (not held) an album or artist is chosen through it: Waiting to sync for what isn't on the
// card once the library is fetched, Included via Entire Library while nothing of it is known.
func (v view) status(kind selection.Kind, id string, total int) string {
	key := selection.Key(kind, id)
	held, selected := v.held[key]
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
	case v.derived[key] != nil:
		for _, sid := range v.derived[key] {
			if v.m.Songs[sid] != nil {
				x++
			}
		}
		total = len(v.derived[key])
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
	viaLib := v.lib && !v.libHeld && !selected && (kind == selection.KindAlbum || kind == selection.KindArtist)
	_, libFetched := v.res[selection.Key(selection.KindLibrary, "all")]
	switch {
	case v.pending[key] || v.unknown[key] && !held || viaLib && libFetched && x < total:
		return "Waiting to sync"
	case viaLib && x == 0:
		return "Included via Entire Library"
	case x == 0 && held: // a held item adds nothing to the card (plan.Wanted)
		return "Held"
	case x == 0 && !selected:
		return ""
	case total == 0 || x >= total: // chosen and empty too: nothing to wait for (I13)
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
	switch {
	case v.touched[s.ID]:
		st = "Waiting to sync"
	case v.m.Songs[s.ID] != nil:
		st = "On Q2"
	case v.held[selection.Key(selection.KindSong, s.ID)]: // a held item adds nothing to the card (plan.Wanted)
		st = "Held"
	}
	return q2spot.Row{Kind: "action", URI: "play", Title: s.Title, Subtitle: join(s.Artist, st),
		Context: fmt.Sprintf("song:%s %s %d", s.ID, listKey, i), Position: "toggle"}
}

// syncNowRow is the shortcut to Sync, present while the sync has something to do: a song of a
// chosen item to fetch, replace or remove, one of an item taken out to remove (M35), a chosen item
// not held whose songs aren't known yet, or files an earlier sync left listed to delete (Stale).
// Cheap: read from the selection, manifest and cached songs, never the network.
func (d *daemon) syncNowRow() (q2spot.Row, bool) {
	v := d.view()
	n := 0
	for _, it := range v.sel.Items {
		if k := selection.Key(it.Kind, it.ID); v.pending[k] || v.unknown[k] && !it.Held {
			n++
		}
	}
	for k, p := range v.pending {
		if p && !v.chosen(k) {
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
