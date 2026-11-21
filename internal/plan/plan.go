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
	Margin     = 16 << 20  // room kept free on top of what a sync needs (the Sync page's previewRows)
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
	Add, Replace     []Want
	Keep             []Want // no transfer, but the record changes: the items holding it, or its MetaKey
	Rename           []Move
	Remove           []*library.Entry
	Playlists        map[string][]string
	PlaylistsStale   bool // recorded .m3u files hold old paths: rewrite them all, even with no song change
	PlaylistsChanged int  // playlist files to write or delete though no song moves: new, changed, renamed or taken out (I10)
	Leftover         int  // files a stopped sync left listed in m.Stale: a run deletes them first
	NeedBytes        int64
	ReplaceBytes     int64 // what the replaces add, or free when negative
	RemoveBytes      int64 // what the removals free (a run short of space removes them first)
	Whole            bool  // the selection holds Entire Library: no song of it can be taken out of Sync (I2)
}

// Changes reports whether p has anything to do: a song to move, a playlist file to write or
// delete, or a file an earlier sync left to delete.
func (p *Plan) Changes() bool {
	return len(p.Add)+len(p.Replace)+len(p.Rename)+len(p.Remove)+p.PlaylistsChanged+p.Leftover > 0 || p.PlaylistsStale
}

// Moving is the songs p renames, replaces or removes, by id: a path of theirs on the card is
// about to go.
func (p *Plan) Moving() map[string]bool {
	ids := map[string]bool{}
	for _, mv := range p.Rename {
		ids[mv.ID] = true
	}
	for _, w := range p.Replace {
		ids[w.Song.ID] = true
	}
	for _, e := range p.Remove {
		ids[e.ID] = true
	}
	return ids
}

// estimate is the bytes a song takes at q: its server size for Original, else its duration at the
// transcode's bitrate.
func estimate(s jellyfin.Song, q string) int64 {
	if quality.IsOriginal(q) {
		return s.Size
	}
	return int64(s.Duration) * quality.Bitrate(q) / 8
}

// Resized reports whether an original (q) whose server file is size bytes changed size since it
// was synced (a re-rip).
func Resized(e *library.Entry, q string, size int64) bool {
	return q == "original" && e.SrcSize > 0 && size > 0 && e.SrcSize != size
}

// Wanted is the quality a chosen item wants song s at (e: its record on the card, nil when it isn't
// there): syncQ (Sync Quality, "" for Original), but a held item's, which stays at the quality on
// the card; ok is false for a held item's song not on the card, which it doesn't add. reencode is
// Always Re-encode: without it a lossy song stays as it is under a transcode (library.Quality).
// Compute plans each song at the best quality its items want; Browse reads its statuses the same
// way while no preview is at hand.
func Wanted(held bool, s jellyfin.Song, e *library.Entry, syncQ string, reencode bool) (q string, ok bool) {
	q = syncQ
	if q == "" {
		q = "original"
	}
	if held { // only what is on the card, as it is
		if e == nil {
			return "", false
		}
		q, reencode = e.Quality, true
	}
	return library.Quality(q, s, reencode), true
}

// Fetches reports whether the sync fetches song s at quality q: it isn't on the card (e nil), or is
// there at another quality or in another format, or was re-ripped on the server. Any other song
// wanted stays, renamed when its tags changed.
func Fetches(e *library.Entry, s jellyfin.Song, q string) bool {
	return e == nil || e.Quality != q || !strings.EqualFold(filepath.Ext(e.Path), library.Ext(s, q)) || Resized(e, q, s.Size)
}

// Compute diffs the selection (resolved: item key -> its songs; playlistNames: playlist item key
// -> display name) against the manifest, each song at the best quality its items want (Wanted).
func Compute(sel selection.Set, syncQ string, reencode bool, resolved map[string][]jellyfin.Song, playlistNames map[string]string,
	m *library.Manifest, root string) Plan {
	wants := map[string]*Want{}
	multi := map[string]bool{}
	for _, it := range sel.Items {
		key := selection.Key(it.Kind, it.ID)
		for _, s := range resolved[key] {
			if s.Disc > 1 {
				multi[s.AlbumID] = true
			}
			q, ok := Wanted(it.Held, s, m.Songs[s.ID], syncQ, reencode)
			if !ok {
				continue
			}
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
			if !slices.Contains(w.Items, key) {
				w.Items = append(w.Items, key)
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
			!Resized(e, w.Quality, w.Song.Size)
	}
	// Every song owns its current path, keyed by its lyrics path (the name with the extension dropped),
	// case-folded (the card is FAT/exFAT: case variants are one file), for this whole sync: nothing new
	// lands on a file that a rename, a replace or the removals take away, and no two songs share a path
	// or a .lrc file (swapped tags, or the same song in two formats, get their [id] instead; audit S16).
	key := func(path string) string { return strings.ToLower(library.LyricsPath(path)) }
	// In id order: two entries on one stem (left by a version before S16) give it to the same song
	// every time.
	claim := map[string]string{}
	have := make([]string, 0, len(m.Songs))
	for id := range m.Songs {
		have = append(have, id)
	}
	sort.Strings(have)
	for _, id := range have {
		claim[key(m.Songs[id].Path)] = id
	}

	p := Plan{Whole: sel.Has(selection.KindLibrary, "all")}
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
		case Fetches(e, w.Song, w.Quality): // a new format or a re-rip is fetched again, never renamed
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
	for _, ws := range [][]Want{p.Add, p.Replace} {
		for _, w := range ws {
			if d := strings.ToLower(filepath.Dir(w.Path)); w.Song.CoverArt != "" && !folders[d] {
				folders[d] = true
				p.NeedBytes += coverBytes
			}
		}
	}

	p.Playlists = map[string][]string{}
	p.PlaylistsStale, p.Leftover = m.PlaylistsStale, len(m.Stale)
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
	return p
}
