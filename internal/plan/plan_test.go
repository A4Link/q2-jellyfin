package plan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

const root = "/mnt/mmc"

func song(id, title string) jellyfin.Song {
	return jellyfin.Song{ID: id, Title: title, Album: "Alb", AlbumID: "a1", AlbumArtist: "Art", Year: 2020,
		Disc: 1, Track: 1, Duration: 100, Size: 1000, Suffix: "flac"}
}

func item(kind selection.Kind, id string) selection.Item {
	return selection.Item{Kind: kind, ID: id}
}

func entry(s jellyfin.Song, q string, size int64, items ...string) *library.Entry {
	return &library.Entry{ID: s.ID, Path: library.SongPath(root, s, q, false), Quality: q, Size: size,
		Items: items, Meta: library.MetaKey(s, q)}
}

func manifest(es ...*library.Entry) *library.Manifest {
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for _, e := range es {
		m.Songs[e.ID] = e
	}
	return m
}

const big = int64(1) << 40

func TestPlanDuplicatesAcrossItems(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("album", "a1"), item("playlist", "p1")}}
	res := map[string][]jellyfin.Song{"album:a1": {s}, "playlist:p1": {s, s}}
	p := Compute(sel, "original", res, map[string]string{"playlist:p1": "Mix"}, manifest(), root, big)
	if len(p.Add) != 1 || !reflect.DeepEqual(p.Add[0].Items, []string{"album:a1", "playlist:p1"}) {
		t.Fatalf("add = %+v", p.Add)
	}
	if got := p.Playlists["Mix"]; len(got) != 2 || got[0] != p.Add[0].Path {
		t.Fatalf("playlist = %v", got)
	}
}

// Every item's songs are at Sync Quality ("" is Original), whatever quality is saved with the item.
func TestPlanSyncQuality(t *testing.T) {
	s := song("s1", "One")
	it := item("album", "a1")
	it.Quality = "mp3-128" // saved by 0.6: not used
	sel := selection.Set{Items: []selection.Item{it}}
	res := map[string][]jellyfin.Song{"album:a1": {s}}
	if p := Compute(sel, "", res, nil, manifest(), root, big); len(p.Add) != 1 || p.Add[0].Quality != "original" || p.Add[0].Bytes != 1000 {
		t.Fatalf("add = %+v", p.Add)
	}
	if p := Compute(sel, "opus-128", res, nil, manifest(), root, big); len(p.Add) != 1 || p.Add[0].Quality != "opus-128" {
		t.Fatalf("add = %+v", p.Add)
	}
}

// A song a held item and another item share keeps the better of the quality on the card and Sync
// Quality.
func TestPlanBestQualityWins(t *testing.T) {
	s := song("s1", "One")
	held := item("album", "a1")
	held.Held = true
	sel := selection.Set{Items: []selection.Item{held, item("song", "s1")}}
	res := map[string][]jellyfin.Song{"album:a1": {s}, "song:s1": {s}}
	m := manifest(entry(s, "original", 1000, "album:a1"))
	if p := Compute(sel, "opus-128", res, nil, m, root, big); len(p.Add)+len(p.Replace) != 0 {
		t.Fatalf("add %+v replace %+v", p.Add, p.Replace)
	}
}

func TestPlanQualityChangeReplaces(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("song", "s1")}}
	m := manifest(entry(s, "original", 5000, "song:s1"))
	p := Compute(sel, "mp3-320", map[string][]jellyfin.Song{"song:s1": {s}}, nil, m, root, big)
	if len(p.Replace) != 1 || len(p.Add) != 0 || p.Replace[0].Bytes != 100*320*1000/8 {
		t.Fatalf("plan = %+v", p)
	}
	if p.NeedBytes != 4000000-5000 || p.ReplaceBytes != 4000000-5000 { // M20: the preview shows the change
		t.Fatalf("need = %d, replace = %d", p.NeedBytes, p.ReplaceBytes)
	}
}

