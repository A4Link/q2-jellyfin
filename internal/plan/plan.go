// Package plan computes the sync preview: what to add, replace, rename and remove so the card
// mirrors the selection. Compute is pure (no I/O, no clock) and deterministic.
package plan

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

const (
	margin     = 16 << 20
	coverBytes = 256 << 10 // a cover.jpg at 600 px, counted once per new album folder
)

type Want struct {
	Song    jellyfin.Song
	Quality string
	Path    string
	Items   []string
	Bytes   int64
}

type Move struct{ From, To, ID string }

type Plan struct {
	Add, Replace         []Want
	Keep                 []Want // no transfer, but the record changes: the items holding it, or its MetaKey
	Rename               []Move
	Remove               []*library.Entry
	Playlists            map[string][]string
	PlaylistsStale       bool // recorded .m3u files hold old paths: rewrite them all, even with no song change
	PlaylistsChanged     int  // playlist files to write or delete though no song moves: new, changed, renamed or taken out (I10)
	NeedBytes, FreeBytes int64
	ReplaceBytes         int64 // what the replaces add, or free when negative
	RemoveBytes          int64 // what the removals free (a run short of space removes them first)
	Fits                 bool
}

// estimate is the bytes a song takes at q: its server size for Original, else its duration at the
// transcode's bitrate.
func estimate(s jellyfin.Song, q string) int64 {
	if quality.IsOriginal(q) {
		return s.Size
	}
	return int64(s.Duration) * quality.Bitrate(q) / 8
}

// resized reports whether an original's server file changed size since it was synced (a re-rip).
func resized(e *library.Entry, w *Want) bool {
	return w.Quality == "original" && e.SrcSize > 0 && w.Song.Size > 0 && e.SrcSize != w.Song.Size
}

