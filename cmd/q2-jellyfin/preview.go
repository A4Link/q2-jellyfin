package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/selection"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

type preview struct {
	gen    int
	at     time.Time
	done   bool
	plan   *plan.Plan
	err    error
	gone   []selection.Item   // chosen items the server no longer has: out of Sync at Start Sync
	cancel context.CancelFunc // stops its fetches (dropPreview)
}

// startPreview starts computing the preview unless a fresh one exists, and reports whether it did,
// with the preview, which a background start may replace as soon as d.mu is let go.
func (d *daemon) startPreview(cl *jellyfin.Client) (*preview, bool) {
	d.mu.Lock()
	p := d.preview
	if p != nil && p.gen == d.gen && !(p.done && time.Since(p.at) > previewTTL) {
		d.mu.Unlock()
		return p, false
	}
	d.dropPreview()
	d.runner.Forget() // an ended run's plan; the new preview's Start Sync fetches its failed songs too
	// No overall deadline: each request has its own limits, and dropPreview stops the check.
	ctx, cancel := context.WithCancel(context.Background())
	p = &preview{gen: d.gen, at: time.Now(), cancel: cancel}
	d.preview = p
	d.mu.Unlock()
	d.spawn(func() { defer cancel(); d.computePreview(ctx, cl, p) })
	return p, true
}

// dropPreview forgets the preview, stopping its fetches if it is still at work (M11). d.mu is held.
func (d *daemon) dropPreview() {
	if d.preview != nil && d.preview.cancel != nil {
		d.preview.cancel()
	}
	d.preview = nil
}

// batched is what songBatch looked up together: single songs, albums and playlists, by id.
type batched struct {
	songs     map[string]jellyfin.Song
	albums    map[string]jellyfin.Album
	playlists map[string]jellyfin.Playlist
}

// songBatch looks up the single songs among items together, and the albums and playlists together
// (audit 0.8.1 round 3 #14, round 4 #12). A failure or an item left out is left to the item's own
// lookup in songsOf, so only that lookup's 404 makes it gone.
func songBatch(ctx context.Context, cl *jellyfin.Client, items []selection.Item) batched {
	var songs, lists []string
	for _, it := range items {
		switch it.Kind {
		case selection.KindSong:
			songs = append(songs, it.ID)
		case selection.KindAlbum, selection.KindPlaylist:
			lists = append(lists, it.ID)
		}
	}
	var b batched
	if len(songs) > 1 { // one item: its own lookup is the one request
		b.songs, _ = cl.Songs(ctx, songs)
	}
	if len(lists) > 1 {
		b.albums, b.playlists, _ = cl.AlbumsAndPlaylists(ctx, lists)
	}
	return b
}

// itemSongs is songsOf, but an item songBatch answered needs no item request of its own.
func (d *daemon) itemSongs(ctx context.Context, cl *jellyfin.Client, it selection.Item, b batched) ([]jellyfin.Song, string, error) {
	var songs []jellyfin.Song
	var name string
	var err error
	s, isSong := b.songs[it.ID]
	a, isAlbum := b.albums[it.ID]
	p, isPlaylist := b.playlists[it.ID]
	switch {
	case it.Kind == selection.KindSong && isSong:
		songs, name = []jellyfin.Song{s}, s.Title
	case it.Kind == selection.KindAlbum && isAlbum:
		songs, err = cl.AlbumSongs(ctx, it.ID, false)
		oneArtist(a, songs) // as getAlbum does (M41)
		name = a.Name
	case it.Kind == selection.KindPlaylist && isPlaylist:
		songs, err = cl.PlaylistSongs(ctx, it.ID, false)
		name = p.Name
	default:
		return d.songsOf(ctx, cl, it.Kind, it.ID)
	}
	if err != nil {
		return nil, "", err
	}
	if songs == nil {
		songs = []jellyfin.Song{}
	}
	d.mu.Lock()
	d.resolved[selection.Key(it.Kind, it.ID)] = songs // as songsOf keeps it
	d.mu.Unlock()
	return songs, name, nil
}

