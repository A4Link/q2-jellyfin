package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// Browsing (playlists, albums, artists, search, an item's page) and choosing what syncs.

// view is what status subtitles are read from: the selection, the manifest and the songs fetched,
// indexed in one pass per request, so a list of thousands of items never walks the manifest per
// row (I17).
// ponytail: built again per request (twice with Sync Now), about 25 ms per 40k songs on a laptop;
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
	syncQ := d.settings().def() // every chosen item's, but a held one's: as on the card, as the plan does
	for _, it := range sel.Items {
		v.held[selection.Key(it.Kind, it.ID)] = it.Held
		if it.Kind == selection.KindLibrary {
			v.lib, v.libHeld = true, it.Held
		}
	}
	suffix := map[string]string{} // of the fetched songs on the card
	for _, songs := range res {
		for _, s := range songs {
			if m.Songs[s.ID] == nil {
				continue
			}
			if v.albumOn[s.AlbumID] == nil {
				v.albumOn[s.AlbumID] = map[string]bool{}
			}
			v.albumOn[s.AlbumID][s.ID], suffix[s.ID] = true, s.Suffix
		}
	}
	for id, e := range m.Songs {
		want := "" // the best quality among the chosen items holding it, as the plan picks
		for _, k := range e.Items {
			v.keyed[k] = append(v.keyed[k], id)
			if !v.chosen(k) {
				v.pending[k] = true // taken out: its songs go at the next sync
			} else {
				q := syncQ
				// held, or a playlist a held Entire Library takes in: as on the card
				if held, own := v.held[k]; held || !own && v.libHeld {
					q = e.Quality
				}
				if quality.Better(q, want) {
					want = q
				}
			}
		}
		// An original the Q2 can't play syncs as quality.Fallback (library.Quality): not a change, nor
		// is one whose format isn't known here.
		if want == "" || e.Quality == want || e.Quality == library.Quality(want, suffix[id]) {
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
	selected := v.sel.Has(kind, id)
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
	} else if v.sel.Has(selection.KindSong, s.ID) {
		st = "Waiting to sync"
	}
	return q2spot.Row{Kind: "action", URI: "play", Title: s.Title, Subtitle: join(s.Artist, st),
		Context: fmt.Sprintf("song:%s %s %d", s.ID, listKey, i), Position: "toggle"}
}

// syncNowRow is the shortcut to Sync, present while the card differs from the selection: a chosen
// item not all on the Q2 or at another quality, or one taken out whose songs are still there (M35).
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
	if n == 0 {
		return q2spot.Row{}, false
	}
	return list("sync", "Sync Now", fmt.Sprintf("%d waiting", n), ""), true
}

// withSyncNow puts the Sync Now row first on a list's first page (after its search box), pushing the last row to the next.
func (d *daemon) withSyncNow(r q2spot.Reply, cursor string) q2spot.Reply {
	row, ok := d.syncNowRow()
	if !ok || cursor != "" || r.Status != "" {
		return r
	}
	if len(r.Rows) >= q2spot.MaxRows { // room for it: the page's last rows go to the next one
		next := len(r.Rows) // the row the next page starts at: the page's cursor, else its end
		if c, err := strconv.Atoi(r.Cursor); err == nil && r.Cursor != "" {
			next = c
		}
		cut := len(r.Rows) - (q2spot.MaxRows - 1)
		r.Rows, r.Cursor = r.Rows[:q2spot.MaxRows-1], strconv.Itoa(next-cut)
	}
	at := 0 // after a search box and a sort, which Q2 Pod shows in its title bar only when they come first
	if len(r.Rows) > 0 && r.Rows[0].Kind == "search" {
		at = 1
		if len(r.Rows) > 1 && r.Rows[1].URI == "sort" {
			at = 2
		}
	}
	r.Rows = slices.Insert(r.Rows, at, row)
	return r
}