func TestPlanRemovedItemDeletes(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	sel := selection.Set{Items: []selection.Item{item("song", "s2")}}
	m := manifest(entry(a, "original", 1000, "song:s1"), entry(b, "original", 1000, "song:s2"))
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:s2": {b}}, nil, m, root, big)
	if len(p.Remove) != 1 || p.Remove[0].ID != "s1" || len(p.Add)+len(p.Replace)+len(p.Rename) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanArtistNewAlbum(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	b.AlbumID, b.Album = "a2", "Other"
	sel := selection.Set{Items: []selection.Item{item("artist", "ar1")}}
	m := manifest(entry(a, "original", 1000, "artist:ar1"))
	p := Compute(sel, "original", map[string][]jellyfin.Song{"artist:ar1": {a, b}}, nil, m, root, big)
	if len(p.Add) != 1 || p.Add[0].Song.ID != "s2" || len(p.Remove) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanRenameOnMetadataChange(t *testing.T) {
	old, s := song("s1", "One"), song("s1", "Uno")
	sel := selection.Set{Items: []selection.Item{item("song", "s1")}}
	m := manifest(entry(old, "original", 1000, "song:s1"))
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:s1": {s}}, nil, m, root, big)
	if len(p.Rename) != 1 || len(p.Add) != 0 || p.Rename[0].From != m.Songs["s1"].Path ||
		p.Rename[0].To != library.SongPath(root, s, "original", false) || p.Rename[0].ID != "s1" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanNotEnoughSpace(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {a, b}}, nil, manifest(), root, 16<<20+1999)
	if p.Fits || p.NeedBytes != 2000 || p.FreeBytes != 16<<20+1999 {
		t.Fatalf("plan = %+v", p)
	}
	if !Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {a, b}}, nil, manifest(), root, 16<<20+2000).Fits {
		t.Fatal("should fit exactly")
	}
}

func TestPlanUpToDate(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("song", "s1")}}
	m := manifest(entry(s, "original", 1000, "song:s1"))
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:s1": {s}}, nil, m, root, big)
	if len(p.Add)+len(p.Replace)+len(p.Rename)+len(p.Remove) != 0 || p.NeedBytes != 0 || !p.Fits {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanCollisionGetsID(t *testing.T) {
	a, b := song("s1", "Same"), song("s2", "Same")
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {b, a}}, nil, manifest(), root, big)
	if len(p.Add) != 2 || p.Add[0].Path == p.Add[1].Path {
		t.Fatalf("add = %+v", p.Add)
	}
	for _, w := range p.Add {
		if (w.Song.ID == "s2") != (w.Path == library.WithID(library.SongPath(root, b, "original", false), "s2")) {
			t.Fatalf("wrong song suffixed: %+v", w)
		}
	}
}

// An original whose suffix is empty or unsafe is fetched as the Opus 320 fallback, so the .opus it
// is saved as really holds Opus (never FLAC/OGG bytes under that name).
func TestPlanUnsafeSuffixTranscodes(t *testing.T) {
	ok, empty, bad := song("s1", "One"), song("s2", "Two"), song("s3", "Three")
	empty.Suffix, bad.Suffix = "", "dsdiff"
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {ok, empty, bad}}, nil, manifest(), root, big)
	got := map[string]string{}
	for _, w := range p.Add {
		got[w.Song.ID] = w.Quality
		if w.Path != library.SongPath(root, w.Song, w.Quality, false) {
			t.Errorf("%s: path %s", w.Song.ID, w.Path)
		}
	}
	if want := map[string]string{"s1": "original", "s2": "opus-320", "s3": "opus-320"}; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// A song synced under a name SafeName no longer makes (a leading "." the scan hides, or ".." the
