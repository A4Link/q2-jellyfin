package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// Browsing (playlists, albums, artists, search, an item's page) and choosing what syncs.

// withSyncNow puts the Sync Now row first on a list's first page (after its search box), pushing the last row to the next.
// "0" is the first page too: Previous page back to the first window asks for it.
func (d *daemon) withSyncNow(r q2spot.Reply, cursor string) q2spot.Reply {
	if cursor != "" && cursor != "0" || r.Status != "" {
		return r
	}
	row, ok := d.syncNowRow()
	if !ok {
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
		rows = append(d.listTop("playlists", s), v.item(selection.KindFavorites, "all", favName, "", 0))
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
	at     time.Time // when it started
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
	if f == nil || f.cl != cl || time.Since(f.at) > listFor { // an old outcome nobody took: ask again
		d.dropAlbumsFetch()
		bg, cancel := context.WithTimeout(context.Background(), everyAlbumIn)
		f = &albumsFetch{cl: cl, cancel: cancel, done: make(chan struct{}), at: time.Now()}
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
// it, so Add to Sync or Play pressed there asks the server nothing.
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
	sel, serr := d.selection()
	d.mu.Lock()
	if serr == nil {
		d.forgetUnsynced(sel, key)
	}
	if songs != nil {
		d.resolved[key] = songs
	}
	if kind == selection.KindArtist {
		d.artistAlbums[id] = albums
	}
	d.mu.Unlock()
	v := d.view()
	rows := []q2spot.Row{act("toggle", "Add to Sync", key)}
	// a press could neither add nor remove it (R30, M37); Favourites stays its own playlist (audit 7)
	if v.sel.Has(selection.KindLibrary, "all") && kind != selection.KindFavorites {
		rows[0] = info(viaLibrary(v.sel))
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

// forgetUnsynced drops the songs and albums fetched for items outside sel, but key's (the page
// being drawn) and Entire Library's, with its playlists' while it is chosen and its list fetched
// (the view reads a song of one as kept as the plan does): each page opened would otherwise stay
// in memory, and be walked by every view, until the app exits. Such an item then shows the status
// the card records, as after a restart. d.mu is held.
func (d *daemon) forgetUnsynced(sel selection.Set, key string) {
	keep := map[string]bool{key: true}
	_, lib := d.resolved[selection.Key(selection.KindLibrary, "all")] // with every playlist's songs: no page piles up
	lib = lib && sel.Has(selection.KindLibrary, "all")
	for _, it := range sel.Items {
		keep[selection.Key(it.Kind, it.ID)] = true
	}
	for k := range d.resolved {
		kind, _ := parseKey(k)
		if kind != selection.KindLibrary && !(kind == selection.KindPlaylist && lib) && !keep[k] {
			delete(d.resolved, k)
		}
	}
	for id := range d.artistAlbums {
		if !keep[selection.Key(selection.KindArtist, id)] {
			delete(d.artistAlbums, id)
		}
	}
}

// search lists what the server found, artists then albums then songs.
// ponytail: one request per kind shown, 50 of each; page past that when someone needs more.
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
	type found struct {
		ars   []jellyfin.Artist
		als   []jellyfin.Album
		songs []jellyfin.Song
	}
	// Kept for listFor, as latest: an Albums search's second reply (its albums and songs are more
	// than one) reuses the first's answer.
	f, err := cachedList(d, cl, "search "+in+"\t"+term, cursor == "", func() (found, error) {
		var f found
		var err error
		if in != "albums" { // Albums finds albums and songs; Artists, artists; Newly Added, all three
			if f.ars, err = cl.SearchArtists(ctx, term, 0); err != nil {
				return f, err
			}
		}
		if in != "artists" {
			f.als, f.songs, err = cl.Search(ctx, term, 0)
		}
		return f, err
	})
	if err != nil {
		return d.listErr(err, cursor)
	}
	v := d.view()
	var rows []q2spot.Row
	for _, a := range f.ars {
		rows = append(rows, v.item(selection.KindArtist, a.ID, a.Name, plural(a.AlbumCount, "album"), 0))
	}
	for _, a := range f.als {
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount))
	}
	for _, s := range f.songs {
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

// oneArtist gives an album's songs a's artist when they disagree on their album artist (getAlbum,
// songsOf's artist).
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
		// The albums from the list: only their songs to fetch (audit 2026-11-16 #15), 4 at a time.
		// As resolveLibrary's run: the slot is taken before the goroutine starts, and the first
		// failure (in time, so a cancelled album never hides it) stops the rest.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		got := make([][]jellyfin.Song, len(als))
		var wg sync.WaitGroup
		var mu sync.Mutex
		var first error
		sem := make(chan struct{}, 4) // as resolveItems
		for i, al := range als {
			sem <- struct{}{}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				s, e := cl.AlbumSongs(ctx, al.ID, false) // Play never starts an artist: no hearts
				if e != nil {
					mu.Lock()
					first = cmp.Or(first, e)
					mu.Unlock()
					cancel()
					return
				}
				oneArtist(al, s)
				got[i] = s
			}()
		}
		wg.Wait()
		if err := cmp.Or(first, ctx.Err()); err != nil {
			return nil, "", err
		}
		for _, s := range got {
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
	sel, r, done := d.toggleOff(cl, kind, id)
	if done {
		return r
	}
	if r, off := d.noWiFi(true); off {
		return r
	}
	var name string
	var n int
	var songs []jellyfin.Song // an album's or a song's, for coveredBy
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
		nm := shown.name
		if songs = shown.songs; !ok || songs == nil {
			var err error
			if songs, nm, err = d.songsOf(ctx, cl, kind, id); err != nil {
				return d.errToast(err)
			}
		}
		name, n = nm, len(songs)
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if via := d.coveredBy(sel, kind, id, songs); via != "" {
		return q2spot.Reply{Message: "Included via " + via}
	}
	// Loaded again after the fetches: another press may have saved it meanwhile.
	d.selMu.Lock()
	defer d.selMu.Unlock()
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, selDamaged)
	}
	again := ""
	if !sel.Has(kind, id) {
		sel.Toggle(selection.Item{Kind: kind, ID: id, Name: name})
		if err := d.saveSelection(sel); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the selection")
		}
		again = d.replan(cl)
	}
	return q2spot.Reply{Message: "Added to Sync · " + plural(n, "song") + again}
}