// computePreview plans the sync (planPreview) and records the preview in p, then starts the sync
// for Sync Favourites and backfills lyrics. ctx ends with the preview (dropPreview).
func (d *daemon) computePreview(ctx context.Context, cl *jellyfin.Client, p *preview) {
	pl, resolved, gone, err := d.planPreview(ctx, cl)
	d.mu.Lock()
	p.done, p.plan, p.err, p.gone, p.at = true, &pl, err, gone, time.Now()
	auto := d.autoStart && p == d.preview && d.cl == cl // never for an account signed out meanwhile
	again := auto && p.gen != d.gen                     // changed again meanwhile: start from a new preview
	if auto && !again {
		d.autoStart = false
	}
	d.mu.Unlock()
	if again {
		d.startPreview(cl)
		return
	}
	if err != nil {
		return
	}
	// Sync Favourites: start without a second press, but never to take songs off the card: that
	// waits for Start Sync, with the preview to show what goes (audit 2026-11-16 #3).
	if auto && pl.Changes() && len(pl.Remove) == 0 {
		if r := d.syncAction("start"); r.Message != "" {
			d.log.Printf("sync favourites: %s", r.Message)
		}
	}
	bctx, bcancel := context.WithTimeout(ctx, 10*time.Minute) // after the preview shows: a request per song
	d.backfillLyrics(bctx, resolved, pl)
	bcancel()
}

// planPreview resolves the selection on the server (resolveItems) and plans it against the card.
// Any failure fails the preview: a missing item's songs would otherwise read as removed. The
// exception is an item the server says it no longer has (deleted there): it is planned as having
// no songs, so those only it wanted are removed, and it leaves Sync with Start Sync (dropGone). The
// manifest is read failed or not: the pages share the copy it keeps (onQ2Subtitle).
func (d *daemon) planPreview(ctx context.Context, cl *jellyfin.Client) (plan.Plan, map[string][]jellyfin.Song, []selection.Item, error) {
	// The last Entire Library list (and its playlists' but those chosen on their own) goes first: a big
	// one would be live beside the new one for the whole download, or kept after Entire Library is
	// switched off. resolveLibrary sets them again.
	sel, err := d.selection()
	d.mu.Lock()
	if _, ok := d.resolved[selection.Key(selection.KindLibrary, "all")]; ok {
		for k := range d.resolved {
			if kind, id := parseKey(k); kind == selection.KindLibrary || kind == selection.KindPlaylist && !sel.Has(kind, id) {
				delete(d.resolved, k)
			}
		}
	}
	d.mu.Unlock()
	if err != nil {
		d.manifestErr()
		return plan.Plan{}, nil, nil, err
	}
	set, err := d.settingsErr() // not the defaults: a sync at Original would replace every Opus song
	if err != nil {
		d.log.Printf("settings: %v", err)
		d.manifestErr()
		return plan.Plan{}, nil, nil, errors.New(settingsDamaged)
	}
	resolved, names, gone, err := d.resolveItems(ctx, cl, &sel)
	if err == nil && len(gone) > 0 { // or the account lost its music library: nothing goes then
		err = cl.CheckMusic(ctx)
	}
	var ae *jellyfin.APIError // a list of a library the account lost answers 401 (403 from a proxy)
	if errors.As(err, &ae) && (ae.HTTP == http.StatusUnauthorized || ae.HTTP == http.StatusForbidden) {
		if merr := cl.CheckMusic(ctx); errors.Is(merr, jellyfin.ErrNoMusic) {
			err = merr
		}
	}
	man, merr := d.manifestErr() // Sync Favourites and a hold start one with no Sync page to stop it
	if err = cmp.Or(err, merr); err != nil {
		return plan.Plan{}, nil, nil, err
	}
	sel.Items = slices.DeleteFunc(sel.Items, func(it selection.Item) bool { return isGone(gone, it) })
	// Songs deleted from the card are planned again, not taken as there (I9). The plan reads
	// the manifest again below: DropMissing may have changed it.
	if derr := d.runner.DropMissing(man); derr != nil && !errors.Is(derr, syncer.ErrBusy) {
		d.log.Printf("sync: %v", derr)
	}
	pl := plan.Compute(sel, set.def(), set.reencode(), resolved, names, d.manifest(), d.card)
	// The items holding songs already on the card, recorded now: an up-to-date preview starts
	// no sync, and status reads them after a restart (I12). A running sync records its own.
	if rerr := d.runner.Refresh(pl.Keep); rerr != nil && !errors.Is(rerr, syncer.ErrBusy) {
		d.log.Printf("sync: refresh: %v", rerr)
	}
	if !pl.Changes() {
		d.dropGone(gone) // nothing on the Q2 is left to remove for them: no Start Sync to wait for
	}
	return pl, resolved, gone, nil
}

