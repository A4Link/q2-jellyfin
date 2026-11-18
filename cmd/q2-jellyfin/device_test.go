package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

// nav is a Jellyfin stand-in: albums al0..alN-1 ("Album 000"…) by "Artist", each with songsPer
// songs al<i>.<j>; playlist pl1 "Mix" holds al0's songs; artist ar1 has al0 and al1.
type nav struct {
	albumArtist      string // the albums' own artist, when not "Artist"
	albums, songsPer int
	stall            bool          // every request hangs until the client gives up
	errCode          int           // every API call fails: 40 answers 401, any other 404
	block            chan struct{} // downloads wait for it
	failID           string        // getAlbum fails for this album only (HTTP 502); every-song listings fail
	goneID           string        // an album deleted on the server: getAlbum and its songs request answer 404
	allGone          bool          // as goneID, for every album
	rootsGone        bool          // the music library's folder listing answers 404
	albumReqs        atomic.Int32  // getAlbum calls served
	playlists        int           // playlists pl1..plN ("Mix", "Mix 2"…); 0 means 1. pl<k> holds al<k-1>'s songs
	empty            bool          // every list reply is empty
	noMusic          bool          // the account has no music library: /UserViews lists only films
	libLost          atomic.Bool   // as noMusic, and lists of the kept library "music" answer 401
	lastRange        atomic.Value  // the Range header of the latest download, a string
	various          bool          // albums are compilations: album artist "Various Artists", song j by "Artist j"
	mediaErr         int           // download and stream answer 404 (or 401 for 40)
	dup              bool          // each playlist holds its first song twice, last too
	artists          int           // getArtists lists this many more artists, ar2.., with no albums
	listErr          int           // getAlbumList2 fails with this code from offset 50 on
	sameAlbums       bool          // getAlbumList2 ignores offset: every page is the first
	holdAlbum        chan struct{} // getAlbum waits for it
	favs             int           // the first favs songs of al0 are the user's favourites
	albumGaveUp      atomic.Int32  // getAlbum requests the client cancelled while waiting
	images           atomic.Int32  // cover requests served (albums al* have one; anything else 404s)
	artistReqs       atomic.Int32  // /Artists/AlbumArtists calls served
	artistItemReqs   atomic.Int32  // /Items/ar<id> calls served (cl.Artist)
	songListReqs     atomic.Int32  // every-song listing pages served (cl.AllSongs)
	onSongList       func()        // optional: called as each every-song listing page is asked for
	searchReqs       atomic.Int32  // search requests served (a searchTerm), artists' and items'
	lyrics           atomic.Bool   // songs have lyrics on the server
	lyricsHold       chan struct{} // lyrics requests wait for it
	albumSort        atomic.Value  // the latest album list's "sortBy sortOrder", a string
	artistSort       atomic.Value  // the same for the artist list
	playlistSort     atomic.Value  // and the playlist list
	holdList         chan struct{} // album list requests wait for it
	listDelay        time.Duration // each album list request takes this long (a slow server)
	holdArtists      chan struct{} // artist list requests wait for it
	songArt          bool          // songs carry their album's cover (AlbumPrimaryImageTag)
	coverStatus      int           // covers answer this HTTP status instead
	coverSize        int           // covers are this many bytes
	albumLists       atomic.Int32  // album list requests served (Albums, Newly Added)
	rawStatus        atomic.Int32  // when set, every request answers this HTTP status (a proxy's 502)
	pings            atomic.Int32  // /Users/Me requests served
	hideAlbum        string        // an album left out of the album list pages, its songs still listed
	lyricsReqs       sync.Map      // song id -> how many lyrics requests for it, an *atomic.Int32
	reqs             atomic.Int32  // every request served
	reportMu         sync.Mutex
	reports          []string     // play reports: "played <id> <datePlayed>", "start <id>", "stop <id> <ticks>", "progress <id> <sec> <paused>"
	reportStatus     atomic.Int32 // when set, play reports answer this HTTP status
	onReport         func()       // optional: called as each play report or favourite arrives
	dropReply        atomic.Bool  // a play marked is counted, then the connection cut: no answer
	cutBody          atomic.Bool  // a play marked is counted, answered 200, and its body cut short
	lastPlayed       sync.Map     // song id -> the datePlayed last marked, its UserData.LastPlayedDate
	hearts           sync.Map     // song id -> its favourite state as last set (/UserFavoriteItems), over favs
	favStatus        atomic.Int32 // when set, /UserFavoriteItems answers this HTTP status
	songReqs         atomic.Int32 // /Items/al<i>.<j> calls served (cl.Song)
	batchReqs        atomic.Int32 // /Items?ids= calls served (cl.Songs, cl.AlbumsAndPlaylists)
	leaveOut         string       // an item /Items?ids= leaves out of its answer
	playlistReqs     atomic.Int32 // /Items/pl<k> calls served (cl.Playlist)
	userDataSent     sync.Map     // "<list> <enableUserData sent>" -> true, for song and album lists (userDataKey)
}

// reported is the play reports served so far.
func (n *nav) reported() []string {
	n.reportMu.Lock()
	defer n.reportMu.Unlock()
	return append([]string(nil), n.reports...)
}

// jsong is song j of album al in Jellyfin's BaseItemDto shape. Like the Navidrome stand-in this
// replaced, a song names only its own artist (no AlbumArtist), so compilations are told apart by
// the album's artist.
func (n *nav) song(al, j int) map[string]any {
	artist := "Artist"
	if n.various {
		artist = fmt.Sprintf("Artist %d", j)
	}
	return map[string]any{"Id": fmt.Sprintf("al%d.%d", al, j), "Type": "Audio", "Name": fmt.Sprintf("Song %d", j),
		"Album": fmt.Sprintf("Album %03d", al), "AlbumId": fmt.Sprintf("al%d", al), "Artists": []string{artist},
		"ProductionYear": 2020, "ParentIndexNumber": 1, "IndexNumber": j + 1, "RunTimeTicks": 100 * 10_000_000,
		"MediaSources": []any{map[string]any{"Size": 1000, "Container": "flac"}}, "HasLyrics": n.lyrics.Load(),
		"AlbumPrimaryImageTag": map[bool]string{true: "t"}[n.songArt], "UserData": n.userData(fmt.Sprintf("al%d.%d", al, j))}
}

func (n *nav) userData(id string) map[string]any {
	u := map[string]any{"IsFavorite": n.favourite(id)}
	if v, ok := n.lastPlayed.Load(id); ok {
		u["LastPlayedDate"] = v
	}
	return u
}

// favourite is song id's heart on the server: set last, else one of al0's first favs.
func (n *nav) favourite(id string) bool {
	if v, ok := n.hearts.Load(id); ok {
		return v.(bool)
	}
	var j int
	_, err := fmt.Sscanf(id, "al0.%d", &j)
	return err == nil && j < n.favs
}

func (n *nav) album(i int) map[string]any {
	artist := cmp.Or(n.albumArtist, "Artist")
	if n.various {
		artist = "Various Artists"
	}
	return map[string]any{"Id": fmt.Sprintf("al%d", i), "Type": "MusicAlbum", "Name": fmt.Sprintf("Album %03d", i),
		"AlbumArtist": artist, "ProductionYear": 2020, "ChildCount": n.songsPer}
}

func (n *nav) playlist(k int) map[string]any {
	name := "Mix"
	if k > 1 {
		name = fmt.Sprintf("Mix %d", k)
	}
	count := n.songsPer
	if n.dup {
		count++
	}
	return map[string]any{"Id": fmt.Sprintf("pl%d", k), "Type": "Playlist", "MediaType": "Audio", "Name": name, "ChildCount": count,
		"DateCreated": fmt.Sprintf("2026-02-%02dT00:00:00Z", k), "RunTimeTicks": int64(k%3+1) * 300 * 10_000_000}
}

func (n *nav) songs(al int) []map[string]any {
	var out []map[string]any
	for j := 0; j < n.songsPer; j++ {
		out = append(out, n.song(al, j))
	}
	return out
}

// status answers what a Subsonic error code stood for in these tests: 40 wrong credentials (401),
// anything else not found (404).
func status(code int) int {
	if code == 40 {
		return http.StatusUnauthorized
	}
	return http.StatusNotFound
}

func (n *nav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.reqs.Add(1)
	if c := n.rawStatus.Load(); c != 0 {
		http.Error(w, "proxy error", int(c))
		return
	}
	if n.stall {
		<-r.Context().Done()
		return
	}
	q := r.URL.Query()
	if k := userDataKey(r); k != "" {
		n.userDataSent.Store(k+" "+q.Get("enableUserData"), true)
	}
	ok := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	page := func(items []map[string]any) {
		if items == nil {
			items = []map[string]any{}
		}
		ok(map[string]any{"Items": items, "TotalRecordCount": len(items)})
	}
	fail := func(code int) { http.Error(w, "Not found", status(code)) }
	p := r.URL.Path
	if id, isFav := strings.CutPrefix(p, "/UserFavoriteItems/"); isFav { // logged as "fav <id> 1" (POST) or 0 (DELETE)
		if c := n.favStatus.Load(); c != 0 {
			http.Error(w, "favourite failed", int(c))
			return
		}
		if n.onReport != nil {
			n.onReport()
		}
		on := r.Method == "POST"
		n.hearts.Store(id, on)
		n.reportMu.Lock()
		n.reports = append(n.reports, fmt.Sprintf("fav %s %d", id, map[bool]int{true: 1}[on]))
		n.reportMu.Unlock()
		ok(n.userData(id))
		return
	}
	if strings.HasPrefix(p, "/UserPlayedItems/") || strings.HasPrefix(p, "/Sessions/Playing") {
		if c := n.reportStatus.Load(); c != 0 {
			http.Error(w, "report failed", int(c))
			return
		}
		var body struct {
			ItemID        string `json:"ItemId"`
			PositionTicks int64
			IsPaused      bool
		}
		json.NewDecoder(r.Body).Decode(&body)
		line := "played " + strings.TrimPrefix(p, "/UserPlayedItems/") + " " + q.Get("datePlayed")
		switch p {
		case "/Sessions/Playing":
			line = "start " + body.ItemID
		case "/Sessions/Playing/Stopped":
			line = fmt.Sprintf("stop %s %d", body.ItemID, body.PositionTicks)
		case "/Sessions/Playing/Progress":
			line = fmt.Sprintf("progress %s %d %v", body.ItemID, body.PositionTicks/10_000_000, body.IsPaused)
		}
		if n.onReport != nil {
			n.onReport()
		}
		n.reportMu.Lock()
		n.reports = append(n.reports, line)
		n.reportMu.Unlock()
		if id, ok := strings.CutPrefix(p, "/UserPlayedItems/"); ok {
			n.lastPlayed.Store(id, q.Get("datePlayed"))
			if n.dropReply.Load() {
				panic(http.ErrAbortHandler)
			}
			if n.cutBody.Load() {
				w.Header().Set("Content-Length", "10")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("{}"))
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if id, lyr := strings.CutSuffix(strings.TrimPrefix(p, "/Audio/"), "/Lyrics"); lyr && strings.HasPrefix(p, "/Audio/") {
		c, _ := n.lyricsReqs.LoadOrStore(id, new(atomic.Int32))
		c.(*atomic.Int32).Add(1)
		if n.lyricsHold != nil {
			select {
			case <-n.lyricsHold:
			case <-r.Context().Done():
				return
			}
		}
		ok(map[string]any{"Lyrics": []any{map[string]any{"Text": "la"}}})
		return
	}
	media := strings.HasSuffix(p, "/Download") || strings.HasPrefix(p, "/Audio/")
	if n.errCode != 0 && !media && p != "/System/Info/Public" {
		fail(n.errCode)
		return
	}
	if n.libLost.Load() && (p == "/UserViews" || q.Get("parentId") == "music") { // Jellyfin: not permitted to access Library
		if p == "/UserViews" {
			ok(map[string]any{"Items": []any{map[string]any{"Id": "films", "Name": "Movies", "CollectionType": "movies"}}})
			return
		}
		fail(40)
		return
	}
	albumOf := func(id string) int { i, _ := strconv.Atoi(strings.TrimPrefix(id, "al")); return i }
	switch {
	case strings.HasPrefix(p, "/Items/") && strings.HasSuffix(p, "/Images/Primary"):
		n.images.Add(1)
		if n.coverStatus != 0 {
			http.Error(w, "cover failed", n.coverStatus)
			return
		}
		if !strings.HasPrefix(strings.TrimPrefix(p, "/Items/"), "al") {
			fail(404)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		if n.coverSize != 0 {
			w.Write(make([]byte, n.coverSize))
			return
		}
		w.Write([]byte("ÿØjpeg " + p + " " + q.Get("fillWidth")))
	case p == "/System/Info/Public":
		ok(map[string]any{"ProductName": "Jellyfin Server", "Version": "10.10.7"})
	case p == "/Users/Me":
		n.pings.Add(1)
		ok(map[string]any{"Id": "u1", "Name": "u"})
	case p == "/Users/AuthenticateByName":
		ok(map[string]any{"AccessToken": "tok", "User": map[string]any{"Id": "u1", "Name": "u"}})
	case p == "/UserViews" && n.noMusic:
		ok(map[string]any{"Items": []any{map[string]any{"Id": "films", "Name": "Movies", "CollectionType": "movies"}}})
	case p == "/UserViews": // one music library, "music", holding /music (jellyfin/music.go)
		ok(map[string]any{"Items": []any{map[string]any{"Id": "music", "Name": "Music", "CollectionType": "music"}}})
	case p == "/Items" && q.Get("parentId") == "music" && q.Get("includeItemTypes") == "":
		if n.rootsGone {
			fail(70)
			return
		}
		page([]map[string]any{{"Id": "ar1", "Type": "MusicArtist", "Path": "/music/Artist"}})
	case p == "/Artists/AlbumArtists" && q.Get("searchTerm") != "": // "Artist" for a term it holds
		n.searchReqs.Add(1)
		if strings.Contains("artist", strings.ToLower(q.Get("searchTerm"))) {
			page([]map[string]any{{"Id": "ar1", "Name": "Artist", "AlbumCount": 2}})
			return
		}
		page(nil)
	case p == "/Artists/AlbumArtists":
		n.artistReqs.Add(1)
		if n.holdArtists != nil {
			<-n.holdArtists
		}
		n.artistSort.Store(q.Get("sortBy") + " " + q.Get("sortOrder"))
		if n.empty {
			page(nil)
			return
		}
		ars := []map[string]any{{"Id": "ar1", "Name": "Artist", "AlbumCount": 2, "DateCreated": "2026-01-01T00:00:00Z"}}
		for i := 0; i < n.artists; i++ {
			ars = append(ars, map[string]any{"Id": fmt.Sprintf("ar%d", i+2), "Name": fmt.Sprintf("Artist %05d", i+2), "AlbumCount": 1,
				"DateCreated": fmt.Sprintf("2026-01-01T00:%02d:%02dZ", (i+2)/60%60, (i+2)%60)})
		}
		page(ars)
	case p == "/Items":
		kind := q.Get("includeItemTypes")
		off, _ := strconv.Atoi(q.Get("startIndex"))
		switch {
		case q.Get("ids") != "":
			n.batchReqs.Add(1)
			var s []map[string]any
			for _, id := range strings.Split(q.Get("ids"), ",") {
				var a, j int
				switch {
				case n.noMusic || id == n.leaveOut || id == n.failID: // gone, or the account can't see it
				case strings.HasPrefix(id, "pl"):
					k, _ := strconv.Atoi(strings.TrimPrefix(id, "pl"))
					s = append(s, n.playlist(k))
				case !strings.Contains(id, "."):
					s = append(s, n.album(albumOf(id)))
				default:
					if _, err := fmt.Sscanf(id, "al%d.%d", &a, &j); err == nil {
						s = append(s, n.song(a, j))
					}
				}
			}
			page(s)
		case q.Get("isFavorite") == "true":
			var fav []map[string]any
			for j := 0; j < max(n.favs, n.songsPer) && off == 0; j++ {
				if n.favourite(fmt.Sprintf("al0.%d", j)) {
					fav = append(fav, n.song(0, j))
				}
			}
			page(fav)
		case q.Get("searchTerm") != "":
			n.searchReqs.Add(1)
			if n.empty || off > 0 {
				page(nil)
				return
			}
			switch kind {
			case "MusicAlbum":
				page([]map[string]any{n.album(0)})
			case "Audio":
				page(n.songs(0))
			default:
				page(nil)
			}
		case q.Get("parentId") != "" && q.Get("parentId") != "music": // an album's songs
			if q.Get("parentId") == n.goneID || n.allGone {
				fail(70)
				return
			}
			page(n.songs(albumOf(q.Get("parentId"))))
		case q.Get("albumArtistIds") != "":
			page([]map[string]any{n.album(0), n.album(1)})
		case kind == "Audio": // every song, a page at a time
			n.songListReqs.Add(1)
			if n.onSongList != nil {
				n.onSongList()
			}
			if n.failID != "" {
				fail(70)
				return
			}
			size, _ := strconv.Atoi(q.Get("limit"))
			var s []map[string]any
			for i := off; i < n.albums*n.songsPer && i < off+size; i++ {
				s = append(s, n.song(i/n.songsPer, i%n.songsPer))
			}
			if s == nil {
				s = []map[string]any{}
			}
			ok(map[string]any{"Items": s, "TotalRecordCount": n.albums * n.songsPer})
		case kind == "Playlist":
			n.playlistSort.Store(q.Get("sortBy") + " " + q.Get("sortOrder"))
			var pls []map[string]any
			for k := 1; k <= max(n.playlists, 1) && !n.empty; k++ {
				pls = append(pls, n.playlist(k))
			}
			page(pls)
		case kind == "MusicAlbum":
			if n.listErr != 0 && off > 0 { // every page after the first
				fail(n.listErr)
				return
			}
			if n.empty {
				page(nil)
				return
			}
			n.albumSort.Store(q.Get("sortBy") + " " + q.Get("sortOrder"))
			n.albumLists.Add(1)
			if n.holdList != nil {
				<-n.holdList
			}
			select {
			case <-time.After(n.listDelay):
			case <-r.Context().Done():
				return
			}
			size, _ := strconv.Atoi(q.Get("limit"))
			if n.sameAlbums {
				off = 0
			}
			var al []map[string]any
			for i := off; i < n.albums && i < off+size; i++ {
				if a := n.album(i); a["Id"] != n.hideAlbum {
					al = append(al, a)
				}
			}
			page(al)
		default:
			http.NotFound(w, r)
		}
	case strings.HasPrefix(p, "/Playlists/") && strings.HasSuffix(p, "/Items"):
		kk, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(p, "/Playlists/"), "/Items"), "pl"))
		s := n.songs(kk - 1)
		if n.dup {
			s = append(s, n.song(kk-1, 0))
		}
		page(s)
	case media:
		if n.mediaErr != 0 {
			fail(n.mediaErr)
			return
		}
		if n.block != nil {
			select {
			case <-n.block:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "audio/flac")
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") && strings.HasSuffix(rg, "-") {
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			n.lastRange.Store(rg)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-999/1000", from))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(strings.Repeat("x", 1000-from)))
			return
		}
		w.Write([]byte(strings.Repeat("x", 1000)))
	case strings.HasPrefix(p, "/Items/"):
		id := strings.TrimPrefix(p, "/Items/")
		switch {
		case n.noMusic: // Jellyfin: an item of a library the account can't see is not found
			fail(70)
		case strings.HasPrefix(id, "pl"):
			n.playlistReqs.Add(1)
			kk, _ := strconv.Atoi(strings.TrimPrefix(id, "pl"))
			ok(n.playlist(kk))
		case strings.HasPrefix(id, "ar"):
			n.artistItemReqs.Add(1)
			ok(map[string]any{"Id": "ar1", "Type": "MusicArtist", "Name": "Artist", "AlbumCount": 2})
		case strings.Contains(id, "."): // a song: al<i>.<j>
			n.songReqs.Add(1)
			al, j, _ := strings.Cut(strings.TrimPrefix(id, "al"), ".")
			a, _ := strconv.Atoi(al)
			k, _ := strconv.Atoi(j)
			ok(n.song(a, k))
		default: // an album
			n.albumReqs.Add(1)
			if n.holdAlbum != nil {
				select {
				case <-n.holdAlbum:
				case <-r.Context().Done():
					n.albumGaveUp.Add(1)
					return
				}
			}
			if id == n.failID {
				http.Error(w, "proxy error", http.StatusBadGateway)
				return
			}
			if id == n.goneID || n.allGone {
				fail(70)
				return
			}
			ok(n.album(albumOf(id)))
		}
	default:
		http.NotFound(w, r)
	}
}