// browse is a section's list in its sort s, read once for the request (sortNow).
func (d *daemon) browse(ctx context.Context, cl *jellyfin.Client, op, cursor string, s sortNow) q2spot.Reply {
	v := d.view()
	var rows []q2spot.Row
	fail := func(err error) q2spot.Reply { // a failed first page keeps its search and sort, so a sort
		r := d.listErr(err, cursor) // that keeps failing can be changed from it
		if cursor == "" {
			r.Rows = append(d.listTop(op, s), r.Rows...)
		}
		return r
	}
	switch op {
	case "albums": // paged by the server: one more than a page says whether More… follows
		// The cursor counts rows, the search box and sort at the top of the first page included, as
		// every paged list's does (withSyncNow moves it by the rows it cuts): album n is row n+2.
		row, _ := strconv.Atoi(cursor)
		var r q2spot.Reply
		top := d.listTop("albums", s)
		off, size := 0, q2spot.MaxRows-len(top)
		if row == 0 {
			r.Rows = top
		} else {
			off, size = row-len(top), q2spot.MaxRows
		}
		head := len(r.Rows)
		al, err := d.albums(ctx, cl, s, off, size+1)
		switch {
		case errors.Is(err, errLoading) && row == 0: // Random's albums still coming: live till they have
			return q2spot.Reply{Message: "refresh", Rows: append(r.Rows, info("Loading albums…"))}
		case errors.Is(err, errLoading):
			return q2spot.Fail(q2spot.Error, "Loading albums…")
		case err != nil:
			return fail(err)
		}
		more := len(al) > size
		if more {
			al = al[:size]
		}
		for _, a := range al {
			r.Rows = append(r.Rows, v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount))
		}
		if n := fit(r.Rows); n < len(r.Rows) {
			r.Rows, more = r.Rows[:n], true
		}
		if more {
			r.Cursor = strconv.Itoa(off + len(r.Rows) - head + len(top))
		}
		if len(al) == 0 && row == 0 {
			r.Rows = append(r.Rows, info("No albums"))
		}
		return r
	case "playlists": // Favourites first, as Liked Songs and My Collection are elsewhere
		key, order, rev := s.query("playlists")
		pls, err := cachedList(d, cl, key, false, func() ([]jellyfin.Playlist, error) { return cl.Playlists(ctx, order, rev) }) // audit B4
		if err != nil {
			return fail(err)
		}
		pls = shuffled(d, "playlists", s, pls)
		rows = append(d.listTop("playlists", s), list("favorites", favName, "", favKey))
		for _, p := range pls {
			rows = append(rows, v.item(selection.KindPlaylist, p.ID, p.Name, plural(p.SongCount, "song"), p.SongCount))
		}
		if len(pls) == 0 {
			rows = append(rows, q2spot.Row{Kind: "info", Title: "No playlists", Subtitle: "Make one in Jellyfin and it shows here"})
		}
	case "artists":
		key, order, rev := s.query("artists")
		ars, err := cachedList(d, cl, key, false, func() ([]jellyfin.Artist, error) { return cl.Artists(ctx, order, rev) }) // audit B4
		if err != nil {
			return fail(err)
		}
		ars = shuffled(d, "artists", s, ars)
		rows = append(rows, d.listTop("artists", s)...)
		for _, a := range ars {
			rows = append(rows, v.item(selection.KindArtist, a.ID, a.Name, plural(a.AlbumCount, "album"), 0))
		}
		if len(ars) == 0 {
			rows = append(rows, info("No artists"))
		}
	}
	return page(rows, cursor)
}

// albums is a page of Albums in its sort s, from the server (each page kept for listFor); Random is
// a shuffle of every album (everyAlbum), which the server can't page (its Random is new at each
// request), errLoading while every album is still coming.
func (d *daemon) albums(ctx context.Context, cl *jellyfin.Client, s sortNow, off, n int) ([]jellyfin.Album, error) {
	if s.opt != byRandom { // each page kept as the server sent it, for coming back (listFor)
		key, order, rev := s.query("albums")
		return cachedList(d, cl, fmt.Sprintf("%s %d %d", key, off, n), false, func() ([]jellyfin.Album, error) {
			return cl.Albums(ctx, off, n, order, rev)
		})
	}
	all, err := d.everyAlbum(ctx, cl)
	if err != nil {
		return nil, err
	}
	all = shuffled(d, "albums", s, all)
	return all[min(off, len(all)):min(off+n, len(all))], nil
}