// firmware refuses) is moved to its clean path though its metadata is unchanged (I18, M40).
func TestPlanRepathsDottedNames(t *testing.T) {
	s := song("s1", "One")
	s.AlbumArtist = ".38 Special"
	old := library.Entry{ID: "s1", Path: root + "/Jellyfin/.38 Special/2020 - Alb/01 - One.flac", Quality: "original",
		Size: 1000, Items: []string{"song:s1"}, Meta: library.MetaKey(s, "original")}
	sel := selection.Set{Items: []selection.Item{item("song", "s1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:s1": {s}}, nil, manifest(&old), root, big)
	if len(p.Rename) != 1 || p.Rename[0].To != root+"/Jellyfin/_38 Special/2020 - Alb/01 - One.flac" {
		t.Fatalf("plan = %+v", p)
	}
}

// The card is FAT/exFAT: paths that differ only in case are one file. A new song whose path is a
// case variant of a removed song's gets its [id], so neither deletes the other (I5).
func TestPlanCaseOnlyCollisionGetsID(t *testing.T) {
	gone, s := song("s1", "One"), song("s2", "One")
	gone.AlbumArtist = "Abba"
	s.AlbumArtist = "ABBA"
	sel := selection.Set{Items: []selection.Item{item("song", "s2")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:s2": {s}}, nil, manifest(entry(gone, "original", 1000)), root, big)
	if len(p.Add) != 1 || p.Add[0].Path != library.WithID(library.SongPath(root, s, "original", false), "s2") {
		t.Fatalf("plan = %+v", p)
	}
}

// A new song never plans onto the path of a song that moves away in the same sync: the move's
// remove of its old file would delete it (M16).
func TestPlanNoAddOntoMovingPath(t *testing.T) {
	x, y := song("x", "T"), song("y", "T")
	sel := selection.Set{Items: []selection.Item{item("song", "x"), item("song", "y")}}
	m := manifest(entry(x, "mp3-320", 1000, "song:x")) // x goes from .mp3 to .flac; y wants x's .mp3 path
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:x": {x}, "song:y": {y}}, nil, m, root, big)
	for _, w := range p.Add {
		if w.Song.ID == "y" && strings.EqualFold(w.Path, m.Songs["x"].Path) {
			t.Fatalf("y planned onto x's old path %s", w.Path)
		}
	}
}

// Two songs whose tags were swapped never move onto each other's current file (I6).
func TestPlanSwapNeverCrossesPaths(t *testing.T) {
	x, y := song("x", "Alpha"), song("y", "Beta")
	x.Track, y.Track = 1, 2
	ex, ey := entry(x, "original", 1000, "album:a1"), entry(y, "original", 1000, "album:a1")
	x.Title, x.Track, y.Title, y.Track = "Beta", 2, "Alpha", 1
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {x, y}}, nil, manifest(ex, ey), root, big)
	if len(p.Rename) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	for _, mv := range p.Rename {
		for _, e := range []*library.Entry{ex, ey} {
			if e.ID != mv.ID && strings.EqualFold(mv.To, e.Path) {
				t.Errorf("%s moves onto %s's file %s", mv.ID, e.ID, e.Path)
			}
		}
	}
}

// A new format (same id) is fetched again, never renamed: the old bytes under a .flac name would
// stay forever (I7). So is an original whose server size changed (a re-rip).
func TestPlanFormatOrSizeChangeReplaces(t *testing.T) {
	old, s := song("s1", "One"), song("s1", "One")
	old.Suffix = "mp3"
	sel := selection.Set{Items: []selection.Item{item("song", "s1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"song:s1": {s}}, nil, manifest(entry(old, "original", 900, "song:s1")), root, big)
	if len(p.Replace) != 1 || len(p.Rename) != 0 || !strings.HasSuffix(p.Replace[0].Path, ".flac") {
		t.Fatalf("format: plan = %+v", p)
	}
	e := entry(s, "original", 1000, "song:s1")
	e.SrcSize = 1000
	s.Size = 2000
	p = Compute(sel, "original", map[string][]jellyfin.Song{"song:s1": {s}}, nil, manifest(e), root, big)
	if len(p.Replace) != 1 || p.Replace[0].Path != e.Path || p.NeedBytes != 1000 {
		t.Fatalf("size: plan = %+v", p)
	}
}

// A plan that only frees space fits however full the card is: removing is how a user makes room (I11).
func TestPlanRemovalOnlyFits(t *testing.T) {
	a := song("s1", "One")
	p := Compute(selection.Set{}, "original", nil, nil, manifest(entry(a, "original", 1000, "song:s1")), root, 5<<20)
	if len(p.Remove) != 1 || !p.Fits {
		t.Fatalf("plan = %+v", p)
	}
}

// Original quality of a format the Q2 can't play (WavPack, Musepack) syncs as MP3 320, as the
// streamer plays it (M22).
func TestPlanUnplayableOriginalTranscodes(t *testing.T) {
	fl, wv, mpc := song("s1", "One"), song("s2", "Two"), song("s3", "Three")
	wv.Suffix, mpc.Suffix = "wv", "mpc"
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {fl, wv, mpc}}, nil, manifest(), root, big)
	got := map[string]string{}
	for _, w := range p.Add {
		got[w.Song.ID] = w.Quality
	}
	if want := map[string]string{"s1": "original", "s2": "opus-320", "s3": "opus-320"}; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// Each new album folder counts its cover in the space needed (M19); a folder already on the card
// has one.
func TestPlanCountsCovers(t *testing.T) {
	a, b, c := song("s1", "One"), song("s2", "Two"), song("s3", "Three")
	a.CoverArt, b.CoverArt, c.CoverArt = "al-1", "al-1", "al-2"
	c.Album, c.AlbumID = "Other", "a2"
	sel := selection.Set{Items: []selection.Item{item("album", "a1"), item("album", "a2")}}
	have := song("s0", "Zero") // already in album a1's folder
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {have, a, b}, "album:a2": {c}}, nil,
		manifest(entry(have, "original", 1000, "album:a1")), root, big)
	if p.NeedBytes != 3000+coverBytes {
		t.Fatalf("need = %d", p.NeedBytes)
	}
}

// A song with nothing to move still has its record refreshed: the items now holding it (I12), and
// after a rename its MetaKey.
func TestPlanKeepsRecordsFresh(t *testing.T) {
	a, old, b := song("s1", "One"), song("s2", "Two"), song("s2", "Deux")
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	m := manifest(entry(a, "original", 1000, "playlist:p1"), entry(old, "original", 1000, "album:a1"))
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {a, b}}, nil, m, root, big)
	got := map[string]Want{}
	for _, w := range p.Keep {
		got[w.Song.ID] = w
	}
	if w := got["s1"]; !reflect.DeepEqual(w.Items, []string{"album:a1"}) || w.Path != m.Songs["s1"].Path {
		t.Errorf("s1: %+v", w)
	}
	if w := got["s2"]; len(p.Rename) != 1 || w.Path != p.Rename[0].To || w.Song.Title != "Deux" {
		t.Errorf("s2: %+v %+v", w, p.Rename)
	}
	up := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {a}}, nil, manifest(entry(a, "original", 1000, "album:a1")), root, big)
	if len(up.Keep) != 0 {
		t.Errorf("keep with nothing to refresh: %+v", up.Keep)
	}
}