type tdaemon struct {
	*daemon
	tmp string
}

func newTestDaemon(t *testing.T, n *nav, caps string, signedIn bool) *tdaemon {
	t.Helper()
	return newTestDaemonOn(t, t.TempDir(), n, caps, signedIn)
}

// newTestDaemonOn starts the daemon on an existing card folder.
func newTestDaemonOn(t *testing.T, card string, n *nav, caps string, signedIn bool) *tdaemon {
	t.Helper()
	return newTestDaemonIn(t, card, t.TempDir(), n, caps, signedIn)
}

// newTestDaemonIn starts the daemon on an existing card folder and /tmp.
func newTestDaemonIn(t *testing.T, card, tmp string, n *nav, caps string, signedIn bool) *tdaemon {
	t.Helper()
	if caps != "" {
		os.WriteFile(filepath.Join(tmp, "q2-addon.caps"), []byte(caps+"\n"), 0o644)
	}
	srv := httptest.NewTLSServer(n)
	d := newDaemon(filepath.Join(card, ".jellyfin"), card, tmp)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	d.runner.CardMounted = func() bool { return true }
	d.free = func(string) int64 { return 1 << 40 }
	d.localIP = func() string { return "192.168.1.20" } // Wi-Fi on, whatever the test machine has
	d.wifiOff = func() bool { return false }
	if signedIn {
		if err := creds.Save(d.dir, jellyfin.Creds{Server: srv.URL, User: "u", Token: "tok", UserID: "u1"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		d.stream.stop()
		d.runner.Cancel()
		srv.CloseClientConnections()
		srv.Close()
		d.bg.Wait() // a ping or preview still writing log.txt would race the TempDir's removal (M45)
		d.stream.stop()
		d.runner.Cancel()
	})
	return &tdaemon{d, tmp}
}

func (d *tdaemon) do(op string, args ...string) q2spot.Reply {
	return d.device(q2spot.Request{ID: "1", Op: op, Args: args})
}

func titles(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Title)
	}
	return out
}

func find(r q2spot.Reply, title string) (q2spot.Row, bool) {
	for _, row := range r.Rows {
		if row.Title == title || strings.HasPrefix(row.Title, title) {
			return row, true
		}
	}
	return q2spot.Row{}, false
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRootSignedIn(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	r := d.do("menu")
	want := []string{"Newly Added", "Albums", "Artists", "Playlists", "Settings"}
	if got := titles(r); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("titles %q, want %q", got, want)
	}
	for _, row := range r.Rows {
		if row.Kind != "list" {
			t.Errorf("%s kind %q", row.Title, row.Kind)
		}
	}
	// Settings: Sync, then Quality and Storage, then the device's own settings.
	s := d.do("settings")
	want = []string{"Sync", "Quality and Storage", "Swipe Between Sections: On", "Report Plays to Server: On", "Account", "Version "}
	if got := titles(s); len(got) != len(want) {
		t.Fatalf("settings %q", got)
	}
	for i, w := range want {
		if !strings.HasPrefix(s.Rows[i].Title, w) {
			t.Fatalf("settings %q, want %q", titles(s), want)
		}
	}
	if row := s.Rows[1]; row.Kind != "list" || row.URI != "storage" {
		t.Errorf("Quality and Storage %+v", row)
	}
	// Sync's options, first on its page: On This Q2 and Sync Entire Library moved there from Settings.
	r = d.do("sync")
	want = []string{"Sync Jobs", "On This Q2", "Sync Favourites", "Choose Playlists", "Sync Entire Library"}
	if got := titles(r); len(got) < 5 || strings.Join(got[:5], "|") != strings.Join(want, "|") {
		t.Fatalf("sync %q", got)
	}
	if !strings.HasPrefix(r.Rows[1].Subtitle, "0 items · ") || r.Rows[1].URI != "onq2" {
		t.Errorf("On This Q2 %+v", r.Rows[1])
	}
}

// Quality and Storage holds, in order, what was in Settings: the two qualities, then the card's
// space, and Clear Playback Cache while the cache holds songs.
func TestQualityAndStoragePage(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	p := filepath.Join(d.card, ".jellyfin", "cache", "s-original.flac")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, make([]byte, 1000), 0o644)
	r := d.do("storage")
	want := []string{"Sync Quality: Original", "Streaming Quality: Original", "Always Re-encode: Off",
		"Space to Keep Free: 512 MB", "Clear Playback Cache"}
	if got := titles(r); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q, want %q", got, want)
	}
	for i, uri := range []string{"defq", "streamq", "reencode", "keepfree", "clearcache"} {
		if r.Rows[i].URI != uri {
			t.Errorf("%s URI %q", r.Rows[i].Title, r.Rows[i].URI)
		}
	}
	for _, row := range d.do("settings").Rows {
		if strings.HasPrefix(row.Title, "Sync Quality") || strings.HasPrefix(row.Title, "Playback Cache") {
			t.Errorf("still in Settings: %q", row.Title)
		}
	}
}

// U37: Always Re-encode is off by default and says what that means underneath; a press turns it
// on (saved, toasted), the row then says what on means, and a second press turns it off again.
func TestAlwaysReencode(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	row := func() q2spot.Row {
		for _, r := range d.do("storage").Rows {
			if r.URI == "reencode" {
				return r
			}
		}
		t.Fatal("no Always Re-encode row")
		return q2spot.Row{}
	}
	if r := row(); r.Title != "Always Re-encode: Off" || r.Subtitle != "MP3, AAC and Opus songs stay as they are" || r.Kind != "action" {
		t.Fatalf("default %+v", r)
	}
	if m := d.do("reencode").Message; !strings.HasPrefix(m, "Always Re-encode: On") {
		t.Fatalf("toast %q", m)
	}
	if !d.settings().reencode() {
		t.Fatal("not saved")
	}
	if r := row(); r.Title != "Always Re-encode: On" || r.Subtitle != "Lossy songs too: smaller, lower quality" {
		t.Fatalf("on %+v", r)
	}
	if m := d.do("reencode").Message; !strings.HasPrefix(m, "Always Re-encode: Off") || d.settings().reencode() {
		t.Fatalf("second press %q", m)
	}
}

// A sync paused because the server refuses a download (syncer.ErrRefused) says, in a row of its
// own, how Always Re-encode gets round it, when it would: under an Opus Sync Quality with Always
// Re-encode off, and under Original with an Opus one.
func TestDownloadsOffHint(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.client()
	err := fmt.Errorf("%w: %w", syncer.ErrRefused, jellyfin.ErrDownloadsOff)
	plain := jellyfin.Message(err, d.hostName())
	for _, c := range []struct{ q, re, hint string }{
		{"original", "", "|Or choose an Opus Sync Quality and Always Re-encode"},
		{"opus-128", "", "|Turn on Always Re-encode in Quality and Storage"},
		{"opus-128", "on", ""},
	} {
		if e := d.saveSettings(settings{DefaultQuality: c.q, Reencode: c.re}); e != nil {
			t.Fatal(e)
		}
		want := "|" + plain + c.hint + "|Resume"
		if got := strings.Join(titles(d.syncRows(syncer.Snapshot{State: "paused", Err: err})), "|"); !strings.Contains(got, want) {
			t.Errorf("%s reencode %q: %q, want %q", c.q, c.re, got, want)
		}
	}
}

// The Sync page keeps its options in every state, first, so On This Q2 and Sync Entire Library stay
// in reach as they were in Settings, and in one place under the highlight while the rows below them
// change (M33).
func TestSyncOptionsEveryState(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	n.rawStatus.Store(502)
	d := newTestDaemon(t, n, "", true)
	sel := selection.Set{Items: []selection.Item{{Kind: selection.KindAlbum, ID: "al0", Name: "Album 000", Quality: "original"}}}
	if err := sel.Save(filepath.Join(d.dir, "selection.json")); err != nil {
		t.Fatal(err)
	}
	r := d.preview(t)
	if got := strings.Join(titles(r), "|"); !strings.HasPrefix(got, syncOptions+"|The server answered HTTP 502") {
		t.Fatalf("after a failed preview %q", got)
	}
	// Running and paused: the ETA, Now and pause rows come and go below the options.
	for _, s := range []syncer.Snapshot{{State: "running", Total: 2}, {State: "running", Total: 2, ETA: time.Minute, Current: "Song"},
		{State: "paused", Total: 2, Err: syncer.ErrCardFull}} {
		if got := strings.Join(titles(d.syncRows(s)), "|"); !strings.HasPrefix(got, syncOptions+"|") {
			t.Fatalf("%s %q", s.State, got)
		}
	}
	// Stopped: Retry, Check Again and Details below the options.
	d = newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	os.WriteFile(filepath.Join(d.card, ".jellyfin", "manifest.json"), []byte("{"), 0o644)
	d.do("start")
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "error" })
	if got := strings.Join(titles(d.do("sync")), "|"); !strings.HasPrefix(got, syncOptions+"|Sync stopped") ||
		!strings.HasSuffix(got, "|Retry|Check Again|Details") {
		t.Fatalf("stopped %q", got)
	}
}

// syncOptions are the Sync page's options, its first rows in every state.
const syncOptions = "Sync Jobs|On This Q2|Sync Favourites|Choose Playlists|Sync Entire Library"

// afterOptions is the Sync page below its options.
func afterOptions(r q2spot.Reply) []q2spot.Row { return r.Rows[strings.Count(syncOptions, "|")+1:] }

func TestHoldToggleToast(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 14}, "", true)
	row, _ := find(d.do("albums"), "Album 000")
	if row.Position != "toggle" || row.Context != "album:al0" {
		t.Fatalf("hold fields %+v", row)
	}
	if r := d.do(row.Position, row.Context); r.Message != "Added to Sync · 14 songs" {
		t.Fatalf("toast %q", r.Message)
	}
	if r := d.do(row.Position, row.Context); r.Message != "Removed from Sync" {
		t.Fatalf("toast %q", r.Message)
	}
}

// Q2 Pod asks for the page again after an action on it (addon.c got()): an album's, playlist's or
// artist's page asked again after Add to Sync says Remove from Sync, with Sync Now above it, and
// after Remove from Sync says Add to Sync again.
func TestItemPageAfterToggle(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "", true)
	for _, key := range []string{"album:al0", "playlist:pl1", "artist:ar1"} {
		op, _, _ := strings.Cut(key, ":")
		add, ok := find(d.do(op, key), "Add to Sync")
		if !ok {
			t.Fatalf("%s: %q", key, titles(d.do(op, key)))
		}
		d.do(add.URI, add.Context)
		r := d.do(op, key)
		if got := strings.Join(titles(r)[:2], "|"); got != "Sync Now|Remove from Sync" {
			t.Fatalf("%s after Add to Sync: %q", key, titles(r))
		}
		d.do(r.Rows[1].URI, r.Rows[1].Context)
		if r := d.do(op, key); r.Rows[0].Title != "Add to Sync" {
			t.Fatalf("%s after Remove from Sync: %q", key, titles(r))
		}
	}
}

// Add to Sync on a page just opened asks the server nothing: the item's name and songs (an
// artist's albums) are those the page was drawn from. A song added from an album's page is found
// in that page's list too. The toast still counts the songs.
func TestAddToSyncFromPageAsksNothing(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3}
	d := newTestDaemon(t, n, "", true)
	for _, c := range []struct{ key, toast string }{{"album:al0", "Added to Sync · 3 songs"},
		{"playlist:pl1", "Added to Sync · 3 songs"}, {"artist:ar1", "Added to Sync · 6 songs"},
		{favKey, "Added to Sync · 0 songs"}} {
		op, _, _ := strings.Cut(c.key, ":")
		add, _ := find(d.do(op, c.key), "Add to Sync")
		before := n.reqs.Load()
		if r := d.do(add.URI, add.Context); r.Message != c.toast {
			t.Fatalf("%s: toast %q", c.key, r.Message)
		}
		if got := n.reqs.Load() - before; got != 0 {
			t.Errorf("%s: %d requests to the server", c.key, got)
		}
		d.do("toggle", c.key) // out again
	}
	if it := d.selected(t); len(it) != 0 {
		t.Fatalf("%+v", it)
	}
	song, _ := find(d.do("album", "album:al1"), "Song 1")
	before := n.reqs.Load()
	if r := d.do("toggle", song.Context); r.Message != "Added to Sync · 1 song" {
		t.Fatalf("song toast %q", r.Message)
	}
	if got := n.reqs.Load() - before; got != 0 {
		t.Errorf("song: %d requests to the server", got)
	}
	if it := d.selected(t); len(it) != 1 || it[0].Kind != selection.KindSong || it[0].Name != "Song 1" {
		t.Fatalf("%+v", it)
	}
	// Not the page just opened: fetched, with its own name.
	d.do("toggle", song.Context)
	d.do("album", "album:al0")
	before = n.reqs.Load()
	d.do("toggle", "album:al1")
	if got := n.reqs.Load() - before; got == 0 {
		t.Error("album:al1 added with no request")
	}
	if it := d.selected(t); len(it) != 1 || it[0].Name != "Album 001" {
		t.Fatalf("%+v", it)
	}
}

func TestIncludedVia(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 4}, "", true)
	d.do("toggle", "album:al0")
	page := d.do("album", "album:al0")
	song, ok := find(page, "Song 1")
	if !ok || song.Position != "toggle" {
		t.Fatalf("song row %+v in %q", song, titles(page))
	}
	if r := d.do("toggle", song.Context); r.Message != "Included via Album 000" {
		t.Fatalf("toast %q", r.Message)
	}
	sel, _ := selection.Load(filepath.Join(d.dir, "selection.json"))
	if len(sel.Items) != 1 || sel.Items[0].ID != "al0" {
		t.Fatalf("selection changed: %+v", sel.Items)
	}
}

func TestStatusSubtitles(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 4, songsPer: 3}, "", true)
	sel := selection.Set{Version: 1}
	for _, id := range []string{"al0", "al1", "al2"} {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: id, Name: id, Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	add := func(id, key string) {
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Jellyfin", id+".flac"), Size: 1000, Items: []string{key},
			Quality: "original"}
	}
	for j := 0; j < 3; j++ {
		add(fmt.Sprintf("al0.%d", j), "album:al0")
	}
	add("al2.0", "album:al2")
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	r := d.do("albums")
	want := map[string]string{"Album 000": "Artist · On Q2", "Album 001": "Artist · Waiting to sync",
		"Album 002": "Artist · 1 of 3 songs on Q2", "Album 003": "Artist"}
	for title, sub := range want {
		if row, _ := find(r, title); row.Subtitle != sub {
			t.Errorf("%s subtitle %q, want %q", title, row.Subtitle, sub)
		}
	}
}