// errLoading is a list still being fetched in the background when its reply is due.
var errLoading = errors.New("still loading")

// everyAlbumIn is how long fetching every album for Random may take.
const everyAlbumIn = 2 * time.Minute

// albumsFetch is every album being fetched for Random, by everyAlbum: done is closed once all and
// err are set; cancel stops it.
type albumsFetch struct {
	cl     *jellyfin.Client
	cancel context.CancelFunc
	done   chan struct{}
	all    []jellyfin.Album
	err    error
}

// everyAlbum is every album by name, for Random, kept for listFor (cachedList "albums"). It is
// fetched in the background, one fetch at a time, as a large library's (thousands of albums, 500 a
// request, each read on the Q2) can take longer than a reply may (answerIn): errLoading when ctx
// ends first. The fetch is kept until a reply has taken its outcome, so a failure is shown (with
// Search and Sort) rather than lost to a new fetch, and the ask after it tries again.
// ponytail: Random holds every album's row in memory (about 200 bytes each, 2 MB for 10,000).
func (d *daemon) everyAlbum(ctx context.Context, cl *jellyfin.Client) ([]jellyfin.Album, error) {
	d.mu.Lock()
	f := d.albumsAll
	if f == nil || f.cl != cl {
		d.dropAlbumsFetch()
		bg, cancel := context.WithTimeout(context.Background(), everyAlbumIn)
		f = &albumsFetch{cl: cl, cancel: cancel, done: make(chan struct{})}
		d.albumsAll = f
		d.spawn(func() {
			defer cancel()
			f.all, f.err = cachedList(d, cl, "albums", false, func() ([]jellyfin.Album, error) { return allAlbums(bg, cl) })
			close(f.done)
		})
	}
	d.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, errLoading
	}
	d.mu.Lock()
	if d.albumsAll == f { // taken: the next ask takes the kept list, or tries again
		d.albumsAll = nil
	}
	d.mu.Unlock()
	return f.all, f.err
}

// dropAlbumsFetch stops Random's album fetch, if any, and forgets it; d.mu is held.
func (d *daemon) dropAlbumsFetch() {
	if d.albumsAll != nil {
		d.albumsAll.cancel()
		d.albumsAll = nil
	}
}

// allAlbums is every album by name, 500 a request.
func allAlbums(ctx context.Context, cl *jellyfin.Client) ([]jellyfin.Album, error) {
	var all []jellyfin.Album
	seen := map[string]bool{}
	for {
		page, err := cl.Albums(ctx, len(all), 500, "name", false)
		if err != nil {
			return nil, err
		}
		n := len(all)
		for _, a := range page {
			if !seen[a.ID] {
				seen[a.ID] = true
				all = append(all, a)
			}
		}
		// A short page is the last; a page with nothing new is a server that ignores the offset (M10).
		if len(page) < 500 || len(all) == n {
			return all, nil
		}
	}
}

// listFor is how long a list is reused instead of fetched again, so going back to Newly Added,
// Albums (each page), Artists or Playlists within it asks the server nothing; a list changed on the
// server shows after it. The firmware asks for a window's replies one after another (and again
// after Now Playing or an artist's page), and the pages of one window count rows in the same list.
// A new sort drops the list's kept copies (dropLists), so it comes fresh. An item's page fetches
// afresh on its first page.
const listFor = 5 * time.Minute

// itemList is an item page's list, kept for its later pages and for Add to Sync on it (shownList):
// its name, and an artist's albums, else its songs.
type itemList struct {
	key, name string
	songs     []jellyfin.Song
	albums    []jellyfin.Album
}

// shownList is the item page's list while it is key's and fresh (listFor), as itemPage last fetched
// it, so Add to Sync pressed there asks the server nothing.
func (d *daemon) shownList(cl *jellyfin.Client, key string) (itemList, bool) {
	c := &d.lists
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.vals["item"].(itemList)
	if !ok || c.cl != cl || l.key != key || time.Since(c.at["item"]) >= listFor {
		return itemList{}, false
	}
	return l, true
}