// resolveItems resolves every item of sel to its songs, 4 at a time; Entire Library adds its
// playlists to sel. The first failure cancels the rest. An item the server no longer has (but
// Favourites) is in gone, with no songs.
func (d *daemon) resolveItems(ctx context.Context, cl *jellyfin.Client, sel *selection.Set) (resolved map[string][]jellyfin.Song, names map[string]string, gone []selection.Item, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resolved, names = map[string][]jellyfin.Song{}, map[string]string{}
	var mu sync.Mutex // guards resolved, names, gone, err and sel.Items
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	items := sel.Items
	got := songBatch(ctx, cl, items)
	for _, it := range items {
		wg.Add(1)
		go func(it selection.Item) {
			defer wg.Done()
			if it.Kind == selection.KindLibrary {
				// Holds no slot itself: its requests take them (an artist item has 4 more of its own).
				lib, e := d.resolveLibrary(ctx, cl, sem)
				mu.Lock()
				defer mu.Unlock()
				if e != nil && err == nil {
					err = e
					cancel()
				}
				key := selection.Key(it.Kind, it.ID)
				resolved[key] = lib.songs
				for _, p := range lib.playlists {
					pk := selection.Key(selection.KindPlaylist, p.id)
					if sel.Has(selection.KindPlaylist, p.id) {
						continue // chosen on its own: that item's songs (and hold) stand
					}
					sel.Items = append(sel.Items, selection.Item{Kind: selection.KindPlaylist, ID: p.id, Name: p.name,
						Held: it.Held}) // held with it
					resolved[pk], names[pk] = p.songs, p.name
				}
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			songs, name, e := d.itemSongs(ctx, cl, it, got)
			key := selection.Key(it.Kind, it.ID)
			mu.Lock()
			defer mu.Unlock()
			var ae *jellyfin.APIError
			if it.Kind != selection.KindFavorites && errors.As(e, &ae) && ae.Code == jellyfin.CodeGone {
				gone, e = append(gone, it), nil
				d.mu.Lock() // fetched and empty: Browse sees its songs go, as the plan does (A7b-P5-1)
				d.resolved[key] = []jellyfin.Song{}
				d.mu.Unlock()
			}
			if e != nil && err == nil {
				err = e
				cancel()
			}
			resolved[key], names[key] = songs, name
		}(it)
	}
	wg.Wait()
	return resolved, names, gone, err
}

// dropGone takes the items the server no longer has out of Sync. They had no songs in the plan, so
// the plan stays as it is: no new preview.
func (d *daemon) dropGone(gone []selection.Item) {
	if len(gone) == 0 {
		return
	}
	d.selMu.Lock()
	defer d.selMu.Unlock()
	sel, err := d.selection()
	if err == nil {
		sel.Items = slices.DeleteFunc(sel.Items, func(it selection.Item) bool { return isGone(gone, it) })
		err = sel.Save(d.selPath())
	}
	if err != nil {
		d.log.Printf("sync: selection: %v", err)
	}
}

func isGone(gone []selection.Item, it selection.Item) bool {
	return slices.ContainsFunc(gone, func(g selection.Item) bool { return g.Kind == it.Kind && g.ID == it.ID })
}

// previewGone is the items the current preview found deleted on the server.
func (d *daemon) previewGone() []selection.Item {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p := d.preview; p != nil && p.err == nil {
		return p.gone
	}
	return nil
}

type libPlaylist struct {
	id, name string
	songs    []jellyfin.Song
}

type libResult struct {
	songs     []jellyfin.Song
	playlists []libPlaylist
}

const maxAlbums = 100_000 // what Entire Library lists at most: past that the server is broken, or the Q2 too small

// resolveLibrary lists every album and every song (500 a page) and every playlist, then fetches
// each playlist's songs. Every request takes a slot of sem, so the library never runs more than the
// preview's 4 at once. Albums keep their listing order; so do playlists.
func (d *daemon) resolveLibrary(ctx context.Context, cl *jellyfin.Client, sem chan struct{}) (libResult, error) {
	ctx, cancel := context.WithCancel(ctx) // the first failure stops the queued fetches
	defer cancel()
	slot := func() func() { sem <- struct{}{}; return func() { <-sem } }
	var albums []jellyfin.Album
	seen := map[string]bool{}
	for off := 0; ; {
		release := slot()
		page, err := cl.Albums(ctx, off, 500, "name", false)
		release()
		if err != nil {
			return libResult{}, err
		}
		n := len(albums)
		for _, a := range page {
			if !seen[a.ID] {
				seen[a.ID] = true
				albums = append(albums, a)
			}
		}
		// A short page is the last; a page with nothing new is a server that ignores the offset (M10).
		if len(page) < 500 || len(albums) == n {
			break
		}
		if len(albums) > maxAlbums {
			return libResult{}, fmt.Errorf("The library has more than %d albums", maxAlbums)
		}
		off += len(page)
	}
	release := slot()
	all, err := cl.AllSongs(ctx) // every album's songs, 500 a request
	release()
	if err != nil {
		return libResult{}, err
	}
	// In the albums' order, as getAlbum gave them, sorted in place (one copy of a big library). A
	// song whose album the album pages missed (they shifted mid-fetch, audit S14) is kept and sorted
	// last, so a library scan on the server doesn't make the sync delete songs already on the card. A
	// song with no album at all (a loose track) is sorted last the same way (review A5-P1-8).
	at := make(map[string]int, len(albums))
	for i, a := range albums {
		at[a.ID] = i
	}
	pos := func(id string) int {
		if i, ok := at[id]; ok {
			return i
		}
		return len(albums)
	}
	slices.SortStableFunc(all, func(x, y jellyfin.Song) int { return cmp.Compare(pos(x.AlbumID), pos(y.AlbumID)) })
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].AlbumID == all[i].AlbumID {
			j++
		}
		if k, ok := at[all[i].AlbumID]; ok { // skip oneArtist for a song whose album wasn't listed
			oneArtist(albums[k], all[i:j])
		}
		i = j
	}
	release = slot()
	pls, err := cl.Playlists(ctx, "name", false)
	release()
	if err != nil {
		return libResult{}, err
	}
	res := libResult{playlists: make([]libPlaylist, len(pls))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	// The slot is taken before the goroutine starts: fetches run in order, a failure stops the
	// rest at once, and a large library never parks thousands of goroutines on the semaphore.
	run := func(f func() error) {
		release := slot()
		if ctx.Err() != nil {
			release()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer release()
			if e := f(); e != nil {
				mu.Lock()
				first = cmp.Or(first, e)
				mu.Unlock()
				cancel()
			}
		}()
	}
	for i, p := range pls {
		run(func() (err error) {
			res.playlists[i].songs, err = cl.PlaylistSongs(ctx, p.ID, false) // the list named it: no item request
			res.playlists[i].id, res.playlists[i].name = p.ID, p.Name
			return
		})
	}
	wg.Wait()
	if first != nil {
		return libResult{}, first
	}
	res.songs = all
	d.mu.Lock()
	d.resolved[selection.Key(selection.KindLibrary, "all")] = res.songs
	for _, p := range res.playlists { // the view reads a song of one as kept, or not, as the plan does
		d.resolved[selection.Key(selection.KindPlaylist, p.id)] = p.songs
	}
	d.libCount, d.libKnown = [2]int{len(albums), len(pls)}, true
	d.mu.Unlock()
	return res, nil
}