// Under an Opus Sync Quality, status follows the plan's rule for lossy songs (library.Quality) per
// song. After a start (nothing fetched) an MP3 album synced as it is stays On Q2, as does an .m4a one
// (it may be ALAC: not known until fetched); an MP3 song on the card at opus-128 is Waiting to sync
// with Always Re-encode off, as the next sync brings its original.
func TestStatusLossySongs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 1}, "", true)
	if err := d.saveSettings(settings{DefaultQuality: "opus-128"}); err != nil {
		t.Fatal(err)
	}
	sel := selection.Set{Version: 1}
	for _, id := range []string{"al0", "al1", "al2"} {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: id, Name: id, Quality: "opus-128"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for _, e := range []struct{ id, ext, q string }{{"al0.0", "mp3", "original"}, {"al1.0", "m4a", "original"}, {"al2.0", "opus", "opus-128"}} {
		m.Songs[e.id] = &library.Entry{ID: e.id, Path: filepath.Join(d.card, "Jellyfin", e.id+"."+e.ext), Size: 1000,
			Items: []string{"album:" + strings.TrimSuffix(e.id, ".0")}, Quality: e.q}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.resolved = map[string][]jellyfin.Song{"album:al2": {{ID: "al2.0", AlbumID: "al2", Suffix: "mp3", Codec: "mp3"}}}
	d.mu.Unlock()
	r := d.do("albums")
	want := map[string]string{"Album 000": "Artist · On Q2", "Album 001": "Artist · On Q2", "Album 002": "Artist · Waiting to sync",
		"Sync Now": "1 waiting"}
	for title, sub := range want {
		if row, _ := find(r, title); row.Subtitle != sub {
			t.Errorf("%s subtitle %q, want %q", title, row.Subtitle, sub)
		}
	}
}

func TestPagingMore(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "", true)
	r := d.do("albums")
	if len(r.Rows) != 50 || r.Cursor == "" {
		t.Fatalf("first page: %d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	if r.Rows[0].Title != "Search Albums" || r.Rows[1].Title != "Sort" || r.Rows[49].Title != "Album 047" {
		t.Fatalf("first page %q … %q", r.Rows[0].Title, r.Rows[49].Title)
	}
	r = d.do("albums", r.Cursor)
	if len(r.Rows) != 50 || r.Rows[0].Title != "Album 048" || r.Cursor == "" {
		t.Fatalf("second page: %d rows from %q, cursor %q", len(r.Rows), r.Rows[0].Title, r.Cursor)
	}
	if r.Message != "pages" {
		t.Errorf("paged reply Message %q, want pages (F-R8: the firmware auto-pages it)", r.Message)
	}
	r = d.do("albums", r.Cursor)
	if len(r.Rows) != 22 || r.Rows[0].Title != "Album 098" || r.Cursor != "" || r.Message != "" {
		t.Fatalf("last page: %d rows, cursor %q, message %q", len(r.Rows), r.Cursor, r.Message)
	}
	for _, op := range [][]string{{"artists"}, {"search", "Album"}, {"artist", "ar0"}} {
		if r := d.do(op[0], op[1:]...); r.Cursor != "" && r.Message != "pages" {
			t.Errorf("%v: cursor %q with Message %q", op, r.Cursor, r.Message)
		}
	}
}

// preview asks for sync until the preview is ready.
func (d *tdaemon) preview(t *testing.T) q2spot.Reply {
	t.Helper()
	var r q2spot.Reply
	eventually(t, "the preview", func() bool { r = d.do("sync"); return r.Message != "refresh" })
	return r
}

// The Sync page reads the preview it found or started, not d.preview again: a background start
// (Sync Favourites) or dropPreview may clear or replace it in between. Probabilistic: the clearing
// has to land in that window, which a tight loop for 200 ms does many times over.
func TestSyncPagePreviewCleared(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true) // nothing selected: a preview is quick
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			d.mu.Lock()
			if i%2 == 0 {
				d.dropPreview()
			} else {
				d.daemon.preview = &preview{gen: d.gen, at: time.Now(), done: true, plan: &plan.Plan{}}
			}
			d.mu.Unlock()
		}
	}()
	for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); {
		d.do("sync") // panicked on a nil preview
	}
	close(stop)
	wg.Wait()
	d.bg.Wait()
}

func TestSyncPreviewRefreshThenRows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	r := d.do("sync")
	if _, ok := find(r, "Checking server…"); r.Message != "refresh" || !ok {
		t.Fatalf("first reply %q %q", r.Message, titles(r))
	}
	r = d.preview(t)
	for _, w := range []string{"+2 songs · ", "Needs ", "Start Sync"} {
		if _, ok := find(r, w); !ok {
			t.Errorf("no %q row in %q", w, titles(r))
		}
	}
	if row, _ := find(r, "Start Sync"); row.Kind != "action" || row.URI != "start" {
		t.Errorf("Start Sync row %+v", row)
	}
}

// A failed check offers Check Again before Details, as the stopped page does; it checks again.
func TestSyncPreviewFailedCheckAgain(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	n.rawStatus.Store(502)
	r := afterOptions(d.preview(t))
	if len(r) != 3 || r[1].URI != "check" || r[1].Title != "Check Again" || r[2].Title != "Details" {
		t.Fatalf("failed preview %q", titles(q2spot.Reply{Rows: r}))
	}
	n.rawStatus.Store(0)
	d.do("check")
	if r := d.do("sync"); r.Message != "refresh" {
		t.Fatalf("after Check Again %q %q", r.Message, titles(r))
	} else if _, ok := find(r, "Checking server…"); !ok {
		t.Fatalf("after Check Again %q", titles(r))
	}
	d.preview(t)
}

// A check that runs long says why the page waits.
func TestSyncPreviewLongCheck(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1, holdList: make(chan struct{})}
	t.Cleanup(func() { close(n.holdList) })
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	row := func() q2spot.Row {
		r := d.do("sync")
		row, ok := find(r, "Checking server…")
		if r.Message != "refresh" || !ok {
			t.Fatalf("reply %q %q", r.Message, titles(r))
		}
		return row
	}
	if s := row().Subtitle; s != "" {
		t.Fatalf("a quick check says %q", s)
	}
	d.mu.Lock()
	d.daemon.preview.at = d.daemon.preview.at.Add(-30 * time.Second)
	d.mu.Unlock()
	if s := row().Subtitle; s != "A big library takes minutes" {
		t.Fatalf("after 30 s %q", s)
	}
}

// Files an earlier sync left listed in the manifest (Stale) are a change: lists show Sync Now, the
// preview offers Start Sync, and the run deletes them, with nothing else to do, and says so.
func TestSyncPreviewLeftoverFiles(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true) // nothing selected
	f := filepath.Join(d.card, "Jellyfin", "A", "x", "old.flac")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("old"), 0o644)
	m := &library.Manifest{Songs: map[string]*library.Entry{}, Stale: []string{f}}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if row, ok := d.syncNowRow(); !ok || row.Subtitle != "1 waiting" {
		t.Errorf("Sync Now row %+v, %v", row, ok)
	}
	r := d.preview(t)
	for _, w := range []string{"Delete 1 file an earlier sync left", "Start Sync"} {
		if _, ok := find(r, w); !ok {
			t.Fatalf("no %q row in %q", w, titles(r))
		}
	}
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	if _, err := os.Stat(f); !os.IsNotExist(err) || len(d.manifest().Stale) != 0 {
		t.Errorf("left: %v, stale %q", err, d.manifest().Stale)
	}
	if _, ok := find(d.doneRows(d.runner.Snapshot()), "Sync complete · 1 file deleted · 0 failed"); !ok {
		t.Errorf("done rows %q", titles(d.doneRows(d.runner.Snapshot())))
	}
	if _, ok := d.syncNowRow(); ok {
		t.Error("Sync Now row with nothing left to do")
	}
}

// Not all of it fits: the preview says so, and still offers Start Sync, which pauses at Space to
// Keep Free (space_test.go has the run).
func TestSyncNotEnoughSpace(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.free = func(string) int64 { return 0 }
	d.do("toggle", "album:al0")
	r := d.preview(t)
	for _, w := range []string{"Not all of it fits on the card", "It pauses with 512 MB left", "Start Sync"} {
		if _, ok := find(r, w); !ok {
			t.Errorf("no %q in %q", w, titles(r))
		}
	}
}

func TestProgressRow(t *testing.T) {
	snap := syncer.Snapshot{State: "running", Permille: 470, Done: 112, Total: 240, ETA: 6 * time.Minute, Current: "Artist – Song"}
	d := newTestDaemon(t, &nav{}, "urls", true)
	r := d.syncRows(snap)
	if row := afterOptions(r)[0]; r.Message != "refresh" || row.Kind != "progress" || row.Position != "470" {
		t.Fatalf("with progress: %q %+v", r.Message, row)
	}
	for _, w := range []string{"112 of 240 songs", "About 6 min left", "Now: Artist – Song", "Pause", "Cancel"} {
		if _, ok := find(r, w); !ok {
			t.Errorf("no %q in %q", w, titles(r))
		}
	}
	snap.Failed = 3 // a failure shows while the sync runs, in the same row
	if _, ok := find(d.syncRows(snap), "112 of 240 songs · 3 failed"); !ok {
		t.Errorf("no failed count in %q", titles(d.syncRows(snap)))
	}
}

func TestDetailsRow(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, errCode: 70}, "", true)
	r := d.do("album", "album:al0")
	det, ok := find(r, "Details")
	if !ok || det.Kind != "list" || det.URI != "details" {
		t.Fatalf("no Details row in %+v", r.Rows)
	}
	if r.Rows[0].Title != "Not found on the server" {
		t.Errorf("message row %q", r.Rows[0].Title)
	}
	got := strings.Join(titles(d.do(det.URI, det.Context)), "|")
	for _, w := range []string{"Op: Items", "HTTP: 404", "Code: 70", "Time: "} {
		if !strings.Contains(got, w) {
			t.Errorf("details %q lack %q", got, w)
		}
	}
}

// An item's later pages reuse the list its first page fetched; a first page fetches it again.
func TestItemPagesFetchOnce(t *testing.T) {
	n := &nav{albums: 1, songsPer: 120}
	d := newTestDaemon(t, n, "", true)
	songs := 0
	for cursor, i := "", 0; i == 0 || cursor != ""; i++ {
		r := d.do("album", "album:al0", cursor)
		for _, row := range r.Rows {
			if strings.HasPrefix(row.Context, "song:") {
				songs++
			}
		}
		if i > 5 {
			t.Fatal("the pages never end")
		}
		cursor = r.Cursor
	}
	if songs != 120 || n.albumReqs.Load() != 1 {
		t.Fatalf("%d songs, %d album requests", songs, n.albumReqs.Load())
	}
	d.do("album", "album:al0")
	if n.albumReqs.Load() != 2 {
		t.Fatalf("first page again: %d album requests", n.albumReqs.Load())
	}
}

func TestReplyWithin8s(t *testing.T) {
	d := newTestDaemon(t, &nav{stall: true}, "library", true)
	ops := [][]string{{"menu"}, {"playlists"}, {"albums"}, {"artists"}, {"artist", "artist:ar1"}, {"search", "x"},
		{"playlist", "playlist:pl1"}, {"album", "album:al0"}, {"toggle", "album:al0"}, {"toggle", "song:al0.1 album:al0 1"},
		{"play", "album:al0 0"}, {"onq2"}, {"settings"}, {"account"}, {"server"}, {"sync"}, {"library"}, {"details", "getAlbum|0|0|0|"}}
	var wg sync.WaitGroup
	for _, op := range ops {
		wg.Add(1)
		go func(op []string) {
			defer wg.Done()
			start := time.Now()
			d.do(op[0], op[1:]...)
			if took := time.Since(start); took >= 8*time.Second {
				t.Errorf("%v took %v", op, took)
			}
		}(op)
	}
	wg.Wait()
}

// The manifest is read under its own lock: a decode doesn't hold up d.mu's users, and sendLibrary
// can read it with d.mu held.
func TestManifestReadOutsideMu(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	read := make(chan struct{})
	d.mu.Lock()
	go func() { d.manifest(); close(read) }()
	select {
	case <-read:
		d.mu.Unlock()
	case <-time.After(5 * time.Second):
		d.mu.Unlock()
		t.Fatal("manifest waited for d.mu")
	}
}

// While a sync runs, On This Q2's subtitle adds up the manifest already read, not each save of the
// run read again beside it; once the run ends it reads the card's.
func TestOnQ2SubtitleKeepsCopyDuringSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	held := func() *library.Manifest { d.manMu.Lock(); defer d.manMu.Unlock(); return d.man }
	m0 := d.manifest()
	saved := &library.Manifest{Songs: map[string]*library.Entry{"x": {ID: "x", Path: filepath.Join(d.card, "Jellyfin", "x.flac"), Size: 7e6}}}
	if err := saved.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if got := d.onQ2Subtitle(selection.Set{}, nil); !strings.HasSuffix(got, " · 0 MB") || held() != m0 {
		t.Errorf("read again during the sync: %q", got)
	}
	close(n.block)
	eventually(t, "the sync to finish", func() bool { return !d.runner.Busy() })
	d.onQ2Subtitle(selection.Set{}, nil)
	if held() == m0 {
		t.Error("not read again after the sync")
	}
}

func TestStateHoldDuringSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	state := func() string {
		d.tick()
		b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.state"))
		return string(b)
	}
	d.do("toggle", "album:al0")
	d.preview(t)
	if r := d.do("start"); r.Status != "" && r.Status != q2spot.OK {
		t.Fatalf("start: %+v", r)
	}
	eventually(t, "the sync to run", d.runner.Busy)
	if st := state(); !strings.Contains(st, "hold=1") || !strings.Contains(st, "version="+version) {
		t.Fatalf("state during sync:\n%s", st)
	}
	if row, _ := find(d.do("menu"), "Settings"); !strings.HasPrefix(row.Subtitle, "Syncing · ") {
		t.Errorf("Settings subtitle while running %q", row.Subtitle)
	}
	if row, _ := find(d.do("settings"), "Sync"); !strings.HasPrefix(row.Subtitle, "Syncing · ") {
		t.Errorf("Sync subtitle while running %q", row.Subtitle)
	}
	close(n.block)
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	if st := state(); strings.Contains(st, "hold=1") {
		t.Fatalf("hold kept after the sync:\n%s", st)
	}
	r := d.do("sync")
	if afterOptions(r)[0].Title != "Sync complete · 2 songs · 0 failed" {
		t.Errorf("done rows %q", titles(r))
	}
	if row, ok := find(r, "Check Again"); !ok || row.Subtitle != "" { // nothing changed during the sync
		t.Errorf("Check Again subtitle %q", row.Subtitle)
	}
}

// An item in Sync that the server deleted no longer fails the preview: it is named, the songs only
// it wanted are under Removed, nothing goes before Start Sync, and Start Sync takes it out of Sync,
// on its own and under Entire Library alike (audit 0.8.1).
func TestPreviewItemGone(t *testing.T) {
	for _, lib := range []bool{false, true} {
		card := t.TempDir()
		d := newTestDaemonOn(t, card, &nav{albums: 2, songsPer: 1}, "", true)
		if lib {
			d.do("toggle", "library:all")
		} else {
			d.do("toggle", "album:al0")
		}
		sel := selection.Set{Version: 1, Items: d.selected(t)}
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al1", Name: "Album 001"})
		sel.Save(filepath.Join(d.dir, "selection.json"))
		d.preview(t)
		d.do("start")
		eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })

		// The server deletes Album 001: gone from the album and every-song lists, 404 on its own.
		d = newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 1, goneID: "al1"}, "", true)
		r := d.preview(t)
		got := strings.Join(titles(q2spot.Reply{Rows: afterOptions(r)}), "|")
		if !strings.HasPrefix(got, "Album 001 · no longer on the server|−1 song") || !strings.HasSuffix(got, "|Start Sync") {
			t.Fatalf("lib %v: preview %q", lib, got)
		}
		if rm := d.daemon.preview.plan.Remove; len(rm) != 1 || !strings.Contains(rm[0].Path, "Album 001") {
			t.Fatalf("lib %v: Remove %+v", lib, rm)
		}
		if st := d.runner.Snapshot().State; st == "running" || len(d.selected(t)) != 2 {
			t.Fatalf("lib %v: before Start Sync: %s, %+v", lib, st, d.selected(t))
		}
		// Sync Jobs agrees, with nothing to press: holding it would keep none of its songs.
		if row, _ := find(d.do("jobs"), "Album 001"); row.Kind != "info" || row.Subtitle != "No longer on the server" {
			t.Fatalf("lib %v: Sync Jobs row %+v", lib, row)
		}
		// So does On This Q2, and a hold there still takes it out.
		if row, _ := find(d.do("onq2"), "Album 001"); row.Kind != "info" || !strings.HasPrefix(row.Subtitle, "No longer on the server · ") ||
			row.Context != "album:al1" || row.Position != "toggle" {
			t.Fatalf("lib %v: On This Q2 row %+v", lib, row)
		}
		d.do("start")
		eventually(t, "the second sync", func() bool { return d.runner.Snapshot().State == "done" })
		if it := d.selected(t); len(it) != 1 || it[0].ID == "al1" {
			t.Errorf("lib %v: selection after Start Sync %+v", lib, it)
		}
	}
}

// Many deleted items: a few are named and the rest counted, so Start Sync stays within the reply's 50 rows.
func TestPreviewManyItemsGone(t *testing.T) {
	card := t.TempDir()
	d := newTestDaemonOn(t, card, &nav{albums: 60, songsPer: 1}, "", true)
	for i := 0; i < 60; i++ {
		d.do("toggle", fmt.Sprintf("album:al%d", i))
	}
	d.preview(t)
	d.do("start")
	eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })

	d = newTestDaemonOn(t, card, &nav{albums: 60, songsPer: 1, allGone: true}, "", true)
	r := d.preview(t)
	got := titles(q2spot.Reply{Rows: afterOptions(r)})
	if len(got) < 7 || !strings.HasSuffix(got[4], " · no longer on the server") || got[5] != "and 55 more no longer on the server" {
		t.Fatalf("preview %q", got)
	}
	if _, kept := r.Encode("1"); kept != len(r.Rows) || got[len(got)-1] != "Start Sync" {
		t.Fatalf("%d of %d rows kept: %q", kept, len(r.Rows), got)
	}
}

// An artist one of whose albums the server deleted stays in Sync: only that album's songs go.
func TestPreviewArtistAlbumGone(t *testing.T) {
	card := t.TempDir()
	d := newTestDaemonOn(t, card, &nav{albums: 2, songsPer: 1}, "", true)
	d.do("toggle", "artist:ar1") // al0 and al1
	d.preview(t)
	d.do("start")
	eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })

	// Album 001 deleted after the artist's album list: its songs request answers 404.
	d = newTestDaemonOn(t, card, &nav{albums: 2, songsPer: 1, goneID: "al1"}, "", true)
	got := strings.Join(titles(q2spot.Reply{Rows: afterOptions(d.preview(t))}), "|")
	if !strings.HasPrefix(got, "−1 song") {
		t.Fatalf("preview %q", got)
	}
	if rm := d.daemon.preview.plan.Remove; len(rm) != 1 || !strings.Contains(rm[0].Path, "Album 001") {
		t.Fatalf("Remove %+v", rm)
	}
	d.do("start")
	eventually(t, "the second sync", func() bool { return d.runner.Snapshot().State == "done" })
	if it := d.selected(t); len(it) != 1 || it[0].ID != "ar1" {
		t.Errorf("selection %+v", it)
	}
}

// Only an item's own lookup says it is gone: a 404 on the music library's listing, which a playlist
// needs, fails the preview as before and takes nothing out of Sync.
func TestPreviewListNotFoundNotGone(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, rootsGone: true}, "", true)
	sel := selection.Set{Version: 1}
	sel.Toggle(selection.Item{Kind: selection.KindPlaylist, ID: "pl1", Name: "Mix"})
	sel.Save(filepath.Join(d.dir, "selection.json"))
	got := strings.Join(titles(q2spot.Reply{Rows: afterOptions(d.preview(t))}), "|")
	if got != "The server answered HTTP 404|Check Again|Details" {
		t.Fatalf("preview %q", got)
	}
	if it := d.selected(t); len(it) != 1 {
		t.Errorf("selection %+v", it)
	}
}