type listCache struct {
	mu   sync.Mutex
	cl   *jellyfin.Client
	at   map[string]time.Time
	vals map[string]any
}

// cachedList is get's list for key: the one kept for cl when it is newer than listFor and fresh is
// false (a later page), else get's, then kept.
func cachedList[T any](d *daemon, cl *jellyfin.Client, key string, fresh bool, get func() (T, error)) (T, error) {
	c := &d.lists
	c.mu.Lock()
	if c.cl != cl {
		c.cl, c.at, c.vals = cl, map[string]time.Time{}, map[string]any{}
	}
	if v, ok := c.vals[key].(T); ok && !fresh && time.Since(c.at[key]) < listFor {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := get()
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	if c.cl == cl {
		for k, at := range c.at { // Albums' pages would otherwise pile up
			if time.Since(at) >= listFor {
				delete(c.at, k)
				delete(c.vals, k)
			}
		}
		c.at[key], c.vals[key] = time.Now(), v
	}
	c.mu.Unlock()
	return v, nil
}

// dropLists forgets list's kept copies (every sort and page of it), so it is fetched again.
func (d *daemon) dropLists(list string) {
	c := &d.lists
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.vals {
		if k == list || strings.HasPrefix(k, list+" ") {
			delete(c.at, k)
			delete(c.vals, k)
		}
	}
}

// searchTitles are the browse sections' search boxes, the first row of each (and of each later window).
var searchTitles = map[string]string{"albums": "Search Albums", "artists": "Search Artists", "playlists": "Search Playlists"}

// favKey is Favourites' selection key, and favName its name (the Sync, On This Q2 and .m3u title).
const (
	favKey  = string(selection.KindFavorites) + ":all"
	favName = "Favourites"
)

// itemPage is an artist's, album's, playlist's or Favourites' own page: Add/Remove, Play, then its
// contents.
func (d *daemon) itemPage(ctx context.Context, cl *jellyfin.Client, arg, cursor string) q2spot.Reply {
	kind, id := parseKey(arg)
	key := selection.Key(kind, id)
	switch kind {
	case selection.KindAlbum, selection.KindPlaylist, selection.KindArtist, selection.KindFavorites:
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	get := func() (itemList, error) {
		l := itemList{key: key}
		var err error
		switch kind {
		case selection.KindAlbum:
			var a jellyfin.Album
			a, l.songs, err = getAlbum(ctx, cl, id)
			l.name = a.Name
		case selection.KindPlaylist:
			var p jellyfin.Playlist
			p, l.songs, err = cl.Playlist(ctx, id)
			l.name = p.Name
		case selection.KindArtist:
			var a jellyfin.Artist
			a, l.albums, err = cl.Artist(ctx, id)
			l.name = a.Name
		case selection.KindFavorites:
			l.name = favName
			l.songs, err = cl.Favorites(ctx)
			if err == nil && len(l.songs) == 0 {
				l.songs = []jellyfin.Song{} // kept as resolved, and the page says why it's empty
			}
		}
		return l, err
	}
	// A later page reuses the list its first page fetched, as Artists does: one item's at a time.
	l, err := cachedList(d, cl, "item", cursor == "", get)
	if err == nil && l.key != key {
		l, err = cachedList(d, cl, "item", true, get)
	}
	if err != nil {
		return d.listErr(err, cursor)
	}
	songs, albums := l.songs, l.albums
	d.mu.Lock()
	if songs != nil {
		d.resolved[key] = songs
	}
	if kind == selection.KindArtist {
		d.artistAlbums[id] = albums
	}
	d.mu.Unlock()
	v := d.view()
	rows := []q2spot.Row{act("toggle", "Add to Sync", key)}
	if v.sel.Has(selection.KindLibrary, "all") { // a press could neither add nor remove it (R30, M37)
		rows[0] = info("Included via Entire Library")
	}
	if v.sel.Has(kind, id) && rows[0].Kind == "action" {
		rows[0].Title = "Remove from Sync"
	}
	if len(songs) > 0 {
		rows = append(rows, act("play", "Play", key+" 0"))
	}
	for i, s := range songs {
		rows = append(rows, v.song(s, key, i))
	}
	for _, a := range albums {
		year := ""
		if a.Year > 0 {
			year = strconv.Itoa(a.Year)
		}
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, year, a.SongCount))
	}
	if kind == selection.KindFavorites && len(songs) == 0 {
		rows = append(rows, info("No favourites yet"), info("Tap the heart on a song in Jellyfin"))
	}
	return page(rows, cursor)
}