// Playlist files to write or delete though no song moves: new, changed (songs or order) and taken
// out; one written with the same songs is not (I10).
func TestPlanPlaylistsChanged(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	m := manifest(entry(a, "original", 1000, "album:a1"), entry(b, "original", 1000, "album:a1"))
	pa, pb := m.Songs["s1"].Path, m.Songs["s2"].Path
	m.Playlists = map[string]string{"Same": "/x/Same.m3u", "Order": "/x/Order.m3u", "Gone": "/x/Gone.m3u"}
	m.PlaylistSums = map[string]string{"Same": library.PlaylistSum([]string{pa, pb}),
		"Order": library.PlaylistSum([]string{pa, pb}), "Gone": library.PlaylistSum(nil)}
	sel := selection.Set{Items: []selection.Item{item("album", "a1"), item("playlist", "p1"),
		item("playlist", "p2"), item("playlist", "p3")}}
	res := map[string][]jellyfin.Song{"album:a1": {a, b}, "playlist:p1": {a, b}, "playlist:p2": {b, a}, "playlist:p3": {a}}
	names := map[string]string{"playlist:p1": "Same", "playlist:p2": "Order", "playlist:p3": "New"}
	if p := Compute(sel, "original", res, names, m, root, big); p.PlaylistsChanged != 3 || len(p.Add) != 0 {
		t.Fatalf("changed %d, add %d", p.PlaylistsChanged, len(p.Add))
	}
}