// An account that lost its music library gets 404 for each album and song it chose, as for a
// deleted one: the preview says there is no music library, and nothing is removed or taken out of
// Sync (audit 0.8.1 round 4, D1).
func TestPreviewGoneNoMusic(t *testing.T) {
	for _, keys := range [][]string{{"album:al0", "album:al1"}, {"song:al0.0", "song:al1.0"}} {
		card := t.TempDir()
		d := newTestDaemonOn(t, card, &nav{albums: 2, songsPer: 1}, "", true)
		for _, k := range keys {
			d.do("toggle", k)
		}
		d.preview(t)
		d.do("start")
		eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })

		d = newTestDaemonOn(t, card, &nav{albums: 2, songsPer: 1, noMusic: true}, "", true)
		got := strings.Join(titles(q2spot.Reply{Rows: afterOptions(d.preview(t))}), "|")
		if !strings.HasPrefix(got, "No music library on the server for this account") || strings.Contains(got, "no longer on the server") {
			t.Fatalf("%v: preview %q", keys, got)
		}
		d.do("start")
		d.bg.Wait()
		if st := d.runner.Snapshot().State; st == "running" || st == "done" {
			t.Fatalf("%v: sync %s", keys, st)
		}
		if n := len(d.manifest().Songs); n != 2 {
			t.Errorf("%v: %d songs left on the card", keys, n)
		}
		if it := d.selected(t); len(it) != 2 {
			t.Errorf("%v: selection %+v", keys, it)
		}
	}
}

// A music library the account lost while Jellyfin runs: lists of it (Entire Library here) answer
// 401, not 404. The preview checks the libraries and says there is none, not "Sign in again", and
// removes nothing (audit 0.8.1 round 5, D3).
func TestPreviewListLibraryLost(t *testing.T) {
	n := &nav{albums: 2, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	d.do("start")
	eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })

	n.libLost.Store(true)
	d.do("check")
	got := strings.Join(titles(q2spot.Reply{Rows: afterOptions(d.preview(t))}), "|")
	if !strings.HasPrefix(got, "No music library on the server for this account") || strings.Contains(got, "Start Sync") {
		t.Fatalf("preview %q", got)
	}
	if k := len(d.manifest().Songs); k != 2 {
		t.Errorf("%d songs left on the card", k)
	}
	if it := d.selected(t); len(it) != 1 {
		t.Errorf("selection %+v", it)
	}
}

// A deleted item with nothing of its own on the Q2 leaves Sync with the preview: there is no Start
// Sync to wait for.
func TestPreviewItemGoneNothingToRemove(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, goneID: "al1"}, "", true)
	sel := selection.Set{Version: 1}
	sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al1", Name: "Album 001"})
	sel.Save(filepath.Join(d.dir, "selection.json"))
	got := strings.Join(titles(q2spot.Reply{Rows: afterOptions(d.preview(t))}), "|")
	if got != "Album 001 · no longer on the server|Everything is up to date" {
		t.Fatalf("preview %q", got)
	}
	if it := d.selected(t); len(it) != 0 {
		t.Errorf("selection %+v", it)
	}
}

// A fast failure while the preview is still starting its workers (run with -race).
func TestPreviewFailsFastWithoutRace(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 40, songsPer: 1, failID: "al0"}, "", true)
	sel := selection.Set{Version: 1}
	for i := 0; i < 40; i++ {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: fmt.Sprintf("al%d", i), Name: "x", Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	r := d.preview(t)
	if _, ok := find(r, "The server answered HTTP 502"); !ok {
		t.Fatalf("preview rows %q", titles(r))
	}
	if _, ok := find(r, "Start Sync"); ok {
		t.Fatal("a failed preview offers Start Sync")
	}
}

// Ops that stop the sync or the stream answer at once even while a download hangs.
func TestControlOpsReplyFast(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.do("toggle", "album:al0")
	d.preview(t)
	timed := func(op string, args ...string) q2spot.Reply {
		t.Helper()
		start := time.Now()
		r := d.do(op, args...)
		if took := time.Since(start); took >= 2*time.Second {
			t.Errorf("%s took %v", op, took)
		}
		return r
	}
	timed("start")
	eventually(t, "the sync to run", d.runner.Busy)
	timed("play", "album:al1 0")
	eventually(t, "the stream to start", func() bool { return exists(d.stream.urlsFile()) })
	timed("play", "album:al1 1")
	timed("pause")
	timed("resume")
	timed("cancel")
	d.do("signout") // the first press only asks
	timed("signout")
	eventually(t, "the stream to stop", func() bool { return !exists(d.stream.urlsFile()) })
}

// A later page builds no view for a Sync Now row it never shows (round 2 speed 7).
func TestSyncNowRowLaterPageNoView(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	if _, ok := d.syncNowRow(); !ok {
		t.Fatal("no Sync Now row")
	}
	if n := testing.AllocsPerRun(5, func() { d.withSyncNow(q2spot.Reply{}, "50") }); n != 0 && !raceOn {
		t.Fatalf("%v allocations", n)
	}
}

func TestSyncNowRowWhenWaiting(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	for _, c := range [][]string{{"playlists"}, {"albums"}, {"artists"}, {"search", "x"}, {"album", "album:al1"}} {
		r := d.do(c[0], c[1:]...)
		i := 0 // after a search box and a sort, which Q2 Pod shows in the title bar only when they are first
		if len(r.Rows) > 0 && r.Rows[0].Kind == "search" {
			i = 1
			if r.Rows[1].URI == "sort" {
				i = 2
			}
		}
		if len(r.Rows) <= i || r.Rows[i].Title != "Sync Now" || r.Rows[i].Subtitle != "1 waiting" ||
			r.Rows[i].Kind != "list" || r.Rows[i].URI != "sync" {
			t.Errorf("%v: first row %+v", c, r.Rows)
		}
	}
	if r := d.do("albums"); len(r.Rows) < 3 || r.Rows[0].Title != "Search Albums" || r.Rows[1].Title != "Sort" || r.Rows[2].Title != "Sync Now" || r.Rows[3].Title != "Album 000" {
		t.Errorf("order changed: %q", titles(r))
	}
}

func TestNoSyncNowRowWhenUpToDate(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	for _, c := range [][]string{{"albums"}, {"playlists"}, {"menu"}} {
		if _, ok := find(d.do(c[0]), "Sync Now"); ok {
			t.Errorf("%v: Sync Now with nothing selected", c)
		}
	}
	d.do("toggle", "album:al0")
	if _, ok := find(d.do("menu"), "Sync Now"); ok {
		t.Error("Sync Now on the root menu")
	}
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for j := 0; j < 2; j++ {
		id := fmt.Sprintf("al0.%d", j)
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Jellyfin", id+".flac"), Size: 1, Items: []string{"album:al0"},
			Quality: "original"}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Error("Sync Now though everything is on the Q2")
	}
}

func TestSyncNowRowKeepsPagingWhole(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	r := d.do("albums")
	if len(r.Rows) != 50 || r.Rows[0].Title != "Search Albums" || r.Rows[2].Title != "Sync Now" || r.Rows[49].Title != "Album 046" {
		t.Fatalf("first page %d rows, last %q, cursor %q", len(r.Rows), r.Rows[len(r.Rows)-1].Title, r.Cursor)
	}
	if r = d.do("albums", r.Cursor); r.Rows[0].Title != "Album 047" { // none skipped
		t.Errorf("second page starts %q", r.Rows[0].Title)
	}
}

func (d *tdaemon) selected(t *testing.T) []selection.Item {
	t.Helper()
	sel, err := selection.Load(filepath.Join(d.dir, "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	return sel.Items
}

func TestEntireLibraryRow(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, playlists: 2}, "", true)
	libRow := func() q2spot.Row { row, _ := find(d.do("sync"), "Sync Entire Library"); return row }
	row := libRow()
	if row.Title != "Sync Entire Library" || row.Subtitle != "Off" || row.URI != "toggle" ||
		row.Context != "library:all" || row.Position != "toggle" || row.Kind != "action" {
		t.Fatalf("row %+v", row)
	}
	if got := titles(d.do("onq2")); len(got) != 1 || got[0] != "Nothing chosen yet" {
		t.Fatalf("on this q2 while off %q", got)
	}
	if r := d.do(row.URI, row.Context); r.Message != "Added to Sync · Entire Library" {
		t.Fatalf("toast %q", r.Message)
	}
	if it := d.selected(t); len(it) != 1 || it[0].Name != "Entire Library" || it[0].Quality != "original" {
		t.Fatalf("selection %+v", it)
	}
	if got := libRow().Subtitle; got != "On" {
		t.Fatalf("before resolve %q", got)
	}
	d.preview(t)
	if got := libRow().Subtitle; got != "On · 3 albums · 2 playlists" {
		t.Fatalf("after resolve %q", got)
	}
	r := d.do("onq2")
	if len(r.Rows) != 1 || r.Rows[0].Subtitle != "On · 3 albums · 2 playlists" {
		t.Fatalf("on this q2 while on %q", titles(r))
	}
	if q := r.Rows[0]; q.Title != "Entire Library" || q.Kind != "info" || q.URI != "" || q.Position != "toggle" || q.Context != "library:all" {
		t.Fatalf("on this q2 row %+v", q)
	}
	if r := d.do("toggle", "library:all"); r.Message != "Removed from Sync" {
		t.Fatalf("toast %q", r.Message)
	}
	if got := libRow().Subtitle; got != "Off" {
		t.Fatalf("after removal %q", got)
	}
}

func TestEntireLibraryRowShowsZeroCounts(t *testing.T) {
	d := newTestDaemon(t, &nav{empty: true}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if row, _ := find(d.do("sync"), "Sync Entire Library"); row.Subtitle != "On · 0 albums · 0 playlists" {
		t.Fatalf("after resolve %q", row.Subtitle)
	}
}

func TestEntireLibraryResolvesAlbumsAndPlaylists(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, playlists: 2}, "", true)
	d.do("toggle", "library:all")
	if r := d.do("albums"); r.Rows[2].Title != "Sync Now" || r.Rows[2].Subtitle != "1 waiting" {
		t.Fatalf("rows %q %q", titles(r), r.Rows[2].Subtitle)
	}
	d.preview(t)
	p := d.daemon.preview.plan
	if len(p.Add) != 6 {
		t.Errorf("Add %d, want 6", len(p.Add))
	}
	if len(p.Playlists) != 2 || len(p.Playlists["Mix"]) != 2 || len(p.Playlists["Mix 2"]) != 2 {
		t.Errorf("playlists %v", p.Playlists)
	}
}

func TestEntireLibraryIncludedVia(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "library:all")
	sel := selection.Set{Version: 1, Items: d.selected(t)}
	sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al2", Name: "Album 002"})
	sel.Save(filepath.Join(d.dir, "selection.json"))
	for _, key := range []string{"album:al2", "album:al1", "playlist:pl1", "artist:ar1", "song:al0.1"} {
		if r := d.do("toggle", key); r.Message != "Included via Entire Library" {
			t.Errorf("%s: toast %q", key, r.Message)
		}
	}
	if it := d.selected(t); len(it) != 2 || it[0].Kind != selection.KindLibrary || it[1].ID != "al2" {
		t.Fatalf("selection changed: %+v", it)
	}
}

func TestEmptyStates(t *testing.T) {
	d := newTestDaemon(t, &nav{empty: true}, "", true)
	for _, c := range []struct{ op, arg, title, sub string }{
		{"playlists", "", "No playlists", "Make one in Jellyfin and it shows here"},
		{"albums", "", "No albums", ""},
		{"artists", "", "No artists", ""},
		{"search", "x", "No results", ""},
		{"onq2", "", "Nothing chosen yet", "Open a playlist, album or artist and press Add to Sync"},
	} {
		r := d.do(c.op, c.arg)
		row, ok := find(r, c.title)
		if !ok || row.Kind != "info" || row.Subtitle != c.sub {
			t.Errorf("%s: %q row %+v", c.op, titles(r), row)
		}
	}
}

func TestEntireLibraryPagesPast500Albums(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 501, songsPer: 1}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if n := len(d.daemon.preview.plan.Add); n != 501 {
		t.Errorf("Add %d, want 501", n)
	}
}

func TestEntireLibraryFailsFast(t *testing.T) {
	n := &nav{albums: 400, songsPer: 1, failID: "al0"}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	r := d.preview(t)
	if _, ok := find(r, "Start Sync"); ok {
		t.Fatalf("preview should fail: %q", titles(r))
	}
	if got := n.albumReqs.Load(); got > 50 {
		t.Errorf("%d of 400 album requests made after the first failure", got)
	}
}

// syncAlbum runs a one-album sync to done on a daemon with cap library.
func syncAlbum(t *testing.T) *tdaemon {
	t.Helper()
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "library", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	return d
}

func (d *tdaemon) libReqSeq(t *testing.T) string {
	t.Helper()
	var b []byte
	eventually(t, "the library request", func() bool {
		var err error
		b, err = os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
		return err == nil
	})
	return strings.SplitN(string(b), "\n", 2)[0]
}

func TestLibraryRequestedAutomaticallyAfterSync(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
	if !strings.HasPrefix(string(b), seq+"\nscan\n") {
		t.Errorf("request %q", b)
	}
	time.Sleep(100 * time.Millisecond)
	d.do("sync") // opening the page sends nothing more
	if got := d.libReqSeq(t); got != seq {
		t.Errorf("request rewritten: %s -> %s", seq, got)
	}
}

func TestNoLibraryRequestWhenNothingChanged(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	d.syncDone(plan.Plan{})
	if _, err := os.Stat(filepath.Join(d.tmp, "q2-jellyfin.library")); err == nil {
		t.Error("a request was written")
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	if _, err := os.Stat(filepath.Join(d.tmp, "q2-jellyfin.library")); err != nil {
		t.Error("a rewritten playlist sent nothing")
	}
}

func TestLibraryRowShowsPendingThenUpdated(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	r := d.do("sync")
	if r.Message != "refresh" {
		t.Errorf("pending without refresh: %+v", r)
	}
	if _, ok := find(r, "Updating Library…"); !ok {
		t.Fatalf("pending rows %q", titles(r))
	}
	os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.library.done"), []byte(seq+" ok\n"), 0o644)
	if _, ok := find(d.do("sync"), "Library updated"); !ok {
		t.Errorf("ok rows %q", titles(d.do("sync")))
	}
}

func TestLibraryRowShowsFirmwareError(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.library.done"), []byte(seq+" error scan busy\n"), 0o644)
	r := d.do("sync")
	if _, ok := find(r, "scan busy"); !ok {
		t.Errorf("error rows %q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Errorf("no Details: %q", titles(r))
	}
}

// A firmware that refuses a playlist name longer than it keeps answers "error Name too long: <name>"
// (issue #1): shown as any firmware error, with the message in Details too.
func TestLibraryRowShowsNameTooLong(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	msg := "Name too long: " + strings.Repeat("曲", 22)
	os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.library.done"), []byte(seq+" error "+msg+"\n"), 0o644)
	r := d.do("sync")
	if _, ok := find(r, msg); !ok {
		t.Errorf("error rows %q", titles(r))
	}
	det, ok := find(r, "Details")
	if !ok {
		t.Fatalf("no Details: %q", titles(r))
	}
	if _, ok := find(d.do("details", det.Context), "Error: "+msg); !ok {
		t.Errorf("details %q", titles(d.do("details", det.Context)))
	}
}

func TestLibraryRowNoDoubleRequest(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	d.do("library")
	d.do("library")
	if got := d.libReqSeq(t); got != seq {
		t.Errorf("second request: %s -> %s", seq, got)
	}
}

// A play still waiting for playMu when Sign Out is pressed never starts.
func TestSignOutDropsPendingPlay(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.playMu.Lock() // a previous start still running
	d.do("play", "album:al1 0")
	pressSignOut(d)
	time.Sleep(50 * time.Millisecond) // let an unguarded stop run first, as in the race
	d.playMu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if exists(d.stream.urlsFile()) {
		t.Fatal("stream started after sign-out")
	}
}

// A recorded playlist file whose name has an ellipsis is imported: the firmware refuses only a real
// ".." segment (I18).
func TestLibraryRequestKeepsDottedPlaylist(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	good, bad := filepath.Join(d.card, "_explaylist_data", "Mix.m3u"), filepath.Join(d.card, "_explaylist_data", "Wait... What.m3u")
	for _, f := range []string{good, bad} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
	}
	(&library.Manifest{Playlists: map[string]string{"Mix": good, "Wait... What": bad}}).Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json"))
	d.syncDone(plan.Plan{PlaylistsStale: true})
	b, err := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
	if err != nil || !strings.Contains(string(b), "\nscan\n") || !strings.Contains(string(b), good+"\n") || !strings.Contains(string(b), bad+"\n") {
		t.Fatalf("%q %v", b, err)
	}
}

// A sync paused because the server went away or the card filled says why, with Resume (I3).
func TestPausedSyncSaysWhy(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.client()
	for err, want := range map[error]string{syncer.ErrOffline: "Can't reach 127.0.0.1", syncer.ErrCardFull: "The card is full"} {
		r := d.syncRows(syncer.Snapshot{State: "paused", Err: err})
		if _, ok := find(r, want); !ok {
			t.Errorf("%v: %q", err, titles(r))
		}
		if _, ok := find(r, "Resume"); !ok {
			t.Errorf("%v: no Resume: %q", err, titles(r))
		}
	}
}

// The preview shows what a quality change adds or frees (M20).
func TestPreviewReplaceShowsSize(t *testing.T) {
	for want, p := range map[string]*plan.Plan{
		"Replace 2 songs · −5 MB": {Replace: make([]plan.Want, 2), ReplaceBytes: -5e6, Fits: true},
		"Replace 1 song · +3 MB":  {Replace: make([]plan.Want, 1), ReplaceBytes: 3e6, Fits: true},
	} {
		if r := previewRows(p, 512<<20, 10); !strings.Contains(strings.Join(titles(r), "|"), want) {
			t.Errorf("want %q: %q", want, titles(r))
		}
	}
}

// A sync that takes out more than half the songs on the card says so above Start Sync: an item the
// server lost, or a press too many on Remove from Sync, shouldn't empty the card unnoticed (audit
// 2026-11-16 #3).
func TestPreviewWarnsMostRemoved(t *testing.T) {
	const warn = "Removes most of the synced songs"
	for _, c := range []struct{ remove, onCard int }{{3, 4}, {2, 4}, {4, 4}, {1, 10}} {
		p := &plan.Plan{Fits: true}
		for range c.remove {
			p.Remove = append(p.Remove, &library.Entry{Size: 1000})
		}
		got := strings.Contains(strings.Join(titles(previewRows(p, 512<<20, c.onCard)), "|"), warn)
		if want := c.remove*2 > c.onCard; got != want {
			t.Errorf("%d of %d removed: warning %v", c.remove, c.onCard, got)
		}
	}
}

// putOnCard records songs (id -> item key) in the card's manifest, each with a file, as a sync
// leaves them.
func putOnCard(t *testing.T, d *tdaemon, songs map[string]string) {
	t.Helper()
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for id, key := range songs {
		p := filepath.Join(d.card, "Jellyfin", id+".flac")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		m.Songs[id] = &library.Entry{ID: id, Path: p, Size: 1000, Items: []string{key}, Quality: "original"}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

// An account that sees no music library (the server's access to it taken away) fails the preview:
// read as an empty library, every song on the card would be removed, and by a Hold or Sync
// Favourites without a press (audit 2026-11-16 #3).
func TestNoMusicLibraryFailsPreview(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, noMusic: true}, "", true)
	sel := selection.Set{Version: 1}
	sel.Toggle(selection.Item{Kind: selection.KindLibrary, ID: "all", Name: "Entire Library", Quality: "original"})
	if err := sel.Save(filepath.Join(d.dir, "selection.json")); err != nil {
		t.Fatal(err)
	}
	putOnCard(t, d, map[string]string{"al0.0": "library:all", "al0.1": "library:all", "al0.2": "library:all"})
	d.mu.Lock()
	d.autoStart = true // as Sync Favourites or a Hold leaves it
	d.mu.Unlock()
	if r := d.preview(t); !strings.Contains(strings.Join(titles(r), "|"), "No music library on the server for this account") {
		t.Fatalf("%q", titles(r))
	}
	d.bg.Wait()
	if st := d.runner.Snapshot().State; st != "idle" {
		t.Fatalf("sync %s", st)
	}
	if n := len(d.manifest().Songs); n != 3 {
		t.Fatalf("%d songs left on the card", n)
	}
}

// Sync Favourites (and a Hold, Resume or quality change, through the same autoStart) starts only a
// sync that removes nothing: one that would take songs off the card waits for Start Sync (audit
// 2026-11-16 #3).
func TestAutoStartWaitsOnRemovals(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3, favs: 2}, "", true)
	putOnCard(t, d, map[string]string{"al1.0": "album:al1"}) // on the card, no longer in Sync
	if m := d.do("syncfav").Message; m != "Added to Sync · 2 songs" {
		t.Fatalf("%q", m)
	}
	r := d.preview(t)
	d.bg.Wait()
	if st := d.runner.Snapshot().State; st != "idle" {
		t.Fatalf("sync %s with no Start", st)
	}
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("%q", titles(r))
	}
	if _, err := os.Stat(filepath.Join(d.card, "Jellyfin", "al1.0.flac")); err != nil {
		t.Fatal(err)
	}
}