// search lists what the server found, artists then albums then songs.
// ponytail: one search3 call, 50 of each kind; page past that when someone needs more.
func (d *daemon) search(ctx context.Context, cl *jellyfin.Client, term, cursor string) q2spot.Reply {
	d.mu.Lock()
	in := d.searchIn
	d.mu.Unlock()
	if in == "playlists" { // the music playlists whose name holds the term
		pls, err := cl.Playlists(ctx, "name", false)
		if err != nil {
			return d.listErr(err, cursor)
		}
		v := d.view()
		var rows []q2spot.Row
		for _, p := range pls {
			if strings.Contains(strings.ToLower(p.Name), strings.ToLower(strings.TrimSpace(term))) {
				rows = append(rows, v.item(selection.KindPlaylist, p.ID, p.Name, plural(p.SongCount, "song"), p.SongCount))
			}
		}
		if len(rows) == 0 {
			rows = []q2spot.Row{info("No playlists called that")}
		}
		return page(rows, cursor)
	}
	ars, als, songs, err := cl.Search(ctx, term, 0)
	if err != nil {
		return d.listErr(err, cursor)
	}
	switch in { // Albums finds albums and songs; Artists, artists
	case "albums":
		ars = nil
	case "artists":
		als, songs = nil, nil
	}
	v := d.view()
	var rows []q2spot.Row
	for _, a := range ars {
		rows = append(rows, v.item(selection.KindArtist, a.ID, a.Name, plural(a.AlbumCount, "album"), 0))
	}
	for _, a := range als {
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount))
	}
	for _, s := range songs {
		rows = append(rows, v.song(s, "song:"+s.ID, 0))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No results")}
	}
	return page(rows, cursor)
}

// getAlbum is cl.Album, with the album's artist for every song when its songs disagree on their
// album artist: a song without an AlbumArtist from the server carries only its own artist,
// so a compilation stays one folder, not one per track artist (M41). Songs that agree (a server's
// displayAlbumArtist, or one artist's album) keep theirs.
// ponytail: here, not in jellyfin.Client.Album, which another change owns; move it there later.
func getAlbum(ctx context.Context, cl *jellyfin.Client, id string) (jellyfin.Album, []jellyfin.Song, error) {
	a, songs, err := cl.Album(ctx, id)
	oneArtist(a, songs)
	return a, songs, err
}

// oneArtist gives an album's songs a's artist when they disagree on their album artist (getAlbum).
func oneArtist(a jellyfin.Album, songs []jellyfin.Song) {
	for _, s := range songs {
		if a.Artist != "" && s.AlbumArtist != songs[0].AlbumArtist {
			for i := range songs {
				songs[i].AlbumArtist = a.Artist
			}
			return
		}
	}
}