// A playlist recorded under a name longer than the Q2 keeps (issue #1) is an update in the preview,
// though its songs are the same: the sync moves it to a name that fits.
func TestPlanPlaylistLongNameChanged(t *testing.T) {
	a := song("s1", "One")
	m := manifest(entry(a, "original", 1000, "album:a1"))
	long := strings.Repeat("曲", 30)
	m.Playlists = map[string]string{long: "/x/_explaylist_data/" + long + ".m3u"}
	m.PlaylistSums = map[string]string{long: library.PlaylistSum([]string{m.Songs["s1"].Path})}
	sel := selection.Set{Items: []selection.Item{item("album", "a1"), item("playlist", "p1")}}
	res := map[string][]jellyfin.Song{"album:a1": {a}, "playlist:p1": {a}}
	if p := Compute(sel, "original", res, map[string]string{"playlist:p1": long}, m, root, big); p.PlaylistsChanged != 1 {
		t.Fatalf("changed %d", p.PlaylistsChanged)
	}
}

// A song resolved through its album (with the album's own artist) and through a playlist (with only
// its track artist, on a server without displayAlbumArtist) gets the album's artist whichever item
// comes first, so its folder doesn't depend on the selection's order (review G1, Minor 2).
func TestPlanPrefersAlbumArtist(t *testing.T) {
	inList, inAlbum := song("s1", "One"), song("s1", "One")
	inList.Artist, inList.AlbumArtist = "Artist 1", "Artist 1"
	inAlbum.Artist, inAlbum.AlbumArtist = "Artist 1", "Various Artists"
	res := map[string][]jellyfin.Song{"playlist:p1": {inList}, "album:a1": {inAlbum}}
	for _, items := range [][]selection.Item{
		{item("playlist", "p1"), item("album", "a1")},
		{item("album", "a1"), item("playlist", "p1")},
	} {
		p := Compute(selection.Set{Items: items}, "original", res, map[string]string{"playlist:p1": "Mix"}, manifest(), root, big)
		if len(p.Add) != 1 || p.Add[0].Song.AlbumArtist != "Various Artists" {
			t.Errorf("%v: %+v", items[0].Kind, p.Add)
		}
	}
}

// A held item keeps its songs on the card as they are and adds none of its missing ones; its
// playlist file lists what is on the card. Another item still adds a song they share.
func TestPlanHeldItem(t *testing.T) {
	on, missing, shared := song("s1", "One"), song("s2", "Two"), song("s3", "Three")
	held := item("favorites", "all")
	held.Held = true
	sel := selection.Set{Items: []selection.Item{held, item("album", "a2")}}
	res := map[string][]jellyfin.Song{"favorites:all": {on, missing, shared}, "album:a2": {shared}}
	m := manifest(entry(on, "original", 1000, "favorites:all"))
	p := Compute(sel, "original", res, map[string]string{"favorites:all": "Favourites"}, m, root, big)
	if len(p.Add) != 1 || p.Add[0].Song.ID != "s3" || len(p.Replace) != 0 || len(p.Remove) != 0 {
		t.Fatalf("add %+v replace %+v remove %+v", p.Add, p.Replace, p.Remove)
	}
	if got := p.Playlists["Favourites"]; len(got) != 2 || got[0] != m.Songs["s1"].Path || got[1] != p.Add[0].Path {
		t.Fatalf("playlist = %v", got)
	}
}