// Swapping a synced playlist for the album holding the same songs needs no transfer; the songs'
// items are still recorded, so after a restart the album reads On Q2, not Waiting to sync (I12).
func TestItemsRecordedWithoutTransfer(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "playlist:pl1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	d.do("toggle", "album:al0")
	d.do("toggle", "playlist:pl1")
	d.do("check")
	d.preview(t)
	d2 := newTestDaemonOn(t, d.card, n, "", true) // a restart: nothing resolved yet
	if st := d2.view().status(selection.KindAlbum, "al0", 2); st != "On Q2" {
		t.Errorf("album after restart: %q", st)
	}
	if _, ok := find(d2.do("albums"), "Sync Now"); ok {
		t.Error("Sync Now after restart")
	}
}

// A sync stopped because the server refused the account (downloads turned off, R39) says why, in
// jellyfin.Message's words, the one place that copy lives (review G1, Minor 5).
func TestPausedRefusedSaysWhy(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.client()
	err := fmt.Errorf("%w: %w", syncer.ErrRefused, &jellyfin.APIError{Op: "download", HTTP: 200, Code: 50, Msg: "User not authorized"})
	want := jellyfin.Message(err, d.hostName())
	if r := d.syncRows(syncer.Snapshot{State: "paused", Err: err}); !strings.Contains(strings.Join(titles(r), "|"), "|"+want+"|Resume") {
		t.Errorf("want %q: %q", want, titles(r))
	}
}

// An artist in Sync fetches each album's songs, not each album again: the artist's album list has
// them already (audit 2026-11-16 #15). Songs keep the albums' order, and a compilation's songs
// still take the album's artist (M41).
func TestArtistSongsOneRequestPerAlbum(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2, various: true}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "artist:ar1")
	n.reqs.Store(0)
	d.preview(t)
	if a, all := n.albumReqs.Load(), n.reqs.Load(); a != 0 || all != 4 {
		t.Fatalf("%d album requests, %d in all", a, all)
	}
	d.mu.Lock()
	songs := d.resolved["artist:ar1"]
	d.mu.Unlock()
	var got []string
	for _, s := range songs {
		got = append(got, s.ID+" "+s.AlbumArtist)
	}
	want := "al0.0 Various Artists,al0.1 Various Artists,al1.0 Various Artists,al1.1 Various Artists"
	if strings.Join(got, ",") != want {
		t.Fatalf("%q", got)
	}
}

// Songs chosen one by one are looked up together; one the batch leaves out gets its own lookup
// (audit 0.8.1 round 3 #14).
func TestSongsResolvedInOneBatch(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2, leaveOut: "al1.0"}
	d := newTestDaemon(t, n, "", true)
	for _, id := range []string{"al0.0", "al0.1", "al1.0"} {
		d.do("toggle", "song:"+id)
	}
	n.songReqs.Store(0)
	d.preview(t)
	if b, s := n.batchReqs.Load(), n.songReqs.Load(); b != 1 || s != 1 {
		t.Fatalf("%d batch requests, %d song requests", b, s)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range []string{"al0.0", "al0.1", "al1.0"} {
		if s := d.resolved["song:"+id]; len(s) != 1 || s[0].ID != id {
			t.Errorf("%s: %+v", id, s)
		}
	}
}

// Albums and playlists chosen one by one are looked up together; one the batch leaves out gets its
// own lookup, and a batched compilation still syncs as one folder (audit 0.8.1 round 4 #12, M41).
func TestAlbumsAndPlaylistsResolvedInOneBatch(t *testing.T) {
	n := &nav{albums: 3, songsPer: 2, playlists: 2, various: true, leaveOut: "al2"}
	d := newTestDaemon(t, n, "", true)
	for _, k := range []string{"album:al0", "album:al1", "album:al2", "playlist:pl1", "playlist:pl2"} {
		d.do("toggle", k)
	}
	n.albumReqs.Store(0)
	n.playlistReqs.Store(0)
	d.preview(t)
	if b, a, p := n.batchReqs.Load(), n.albumReqs.Load(), n.playlistReqs.Load(); b != 1 || a != 1 || p != 0 {
		t.Fatalf("%d batch requests, %d album requests, %d playlist requests", b, a, p)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range []string{"album:al0", "album:al1", "album:al2", "playlist:pl1", "playlist:pl2"} {
		if s := d.resolved[k]; len(s) != 2 {
			t.Errorf("%s: %+v", k, s)
		}
	}
	if s := d.resolved["album:al0"]; s[0].AlbumArtist != "Various Artists" || s[1].AlbumArtist != "Various Artists" {
		t.Errorf("album artists %q %q", s[0].AlbumArtist, s[1].AlbumArtist)
	}
}

// Entire Library takes each playlist's name from the playlist list: no item request per playlist
// (audit 0.8.1 round 4 #11).
func TestEntireLibraryNoPlaylistItemRequests(t *testing.T) {
	n := &nav{albums: 2, songsPer: 1, playlists: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if p := n.playlistReqs.Load(); p != 0 {
		t.Errorf("%d playlist item requests", p)
	}
	if pl := d.daemon.preview.plan.Playlists; len(pl["Mix"]) != 1 || len(pl["Mix 2"]) != 1 {
		t.Errorf("playlists %v", pl)
	}
}

// userDataKey names the song and album lists whose user data a test watches: a playlist's songs,
// an album's songs, /Items' albums and /Items?ids=.
func userDataKey(r *http.Request) string {
	q := r.URL.Query()
	switch {
	case strings.HasPrefix(r.URL.Path, "/Playlists/") && strings.HasSuffix(r.URL.Path, "/Items"):
		return "playlist"
	case r.URL.Path != "/Items":
		return ""
	case q.Get("ids") != "":
		return "ids"
	case q.Get("includeItemTypes") == "Audio" && strings.HasPrefix(q.Get("parentId"), "al"):
		return "album"
	case q.Get("includeItemTypes") == "MusicAlbum":
		return "albums"
	}
	return ""
}

// The Sync check asks for no user data (hearts): not for playlists' or albums' songs, album pages
// or ids= lookups. An album's or playlist's page still does, for Now Playing's heart (audit 0.8.1
// round 6 speed 2).
func TestCheckAsksNoUserData(t *testing.T) {
	n := &nav{albums: 3, songsPer: 2, playlists: 2}
	d := newTestDaemon(t, n, "", true)
	for _, k := range []string{"album:al0", "playlist:pl1", "artist:ar1", "song:al2.0", "song:al2.1", "library:all"} {
		d.do("toggle", k)
	}
	n.userDataSent.Range(func(k, _ any) bool { // Add to Sync without a page looks the item up
		n.userDataSent.Delete(k)
		return true
	})
	d.preview(t)
	want := map[string]bool{"album false": true, "albums false": true, "ids false": true, "playlist false": true}
	n.userDataSent.Range(func(k, _ any) bool {
		if !want[k.(string)] {
			t.Errorf("check: %s", k)
		}
		delete(want, k.(string))
		return true
	})
	if len(want) != 0 {
		t.Errorf("check: no %v", want)
	}
	for _, k := range []string{"album:al1", "playlist:pl2"} {
		op, _, _ := strings.Cut(k, ":")
		d.do(op, k)
	}
	for _, k := range []string{"album true", "playlist true"} {
		if _, ok := n.userDataSent.Load(k); !ok {
			t.Errorf("pages: no %q", k)
		}
	}
}

// On a server without OpenSubsonic's displayAlbumArtist, a compilation's songs take the album's
// artist, so it syncs as one folder, not one per track artist (M41).
func TestCompilationOneFolder(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, various: true}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.mu.Lock()
	p := d.daemon.preview.plan
	d.mu.Unlock()
	if len(p.Add) != 3 {
		t.Fatalf("%+v", p)
	}
	for _, w := range p.Add {
		if !strings.HasPrefix(w.Path, filepath.Join(d.card, "Jellyfin", "Various Artists")+"/") {
			t.Errorf("%s", w.Path)
		}
	}
}

// syncPlaylist runs a sync of playlist Mix to done on a daemon with cap library.
func syncPlaylist(t *testing.T) *tdaemon {
	t.Helper()
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "library", true)
	d.do("toggle", "playlist:pl1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	return d
}

// libReq is the library request on file: its seq and its other lines.
func (d *tdaemon) libReq(t *testing.T) (string, []string) {
	t.Helper()
	d.libReqSeq(t)
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	return lines[0], lines[1:]
}

func (d *tdaemon) answer(seq string, lines ...string) {
	os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.library.done"), []byte(strings.Join(append([]string{seq}, lines...), "\n")+"\n"), 0o644)
}

// R35: a playlist the app hasn't imported yet is sent as "add", which never replaces a playlist
// the user made; once the firmware confirms it, it is the app's own and later requests replace it.
func TestLibraryAddsThenImports(t *testing.T) {
	d := syncPlaylist(t)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	seq, lines := d.libReq(t)
	if strings.Join(lines, "|") != "scan|add "+mix {
		t.Fatalf("request %q", lines)
	}
	d.answer(seq + " ok")
	if _, ok := find(d.do("sync"), "Library updated"); !ok {
		t.Fatalf("%q", titles(d.do("sync")))
	}
	if !d.manifest().Imported[mix] {
		t.Fatal("not recorded as imported")
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	if seq2, lines := d.libReq(t); seq2 == seq || strings.Join(lines, "|") != "scan|import "+mix {
		t.Fatalf("next request %s %q", seq2, lines)
	}
}

// R35: a playlist taken out of Sync is no longer the app's: the user may delete the Q2's copy and
// make their own of that name. Added back, it goes as "add", which never replaces theirs.
func TestLibraryReaddedPlaylistAdds(t *testing.T) {
	d := syncPlaylist(t)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.tick()
	if !d.manifest().Imported[mix] {
		t.Fatal("not recorded as imported")
	}
	d.do("toggle", "playlist:pl1")
	d.do("check")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" && !exists(mix) })
	if d.manifest().Imported[mix] {
		t.Fatal("still recorded as imported")
	}
	var seq2 string
	eventually(t, "the next request", func() bool { seq2, _ = d.libReq(t); return seq2 != seq })
	d.answer(seq2 + " ok")
	d.tick()
	d.do("toggle", "playlist:pl1")
	d.do("check")
	d.preview(t)
	d.do("start")
	var lines []string
	eventually(t, "the request adding it", func() bool {
		var seq3 string
		seq3, lines = d.libReq(t)
		return seq3 != seq2
	})
	if strings.Join(lines, "|") != "scan|add "+mix {
		t.Fatalf("request %q", lines)
	}
}

// A playlist added back twice while the Q2 keeps its copies: the second time the Q2 has
// "Mix (Jellyfin)" too, so the clash lasts, and its message names the next step. A playlist
// renamed in the same answer still gets its follow-up, whose own clash is counted; once the Q2's
// copies are gone, the next sync adds them.
func TestLibrarySecondReaddClash(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2, playlists: 2}, "library", true)
	d.do("toggle", "playlist:pl1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	dir := filepath.Join(d.card, "_explaylist_data")
	mix, mixS := filepath.Join(dir, "Mix.m3u"), filepath.Join(dir, "Mix (Jellyfin).m3u")
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.tick()
	// readd takes the playlist (now at f) out of Sync, syncs, adds it back and syncs.
	readd := func(seq, f string) string {
		d.do("toggle", "playlist:pl1")
		d.do("check")
		d.preview(t)
		d.do("start")
		eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" && !exists(f) })
		var seq2 string
		eventually(t, "the next request", func() bool { seq2, _ = d.libReq(t); return seq2 != seq })
		d.answer(seq2 + " ok")
		d.tick()
		d.do("toggle", "playlist:pl1")
		d.do("check")
		d.preview(t)
		d.do("start")
		var seq3 string
		var lines []string
		eventually(t, "the request adding it", func() bool { seq3, lines = d.libReq(t); return seq3 != seq2 })
		if strings.Join(lines, "|") != "scan|add "+mix {
			t.Fatalf("request %q", lines)
		}
		d.answer(seq3+" ok", "exists "+mix)
		d.tick()
		var seq4 string
		eventually(t, "the follow-up", func() bool { seq4, lines = d.libReq(t); return seq4 != seq3 })
		if strings.Join(lines, "|") != "add "+mixS {
			t.Fatalf("follow-up %q", lines)
		}
		return seq4
	}
	seq = readd(seq, mix)
	d.answer(seq + " ok")
	d.tick()
	if !d.manifest().Imported[mixS] {
		t.Fatal("Mix (Jellyfin) not recorded as imported")
	}
	seq = readd(seq, mixS)
	d.answer(seq+" ok", "exists "+mixS)
	r := d.do("sync")
	if _, ok := find(r, "Couldn't import Mix (Jellyfin): the Q2 has a playlist with that name. Delete it in Library → Playlists, then Check Again"); !ok {
		t.Fatalf("%q", titles(r))
	}
	if m := d.manifest(); m.Playlists["Mix"] != mixS || m.Imported[mixS] {
		t.Fatalf("%+v %+v", m.Playlists, m.Imported)
	}

	mix2, mix2S := filepath.Join(dir, "Mix 2.m3u"), filepath.Join(dir, "Mix 2 (Jellyfin).m3u")
	d.do("toggle", "playlist:pl2")
	d.do("check")
	d.preview(t)
	d.do("start")
	seq2 := seq
	eventually(t, "the request adding Mix 2", func() bool { seq2, _ = d.libReq(t); return seq2 != seq })
	d.answer(seq2+" ok", "exists "+mix2, "exists "+mixS)
	d.tick()
	seq3, lines := d.libReq(t)
	if seq3 == seq2 || strings.Join(lines, "|") != "add "+mix2S {
		t.Fatalf("follow-up %s %q", seq3, lines)
	}
	d.answer(seq3+" ok", "exists "+mix2S)
	r = d.do("sync")
	if _, ok := find(r, "Couldn't import Mix (Jellyfin) and 1 more: the Q2 has playlists with those names. Delete them in Library → Playlists, then Check Again"); !ok {
		t.Fatalf("%q", titles(r))
	}

	d.do("check")
	r = d.preview(t)
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("no Start Sync after the clash: %q", titles(r))
	}
	d.do("start")
	seq4 := seq3
	eventually(t, "the request adding them again", func() bool { seq4, lines = d.libReq(t); return seq4 != seq3 })
	if l := strings.Join(lines, "|"); !strings.Contains(l, "add "+mixS) || !strings.Contains(l, "add "+mix2S) {
		t.Fatalf("request %q", lines)
	}
	d.answer(seq4 + " ok")
	d.tick()
	if m := d.manifest(); !m.Imported[mixS] || !m.Imported[mix2S] {
		t.Fatalf("%+v", m.Imported)
	}
}

// R35: an answer taken after the sync that removed the playlist doesn't mark it as the app's again.
func TestLibraryLateAnswerSkipsRemovedPlaylist(t *testing.T) {
	d := syncPlaylist(t)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	seq, _ := d.libReq(t)
	d.do("toggle", "playlist:pl1")
	d.do("check")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" && !exists(mix) })
	d.answer(seq + " ok")
	eventually(t, "the answer taken", func() bool { d.tick(); s, _ := d.libReq(t); return s != seq })
	if d.manifest().Imported[mix] {
		t.Fatal("recorded as imported after its removal")
	}
}