// backfillLyrics gives a song already on the card, whose lyrics the server has, the .lrc it lacks:
// songs synced before the app wrote lyrics (0.1), or whose lyrics were added on the server since.
// Songs the plan moves, replaces or removes are left to the sync. A song is asked for once a run
// (lyricsTried): one the server has no lyrics for after all would be asked at every preview. It is
// marked before the request, so an overlapping backfill doesn't ask too, and unmarked when no
// answer came (the preview dropped, the server out of reach), so a later preview asks again.
func (d *daemon) backfillLyrics(ctx context.Context, resolved map[string][]jellyfin.Song, pl plan.Plan) {
	busy := pl.Moving()
	m := d.manifest()
	for _, songs := range resolved {
		for _, s := range songs {
			e := m.Songs[s.ID]
			if !s.HasLyrics || e == nil || busy[s.ID] || ctx.Err() != nil || exists(library.LyricsPath(e.Path)) {
				continue
			}
			d.mu.Lock()
			tried := d.lyricsTried[s.ID]
			d.lyricsTried[s.ID] = true
			d.mu.Unlock()
			if tried {
				continue
			}
			err := syncer.WriteLyrics(ctx, src{d}, s, e.Path, d.log.Printf)
			var ae *jellyfin.APIError
			if ctx.Err() != nil || errors.Is(err, jellyfin.ErrUnreachable) && !(errors.As(err, &ae) && ae.HTTP != 0) {
				d.mu.Lock()
				delete(d.lyricsTried, s.ID)
				d.mu.Unlock()
			}
		}
	}
}