// Two selected playlists with one name, or one named like the Favourites item, each keep their own
// file, named the same way on every sync (audit #5).
func TestPlanSameNamePlaylistsKeptApart(t *testing.T) {
	a, b, c := song("s1", "One"), song("s2", "Two"), song("s3", "Three")
	sel := selection.Set{Items: []selection.Item{item("favorites", "all"), item("playlist", "p1"),
		item("playlist", "p2"), item("playlist", "p3")}}
	res := map[string][]jellyfin.Song{"favorites:all": {a}, "playlist:p1": {b}, "playlist:p2": {c}, "playlist:p3": {a, c}}
	names := map[string]string{"favorites:all": "Favourites", "playlist:p1": "Mix", "playlist:p2": "Mix", "playlist:p3": "Favourites"}
	path := func(s jellyfin.Song) string { return library.SongPath(root, s, "original", false) }
	want := map[string][]string{"Favourites": {path(a)}, "Mix": {path(b)}, "Mix (2)": {path(c)}, "Favourites (2)": {path(a), path(c)}}
	for i := 0; i < 2; i++ {
		if p := Compute(sel, "original", res, names, manifest(), root, big); !reflect.DeepEqual(p.Playlists, want) {
			t.Fatalf("playlists = %v", p.Playlists)
		}
	}
}

// A song given its " [id]" in a name clash takes its own name back once the clash is gone, and
// keeps the [id] while another song still has that name (audit #8).
func TestPlanIDSuffixDroppedAfterClash(t *testing.T) {
	a, b := song("s1", "Same"), song("s2", "Same")
	normal := library.SongPath(root, b, "original", false)
	eb := entry(b, "original", 1000, "album:a1")
	eb.Path = library.WithID(normal, "s2")
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}

	gone := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {b}}, nil, manifest(eb), root, big)
	if len(gone.Rename) != 1 || gone.Rename[0] != (Move{From: eb.Path, To: normal, ID: "s2"}) ||
		len(gone.Keep) != 1 || gone.Keep[0].Path != normal || len(gone.Add)+len(gone.Replace)+len(gone.Remove) != 0 {
		t.Fatalf("clash gone: %+v", gone)
	}

	still := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {a, b}}, nil, manifest(entry(a, "original", 1000, "album:a1"), eb), root, big)
	if len(still.Rename)+len(still.Add)+len(still.Replace)+len(still.Remove)+len(still.Keep) != 0 {
		t.Fatalf("clash still there: %+v", still)
	}

	// A new song with that name, after it in id order, gets the name; the suffixed one stays put.
	c := song("s3", "Same")
	added := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {b, c}}, nil, manifest(eb), root, big)
	if len(added.Rename) != 0 || len(added.Add) != 1 || added.Add[0].Path != normal {
		t.Fatalf("new song: %+v", added)
	}
}

// The same song in two formats (FLAC and MP3) lands in one folder as "x.flac" and "x.mp3". Their
// lyrics would both be "x.lrc", so the second format gets its [id] (audit S16).
func TestPlanSameNameDifferentFormatGetsID(t *testing.T) {
	flac, mp3 := song("s1", "Intro"), song("s2", "Intro")
	mp3.Suffix = "mp3"
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	p := Compute(sel, "original", map[string][]jellyfin.Song{"album:a1": {flac, mp3}}, nil, manifest(), root, big)
	lrc := map[string]string{}
	for _, w := range p.Add {
		lrc[w.Song.ID] = library.LyricsPath(w.Path)
	}
	if len(p.Add) != 2 || lrc["s1"] == lrc["s2"] {
		t.Fatalf("lyrics paths clash: %+v", lrc)
	}
	if lrc["s2"] != library.LyricsPath(library.WithID(library.SongPath(root, mp3, "original", false), "s2")) {
		t.Fatalf("s2 not suffixed: %q", lrc["s2"])
	}
}

// What the removals free counts towards fitting: a run short of space removes them first
// (audit 2026-11-10 #2).
func TestFitsCountsRemovals(t *testing.T) {
	a, b, old := song("s1", "One"), song("s2", "Two"), song("s9", "Nine")
	sel := selection.Set{Items: []selection.Item{item("album", "a1")}}
	res := map[string][]jellyfin.Song{"album:a1": {a, b}}
	p := Compute(sel, "original", res, nil, manifest(entry(old, "original", 5000, "song:s9")), root, 16<<20+1999)
	if len(p.Remove) != 1 || p.RemoveBytes != 5000 || !p.Fits {
		t.Fatalf("plan = %+v", p)
	}
}