// A playlist file an older version recorded under a name longer than the Q2 keeps is never sent
// (issue #1): the Q2 would add another copy, or refuse the whole request. The next sync moves it.
func TestLibraryNeverSendsLongName(t *testing.T) {
	d := syncPlaylist(t)
	dir := filepath.Join(d.card, "_explaylist_data")
	mix := filepath.Join(dir, "Mix.m3u")
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.do("sync")
	imp, add := filepath.Join(dir, strings.Repeat("曲", 30)+".m3u"), filepath.Join(dir, strings.Repeat("Песня", 10)+".m3u")
	m := d.manifest()
	m.Playlists["Long 1"], m.Playlists["Long 2"], m.Imported[imp] = imp, add, true
	for _, f := range []string{imp, add} {
		os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	seq2, lines := d.libReq(t)
	for _, l := range lines {
		if _, f, ok := strings.Cut(l, " "); ok && len(strings.TrimSuffix(filepath.Base(f), ".m3u")) > 63 {
			t.Errorf("sent %q", l)
		}
	}
	if seq2 == seq || strings.Join(lines, "|") != "scan|import "+mix {
		t.Fatalf("request %s %q", seq2, lines)
	}
}

// R35: a playlist of the same name already on the Q2 (the user's own) is never replaced: the file
// is renamed "Mix (Jellyfin).m3u" and added again; a second clash is an error with Details.
func TestLibraryExistsRenamesThenClash(t *testing.T) {
	d := syncPlaylist(t)
	dir := filepath.Join(d.card, "_explaylist_data")
	mix, mixS := filepath.Join(dir, "Mix.m3u"), filepath.Join(dir, "Mix (Jellyfin).m3u")
	seq, _ := d.libReq(t)
	d.answer(seq+" ok", "exists "+mix)
	if _, ok := find(d.do("sync"), "Updating Library…"); !ok {
		t.Fatalf("%q", titles(d.do("sync")))
	}
	seq2, lines := d.libReq(t)
	if seq2 == seq || strings.Join(lines, "|") != "add "+mixS {
		t.Fatalf("follow-up %s %q", seq2, lines)
	}
	if _, err := os.Stat(mix); !os.IsNotExist(err) {
		t.Error("Mix.m3u kept")
	}
	if m := d.manifest(); m.Playlists["Mix"] != mixS || m.Imported[mix] {
		t.Fatalf("%+v %+v", m.Playlists, m.Imported)
	}
	d.answer(seq2+" ok", "exists "+mixS)
	r := d.do("sync")
	if _, ok := find(r, "Couldn't import Mix (Jellyfin)"); !ok {
		t.Fatalf("%q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Fatalf("no Details: %q", titles(r))
	}
}

// An answer with nothing new to record is taken from the manifest copy pages share: the file isn't
// loaded again under d.mu (round 2 speed 3). The file here is unreadable but the same size and
// time as the copy, so only a load would see it.
func TestLibraryAnswerSkipsLoad(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.do("sync")
	d.syncDone(plan.Plan{PlaylistsStale: true})
	seq2, lines := d.libReq(t)
	if seq2 == seq || len(lines) != 2 || !strings.HasPrefix(lines[1], "import ") {
		t.Fatalf("request %s %q", seq2, lines)
	}
	if _, err := d.manifestErr(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d.card, ".jellyfin", "manifest.json")
	st, _ := os.Stat(p)
	os.WriteFile(p, []byte(strings.Repeat("x", int(st.Size()))), 0o644)
	os.Chtimes(p, st.ModTime(), st.ModTime())
	d.answer(seq2 + " ok")
	r := d.do("sync")
	if _, ok := find(r, "Library updated"); !ok {
		t.Fatalf("%q", titles(r))
	}
}

// An answer that comes while a sync's OnDone runs waits for the first tick after it: the tick would
// load the whole manifest under d.mu beside syncDone's own read (round 5 speed 1). The request
// wanted meanwhile is sent once the answer is taken.
func TestLibraryAnswerWaitsForOnDone(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.do("sync")
	d.syncDone(plan.Plan{PlaylistsStale: true})
	seq2, lines := d.libReq(t)
	if seq2 == seq || len(lines) != 2 || !strings.HasPrefix(lines[1], "import ") {
		t.Fatalf("request %s %q", seq2, lines)
	}
	hold, in := make(chan struct{}), make(chan struct{})
	d.runner.OnDone = func(plan.Plan) { close(in); <-hold; d.syncDone(plan.Plan{PlaylistsStale: true}) }
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	<-in
	d.answer(seq2 + " ok")
	poll := func() (int64, bool) { d.mu.Lock(); defer d.mu.Unlock(); d.libPoll(); return d.libTaken, d.libAgain }
	if taken, _ := poll(); strconv.FormatInt(taken, 10) == seq2 {
		t.Fatal("answer taken during OnDone")
	}
	close(hold)
	eventually(t, "OnDone to return", func() bool { return !d.runner.Busy() })
	if taken, again := poll(); strconv.FormatInt(taken, 10) != seq2 || again {
		t.Fatalf("answer not taken after OnDone: taken %d, again %v", taken, again)
	}
	if seq3, _ := d.libReq(t); seq3 == seq2 {
		t.Fatal("no new request after the answer")
	}
}

// One request at a time: one wanted while another awaits its answer is sent once that comes, so
// no answer (and no import to record) is lost.
func TestLibraryOneRequestAtATime(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	d.syncDone(plan.Plan{PlaylistsStale: true})
	if seq2, _ := d.libReq(t); seq2 != seq {
		t.Fatal("request replaced before its answer")
	}
	d.answer(seq + " ok")
	d.tick()
	if seq2, lines := d.libReq(t); seq2 == seq || !strings.Contains(strings.Join(lines, "|"), "import ") {
		t.Fatalf("next request %s %q", seq2, lines)
	}
}

// The answer is taken without the Sync page, and by a later run of the app (the request on file
// says what was sent).
func TestLibraryAnswerTakenByTick(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	os.MkdirAll(filepath.Dir(mix), 0o755)
	os.WriteFile(mix, []byte("#EXTM3U\n"), 0o644)
	(&library.Manifest{Playlists: map[string]string{"Mix": mix}, Imported: map[string]bool{}}).Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json"))
	os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.library"), []byte("41\nscan\nadd "+mix+"\n"), 0o644)
	d.answer("41 ok")
	d.tick()
	if !d.manifest().Imported[mix] {
		t.Fatal("answer not taken")
	}
}

// M1: Account shows the server without a user:password@ part.
func TestAccountHidesServerPassword(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	if err := creds.Save(d.dir, jellyfin.Creds{Server: "https://alice:proxySecret42@music.example/sub", User: "u", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	r := d.do("account")
	if len(r.Rows) == 0 || r.Rows[0].Subtitle != "https://music.example/sub" {
		t.Fatalf("rows %+v", r.Rows)
	}
	r.Rows = append(r.Rows, d.do("server").Rows...)
	for _, row := range r.Rows {
		if strings.Contains(row.Title+row.Subtitle, "proxySecret42") {
			t.Fatalf("password on screen: %+v", row)
		}
	}
}

// Account is the server, who is signed in and Sign Out; what is stored on the card, and the tip
// to use a server user just for the Q2, are in the README instead (I23 moved there).
func TestAccountRows(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	r := d.do("account")
	if got := strings.Join(titles(r)[1:], "|"); got != "Signed in as:|Sign Out" {
		t.Fatalf("%q", titles(r))
	}
	// A Q2 signed in with an API key before API key sign-in was turned off is told what it keeps.
	d.signOut()
	creds.Save(d.dir, jellyfin.Creds{Server: "https://music.example", User: "u", Token: "k", UserID: "u1", Kind: jellyfin.KindAPIKey})
	r = d.do("account")
	if got := strings.Join(titles(r)[1:], "|"); got != "Signed in as:|An admin-level API key is stored on this card|Sign Out" {
		t.Fatalf("key sign-in: %q", titles(r))
	}
}

// Each Details row names what failed: the server's answer when there was one, else its own name
// ("" for a list, "sync" for a sync), with its time and song.
func TestDetailsNames(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	t0 := time.Unix(1_000_000, 0)
	stoppedCtx := func(err error) string {
		r := d.stoppedRows(syncer.Snapshot{State: "error", Err: err, ErrAt: t0})
		return r.Rows[len(r.Rows)-1].Context
	}
	listCtx := func(err error) string {
		f := strings.SplitN(d.errRows(err).Rows[1].Context, "|", 5)
		f[3] = "now" // the time of the list's failure
		return strings.Join(f, "|")
	}
	api := &jellyfin.APIError{Op: "download", HTTP: 500}
	for _, c := range []struct {
		ctx, want string
	}{
		{listCtx(io.ErrUnexpectedEOF), "|0|0|now|"},
		{listCtx(api), "download|500|0|now|"},
		{stoppedCtx(os.ErrPermission), "sync|0|0|1000000||"},
		{stoppedCtx(api), "download|500|0|1000000||"},
		{stoppedCtx(nil), "sync|0|0|1000000|"},
	} {
		if !strings.HasPrefix(c.ctx, c.want) {
			t.Errorf("%q, want %q…", c.ctx, c.want)
		}
	}
}

// Songs an earlier run streamed are deleted when the app starts (M26).
func TestStreamFolderEmptiedAtStart(t *testing.T) {
	card := t.TempDir()
	old := filepath.Join(card, ".jellyfin", "stream", "0003 - Song [x].flac")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("x"), 0o644)
	newTestDaemonOn(t, card, &nav{}, "", true)
	if _, err := os.Stat(old); err == nil {
		t.Fatal("left on the card")
	}
}

// The end of a sync counts as activity: an unattended sync's summary stays idleAfter (I15).
func TestStaysUpAfterSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "library", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.mu.Lock()
	d.lastReq = time.Now().Add(-time.Hour) // an unattended sync: no request for an hour
	d.mu.Unlock()
	close(n.block)
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	if d.idle(time.Now()) {
		t.Fatal("exits the moment the sync ends")
	}
	if d.idle(time.Now().Add(idleAfter - time.Minute)) {
		t.Fatal("exits before idleAfter")
	}
}

// After a sync the app exits idleAfter on, its library request answered or not: the firmware
// holds one while anything plays, and a running app keeps Smart Wi-Fi's radio on. The firmware
// answers with the app gone, and the next start takes the answer (I15).
func TestExitsBeforeLibraryAnswer(t *testing.T) {
	d := syncAlbum(t)
	d.libReqSeq(t)
	if !d.idle(time.Now().Add(idleAfter)) {
		t.Fatal("an unanswered library request keeps the app running")
	}
}

// While the app runs, a library request awaiting its answer has it tick every second, so its
// follow-ups go at once, for libWait only.
func TestUnansweredLibraryRequestIsBounded(t *testing.T) {
	d := syncAlbum(t)
	d.libReqSeq(t)
	if !d.urgent() {
		t.Fatal("no tick every second while the answer is awaited")
	}
	d.mu.Lock()
	d.lib.seq = time.Now().Add(-libWait).UnixNano()
	d.mu.Unlock()
	if d.urgent() {
		t.Error("an unanswered library request ticks every second past libWait")
	}
}

// restart is the app's next run on d's card. keepTmp: the Q2 stayed on, so the library request
// is still in /tmp; else /tmp starts empty, as after the Q2 powered off (/tmp is RAM).
func (d *tdaemon) restart(t *testing.T, keepTmp bool) *tdaemon {
	t.Helper()
	tmp := t.TempDir()
	if keepTmp {
		b, err := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(tmp, "q2-jellyfin.library"), b, 0o644)
	}
	d2 := newTestDaemonIn(t, d.card, tmp, &nav{albums: 1, songsPer: 2}, "library", true)
	d2.bg.Wait() // libStart
	return d2
}

// The app exited before the firmware answered; the answer comes, and the next start takes it: the
// playlist added is the app's own.
func TestLibraryAnsweredAfterExit(t *testing.T) {
	d := syncPlaylist(t)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	seq, _ := d.libReq(t)
	d2 := d.restart(t, true)
	d2.answer(seq + " ok")
	d2.tick()
	if !d2.manifest().Imported[mix] {
		t.Fatal("answer not taken")
	}
}

// The Q2 powered off before the firmware ran the request (it waits while music plays): the next
// start sends it again, so the new songs and playlists still reach the Library.
func TestLibraryRequestLostResent(t *testing.T) {
	d := syncPlaylist(t)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	d.libReq(t)
	d2 := d.restart(t, false)
	if _, err := os.Stat(filepath.Join(d2.tmp, "q2-jellyfin.library")); err != nil {
		t.Fatal("the lost request is not sent again")
	}
	if _, lines := d2.libReq(t); strings.Join(lines, "|") != "scan|add "+mix {
		t.Fatalf("request %q", lines)
	}
}

// The start doesn't wait for the resend of a lost request (libStart), which reads the whole
// manifest: the socket listens at once. Here the manifest is a FIFO, so reading it blocks until fed.
func TestLibraryResentInBackground(t *testing.T) {
	d := syncPlaylist(t)
	d.libReq(t)
	mf := filepath.Join(d.card, ".jellyfin", "manifest.json")
	b, _ := os.ReadFile(mf)
	os.Remove(mf)
	if err := syscall.Mkfifo(mf, 0o644); err != nil {
		t.Fatal(err)
	}
	// feed writes the manifest once libStart opens it, a plain file taking the FIFO's place first for
	// sendLibrary's read after it (the FIFO's time changed); non-blocking, so it fails, not hangs, if
	// never read.
	feed := func() error {
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			f, err := os.OpenFile(mf, os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				os.Remove(mf)
				os.WriteFile(mf, b, 0o644)
				_, err = f.Write(b)
				f.Close()
				return err
			}
			if time.Now().After(end) {
				return fmt.Errorf("the manifest is never read: %v", err)
			}
		}
	}
	late := make(chan error, 1)
	timer := time.AfterFunc(5*time.Second, func() { late <- feed() })
	tmp := t.TempDir()
	d2 := newTestDaemonIn(t, d.card, tmp, &nav{albums: 1, songsPer: 2}, "library", true)
	if !timer.Stop() {
		<-late
		t.Fatal("the start waits for the library request to be sent again")
	}
	if err := feed(); err != nil {
		t.Fatal(err)
	}
	d2.bg.Wait()
	if _, err := os.Stat(filepath.Join(tmp, "q2-jellyfin.library")); err != nil {
		t.Fatal("the lost request is not sent again")
	}
}

// A request this run sent before libStart ran names every playlist: libStart reading library.more
// late adds no follow-up request.
func TestLibStartAfterThisRunsRequest(t *testing.T) {
	d := syncPlaylist(t)
	d.libReq(t)
	d2 := d.restart(t, false)
	seq, _ := d2.libReq(t)
	os.WriteFile(d2.libMore(), []byte("more"), 0o644)
	d2.libStart()
	d2.answer(seq + " ok")
	d2.tick()
	if seq2, _ := d2.libReq(t); seq2 != seq {
		t.Fatal("an extra request after libStart")
	}
}

// The earlier run's request was answered while the app was closed, and library.more says "more":
// the first tick takes the answer before libStart runs, and the full follow-up is sent anyway.
// libStart is held off by d.mu, taken before it can run (one P, so the go statement doesn't run it
// at once), and, where it reads the manifest first, by the manifest as a FIFO, which waits until fed.
// The answer has an "exists" line, so libPoll records it without waiting for that read.
func TestLibraryMoreReadBeforeFirstTick(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	os.WriteFile(d.libMore(), []byte("more"), 0o644)
	tmp := t.TempDir()
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
	os.WriteFile(filepath.Join(tmp, "q2-jellyfin.library"), b, 0o644)
	os.WriteFile(filepath.Join(tmp, "q2-jellyfin.library.done"), []byte(seq+" ok\nexists /mnt/sdcard/Playlists/Other.m3u\n"), 0o644)
	os.WriteFile(filepath.Join(tmp, "q2-addon.caps"), []byte("library\n"), 0o644)
	mf := filepath.Join(d.card, ".jellyfin", "manifest.json")
	man, _ := os.ReadFile(mf)
	os.Remove(mf)
	if err := syscall.Mkfifo(mf, 0o644); err != nil {
		t.Fatal(err)
	}
	procs := runtime.GOMAXPROCS(1)
	d2 := newDaemon(filepath.Join(d.card, ".jellyfin"), d.card, tmp)
	d2.mu.Lock()
	runtime.GOMAXPROCS(procs)
	t.Cleanup(d2.bg.Wait) // its log.txt is written before the TempDir goes
	// Hold libStart in its read, if it reads the manifest: a writer that opens is the FIFO's reader
	// waiting. The tick's reads get a plain file.
	var w *os.File
	for end := time.Now().Add(time.Second); w == nil && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		w, _ = os.OpenFile(mf, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	}
	os.Remove(mf)
	os.WriteFile(mf, man, 0o644)
	done := make(chan struct{})
	go func() { d2.libPoll(); d2.mu.Unlock(); close(done) }() // the first tick's
	// its follow-up may wait for libStart's read (one manifest read at a time)
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	if w != nil {
		w.Write(man)
		w.Close()
	}
	<-done
	d2.bg.Wait()
	b, _ = os.ReadFile(filepath.Join(tmp, "q2-jellyfin.library"))
	if lines := strings.Split(string(b), "\n"); lines[0] == seq || len(lines) < 2 || lines[1] != "scan" {
		t.Fatalf("no full follow-up request: %q", lines)
	}
}