// songsOf fetches a selected item's songs (an artist's: every album's), and keeps them for the
// status subtitles.
func (d *daemon) songsOf(ctx context.Context, cl *jellyfin.Client, kind selection.Kind, id string) ([]jellyfin.Song, string, error) {
	var songs []jellyfin.Song
	var name string
	switch kind {
	case selection.KindPlaylist:
		p, s, err := cl.Playlist(ctx, id)
		if err != nil {
			return nil, "", err
		}
		songs, name = s, p.Name
	case selection.KindAlbum:
		a, s, err := getAlbum(ctx, cl, id)
		if err != nil {
			return nil, "", err
		}
		songs, name = s, a.Name
	case selection.KindArtist:
		a, als, err := cl.Artist(ctx, id)
		if err != nil {
			return nil, "", err
		}
		d.mu.Lock()
		d.artistAlbums[id] = als
		d.mu.Unlock()
		name = a.Name
		for _, al := range als {
			_, s, err := getAlbum(ctx, cl, al.ID)
			if err != nil {
				return nil, "", err
			}
			songs = append(songs, s...)
		}
	case selection.KindSong:
		s, err := cl.Song(ctx, id)
		if err != nil {
			return nil, "", err
		}
		songs, name = []jellyfin.Song{s}, s.Title
	case selection.KindFavorites:
		s, err := cl.Favorites(ctx)
		if err != nil {
			return nil, "", err
		}
		songs, name = s, favName
	default:
		return nil, "", errors.New("unknown kind " + string(kind))
	}
	if songs == nil {
		songs = []jellyfin.Song{}
	}
	d.mu.Lock()
	d.resolved[selection.Key(kind, id)] = songs
	d.mu.Unlock()
	return songs, name, nil
}

// toggle adds or removes an item; one already inside a selected item is left as it is.
func (d *daemon) toggle(ctx context.Context, cl *jellyfin.Client, arg string) q2spot.Reply {
	kind, id := parseKey(arg)
	sel, r, done := d.toggleOff(kind, id)
	if done {
		return r
	}
	if r, off := d.noWiFi(true); off {
		return r
	}
	var name string
	var n int
	var song jellyfin.Song
	// Pressed on the item's page just drawn (or a song's row there): its list, asking nothing.
	shown, ok := d.shownList(cl, selection.Key(kind, id))
	if f := strings.Fields(arg); kind == selection.KindSong && len(f) > 1 { // "song:id list:id i"
		l, _ := d.shownList(cl, f[1])
		for _, s := range l.songs {
			if s.ID == id {
				shown, ok = itemList{name: s.Title, songs: []jellyfin.Song{s}}, true
				d.mu.Lock()
				d.resolved[selection.Key(kind, id)] = shown.songs // as songsOf keeps them
				d.mu.Unlock()
				break
			}
		}
	}
	switch kind {
	case selection.KindArtist: // counted from its albums, without fetching each
		if !ok {
			a, als, err := cl.Artist(ctx, id)
			if err != nil {
				return d.errToast(err)
			}
			shown = itemList{name: a.Name, albums: als}
		}
		d.mu.Lock()
		d.artistAlbums[id] = shown.albums // for coveredBy, as songsOf keeps them
		d.mu.Unlock()
		name, n = shown.name, 0
		for _, al := range shown.albums {
			n += al.SongCount
		}
	case selection.KindAlbum, selection.KindPlaylist, selection.KindSong, selection.KindFavorites:
		songs, nm := shown.songs, shown.name
		if !ok || songs == nil {
			var err error
			if songs, nm, err = d.songsOf(ctx, cl, kind, id); err != nil {
				return d.errToast(err)
			}
		}
		name, n = nm, len(songs)
		if kind == selection.KindSong && len(songs) == 1 {
			song = songs[0]
		}
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if via := d.coveredBy(ctx, cl, sel, kind, id, song); via != "" {
		return q2spot.Reply{Message: "Included via " + via}
	}
	// Loaded again after the fetches: another press may have saved it meanwhile.
	d.selMu.Lock()
	defer d.selMu.Unlock()
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, selDamaged)
	}
	if !sel.Has(kind, id) {
		sel.Toggle(selection.Item{Kind: kind, ID: id, Name: name})
		if err := d.saveSelection(sel); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the selection")
		}
	}
	return q2spot.Reply{Message: "Added to Sync · " + plural(n, "song")}
}