// Compute diffs the selection (resolved: item key -> its songs; playlistNames: playlist item key
// -> display name) against the manifest. Every item's songs are at syncQ (Sync Quality, "" for
// Original), but a held item's, which stay at the quality on the card.
func Compute(sel selection.Set, syncQ string, resolved map[string][]jellyfin.Song, playlistNames map[string]string,
	m *library.Manifest, root string, free int64) Plan {
	wants := map[string]*Want{}
	multi := map[string]bool{}
	if syncQ == "" {
		syncQ = "original"
	}
	for _, it := range sel.Items {
		key := selection.Key(it.Kind, it.ID)
		for _, s := range resolved[key] {
			if s.Disc > 1 {
				multi[s.AlbumID] = true
			}
			q := syncQ
			if it.Held { // only what is on the card, as it is
				e := m.Songs[s.ID]
				if e == nil {
					continue
				}
				q = e.Quality
			}
			q = library.Quality(q, s.Suffix)
			w := wants[s.ID]
			if w == nil {
				w = &Want{Song: s, Quality: q}
				wants[s.ID] = w
			} else if quality.Better(q, w.Quality) {
				w.Quality = q
			}
			if w.Song.AlbumArtist == w.Song.Artist && s.AlbumArtist != s.Artist {
				w.Song = s // its album's own artist over its track artist, whichever item came first (M41)
			}
			if n := len(w.Items); n == 0 || w.Items[n-1] != key {
				seen := false
				for _, k := range w.Items {
					seen = seen || k == key
				}
				if !seen {
					w.Items = append(w.Items, key)
				}
			}
		}
	}

	ids := make([]string, 0, len(wants))
	for id := range wants {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// unchanged: the song stays as it is on the card.
	unchanged := func(e *library.Entry, w *Want) bool {
		return e.Quality == w.Quality && e.Meta == library.MetaKey(w.Song, w.Quality) && !library.StaleName(root, e.Path) &&
			!resized(e, w)
	}
	// Every song owns its current path, keyed by its lyrics path (the name with the extension dropped),
	// case-folded (the card is FAT/exFAT: case variants are one file), for this whole sync: nothing new
	// lands on a file that a rename, a replace or the removals take away, and no two songs share a path
	// or a .lrc file (swapped tags, or the same song in two formats, get their [id] instead; audit S16).
	key := func(path string) string { return strings.ToLower(library.LyricsPath(path)) }
	claim := map[string]string{}
	for id, e := range m.Songs {
		claim[key(e.Path)] = id
	}

	var p Plan
	final := map[string]string{} // song id -> path it will have
	var back []string            // unchanged songs that may still have the " [id]" name of a past clash
	for _, id := range ids {
		w := wants[id]
		e := m.Songs[id]
		if e != nil && unchanged(e, w) {
			w.Path = e.Path
			final[id] = e.Path
			if strings.HasSuffix(strings.TrimSuffix(e.Path, filepath.Ext(e.Path)), "]") {
				back = append(back, id)
			} else if !slices.Equal(e.Items, w.Items) {
				p.Keep = append(p.Keep, *w)
			}
			continue
		}
		path := library.SongPath(root, w.Song, w.Quality, multi[w.Song.AlbumID])
		if owner, ok := claim[key(path)]; ok && owner != id {
			path = library.WithID(path, id)
		}
		claim[key(path)] = id
		w.Path, final[id] = path, path
		w.Bytes = estimate(w.Song, w.Quality)
		switch {
		case e == nil:
			p.Add = append(p.Add, *w)
			p.NeedBytes += w.Bytes
		case e.Quality != w.Quality || !strings.EqualFold(filepath.Ext(e.Path), filepath.Ext(path)) || resized(e, w):
			// a new format or a re-rip is fetched again, never renamed
			p.Replace = append(p.Replace, *w)
			p.NeedBytes += w.Bytes - e.Size
			p.ReplaceBytes += w.Bytes - e.Size
		default:
			if e.Path != path {
				p.Rename = append(p.Rename, Move{From: e.Path, To: path, ID: id})
			}
			p.Keep = append(p.Keep, *w) // its new MetaKey, recorded once it is at path
		}
	}
	// A song named " [id]" in a clash takes its own name back once no song (kept, new or being
	// removed) claims it. Decided after every other song has its path, so it never takes the name
	// from a song later in the order, and the next sync finds it unchanged.
	for _, id := range back {
		w, e := wants[id], m.Songs[id]
		path := library.SongPath(root, w.Song, w.Quality, multi[w.Song.AlbumID])
		if _, ok := claim[key(path)]; !ok && e.Path == library.WithID(path, id) {
			claim[key(path)] = id
			w.Path, final[id] = path, path
			p.Rename = append(p.Rename, Move{From: e.Path, To: path, ID: id})
			p.Keep = append(p.Keep, *w)
		} else if !slices.Equal(e.Items, w.Items) {
			p.Keep = append(p.Keep, *w)
		}
	}
	for id, e := range m.Songs {
		if wants[id] == nil {
			p.Remove = append(p.Remove, e)
			p.RemoveBytes += e.Size
		}
	}
	folders := map[string]bool{} // album folders on the card, which have their cover
	for _, e := range m.Songs {
		folders[strings.ToLower(filepath.Dir(e.Path))] = true
	}
	for _, w := range append(append([]Want(nil), p.Add...), p.Replace...) {
		if d := strings.ToLower(filepath.Dir(w.Path)); w.Song.CoverArt != "" && !folders[d] {
			folders[d] = true
			p.NeedBytes += coverBytes
		}
	}

	p.Playlists = map[string][]string{}
	p.PlaylistsStale = m.PlaylistsStale
	for _, it := range sel.Items {
		if it.Kind != selection.KindPlaylist && it.Kind != selection.KindFavorites {
			continue
		}
		key := selection.Key(it.Kind, it.ID)
		paths := []string{}
		for _, s := range resolved[key] {
			if f := final[s.ID]; f != "" { // a held item's song not on the card has none
				paths = append(paths, f)
			}
		}
		// Two playlists with one name (or one named like Favourites) each keep their own file:
		// later ones, in selection order, are "Name (2)", "Name (3)"...
		name := playlistNames[key]
		for n := 2; p.Playlists[name] != nil; n++ {
			name = fmt.Sprintf("%s (%d)", playlistNames[key], n)
		}
		p.Playlists[name] = paths
	}
	for name, paths := range p.Playlists {
		if f := m.Playlists[name]; f == "" || library.StalePlaylistFile(f) || m.PlaylistSums[name] != library.PlaylistSum(paths) {
			p.PlaylistsChanged++
		}
	}
	for name := range m.Playlists {
		if _, ok := p.Playlists[name]; !ok {
			p.PlaylistsChanged++
		}
	}

	byPath := func(a []Want) { sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path }) }
	byPath(p.Add)
	byPath(p.Replace)
	sort.Slice(p.Rename, func(i, j int) bool { return p.Rename[i].To < p.Rename[j].To })
	sort.Slice(p.Remove, func(i, j int) bool { return p.Remove[i].Path < p.Remove[j].Path })
	p.FreeBytes = free
	p.Fits = p.NeedBytes <= 0 || p.NeedBytes-p.RemoveBytes+margin <= free // freeing space always fits
	return p
}