// The app is stopped while syncDone reads the manifest, before the request is written ('Sync
// complete' already shows): library.more is on the card, so the next start sends the request. The
// manifest is a FIFO, so syncDone's read waits until fed.
func TestLibraryStopDuringSyncDone(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	eventually(t, "the sync's OnDone to return", func() bool { return !d.runner.Busy() })
	d.answer(seq + " ok")
	d.tick()
	if exists(d.libMore()) {
		t.Fatal("the answer is not taken")
	}
	d.bg.Wait() // the preview after the sync reads the manifest too
	mf := filepath.Join(d.card, ".jellyfin", "manifest.json")
	man, _ := os.ReadFile(mf)
	os.Remove(mf)
	if err := syscall.Mkfifo(mf, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { d.syncDone(plan.Plan{PlaylistsStale: true}); close(done) }()
	// a writer that opens is syncDone's read waiting; it gets nothing until fed
	var w *os.File
	for end := time.Now().Add(5 * time.Second); w == nil; time.Sleep(10 * time.Millisecond) {
		if w, _ = os.OpenFile(mf, os.O_WRONLY|syscall.O_NONBLOCK, 0); w == nil && time.Now().After(end) {
			t.Fatal("syncDone never reads the manifest")
		}
	}
	os.Remove(mf)
	os.WriteFile(mf, man, 0o644)
	d2 := d.restart(t, false) // the earlier run killed here
	w.Write(man)
	w.Close()
	<-done
	if _, lines := d2.libReq(t); len(lines) == 0 || lines[0] != "scan" {
		t.Fatalf("request %q", lines)
	}
}

// Once its answer is taken, nothing is owed: the next start, /tmp emptied or not, sends nothing.
func TestLibraryAnsweredNotResent(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.tick()
	if exists(d.libMore()) {
		t.Fatal("library.more kept once answered")
	}
	d2 := d.restart(t, false)
	if _, err := os.Stat(filepath.Join(d2.tmp, "q2-jellyfin.library")); err == nil {
		t.Fatal("an answered request sent again")
	}
}

// A request still in /tmp awaits its answer: the next start doesn't send another.
func TestLibraryWaitingNotResent(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	d2 := d.restart(t, true)
	if seq2, _ := d2.libReq(t); seq2 != seq {
		t.Fatalf("request rewritten: %s -> %s", seq, seq2)
	}
}

// The answer is lost (the Q2 powered off before Jellyfin was opened again), but the firmware had
// added Mix: the next start sends the request again and the firmware answers "exists". Only the lost
// answer could tell that from a playlist the user made, so Mix is taken for the user's, as any
// "exists" is: renamed "Mix (Jellyfin)" and added, a second copy, but the Q2's Mix never replaced.
func TestLibraryAnswerLostNeverReplaces(t *testing.T) {
	d := syncPlaylist(t)
	dir := filepath.Join(d.card, "_explaylist_data")
	mix, mixJ := filepath.Join(dir, "Mix.m3u"), filepath.Join(dir, "Mix (Jellyfin).m3u")
	d.libReq(t)
	d2 := d.restart(t, false)
	seq, lines := d2.libReq(t)
	if strings.Join(lines, "|") != "scan|add "+mix {
		t.Fatalf("request %q", lines)
	}
	d2.answer(seq+" ok", "exists "+mix)
	d2.tick()
	if m := d2.manifest(); m.Playlists["Mix"] != mixJ || m.Imported[mix] {
		t.Fatalf("%+v %+v", m.Playlists, m.Imported)
	}
	if seq2, lines := d2.libReq(t); seq2 == seq || strings.Join(lines, "|") != "add "+mixJ {
		t.Fatalf("follow-up %s %q", seq2, lines)
	}
}

// After a lost answer, a playlist the app never sent, meeting one the user made, is renamed
// "<name> (Jellyfin)" and added again, and one the firmware added is the app's (R35).
func TestLibraryAnswerLostUsersPlaylist(t *testing.T) {
	d := syncPlaylist(t)
	dir := filepath.Join(d.card, "_explaylist_data")
	mix, rock, rockJ := filepath.Join(dir, "Mix.m3u"), filepath.Join(dir, "Rock.m3u"), filepath.Join(dir, "Rock (Jellyfin).m3u")
	d.libReq(t)
	os.WriteFile(rock, []byte("#EXTM3U\n"), 0o644)
	m := d.manifest()
	m.Playlists["Rock"] = rock
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d2 := d.restart(t, false)
	seq, lines := d2.libReq(t)
	if strings.Join(lines, "|") != "scan|add "+mix+"|add "+rock {
		t.Fatalf("request %q", lines)
	}
	d2.answer(seq+" ok", "exists "+rock)
	d2.tick()
	if m := d2.manifest(); m.Playlists["Rock"] != rockJ || m.Imported[rock] || !m.Imported[mix] {
		t.Fatalf("%+v %+v", m.Playlists, m.Imported)
	}
	if seq2, lines := d2.libReq(t); seq2 == seq || strings.Join(lines, "|") != "add "+rockJ {
		t.Fatalf("follow-up %s %q", seq2, lines)
	}
}

// manyPlaylists records 400 playlists, more than one library request holds, half of them imported.
func (d *tdaemon) manyPlaylists(t *testing.T) (want map[string]bool) {
	t.Helper()
	m := &library.Manifest{Playlists: map[string]string{}, Imported: map[string]bool{}}
	want = map[string]bool{}
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("%03d %s", i, strings.Repeat("x", 59)) // 63 bytes, the most the Q2 keeps
		f := filepath.Join(d.card, "_explaylist_data", name+".m3u")
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
		m.Playlists[name], m.Imported[f] = f, i%2 == 0
		want[f] = true
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	return want
}

// More playlists than one request holds (the firmware takes 32 KiB) go in several: scan and the
// first in one, the rest in follow-ups, each sent once the one before is answered (I20).
func TestLibraryRequestSplit(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	want := d.manyPlaylists(t)
	d.syncDone(plan.Plan{PlaylistsStale: true})
	d.answerAll(t, want)
}

// A request that left playlists for a follow-up (I20) is answered after the app exited: the next
// start, taking the answer, sends them all again (library.more), not only at the next sync.
func TestLibraryRestAfterExit(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	want := d.manyPlaylists(t)
	d.syncDone(plan.Plan{PlaylistsStale: true})
	seq, _ := d.libReq(t)
	d2 := d.restart(t, true)
	d2.answer(seq + " ok")
	d2.tick()
	if seq2, _ := d2.libReq(t); seq2 == seq {
		t.Fatal("the rest is never sent")
	}
	d2.answerAll(t, want)
	if exists(d2.libMore()) {
		t.Error("library.more kept once all are sent")
	}
}

// answerAll answers the request on file and each follow-up, until the last: together they name
// every playlist of want once, each within the firmware's limit, scan in the first only.
func (d *tdaemon) answerAll(t *testing.T, want map[string]bool) {
	t.Helper()
	sent := map[string]bool{}
	for k := 0; ; k++ {
		b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.library"))
		if len(b) >= 32<<10-2 {
			t.Fatalf("request %d: %d bytes, the firmware's limit is 32 KiB", k, len(b))
		}
		seq, lines := d.libReq(t)
		if scan := len(lines) > 0 && lines[0] == "scan"; scan != (k == 0) {
			t.Fatalf("request %d: scan %v", k, scan)
		} else if scan {
			lines = lines[1:]
		}
		for _, l := range lines {
			_, p, _ := strings.Cut(l, " ")
			if sent[p] || !want[p] {
				t.Fatalf("request %d: %q", k, l)
			}
			sent[p] = true
		}
		d.answer(seq + " ok")
		d.tick()
		if d.lib.done {
			break
		}
		if k > 10 {
			t.Fatal("no end")
		}
	}
	if len(sent) != len(want) || !d.lib.ok {
		t.Fatalf("%d sent, ok %v", len(sent), d.lib.ok)
	}
}

// syncOK runs a sync of what is chosen to done.
func (d *tdaemon) syncOK(t *testing.T) {
	t.Helper()
	d.do("check")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
}

// A synced playlist that holds a song twice is On Q2, also after a restart (its server count has
// the duplicate), and so is a chosen empty one: no Sync Now for them (I13).
func TestPlaylistDuplicateOnQ2(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, dup: true}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "playlist:pl1")
	d.syncOK(t)
	for _, dd := range []*tdaemon{d, newTestDaemonOn(t, d.card, n, "", true)} {
		r := dd.do("playlists")
		if row, _ := find(r, "Mix"); row.Subtitle != "4 songs · On Q2" {
			t.Errorf("Mix %q", row.Subtitle)
		}
		if _, ok := find(r, "Sync Now"); ok {
			t.Error("Sync Now")
		}
	}
	e := newTestDaemon(t, &nav{albums: 1, songsPer: 0}, "", true)
	e.do("toggle", "playlist:pl1")
	e.syncOK(t)
	if row, _ := find(e.do("playlists"), "Mix"); row.Subtitle != "0 songs · On Q2" {
		t.Errorf("empty Mix %q", row.Subtitle)
	}
}

// Artists builds each row's status from one index of the manifest, not a walk of it per artist:
// thousands of artists over a big library still answer at once (I17).
func TestArtistsFastOnBigLibrary(t *testing.T) {
	d := newTestDaemon(t, &nav{artists: 4000}, "", true)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for i := 0; i < 40000; i++ {
		id := fmt.Sprintf("s%d", i)
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Jellyfin", id+".flac"), Size: 1,
			Items: []string{fmt.Sprintf("album:al%d", i/10), "library:all"}}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".jellyfin", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d.do("onq2") // the manifest read once
	start := time.Now()
	r := d.do("artists")
	took := time.Since(start)
	if took > time.Second || len(r.Rows) != 50 {
		t.Fatalf("%d rows in %v", len(r.Rows), took)
	}
	t.Logf("%v", took)
}

// On This Q2 pages like every list: past 50 chosen items, More… (I21).
func TestOnQ2Pages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 70, songsPer: 1}, "", true)
	sel := selection.Set{Version: 1}
	for i := 0; i < 70; i++ {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: fmt.Sprintf("al%d", i), Name: fmt.Sprintf("Album %03d", i), Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	r := d.do("onq2")
	if len(r.Rows) != 50 || r.Cursor == "" || r.Message != "pages" {
		t.Fatalf("%d rows, cursor %q, message %q", len(r.Rows), r.Cursor, r.Message)
	}
	if r = d.do("onq2", r.Cursor); len(r.Rows) != 20 || r.Rows[19].Title != "Album 069" {
		t.Fatalf("second page %q", titles(r))
	}
}

// No item has a Quality row: not a chosen song or Entire Library in On This Q2, nor an album's,
// artist's, playlist's or Favourites' page. Sync Quality is every item's.
func TestNoItemQuality(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "song:al0.1 album:al0 1")
	d.do("toggle", "favorites:all")
	pages := []q2spot.Reply{d.do("onq2"), d.do("favorites", favKey)}
	d.do("toggle", "song:al0.1 album:al0 1")
	d.do("toggle", "favorites:all")
	d.do("toggle", "album:al0")
	d.do("toggle", "artist:ar1")
	d.do("toggle", "playlist:pl1")
	pages = append(pages, d.do("album", "album:al0"), d.do("artist", "artist:ar1"), d.do("playlist", "playlist:pl1"))
	d.do("toggle", "library:all")
	pages = append(pages, d.do("onq2"))
	for i, r := range pages {
		for _, row := range r.Rows {
			if strings.HasPrefix(row.Title, "Quality") || row.URI == "quality" {
				t.Errorf("page %d: %+v in %q", i, row, titles(r))
			}
		}
	}
	if r := d.do("quality", "album:al0"); r.Status == "" || r.Status == q2spot.OK {
		t.Errorf("the quality page is gone: %+v", r)
	}
}

// A page whose rows are too big for one 64 KiB reply ends where the reply ends, and the next page
// starts there: no row is skipped (M28).
func TestPageCutsBySize(t *testing.T) {
	var rows []q2spot.Row
	for i := 0; i < 60; i++ {
		rows = append(rows, list("album", fmt.Sprintf("%02d", i)+strings.Repeat("%", 250), strings.Repeat("%", 255), "album:x"))
	}
	r := page(rows, "")
	if _, kept := r.Encode("4294967295"); kept != len(r.Rows) || r.Cursor != strconv.Itoa(len(r.Rows)) {
		t.Fatalf("%d rows, %d sent, cursor %q", len(r.Rows), kept, r.Cursor)
	}
	r.Rows = append([]q2spot.Row{list("sync", "Sync Now", "999 waiting", "")}, r.Rows...)
	if _, kept := r.Encode("4294967295"); kept != len(r.Rows) {
		t.Fatalf("with Sync Now: %d of %d sent", kept, len(r.Rows))
	}
}

// Failed Songs pages (M29).
func TestFailedSongsPages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 60, mediaErr: 70}, "", true)
	d.runner.Backoff = []time.Duration{time.Millisecond}
	d.do("toggle", "album:al0")
	d.syncOK(t)
	r := d.do("failed")
	if len(r.Rows) != 50 || r.Cursor == "" {
		t.Fatalf("%d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	if r = d.do("failed", r.Cursor); len(r.Rows) != 11 {
		t.Fatalf("second page %d rows", len(r.Rows))
	}
	if m := d.do("retry").Message; m != "Sync started again" {
		t.Errorf("retry %q", m)
	}
}

// Failed Songs lists the failures the runner keeps and says how many more failed.
func TestFailedSongsCapped(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 503, mediaErr: 70}, "", true)
	d.runner.Backoff = []time.Duration{time.Millisecond}
	d.do("toggle", "album:al0")
	d.syncOK(t)
	var rows []q2spot.Row
	// Q2 Pod loads at most 10 replies before More… (ADDON_PAGES).
	for r, n := d.do("failed"), 1; ; r, n = d.do("failed", r.Cursor), n+1 {
		rows = append(rows, r.Rows...)
		if r.Cursor == "" || n == 10 {
			break
		}
	}
	if len(rows) < 2 || rows[1].Title != "503 failed · the first 500 are listed" {
		t.Fatalf("%d rows, second %q", len(rows), rows[min(1, len(rows)-1)].Title)
	}
}

// A later page that fails while the firmware loads the list page by page is a failed reply, so the
// list keeps More… to try again; a first page still shows the message with Details (M30).
func TestLaterPageFails(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1, listErr: 70}, "", true)
	r := d.do("albums")
	if r.Status != "" || r.Cursor == "" {
		t.Fatalf("first page %+v", r)
	}
	if r = d.do("albums", r.Cursor); r.Status == "" || r.Status == q2spot.OK || len(r.Rows) != 0 {
		t.Fatalf("later page %+v", r)
	}
}

// A removed item and a quality change wait for a sync, with Sync Now (M35).
func TestSyncNowForRemovalAndQuality(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "album:al1")
	d.syncOK(t)
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Fatal("Sync Now after the sync")
	}
	d.do("toggle", "album:al0")
	d.do("setdefq", "opus-192") // Sync Quality is album:al1's too
	r := d.do("albums")
	if row, _ := find(r, "Sync Now"); row.Subtitle != "2 waiting" {
		t.Errorf("Sync Now %q in %q", row.Subtitle, titles(r))
	}
	for _, a := range []string{"Album 000", "Album 001"} {
		if row, _ := find(r, a); row.Subtitle != "Artist · Waiting to sync" {
			t.Errorf("%s %q", a, row.Subtitle)
		}
	}
}

// An artist that isn't chosen shows how much of it is on the Q2 through other items, once its page
// has been opened (M36).
func TestArtistPartialStatus(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	d.do("artist", "artist:ar1")
	if row, _ := find(d.do("artists"), "Artist"); row.Subtitle != "2 albums · 2 of 4 songs on Q2" {
		t.Errorf("artist %q", row.Subtitle)
	}
}

// With Entire Library on, an item's page says it is included that way instead of offering a
// removal that can't happen (M37).
func TestItemPageUnderLibrary(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al2")
	d.do("toggle", "library:all")
	for _, a := range []string{"album:al2", "album:al1"} {
		r := d.do("album", a)
		if r.Rows[0].Title == "Sync Now" {
			r.Rows = r.Rows[1:]
		}
		if r.Rows[0].Kind != "info" || r.Rows[0].Title != "Included via Entire Library" {
			t.Errorf("%s: %+v", a, r.Rows[0])
		}
	}
}

// A synced song whose file was deleted from the card (by hand, or the folder after removing the
// app) is planned again, not taken as on the card (I9).
func TestDeletedFileFetchedAgain(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	for _, e := range d.manifest().Songs {
		os.Remove(e.Path)
		break
	}
	d.do("check")
	r := d.preview(t)
	if _, ok := find(r, "+1 song · "); !ok {
		t.Fatalf("preview %q", titles(r))
	}
	if n := len(d.manifest().Songs); n != 1 {
		t.Errorf("%d songs recorded", n)
	}
}

// A playlist change that moves no song still syncs: a new one whose songs are on the card, and one
// taken out whose songs stay (I10).
func TestPlaylistOnlyChangeSyncs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "library", true)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	d.do("toggle", "album:al0")
	d.syncOK(t)
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.tick()
	d.do("toggle", "playlist:pl1")
	d.do("check")
	r := d.preview(t)
	if _, ok := find(r, "Update 1 playlist"); !ok {
		t.Fatalf("preview %q", titles(r))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if !exists(mix) {
		t.Fatal("Mix.m3u not written")
	}
	if r := d.do("sync"); afterOptions(r)[0].Title != "Sync complete · Playlists updated · 0 failed" {
		t.Errorf("done %q", titles(r))
	}
	if _, lines := d.libReq(t); strings.Join(lines, "|") != "scan|add "+mix {
		t.Errorf("library request %q", lines)
	}
	d.do("check")
	if _, ok := find(d.preview(t), "Everything is up to date"); !ok {
		t.Fatalf("after the sync %q", titles(d.preview(t)))
	}
	d.do("toggle", "playlist:pl1")
	d.do("check")
	if _, ok := find(d.preview(t), "Update 1 playlist"); !ok {
		t.Fatal("taking it out is not offered")
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" && !exists(mix) })
}

// A server that ignores the album list's offset ends Entire Library's listing once a page brings
// nothing new, instead of growing it until the memory runs out (M10).
func TestEntireLibraryPagingEnds(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 500, songsPer: 1, sameAlbums: true}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if n := len(d.daemon.preview.plan.Add); n != 500 {
		t.Errorf("Add %d, want 500", n)
	}
}

// A preview is reused for previewTTL (audit 18): opening Sync a few minutes later asks the server
// nothing, even for Entire Library; one older is computed again, and so is one after a selection
// change, Check Again or Sync Favourites, however new.
func TestPreviewReused(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	age := func(by time.Duration) {
		d.mu.Lock()
		d.daemon.preview.at = d.daemon.preview.at.Add(-by)
		d.mu.Unlock()
	}
	again := func(what string, want bool) {
		t.Helper()
		before := n.songListReqs.Load()
		d.preview(t)
		if got := n.songListReqs.Load() > before; got != want {
			t.Errorf("%s: fetched again %v, want %v", what, got, want)
		}
	}
	age(4 * time.Minute)
	again("4 minutes on", false)
	age(previewTTL)
	again("past previewTTL", true)
	d.do("check")
	again("Check Again", true)
	d.do("setdefq", "opus-160") // the selection's quality: a change (gen)
	again("a selection change", true)
	before := n.songListReqs.Load()
	d.do("syncfav")
	eventually(t, "Sync Favourites' preview", func() bool { return n.songListReqs.Load() > before })
}

// A heart given or taken on the Q2, once on the server, makes the next Sync compute a new preview
// when Favourites is in Sync, however new the old one is: an un-hearted song leaves at that sync.
// Entire Library alone keeps its preview: it would ask the server for every song again.
func TestPreviewAfterHeart(t *testing.T) {
	for _, c := range []struct {
		key  string
		want bool
	}{{favKey, true}, {"library:all", false}} {
		t.Run(c.key, func(t *testing.T) {
			d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, favs: 2}, "", true)
			d.do("toggle", c.key)
			d.preview(t)
			d.mu.Lock()
			p := d.daemon.preview
			d.mu.Unlock()
			d.favsChanged()
			d.preview(t)
			d.mu.Lock()
			again := d.daemon.preview != p
			d.mu.Unlock()
			if again != c.want {
				t.Errorf("a new preview %v, want %v", again, c.want)
			}
		})
	}
}