// toggleOff is what toggle does without the server: a removal, Entire Library, or a refusal, then
// done is true. Otherwise it returns the selection, for an item to add.
func (d *daemon) toggleOff(kind selection.Kind, id string) (sel selection.Set, r q2spot.Reply, done bool) {
	d.selMu.Lock()
	defer d.selMu.Unlock()
	sel, err := d.selection()
	switch {
	case err != nil:
		return sel, q2spot.Fail(q2spot.Error, selDamaged), true
	case kind != selection.KindLibrary && sel.Has(selection.KindLibrary, "all"):
		return sel, q2spot.Reply{Message: "Included via Entire Library"}, true
	case sel.Has(kind, id):
		sel.Toggle(selection.Item{Kind: kind, ID: id})
		r = q2spot.Reply{Message: "Removed from Sync"}
	case kind == selection.KindLibrary:
		sel.Toggle(selection.Item{Kind: kind, ID: id, Name: "Entire Library"})
		r = q2spot.Reply{Message: "Added to Sync · Entire Library"}
	default:
		return sel, r, false
	}
	if err := d.saveSelection(sel); err != nil {
		r = q2spot.Fail(q2spot.Error, "Couldn't save the selection")
	}
	return sel, r, true
}

// coveredBy names a selected item that already holds this album or song, or "".
func (d *daemon) coveredBy(ctx context.Context, cl *jellyfin.Client, sel selection.Set, kind selection.Kind, id string, s jellyfin.Song) string {
	album := id
	if kind == selection.KindSong {
		album = s.AlbumID
	} else if kind != selection.KindAlbum {
		return ""
	}
	for _, it := range sel.Items {
		switch {
		case it.Kind == selection.KindAlbum && it.ID == album:
			return it.Name
		case it.Kind == selection.KindArtist: // the albums fetched already (songsOf), else asked
			d.mu.Lock()
			als, ok := d.artistAlbums[it.ID]
			d.mu.Unlock()
			if !ok {
				var err error
				if _, als, err = cl.Artist(ctx, it.ID); err != nil {
					// Out of time or the server failed: added as asked; the plan holds a song once.
					d.log.Printf("included via: %v", err)
					return ""
				}
				d.mu.Lock()
				d.artistAlbums[it.ID] = als
				d.mu.Unlock()
			}
			for _, a := range als {
				if a.ID == album {
					return it.Name
				}
			}
		case (it.Kind == selection.KindPlaylist || it.Kind == selection.KindFavorites) && kind == selection.KindSong:
			d.mu.Lock()
			songs, ok := d.resolved[selection.Key(it.Kind, it.ID)]
			d.mu.Unlock()
			if !ok {
				songs, _, _ = d.songsOf(ctx, cl, it.Kind, it.ID)
			}
			for _, x := range songs {
				if x.ID == s.ID {
					return it.Name
				}
			}
		}
	}
	return ""
}

// play streams a list from an index ("kind:id index", after a song row's own "song:<id>"). A song
// that plays at once gets no message: Q2 Pod toasts a message full-screen before Now Playing opens.
func (d *daemon) play(ctx context.Context, cl *jellyfin.Client, arg string) q2spot.Reply {
	r, title, ok := d.startPlay(ctx, cl, arg)
	if !ok {
		return r
	}
	return d.playing(title)
}

// playing is a Play's reply once its song title is started: "Buffering …" while it waits.
func (d *daemon) playing(title string) q2spot.Reply {
	if st, waiting := d.waitingNow(); waiting && st.title == title {
		return q2spot.Reply{Message: "Buffering " + title + "…"}
	}
	return q2spot.Reply{}
}

// startPlay starts the list and song arg names ("[song:id] <list kind:id> <index>"), reporting the
// song's title, or a failed reply.
func (d *daemon) startPlay(ctx context.Context, cl *jellyfin.Client, arg string) (q2spot.Reply, string, bool) {
	f := strings.Fields(arg)
	if len(f) == 3 {
		f = f[1:]
	}
	if len(f) != 2 {
		return q2spot.Fail(q2spot.Bad, "Bad request"), "", false
	}
	i, _ := strconv.Atoi(f[1])
	kind, id := parseKey(f[0])
	songs := d.pageSongs(cl, selection.Key(kind, id)) // the list the page showed, if fresh (audit B3)
	if songs == nil {
		var err error
		if songs, _, err = d.songsOf(ctx, cl, kind, id); err != nil {
			return d.errToast(err), "", false
		}
	}
	if i < 0 || i >= len(songs) {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play"), "", false
	}
	d.startSongs(songs, i, f[0])
	return q2spot.Reply{}, songs[i].Title, true
}