// toggleOff is what toggle does without the server: a removal, Entire Library, or a refusal, then
// done is true. Otherwise it returns the selection, for an item to add.
func (d *daemon) toggleOff(cl *jellyfin.Client, kind selection.Kind, id string) (sel selection.Set, r q2spot.Reply, done bool) {
	d.selMu.Lock()
	defer d.selMu.Unlock()
	sel, err := d.selection()
	switch {
	case err != nil:
		return sel, q2spot.Fail(q2spot.Error, selDamaged), true
	case kind != selection.KindLibrary && kind != selection.KindFavorites && sel.Has(selection.KindLibrary, "all"):
		return sel, q2spot.Reply{Message: viaLibrary(sel)}, true
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
		return sel, q2spot.Fail(q2spot.Error, "Couldn't save the selection"), true
	}
	r.Message += d.replan(cl)
	return sel, r, true
}

// viaLibrary is what an item's page and Add to Sync say under Entire Library. A held one adds
// nothing new, so a new album would never arrive (A7b-P2-6).
func viaLibrary(sel selection.Set) string {
	for _, it := range sel.Items {
		if it.Kind == selection.KindLibrary && it.Held {
			return "Entire Library is held: resume it in Sync Jobs"
		}
	}
	return "Included via Entire Library"
}

// coveredBy names a selected item that already holds this album or song (songs: its songs), or "".
// It asks the server nothing, so a press answers at once: an artist's albums or a list's songs not
// in memory (after a restart, until the Sync page or the item's own page fetches them) don't count,
// and the album or song is added on its own; the plan holds a song once (audit 2026-11-16 #16). A
// held item adds nothing new, so it covers an album or song only when every song of it is on the
// card.
func (d *daemon) coveredBy(sel selection.Set, kind selection.Kind, id string, songs []jellyfin.Song) string {
	var s jellyfin.Song
	if len(songs) > 0 {
		s = songs[0]
	}
	album := id
	if kind == selection.KindSong {
		album = s.AlbumID
	} else if kind != selection.KindAlbum {
		return ""
	}
	m := d.manifest()
	newSong := slices.ContainsFunc(songs, func(s jellyfin.Song) bool { return m.Songs[s.ID] == nil })
	for _, it := range sel.Items {
		switch {
		case it.Held && newSong: // covers nothing
		case it.Kind == selection.KindAlbum && it.ID == album:
			return it.Name
		case it.Kind == selection.KindArtist: // the albums fetched already (songsOf, toggle, its page)
			d.mu.Lock()
			als := d.artistAlbums[it.ID]
			d.mu.Unlock()
			for _, a := range als {
				if a.ID == album {
					return it.Name
				}
			}
		case (it.Kind == selection.KindPlaylist || it.Kind == selection.KindFavorites) && kind == selection.KindSong:
			d.mu.Lock()
			songs := d.resolved[selection.Key(it.Kind, it.ID)]
			d.mu.Unlock()
			for _, x := range songs {
				if x.ID == s.ID {
					return it.Name
				}
			}
		}
	}
	return ""
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