// A heart the server didn't take changed nothing there, so the Sync preview stays; one it took
// makes the next Sync open preview again (Favourites in Sync).
func TestPreviewAfterHeartSent(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, favs: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", favKey)
	same := func() bool {
		d.mu.Lock()
		p := d.daemon.preview
		d.mu.Unlock()
		d.preview(t)
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.daemon.preview == p
	}
	d.preview(t)
	d.rep.box.add(report{Kind: favKind, Item: "al0.0", On: true})
	n.favStatus.Store(503)
	d.sendNow(t)
	if !same() {
		t.Error("a heart that failed to send previewed again")
	}
	n.favStatus.Store(0)
	d.sendNow(t)
	if len(d.outbox()) != 0 || same() {
		t.Errorf("a heart sent: outbox %+v, preview kept", d.outbox())
	}
}

// Sign Out and Check Again cancel a preview still fetching (M11).
func TestPreviewCancelled(t *testing.T) {
	for _, op := range []string{"signout", "check"} {
		t.Run(op, func(t *testing.T) {
			n := &nav{albums: 1, songsPer: 1, holdAlbum: make(chan struct{})}
			d := newTestDaemon(t, n, "", true)
			sel := selection.Set{Version: 1}
			sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al0", Name: "Album 000", Quality: "original"})
			sel.Save(filepath.Join(d.dir, "selection.json"))
			d.do("sync")
			eventually(t, "the preview's request", func() bool { return n.albumReqs.Load() == 1 })
			if op == "signout" {
				d.do(op) // the first press only asks
			}
			d.do(op)
			eventually(t, "the request cancelled", func() bool { return n.albumGaveUp.Load() == 1 })
			close(n.holdAlbum)
		})
	}
}

// Sign Out cancels the sync: a later sign-in, maybe to another server, never resumes the old
// account's plan (M12).
func TestSignOutCancelsSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	pressSignOut(d)
	eventually(t, "the sync to stop", func() bool { return !d.runner.Busy() })
	if st := d.runner.Snapshot().State; st != "idle" {
		t.Fatalf("state %q", st)
	}
}

// A damaged selection.json says so, with Reset Selection, instead of reading as empty (M21).
func TestDamagedSelection(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	os.WriteFile(filepath.Join(d.dir, "selection.json"), []byte(`{"items":[{"kind":`), 0o644)
	for _, op := range []string{"onq2", "sync"} {
		r := d.do(op)
		if r.Rows[0].Title != "Selection file is damaged" {
			t.Fatalf("%s: %q", op, titles(r))
		}
	}
	if r := d.do("toggle", "album:al0"); !strings.Contains(r.Message, "Selection file is damaged") {
		t.Errorf("toggle %q", r.Message)
	}
	reset, ok := find(d.do("onq2"), "Reset Selection")
	if !ok || reset.Kind != "action" {
		t.Fatalf("reset %+v", reset)
	}
	d.do(reset.URI, reset.Context)
	if _, err := os.Stat(filepath.Join(d.dir, "selection.json.bad")); err != nil {
		t.Fatal("damaged file not kept aside")
	}
	if r := d.do("onq2"); r.Rows[0].Title != "Nothing chosen yet" {
		t.Fatalf("after reset %q", titles(r))
	}
}

// A sync stopped by a card error says what it means in a sentence, with Details (M34).
func TestSyncStoppedDetails(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	os.WriteFile(filepath.Join(d.card, ".jellyfin", "manifest.json"), []byte("{"), 0o644)
	d.do("start")
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "error" })
	r := d.do("sync")
	if afterOptions(r)[0].Title != "Sync stopped · Couldn't read the sync records on the card" {
		t.Errorf("rows %q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Errorf("no Details: %q", titles(r))
	}
	d.mu.Lock()
	d.gen = d.runGen + 1 // Sync changed after the sync started
	d.mu.Unlock()
	if row, _ := find(d.do("sync"), "Check Again"); row.Subtitle != "Sync changed since it started" {
		t.Errorf("Check Again subtitle %q", row.Subtitle)
	}
	if got := d.stopMessage(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}); got != "The card is full" {
		t.Errorf("ENOSPC: %q", got)
	}
	if got := d.stopMessage(&os.LinkError{Op: "rename", Old: "x.part", New: "x", Err: syscall.EIO}); got != "Couldn't write to the card. Check it in a computer" {
		t.Errorf("rename EIO: %q", got)
	}
}

// A stopped or paused sync lists the songs that failed before it, as a finished one does, and
// Settings says a sync stopped.
func TestStoppedListsFailedSongs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	for _, r := range []q2spot.Reply{d.stoppedRows(syncer.Snapshot{State: "error", Failed: 2, Err: os.ErrPermission}),
		d.syncRows(syncer.Snapshot{State: "paused", Total: 3, Failed: 2, Err: syncer.ErrCardFull})} {
		if _, ok := find(r, "Failed Songs"); !ok {
			t.Errorf("no Failed Songs: %q", titles(r))
		}
	}
	if r := d.syncRows(syncer.Snapshot{State: "paused", Total: 3, Err: syncer.ErrCardFull}); strings.Contains(strings.Join(titles(r), "|"), "Failed Songs") {
		t.Errorf("Failed Songs with none failed: %q", titles(r))
	}
	d.do("toggle", "album:al0")
	d.preview(t)
	os.WriteFile(filepath.Join(d.card, ".jellyfin", "manifest.json"), []byte("{"), 0o644)
	d.do("start")
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "error" })
	if row, _ := find(d.do("settings"), "Sync"); row.Subtitle != "Sync stopped" {
		t.Errorf("Sync subtitle %q", row.Subtitle)
	}
}

// Choosing something while a sync runs keeps its summary for when it ends (M38), whose Check Again
// says that the choice waits.
func TestDonePageKeptAfterChoosingDuringSync(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.do("toggle", "album:al1")
	close(n.block)
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	r := d.do("sync")
	if rows := afterOptions(r); !strings.HasPrefix(rows[0].Title, "Sync complete") ||
		rows[len(rows)-1].Title != "Check Again" || rows[len(rows)-1].Subtitle != "Sync changed since it started" {
		t.Fatalf("rows %q", titles(r))
	}
}

// What a crashed run left on stderr, and its exit code, which the launcher keeps in
// /tmp/q2-jellyfin.crash, goes into log.txt at the next start (M43).
func TestCrashLogged(t *testing.T) {
	card, tmp := t.TempDir(), t.TempDir()
	crash := filepath.Join(tmp, "q2-jellyfin.crash")
	os.WriteFile(crash, []byte("panic: boom\n\ngoroutine 1 [running]:\n"+strings.Repeat("main.f()\n", 2000)+"exit 2\n"), 0o644)
	newDaemon(filepath.Join(card, ".jellyfin"), card, tmp)
	b, _ := os.ReadFile(filepath.Join(card, ".jellyfin", "log.txt"))
	if !strings.Contains(string(b), "panic: boom") || !strings.Contains(string(b), "exit 2") || len(b) > 8<<10 {
		t.Fatalf("log %d bytes:\n%.300s", len(b), b)
	}
	if _, err := os.Stat(crash); err == nil {
		t.Fatal("crash file kept: logged again at every start")
	}
}

// A removal-only sync says what it did, not "0 songs" (review G3, Minor 7).
func TestDoneSaysRemoved(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	if r := d.do("sync"); afterOptions(r)[0].Title != "Sync complete · 2 songs removed · 0 failed" {
		t.Errorf("done %q", titles(r))
	}
}

// Removals held back by failed songs say so on the done page (I2).
func TestDoneSaysNotRemoved(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	r := d.doneRows(syncer.Snapshot{State: "done", Total: 2, Failed: 2, Held: 3})
	if got := afterOptions(r); got[1].Title != "3 songs not removed: songs failed to sync" ||
		got[1].Subtitle != "Retry in Failed Songs, or take them out of Sync" || got[2].Title != "Failed Songs" {
		t.Errorf("done %q", titles(r))
	}
	// A lost connection (or card) held them: taking songs out of Sync doesn't help, and under
	// Entire Library can't be done.
	r = d.doneRows(syncer.Snapshot{State: "done", Total: 2, Failed: 1, Held: 3, HeldRetry: true})
	if got := afterOptions(r); got[1].Subtitle != "Retry in Failed Songs" {
		t.Errorf("after a lost connection: %q", got[1].Subtitle)
	}
}

// Details of a stopped sync carry when it stopped, not when they were opened, and its error
// (review G3, Minor 3).
func TestStoppedDetailsTime(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	os.WriteFile(filepath.Join(d.card, ".jellyfin", "manifest.json"), []byte("{"), 0o644)
	d.do("start")
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "error" })
	time.Sleep(1100 * time.Millisecond)
	det, _ := find(d.do("sync"), "Details")
	f := strings.Split(det.Context, "|")
	if unix, _ := strconv.ParseInt(f[3], 10, 64); unix >= time.Now().Unix() {
		t.Errorf("Details time %d is when they were opened", unix)
	}
	if got := strings.Join(titles(d.do(det.URI, det.Context)), "|"); !strings.Contains(got, "Error: ") {
		t.Errorf("no error in %q", got)
	}
}

// A resolved item is On Q2 only when its songs are: a song a server playlist dropped doesn't stand
// in for the one it gained (review G3, Minor 4).
func TestResolvedItemCountsItsOwnSongs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "playlist:pl1")
	d.syncOK(t)
	d.mu.Lock()
	d.resolved["playlist:pl1"] = []jellyfin.Song{{ID: "al0.0", Suffix: "flac"}, {ID: "al1.0", Suffix: "flac"}} // al0.1 swapped for al1.0
	d.mu.Unlock()
	r := d.do("playlists")
	if row, _ := find(r, "Mix"); row.Subtitle != "2 songs · 1 of 2 songs on Q2" {
		t.Errorf("Mix %q", row.Subtitle)
	}
	if _, ok := find(r, "Sync Now"); !ok {
		t.Error("no Sync Now")
	}
}

// A sync that ends paused or stopped counts as activity too: its reason stays on screen as long as
// a finished sync's summary (review G3, Minor 8).
func TestStaysUpAfterPausedSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.tick()
	d.mu.Lock()
	d.lastReq = time.Now().Add(-time.Hour)
	d.mu.Unlock()
	d.runner.Pause() // as Wi-Fi going would
	d.tick()
	if d.idle(time.Now()) {
		t.Fatal("exits the moment the sync pauses")
	}
}

// A manifest that can't be read is not an empty card: Sync says so and offers no preview, which
// would plan the whole selection again (M7).
func TestDamagedManifest(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	os.MkdirAll(filepath.Join(d.card, ".jellyfin"), 0o755)
	os.WriteFile(filepath.Join(d.card, ".jellyfin", "manifest.json"), []byte(`{"Songs":`), 0o644)
	r := d.do("sync")
	if _, ok := find(r, "Couldn't read the sync records on the card"); !ok || r.Message == "refresh" {
		t.Fatalf("%q %q", r.Message, titles(r))
	}
	if _, ok := find(r, "Start Sync"); ok || d.daemon.preview != nil {
		t.Fatalf("a preview: %q", titles(r))
	}
}

// The lyrics backfill runs once the preview shows, not before, and asks for a song once a run.
func TestLyricsBackfillAfterPreview(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, lyricsHold: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	n.lyrics.Store(true) // the server has lyrics for the songs on the card now
	d.do("check")
	if _, ok := find(d.preview(t), "Everything is up to date"); !ok { // while the lyrics server stalls
		t.Fatal("preview")
	}
	reqs := func(id string) int32 {
		c, _ := n.lyricsReqs.LoadOrStore(id, new(atomic.Int32))
		return c.(*atomic.Int32).Load()
	}
	eventually(t, "the lyrics requests", func() bool { return reqs("al0.0") == 1 })
	close(n.lyricsHold)
	lrc := library.LyricsPath(d.manifest().Songs["al0.1"].Path)
	eventually(t, "the .lrc", func() bool { return exists(lrc) })
	os.Remove(lrc)
	d.do("check")
	d.preview(t)
	d.bg.Wait()
	if a, b := reqs("al0.0"), reqs("al0.1"); a != 1 || b != 1 {
		t.Fatalf("asked %d and %d times", a, b)
	}
}

// A backfill dropped mid-request leaves the song to the next one; a song the server answered for
// isn't asked again.
func TestLyricsBackfillCancelledAsksAgain(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, lyricsHold: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	n.lyrics.Store(true)
	d.do("check")
	d.preview(t)
	reqs := func(id string) int32 {
		c, _ := n.lyricsReqs.LoadOrStore(id, new(atomic.Int32))
		return c.(*atomic.Int32).Load()
	}
	eventually(t, "the lyrics request", func() bool { return reqs("al0.0") == 1 })
	d.do("check") // drops the preview mid-request
	d.bg.Wait()
	d.preview(t)
	eventually(t, "the request again", func() bool { return reqs("al0.0") == 2 })
	close(n.lyricsHold)
	d.bg.Wait()
	m := d.manifest()
	for _, id := range []string{"al0.0", "al0.1"} {
		lrc := library.LyricsPath(m.Songs[id].Path)
		if !exists(lrc) {
			t.Fatalf("no .lrc for %s", id)
		}
		os.Remove(lrc)
	}
	d.do("check")
	d.preview(t)
	d.bg.Wait()
	if a, b := reqs("al0.0"), reqs("al0.1"); a != 2 || b != 1 {
		t.Fatalf("asked %d and %d times", a, b)
	}
}

// Entire Library lists every song a page of 500 at a time, not an album at a time, and finds the
// same songs in the albums' order.
func TestEntireLibraryOneListing(t *testing.T) {
	n := &nav{albums: 120, songsPer: 5}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if a, s := n.albumReqs.Load(), n.songListReqs.Load(); a != 0 || s != 2 {
		t.Fatalf("%d album requests, %d song listings", a, s)
	}
	d.mu.Lock()
	songs := d.resolved["library:all"]
	d.mu.Unlock()
	if len(songs) != 600 || len(d.daemon.preview.plan.Add) != 600 {
		t.Fatalf("%d songs", len(songs))
	}
	for i, s := range songs {
		if want := fmt.Sprintf("al%d.%d", i/5, i%5); s.ID != want {
			t.Fatalf("song %d is %s, want %s", i, s.ID, want)
		}
	}
}

// A check drops the last Entire Library list before listing the songs again: a big one would be
// live beside the new one for the whole download (round 6 speed 0). The row keeps its counts.
func TestEntireLibraryListDroppedDuringCheck(t *testing.T) {
	var d *tdaemon
	var armed atomic.Bool
	listing, release := make(chan bool), make(chan struct{})
	n := &nav{albums: 3, songsPer: 2, playlists: 2, onSongList: func() {
		if !armed.Load() {
			return
		}
		d.mu.Lock()
		_, kept := d.resolved["library:all"]
		d.mu.Unlock()
		listing <- kept
		<-release
	}}
	d = newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	armed.Store(true)
	d.do("check")
	d.do("sync") // the new check
	if <-listing {
		t.Error("the last list kept during the check")
	}
	row, _ := find(d.do("sync"), "Sync Entire Library")
	armed.Store(false)
	close(release)
	if row.Subtitle != "On · 3 albums · 2 playlists" {
		t.Errorf("row during the check %q", row.Subtitle)
	}
	d.preview(t)
	d.mu.Lock()
	songs := d.resolved["library:all"]
	d.mu.Unlock()
	if len(songs) != 6 {
		t.Errorf("%d songs after the check", len(songs))
	}
}

// Entire Library switched off leaves no list once Sync is opened (round 6 speed 0).
func TestEntireLibraryOffDropsList(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	d.do("toggle", "library:all")
	d.do("toggle", "album:al0")
	d.preview(t)
	d.mu.Lock()
	_, kept := d.resolved["library:all"]
	d.mu.Unlock()
	if kept {
		t.Fatal("the list kept after Entire Library was switched off")
	}
}

// Adding an album checks the artists in Sync through the albums already fetched: no request.
func TestIncludedViaCachedArtist(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "artist:ar1")
	d.do("artist", "artist:ar1") // its albums, fetched
	n.artistItemReqs.Store(0)
	if r := d.do("toggle", "album:al1"); r.Message != "Included via Artist" {
		t.Fatalf("%q", r.Message)
	}
	if got := n.artistItemReqs.Load(); got != 0 {
		t.Fatalf("%d artist requests", got)
	}
}

// After a restart, with the playlist's songs and the artist's albums not in memory, adding a song
// or an album asks nothing more of the server: it is added on its own (audit 2026-11-16 #16). The
// plan holds the song once, and removing the playlist and the artist leaves both in Sync.
func TestIncludedViaNotFetched(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	file := filepath.Join(d.dir, "selection.json")
	sel := selection.Set{Version: 1}
	sel.Toggle(selection.Item{Kind: selection.KindPlaylist, ID: "pl1", Name: "Mix"}) // al0's songs
	sel.Save(file)
	before := n.reqs.Load()
	if r := d.do("toggle", "song:al0.1"); r.Message != "Added to Sync · 1 song" {
		t.Fatalf("%q", r.Message)
	}
	if got := n.reqs.Load() - before; got != 3 { // the song and its music library (cl.Song), not Mix
		t.Fatalf("%d requests", got)
	}
	sel, _ = selection.Load(file)
	sel.Toggle(selection.Item{Kind: selection.KindArtist, ID: "ar1", Name: "Artist"}) // al0 and al1
	sel.Save(file)
	if r := d.do("toggle", "album:al1"); r.Message != "Added to Sync · 2 songs" || n.artistItemReqs.Load() != 0 {
		t.Fatalf("%q, %d artist requests", r.Message, n.artistItemReqs.Load())
	}
	d.preview(t)
	d.mu.Lock()
	add := len(d.daemon.preview.plan.Add)
	d.mu.Unlock()
	if add != 4 { // the artist's 4 songs; al0.1 (in Mix and on its own too) once
		t.Fatalf("%d songs to add", add)
	}
	d.do("toggle", "playlist:pl1")
	d.do("toggle", "artist:ar1")
	if it := d.selected(t); len(it) != 2 || it[0].ID != "al0.1" || it[1].ID != "al1" {
		t.Fatalf("%+v", it)
	}
}

// Two adds at once both stay in Sync: each loads the selection again after its fetch (M16).
func TestConcurrentToggles(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2, holdAlbum: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	var wg sync.WaitGroup
	for _, key := range []string{"album:al0", "album:al1"} {
		wg.Add(1)
		go func() { defer wg.Done(); d.do("toggle", key) }()
	}
	eventually(t, "both fetches", func() bool { return n.albumReqs.Load() == 2 })
	close(n.holdAlbum)
	wg.Wait()
	if it := d.selected(t); len(it) != 2 {
		t.Fatalf("%+v", it)
	}
}