// startSongs plays songs from i, of the list key ("kind:id"; status.go shows its song waiting).
func (d *daemon) startSongs(songs []jellyfin.Song, i int, key string) {
	// R27: starting stops the last stream, which may be mid-fetch; reply now, start behind it.
	d.mu.Lock()
	d.playSeq++
	seq := d.playSeq
	d.playKey = key
	d.mu.Unlock()
	// status.go: the list shows this song waiting until it plays; one on the card plays at once.
	if e := d.manifest().Songs[songs[i].ID]; d.caps.URLs || d.caps.Playlist && d.stream.synced(e) {
		// URL queues (stream_urls.go): Now Playing shows the firmware's own buffering
		d.stream.setStatus(streamStatus{})
	} else {
		d.stream.setStatus(streamStatus{seq: -1, index: i, id: songs[i].ID, title: songs[i].Title, at: time.Now()})
	}
	d.spawn(func() {
		d.playMu.Lock()
		defer d.playMu.Unlock()
		d.mu.Lock()
		latest := seq == d.playSeq
		d.mu.Unlock()
		if latest {
			d.stream.start(songs, i)
		}
	})
}

// pageSongs is the songs item key's page fetched, within listFor: what the row pressed was counted
// in, without asking the server for the whole list again. Nil when the page's list is another
// item's, an artist's albums, or older.
func (d *daemon) pageSongs(cl *jellyfin.Client, key string) []jellyfin.Song {
	c := &d.lists
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.vals["item"].(itemList)
	if c.cl != cl || !ok || l.key != key || l.songs == nil || time.Since(c.at["item"]) >= listFor {
		return nil
	}
	return l.songs
}

// onQ2Subtitle is On This Q2's subtitle: how many items are chosen, and the bytes on the card. sel
// and err are the selection syncSources already read.
func (d *daemon) onQ2Subtitle(sel selection.Set, err error) string {
	if err != nil {
		return "Selection file is damaged"
	}
	var bytes int64
	for _, e := range d.manifest().Songs {
		bytes += e.Size
	}
	return plural(len(sel.Items), "item") + " · " + size(bytes)
}

// latest is Newly Added: the albums most recently added to the music libraries, newest first.
func (d *daemon) latest(ctx context.Context, cl *jellyfin.Client, cursor string) q2spot.Reply {
	// Kept for listFor: its second page (the reply is split in two, audit B10) and coming back reuse it.
	al, err := cachedList(d, cl, "latest", false, func() ([]jellyfin.Album, error) { return cl.Latest(ctx) })
	if err != nil {
		return d.listErr(err, cursor)
	}
	v := d.view()
	rows := []q2spot.Row{searchRow("Search")} // artists, albums and songs
	for _, a := range al {
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount))
	}
	if len(al) == 0 {
		rows = append(rows, info("Nothing added yet"))
	}
	return page(rows, cursor) // the search box and 50 albums are more than one reply
}

// searchRow is a section's search box, at its top.
func searchRow(title string) q2spot.Row { return q2spot.Row{Kind: "search", Title: title} }

// searchSections are the ops whose page has a search box, and what text typed in it searches. Q2
// Pod's "search" request names no section, so the box is the page asked for last (device). It is
// kept when the request comes, not when its reply is made: Q2 Pod asks for one page at a time and
// drops the one before, so a slow Artists the user swiped away from, finishing after Albums
// answered, would otherwise make a search typed on Albums search Artists. The sign-in pages' boxes
// (searchLogin) are never a search: they may hold a password.
var searchSections = map[string]string{"latest": "all", "albums": "albums", "artists": "artists",
	"playlists": "playlists", "server": fieldServer, "account": searchLogin, "pwlogin": searchLogin,
	"keylogin": searchLogin}

const searchLogin = "login"

// searchAgain answers a text typed before any box was offered since the app started (device).
// Return asks for the page with the box again, and the next text typed in it is searched.
const searchAgain = "Press Return and search again"
