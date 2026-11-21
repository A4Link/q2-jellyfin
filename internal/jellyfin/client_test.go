package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/A4Link/q2-jellyfin/internal/applog"
)

// fake is a small Jellyfin server: user "u" / "pw" gets token "tok". Every request it sees is kept.
type fake struct {
	mu   sync.Mutex
	seen []*http.Request
	h    map[string]http.HandlerFunc // path -> handler, checked before the defaults
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Clone(context.Background()))
	f.mu.Unlock()
	if musicLibrary(w, r) {
		return
	}
	if h := f.h[r.URL.Path]; h != nil {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/System/Info/Public":
		w.Write([]byte(`{"ProductName":"Jellyfin Server","Version":"10.10.7"}`))
	case "/Users/AuthenticateByName":
		var in struct{ Username, Pw string }
		json.NewDecoder(r.Body).Decode(&in)
		if in.Username != "u" || in.Pw != "pw" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"AccessToken":"tok","User":{"Id":"u1","Name":"u"}}`))
	case "/Users/Me":
		if !strings.Contains(r.Header.Get("Authorization"), `Token="tok"`) {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"Id":"u1","Name":"u"}`))
	default:
		http.NotFound(w, r)
	}
}

// musicLibrary answers the client's music-library lookup (music.go) for every fake: one music
// library, "lib1", holding the folder /music. It reports whether it answered.
func musicLibrary(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/UserViews":
		w.Write([]byte(`{"Items":[{"Id":"films","Name":"Movies","CollectionType":"movies"},{"Id":"lib1","Name":"Music","CollectionType":"music"}]}`))
	case r.URL.Path == "/Items" && q.Get("parentId") == "lib1" && q.Get("includeItemTypes") == "":
		w.Write([]byte(`{"Items":[{"Id":"ar","Type":"MusicArtist","Path":"/music/Artist"}],"TotalRecordCount":1}`))
	default:
		return false
	}
	return true
}

func newFake(t *testing.T, h map[string]http.HandlerFunc) (*fake, *httptest.Server) {
	f := &fake{h: h}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func signedIn(t *testing.T, srv *httptest.Server) *Client {
	c, err := New(Creds{Server: srv.URL, User: "u", Token: "tok", UserID: "u1", DeviceID: "dev"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The password is swapped for a token and dropped: what SignIn returns holds no password.
func TestSignInKeepsOnlyToken(t *testing.T) {
	_, srv := newFake(t, nil)
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	got, err := c.SignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "" || got.Token != "tok" || got.UserID != "u1" || got.User != "u" || got.DeviceID == "" {
		t.Fatalf("%+v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "pw") || strings.Contains(string(b), "Password") {
		t.Fatalf("password kept: %s", b)
	}
}

func TestSignInWrongPassword(t *testing.T) {
	_, srv := newFake(t, nil)
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "nope"}, srv.Client())
	_, err := c.SignIn(context.Background())
	if !errors.Is(err, ErrAuth) || Message(err, "h") != "Wrong username or password" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// A token the server no longer takes (the Q2 revoked under Dashboard → Devices), or a library the
// account lost, says where to sign in again, not "Wrong username or password": no password was typed.
func TestRevokedTokenSaysSignInAgain(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusForbidden) },
	})
	revoked, _ := New(Creds{Server: srv.URL, User: "u", Token: "old", UserID: "u1", DeviceID: "dev"}, srv.Client())
	err1 := revoked.Ping(context.Background())
	_, err2 := signedIn(t, srv).Playlists(context.Background(), "name", false)
	for _, err := range []error{err1, err2} {
		if !errors.Is(err, ErrAuth) || Message(err, "h") != "Server refused this Q2. Sign in again: Settings → Account → Sign Out" {
			t.Fatalf("%v / %q", err, Message(err, "h"))
		}
	}
}

// A server that isn't Jellyfin is refused before the password is sent.
func TestSignInRefusesOtherServer(t *testing.T) {
	f, srv := newFake(t, map[string]http.HandlerFunc{
		"/System/Info/Public": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ProductName":"Emby Server"}`)) },
	})
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if _, err := c.SignIn(context.Background()); !errors.Is(err, ErrNotJellyfin) {
		t.Fatal(err)
	}
	for _, r := range f.seen {
		if r.URL.Path == "/Users/AuthenticateByName" {
			t.Fatal("password sent to a server that isn't Jellyfin")
		}
	}
}

func TestNewRefusesHTTP(t *testing.T) {
	if _, err := New(Creds{Server: "http://music.example"}, http.DefaultClient); !errors.Is(err, ErrInsecure) {
		t.Fatal(err)
	}
}

// The token travels in the Authorization header only: no URL the app builds or logs carries it.
func TestTokenNeverInURL(t *testing.T) {
	f, srv := newFake(t, nil)
	c := signedIn(t, srv)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{c.MediaURL("s1", "original"), c.MediaURL("s1", "opus-320"), c.PlayURL("s1", "original"), c.CoverURL("al1")} {
		if strings.Contains(u, "tok") {
			t.Errorf("token in %s", u)
		}
	}
	h := f.seen[len(f.seen)-1].Header.Get("Authorization")
	for _, want := range []string{`MediaBrowser Client="Q2 Jellyfin"`, `DeviceId="dev"`, `Token="tok"`} {
		if !strings.Contains(h, want) {
			t.Errorf("Authorization %q lacks %s", h, want)
		}
	}
}

// A redirect to another host never carries the token there.
func TestRedirectDropsTokenOffHost(t *testing.T) {
	var got string
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "audio/flac")
		w.Write([]byte("audio"))
	}))
	t.Cleanup(other.Close)
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/s1/Download": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/file", http.StatusFound)
		},
	})
	c := signedIn(t, srv)
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.InsecureSkipVerify = true // the second test server has its own certificate
	c.hc.Transport = tr
	resp, err := c.Get(context.Background(), c.MediaURL("s1", "original"), 0)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if strings.Contains(got, "tok") {
		t.Fatalf("token sent to another host: %q", got)
	}
}

// A 307 or 308 at sign-in to another host is refused: Go would send the password there again.
func TestRedirectKeepsPasswordOnHost(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var got []string
		other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = append(got, r.Method+" "+string(b))
		}))
		t.Cleanup(other.Close)
		_, srv := newFake(t, map[string]http.HandlerFunc{
			"/Users/AuthenticateByName": func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/Users/AuthenticateByName", code)
			},
		})
		tr := srv.Client().Transport.(*http.Transport).Clone()
		tr.TLSClientConfig.InsecureSkipVerify = true // the second test server has its own certificate
		c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, &http.Client{Transport: tr})
		_, err := c.SignIn(context.Background())
		if !errors.Is(err, ErrRedirect) || len(got) > 0 {
			t.Fatalf("%d: err %v; other host got %q", code, err, got)
		}
	}
}

func TestSongMapping(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/s1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"s1","Type":"Audio","Name":"Tears","Album":"Tender","AlbumId":"al1","Artists":["Broadcast","Guest"],
				"AlbumArtist":"Broadcast","ProductionYear":2005,"ParentIndexNumber":2,"IndexNumber":7,"RunTimeTicks":2150000000,
				"AlbumPrimaryImageTag":"x","MediaSources":[{"Size":12345,"Bitrate":900000,"Container":"flac","Path":"/music/B/T/07 Tears.FLAC"}]}`))
		},
	})
	s, err := signedIn(t, srv).Song(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	want := Song{ID: "s1", Title: "Tears", Album: "Tender", AlbumID: "al1", Artist: "Broadcast, Guest", AlbumArtist: "Broadcast",
		Year: 2005, Disc: 2, Track: 7, Duration: 215, Size: 12345, Suffix: "flac", CoverArt: "al1", BitRate: 900}
	if s != want {
		t.Fatalf("got  %+v\nwant %+v", s, want)
	}
}

// A song's codec is its first audio stream's, lower case (an embedded cover is a video stream);
// none named is "".
func TestSongCodec(t *testing.T) {
	var w wireItem
	if err := json.Unmarshal([]byte(`{"Id":"s1","MediaSources":[{"Container":"m4a","MediaStreams":[
		{"Type":"Video","Codec":"mjpeg"},{"Type":"Audio","Codec":"AAC"},{"Type":"Audio","Codec":"alac"}]}]}`), &w); err != nil {
		t.Fatal(err)
	}
	if got := w.song().Codec; got != "aac" {
		t.Errorf("codec %q, want aac", got)
	}
	if got := (wireItem{}).song().Codec; got != "" {
		t.Errorf("no streams: codec %q", got)
	}
}

func TestSuffixFromContainer(t *testing.T) {
	for in, want := range map[string]string{"mov,mp4,m4a": "m4a", "mp3": "mp3", "ogg": "ogg"} {
		if got := (wireItem{Container: in}).suffix(); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A song's suffix is its own string: a slice of the server path would keep the whole path in memory
// for every fetched song (round 2 speed 6).
func TestSuffixOwnString(t *testing.T) {
	var w wireItem
	if err := json.Unmarshal([]byte(`{"Id":"s1","MediaSources":[{"Path":"/data/music/Artist/Album (2001)/01 - Title.flac"}]}`), &w); err != nil {
		t.Fatal(err)
	}
	p, s := w.MediaSources[0].Path, w.suffix()
	start, at := uintptr(unsafe.Pointer(unsafe.StringData(p))), uintptr(unsafe.Pointer(unsafe.StringData(s)))
	if s != "flac" || at >= start && at < start+uintptr(len(p)) {
		t.Fatalf("suffix %q shares the path's memory", s)
	}
}

// Albums pages through MusicAlbum items for the signed-in user.
func TestAlbumsQuery(t *testing.T) {
	var q map[string]string
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			q = map[string]string{}
			for k := range r.URL.Query() {
				q[k] = r.URL.Query().Get(k)
			}
			w.Write([]byte(`{"Items":[{"Id":"al1","Name":"Tender","AlbumArtist":"Broadcast","ProductionYear":2005,"ChildCount":12,"ImageTags":{"Primary":"t"}}],"TotalRecordCount":1,"StartIndex":50}`))
		},
	})
	al, err := signedIn(t, srv).Albums(context.Background(), 50, 25, "name", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(al) != 1 || al[0] != (Album{ID: "al1", Name: "Tender", Artist: "Broadcast", Year: 2005, SongCount: 12, CoverArt: "al1"}) {
		t.Fatalf("%+v", al)
	}
	for k, v := range map[string]string{"userId": "u1", "parentId": "lib1", "includeItemTypes": "MusicAlbum", "recursive": "true", "startIndex": "50", "limit": "25",
		"sortBy": "SortName", "sortOrder": "Ascending"} {
		if q[k] != v {
			t.Errorf("%s=%q, want %q", k, q[k], v)
		}
	}
}

// Every list order asks the server for its keys: the natural direction, and turned round with
// the name tie-break kept A to Z. An order the server can't sort by fails rather than listing by name.
func TestListOrders(t *testing.T) {
	var by, order string
	got := func(w http.ResponseWriter, r *http.Request) {
		by, order = r.URL.Query().Get("sortBy"), r.URL.Query().Get("sortOrder")
		w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
	}
	_, srv := newFake(t, map[string]http.HandlerFunc{"/Items": got, "/Artists/AlbumArtists": got})
	c := signedIn(t, srv)
	ctx := context.Background()
	fetch := map[string]func(string, bool) error{
		"albums":    func(o string, r bool) error { _, err := c.Albums(ctx, 0, 10, o, r); return err },
		"artists":   func(o string, r bool) error { _, err := c.Artists(ctx, o, r); return err },
		"playlists": func(o string, r bool) error { _, err := c.Playlists(ctx, o, r); return err },
	}
	for _, tc := range []struct {
		list, order       string
		by, natural, turn string
	}{
		{"albums", "name", "SortName", "Ascending", "Descending"},
		{"albums", "released", "ProductionYear,PremiereDate,SortName", "Descending,Descending,Ascending", "Ascending,Ascending,Ascending"},
		{"albums", "added", "DateCreated,SortName", "Descending,Ascending", "Ascending,Ascending"},
		{"albums", "albumartist", "AlbumArtist,SortName", "Ascending,Ascending", "Descending,Ascending"},
		{"artists", "name", "SortName", "Ascending", "Descending"},
		{"artists", "added", "DateCreated,SortName", "Descending,Ascending", "Ascending,Ascending"},
		{"playlists", "name", "SortName", "Ascending", "Descending"},
		{"playlists", "added", "DateCreated,SortName", "Descending,Ascending", "Ascending,Ascending"},
		{"playlists", "duration", "Runtime,SortName", "Descending,Ascending", "Ascending,Ascending"},
	} {
		for _, rev := range []bool{false, true} {
			by, order = "", ""
			if err := fetch[tc.list](tc.order, rev); err != nil {
				t.Fatalf("%s %s: %v", tc.list, tc.order, err)
			}
			want := map[bool]string{false: tc.natural, true: tc.turn}[rev]
			if by != tc.by || order != want {
				t.Errorf("%s %s reverse=%v: sortBy=%q sortOrder=%q, want %q %q", tc.list, tc.order, rev, by, order, tc.by, want)
			}
		}
	}
	for list, f := range fetch {
		by = ""
		if err := f("random", false); err == nil || by != "" { // the server's Random can't be paged
			t.Errorf("%s: an unknown order asked the server (%q) or no error: %v", list, by, err)
		}
	}
	if err := fetch["artists"]("duration", false); err == nil {
		t.Error("artists by duration: no error")
	}
}

// A playlist keeps its order and duplicates, and leaves out anything that isn't a song.
func TestPlaylistOrderAndDuplicates(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/pl1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"pl1","Name":"Mix","Type":"Playlist","MediaType":"Audio"}`))
		},
		"/Playlists/pl1/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"b","Type":"Audio","Path":"/music/A/b.flac"},{"Id":"a","Type":"Audio","Path":"/music/A/a.flac"},` +
				`{"Id":"v","Type":"Video","Path":"/music/A/v.mp4"},{"Id":"m","Type":"Audio","Path":"/mixed/memo.mp3"},` +
				`{"Id":"b","Type":"Audio","Path":"/music/A/b.flac"}]}`))
		},
	})
	p, s, err := signedIn(t, srv).Playlist(context.Background(), "pl1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range s {
		ids = append(ids, x.ID)
	}
	if p.Name != "Mix" || strings.Join(ids, ",") != "b,a,b" {
		t.Fatalf("%+v %v", p, ids)
	}
}

// Songs asks for 100 ids a request and keeps only the music songs answered (audit 0.8.1 round 3
// #14): not a video, not a song outside the music libraries, not an id the server left out.
func TestSongsBatched(t *testing.T) {
	var batches []int
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			batches = append(batches, len(ids))
			var items []string
			for _, id := range ids {
				switch id {
				case "s0":
					items = append(items, `{"Id":"s0","Type":"Video","Path":"/music/Artist/v.mp4"}`)
				case "s1":
					items = append(items, `{"Id":"s1","Type":"Audio","Path":"/mixed/memo.mp3"}`)
				case "s2": // left out
				default:
					items = append(items, `{"Id":"`+id+`","Type":"Audio","Name":"T","Path":"/music/Artist/`+id+`.flac"}`)
				}
			}
			w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `]}`))
		},
	})
	var ids []string
	for i := 0; i < 150; i++ {
		ids = append(ids, fmt.Sprintf("s%d", i))
	}
	got, err := signedIn(t, srv).Songs(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(batches) != "[100 50]" {
		t.Fatalf("batches %v", batches)
	}
	if len(got) != 147 || got["s3"].ID != "s3" {
		t.Fatalf("%d songs, s3 %+v", len(got), got["s3"])
	}
	for _, id := range []string{"s0", "s1", "s2"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s kept", id)
		}
	}
}

// AlbumsAndPlaylists asks for 100 ids a request and keeps the albums and playlists answered (audit
// 0.8.1 round 4 #12): not a song, not an id the server left out.
func TestAlbumsAndPlaylistsBatched(t *testing.T) {
	var batches []int
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			batches = append(batches, len(ids))
			var items []string
			for _, id := range ids {
				switch {
				case id == "a0":
					items = append(items, `{"Id":"a0","Type":"Audio","Path":"/music/Artist/a0.flac"}`)
				case id == "a1": // left out
				case strings.HasPrefix(id, "p"):
					items = append(items, `{"Id":"`+id+`","Type":"Playlist","Name":"Mix","ChildCount":3}`)
				default:
					items = append(items, `{"Id":"`+id+`","Type":"MusicAlbum","Name":"Album","AlbumArtist":"Artist"}`)
				}
			}
			w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `]}`))
		},
	})
	ids := []string{"p0", "p1"}
	for i := 0; i < 148; i++ {
		ids = append(ids, fmt.Sprintf("a%d", i))
	}
	als, pls, err := signedIn(t, srv).AlbumsAndPlaylists(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(batches) != "[100 50]" {
		t.Fatalf("batches %v", batches)
	}
	if len(als) != 146 || als["a2"].Artist != "Artist" || len(pls) != 2 || pls["p1"].Name != "Mix" {
		t.Fatalf("%d albums, a2 %+v, %d playlists, p1 %+v", len(als), als["a2"], len(pls), pls["p1"])
	}
	for _, id := range []string{"a0", "a1"} {
		if _, ok := als[id]; ok {
			t.Errorf("%s kept", id)
		}
	}
}

// PlaylistSongs pages the playlist's songs without asking for the playlist's item (audit 0.8.1
// round 4 #11).
func TestPlaylistSongsNoItemRequest(t *testing.T) {
	f, srv := newFake(t, map[string]http.HandlerFunc{
		"/Playlists/p1/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"s1","Type":"Audio","Path":"/music/Artist/s1.flac"}],"TotalRecordCount":1}`))
		},
	})
	s, err := signedIn(t, srv).PlaylistSongs(context.Background(), "p1", false)
	if err != nil || len(s) != 1 || s[0].ID != "s1" {
		t.Fatalf("%+v %v", s, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.seen {
		if r.URL.Path == "/Items/p1" {
			t.Errorf("item request %s", r.URL)
		}
	}
}

// isFolderScan is the music libraries' folder lookup (musicFolders): every top-level item of lib1.
func isFolderScan(r *http.Request) bool {
	return r.URL.Path == "/Items" && r.URL.Query().Get("parentId") == "lib1" && r.URL.Query().Get("includeItemTypes") == ""
}

func folderScans(f *fake) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.seen {
		if isFolderScan(r) {
			n++
		}
	}
	return n
}

// A list doesn't wait for the library's folders: the first page of Albums is the libraries'
// lookup and the page, nothing more (audit 2026-11-16 #14).
func TestListAsksNoFolders(t *testing.T) {
	f, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"al1","Name":"Tender"}],"TotalRecordCount":1}`))
		},
	})
	if _, err := signedIn(t, srv).Albums(context.Background(), 0, 25, "name", false); err != nil {
		t.Fatal(err)
	}
	if n := folderScans(f); n != 0 || len(f.seen) != 2 {
		t.Fatalf("%d folder scans, %d requests, want 0 and 2", n, len(f.seen))
	}
}

// The folders are asked once, for the first playlist or song, and kept for the next.
func TestFoldersAskedOnce(t *testing.T) {
	f, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/pl1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"pl1","Name":"Mix","Type":"Playlist"}`))
		},
		"/Playlists/pl1/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"a","Type":"Audio","Path":"/music/A/a.flac"}]}`))
		},
		"/Items/a": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"a","Type":"Audio","Path":"/music/A/a.flac"}`))
		},
	})
	c := signedIn(t, srv)
	for i := 0; i < 2; i++ {
		if _, s, err := c.Playlist(context.Background(), "pl1"); err != nil || len(s) != 1 {
			t.Fatal(s, err)
		}
	}
	if _, err := c.Song(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if n := folderScans(f); n != 1 {
		t.Fatalf("%d folder scans, want 1", n)
	}
}

// A folder lookup that runs out of time isn't kept: the next playlist asks again, and filters as
// before.
func TestFoldersRetriedAfterTimeout(t *testing.T) {
	var scans atomic.Int32
	f := &fake{h: map[string]http.HandlerFunc{
		"/Items/pl1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"pl1","Name":"Mix","Type":"Playlist"}`))
		},
		"/Playlists/pl1/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"a","Type":"Audio","Path":"/music/A/a.flac"},{"Id":"m","Type":"Audio","Path":"/mixed/memo.mp3"}]}`))
		},
	}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isFolderScan(r) && scans.Add(1) == 1 {
			<-r.Context().Done() // the first lookup outlasts its caller
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := signedIn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := c.Playlist(ctx, "pl1"); err == nil {
		t.Fatal("no error from a lookup that timed out")
	}
	_, s, err := c.Playlist(context.Background(), "pl1")
	if err != nil || len(s) != 1 || s[0].ID != "a" || scans.Load() != 2 {
		t.Fatalf("%+v %v, %d scans", s, err, scans.Load())
	}
}

// An account that sees no music library gets an error, not an empty list: a sync would read that
// as every song gone. It isn't kept, so the library given back later is found (audit 2026-11-16 #3).
func TestNoMusicLibrary(t *testing.T) {
	var views atomic.Int32
	f := &fake{h: map[string]http.HandlerFunc{"/Items": func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
	}}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/UserViews" && views.Add(1) == 1 {
			w.Write([]byte(`{"Items":[{"Id":"films","Name":"Movies","CollectionType":"movies"}]}`))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := signedIn(t, srv)
	if s, err := c.AllSongs(context.Background()); err == nil || Message(err, "h") != "No music library on the server for this account" {
		t.Fatalf("%d songs, %v", len(s), err)
	}
	if _, err := c.AllSongs(context.Background()); err != nil || views.Load() != 2 {
		t.Fatalf("%v, %d lookups", err, views.Load())
	}
}

// A download the server refuses for the account says so, and counts as a refusal (ErrAuth).
func TestDownloadsOff(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/s1/Download": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusForbidden) },
	})
	c := signedIn(t, srv)
	_, err := c.Get(context.Background(), c.MediaURL("s1", "original"), 0)
	if !errors.Is(err, ErrDownloadsOff) || !errors.Is(err, ErrAuth) || Message(err, "h") != "Downloads are turned off for this account on the server" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// A song the server no longer has is CodeGone, which the syncer and streamer don't retry.
func TestMissingSongIsGone(t *testing.T) {
	_, srv := newFake(t, nil)
	c := signedIn(t, srv)
	_, err := c.Get(context.Background(), c.PlayURL("s9", "original"), 0)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != CodeGone {
		t.Fatal(err)
	}
}

// CheckMusic asks /UserViews again rather than trusting the kept libraries: a music library the
// account lost since is ErrNoMusic.
func TestCheckMusicAsksAgain(t *testing.T) {
	var lost atomic.Bool
	f := &fake{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/UserViews" && lost.Load() {
			w.Write([]byte(`{"Items":[{"Id":"films","Name":"Movies","CollectionType":"movies"}]}`))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := signedIn(t, srv)
	if _, err := c.music(context.Background()); err != nil {
		t.Fatal(err)
	}
	lost.Store(true)
	if err := c.CheckMusic(context.Background()); !errors.Is(err, ErrNoMusic) {
		t.Fatalf("got %v, want ErrNoMusic", err)
	}
}

// An error sent as JSON with a 200 is never handed back as a song's bytes.
func TestGetRefusesJSONBody(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Audio/s1/universal": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"error":"x"}`))
		},
	})
	c := signedIn(t, srv)
	if _, err := c.Get(context.Background(), c.MediaURL("s1", "opus-128"), 0); err == nil {
		t.Fatal("JSON taken as audio")
	}
}

// An auth proxy's login page with a 200 is never saved as a song (audit #10).
func TestGetRefusesHTMLBody(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/s1/Download": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<html>Sign in</html>`))
		},
	})
	c := signedIn(t, srv)
	if _, err := c.Get(context.Background(), c.MediaURL("s1", "original"), 0); err == nil {
		t.Fatal("HTML taken as audio")
	}
}

// A list whose total changes between pages (a song deleted mid-listing shifts the rest, so one
// would be skipped) is an error, not a short list (audit #18).
func TestListChangedWhilePaged(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			total := 1000
			if r.URL.Query().Get("startIndex") != "0" {
				total = 999
			}
			var items []string
			for i := 0; i < 500; i++ {
				items = append(items, fmt.Sprintf(`{"Id":"s%s-%d","Type":"Audio"}`, r.URL.Query().Get("startIndex"), i))
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":%d,"StartIndex":%s}`, strings.Join(items, ","), total, r.URL.Query().Get("startIndex"))
		},
	})
	_, err := signedIn(t, srv).AllSongs(context.Background())
	if !errors.Is(err, ErrChanged) || Message(err, "h") != "The library changed while it was read. Try again" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// Songs tied on every sort key that swap between pages (one sent twice, one never) are an error,
// not a short list a sync would delete the missed song for (audit 2026-11-10 #3). DateCreated
// breaks such ties.
func TestTiedSongsSwappedBetweenPages(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			if sb := r.URL.Query().Get("sortBy"); !strings.HasSuffix(sb, ",DateCreated") {
				t.Errorf("sortBy %q", sb)
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
			var items []string
			for i := start; i < start+500 && i < 1000; i++ {
				id := fmt.Sprintf("s%d", i)
				if i == 500 { // the tie: s499 again in place of s500
					id = "s499"
				}
				items = append(items, fmt.Sprintf(`{"Id":"%s","Type":"Audio"}`, id))
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1000,"StartIndex":%d}`, strings.Join(items, ","), start)
		},
	})
	if _, err := signedIn(t, srv).AllSongs(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("%v", err)
	}
}

// AllSongs asks for no user data: each song's would be downloaded at every Entire Library preview
// and thrown away (round 5 speed 2).
func TestAllSongsNoUserData(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			if v := r.URL.Query().Get("enableUserData"); v != "false" {
				t.Errorf("enableUserData %q", v)
			}
			fmt.Fprint(w, `{"Items":[{"Id":"s1","Type":"Audio"}],"TotalRecordCount":1}`)
		},
	})
	if _, err := signedIn(t, srv).AllSongs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A song page that fails once is asked again, not the end of the whole list (round 6 speed 1).
func TestAllSongsAsksFailedPageAgain(t *testing.T) {
	var failed atomic.Bool
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
			if start == 1000 && !failed.Swap(true) {
				http.Error(w, "", http.StatusServiceUnavailable)
				return
			}
			var items []string
			for i := start; i < start+500 && i < 1200; i++ {
				items = append(items, fmt.Sprintf(`{"Id":"s%d","Type":"Audio"}`, i))
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1200,"StartIndex":%d}`, strings.Join(items, ","), start)
		},
	})
	ss, err := signedIn(t, srv).AllSongs(context.Background())
	if err != nil || len(ss) != 1200 || !failed.Load() {
		t.Fatalf("%d songs, %v", len(ss), err)
	}
}

// An artist's albums come a page at a time: "Various Artists" can have more than one answer holds
// (audit 2026-11-10 #4).
func TestArtistAlbumsPaged(t *testing.T) {
	var swap atomic.Bool
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/ar1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"ar1","Type":"MusicArtist","Name":"Various Artists"}`))
		},
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			start, _ := strconv.Atoi(q.Get("startIndex"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			if q.Get("albumArtistIds") != "ar1" || limit == 0 || limit > 500 {
				t.Errorf("query %v", q)
			}
			var items []string
			if !strings.Contains(q.Get("sortBy"), "DateCreated") {
				t.Errorf("sortBy %q", q.Get("sortBy"))
			}
			for i := start; i < start+limit && i < 1200; i++ {
				n := i
				if swap.Load() && i == 500 {
					n = 499
				}
				items = append(items, fmt.Sprintf(`{"Id":"al%d","Type":"MusicAlbum","Name":"Album %d"}`, n, n))
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1200,"StartIndex":%d}`, strings.Join(items, ","), start)
		},
	})
	_, als, err := signedIn(t, srv).Artist(context.Background(), "ar1")
	if err != nil || len(als) != 1200 || als[1199].ID != "al1199" {
		t.Fatalf("%d albums, %v", len(als), err)
	}
	swap.Store(true) // al499 sent again in al500's place: al500 never comes
	if _, _, err := signedIn(t, srv).Artist(context.Background(), "ar1"); !errors.Is(err, ErrChanged) {
		t.Fatalf("a missed album: %v", err)
	}
}

// A server that ignores startIndex (or a proxy that caches without the query) sends the same page
// again and again under its total. The lists stop at the repeat, not at the caller's time limit
// (audit 2026-11-16 #23).
func TestPagingStopsWhenServerIgnoresStart(t *testing.T) {
	page := func(item string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var items []string
			for i := 0; i < 500; i++ {
				items = append(items, fmt.Sprintf(item, i))
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":50000,"StartIndex":0}`, strings.Join(items, ","))
		}
	}
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items":                page(`{"Id":"s%d","Type":"Audio"}`),
		"/Artists/AlbumArtists": page(`{"Id":"ar%d","Name":"Artist","Type":"MusicArtist"}`),
	})
	c := signedIn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.AllSongs(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("songs: %v", err)
	}
	if _, err := c.Artists(ctx, "name", false); !errors.Is(err, ErrChanged) {
		t.Fatalf("artists: %v", err)
	}
}

// Albums' later windows (Albums, More…) from a server that ignores startIndex would show page one
// again: they fail instead.
func TestAlbumsPageStartsWhereAsked(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"al0","Name":"First","Type":"MusicAlbum"}],"TotalRecordCount":100,"StartIndex":0}`))
		},
	})
	c := signedIn(t, srv)
	if al, err := c.Albums(context.Background(), 0, 25, "name", false); err != nil || len(al) != 1 {
		t.Fatalf("page one: %+v %v", al, err)
	}
	if al, err := c.Albums(context.Background(), 50, 25, "name", false); !errors.Is(err, ErrChanged) {
		t.Fatalf("page one again: %+v %v", al, err)
	}
}

// A playlist of one song added 1000 times starts both pages with that song, and Jellyfin 12.4 gives
// every entry of it the same PlaylistItemId (checked on the test server): pages that start where
// they were asked are not taken for a repeated page.
func TestPlaylistOfOneSongPaged(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/pl1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"pl1","Name":"Mix","Type":"Playlist","MediaType":"Audio"}`))
		},
		"/Playlists/pl1/Items": func(w http.ResponseWriter, r *http.Request) {
			start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
			var items []string
			for i := start; i < min(start+500, 1000); i++ {
				items = append(items, `{"Id":"s1","PlaylistItemId":"s1","Type":"Audio","Path":"/music/A/1.flac"}`)
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1000,"StartIndex":%d}`, strings.Join(items, ","), start)
		},
	})
	if _, ss, err := signedIn(t, srv).Playlist(context.Background(), "pl1"); err != nil || len(ss) != 1000 {
		t.Fatalf("%d songs, %v", len(ss), err)
	}
}

func TestGetResumesWithRange(t *testing.T) {
	var rg string
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Audio/s1/stream": func(w http.ResponseWriter, r *http.Request) {
			rg = r.Header.Get("Range")
			w.Header().Set("Content-Type", "audio/flac")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("rest"))
		},
	})
	c := signedIn(t, srv)
	resp, err := c.Get(context.Background(), c.PlayURL("s1", "original"), 10)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rg != "bytes=10-" || string(b) != "rest" {
		t.Fatalf("%q %q", rg, b)
	}
}

func TestTranscodeURL(t *testing.T) {
	_, srv := newFake(t, nil)
	c := signedIn(t, srv)
	// Opus at every offered bitrate: the server transcodes to Ogg Opus at that rate.
	for _, kbps := range []int{64, 96, 128, 160, 192, 224, 256, 288, 320} {
		q, rate := fmt.Sprintf("opus-%d", kbps), fmt.Sprintf("%d000", kbps)
		for _, u := range []string{c.MediaURL("s1", q), c.PlayURL("s1", q)} {
			for _, want := range []string{"/Audio/s1/universal", "audioCodec=opus", "container=opus", "transcodingContainer=ogg",
				"maxStreamingBitrate=" + rate, "audioBitRate=" + rate} {
				if !strings.Contains(u, want) {
					t.Errorf("%s: %s lacks %s", q, u, want)
				}
			}
		}
	}
	// MP3, only as the old-firmware fallback.
	if u := c.MediaURL("s1", "mp3-160"); !strings.Contains(u, "audioCodec=mp3") || !strings.Contains(u, "transcodingContainer=mp3") ||
		!strings.Contains(u, "maxStreamingBitrate=160000") {
		t.Error(u)
	}
	if u := c.MediaURL("s1", "original"); !strings.HasSuffix(u, "/Items/s1/Download") {
		t.Error(u)
	}
	if u := c.PlayURL("s1", "original"); !strings.Contains(u, "/Audio/s1/stream?static=true") {
		t.Error(u)
	}
}

func TestQuickConnect(t *testing.T) {
	approved := false
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/QuickConnect/Initiate": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Code":"123456","Secret":"sec"}`))
		},
		"/QuickConnect/Connect": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("secret") != "sec" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"Authenticated":` + map[bool]string{true: "true", false: "false"}[approved] + `}`))
		},
		"/Users/AuthenticateWithQuickConnect": func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Secret string }
			json.NewDecoder(r.Body).Decode(&in)
			if in.Secret != "sec" || !approved {
				http.Error(w, "", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"AccessToken":"tok","User":{"Id":"u1","Name":"u"}}`))
		},
	})
	c, _ := New(Creds{Server: srv.URL}, srv.Client())
	ctx := context.Background()
	code, secret, err := c.QuickConnectInitiate(ctx)
	if err != nil || code != "123456" || secret != "sec" {
		t.Fatal(code, secret, err)
	}
	if ok, err := c.QuickConnectPoll(ctx, secret); ok || err != nil {
		t.Fatal("approved too early", err)
	}
	approved = true
	if ok, err := c.QuickConnectPoll(ctx, secret); !ok || err != nil {
		t.Fatal("not approved", err)
	}
	cr, err := c.QuickConnectSignIn(ctx, secret)
	if err != nil || cr.Token != "tok" || cr.User != "u" || cr.Password != "" {
		t.Fatalf("%+v %v", cr, err)
	}
}

func TestQuickConnectOff(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/QuickConnect/Initiate": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusUnauthorized) },
	})
	c, _ := New(Creds{Server: srv.URL}, srv.Client())
	if _, _, err := c.QuickConnectInitiate(context.Background()); !errors.Is(err, ErrQuickConnectOff) {
		t.Fatal(err)
	}
}

// An API key signs in as the named user (any case), checked by listing the server's users, and is
// kept marked as a key; Ping then checks that user, not /Users/Me (an API key has no "me").
func TestSignInWithKey(t *testing.T) {
	f, srv := newFake(t, map[string]http.HandlerFunc{
		"/Users": func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), `Token="KEY"`) {
				http.Error(w, "", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`[{"Id":"u1","Name":"u"},{"Id":"u2","Name":"Andreas"}]`))
		},
		"/Users/u2": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"Id":"u2","Name":"Andreas"}`)) },
	})
	c, _ := New(Creds{Server: srv.URL, Token: "KEY", Kind: KindAPIKey}, srv.Client())
	got, err := c.SignInWithKey(context.Background(), " andreas ")
	if err != nil || got.UserID != "u2" || got.User != "Andreas" || got.Kind != KindAPIKey || got.Token != "KEY" {
		t.Fatalf("%+v %v", got, err)
	}
	k, _ := New(got, srv.Client())
	if !k.KeyAuth() {
		t.Fatal("not marked as a key")
	}
	if err := k.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := f.seen[len(f.seen)-1].URL.Path; p != "/Users/u2" {
		t.Fatalf("ping asked %s", p)
	}
	bad, _ := New(Creds{Server: srv.URL, Token: "WRONG", Kind: KindAPIKey}, srv.Client())
	if _, err := bad.SignInWithKey(context.Background(), "u"); !errors.Is(err, ErrBadKey) || Message(err, "h") != "API key not accepted, or not an admin key" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
	if _, err := c.SignInWithKey(context.Background(), "nobody"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatal(err)
	}
}

// A secret the server no longer knows (404 "Unknown quick connect secret") is an expired code, not
// a network blip to retry.
func TestQuickConnectExpired(t *testing.T) {
	_, srv := newFake(t, nil) // /QuickConnect/Connect: 404
	c, _ := New(Creds{Server: srv.URL}, srv.Client())
	_, err := c.QuickConnectPoll(context.Background(), "old")
	if !errors.Is(err, ErrQuickConnectExpired) || errors.Is(err, ErrUnreachable) || Message(err, "h") != "The Quick Connect code expired. Try again" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// A code that expires between the poll that saw it approved and the swap for a token says so, not
// where a signed-in Q2 signs in again (it isn't signed in).
func TestQuickConnectSignInExpired(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Users/AuthenticateWithQuickConnect": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusUnauthorized) },
	})
	c, _ := New(Creds{Server: srv.URL}, srv.Client())
	_, err := c.QuickConnectSignIn(context.Background(), "old")
	if !errors.Is(err, ErrQuickConnectExpired) || Message(err, "h") != "The Quick Connect code expired. Try again" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// Favourites asks for the heart-marked songs and follows every page.
func TestFavoritesPages(t *testing.T) {
	var starts []string
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("isFavorite") != "true" || q.Get("includeItemTypes") != "Audio" || q.Get("recursive") != "true" {
				http.Error(w, "", 400)
				return
			}
			starts = append(starts, q.Get("startIndex"))
			n := 500
			if q.Get("startIndex") == "500" {
				n = 3
			}
			var items []string
			for i := 0; i < n; i++ {
				items = append(items, fmt.Sprintf(`{"Id":"s%s-%d","Type":"Audio"}`, q.Get("startIndex"), i))
			}
			w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `],"TotalRecordCount":503,"StartIndex":` + q.Get("startIndex") + `}`))
		},
	})
	got, err := signedIn(t, srv).Favorites(context.Background())
	if err != nil || len(got) != 503 || strings.Join(starts, ",") != "0,500" {
		t.Fatalf("%d songs, starts %v, %v", len(got), starts, err)
	}
}

// A big playlist and a long album-artist list come 500 at a time, so no answer passes maxBody;
// every item comes back, in order (audit #10). The artists come without user data; a playlist's
// songs with it, for their hearts (U30).
func TestLongListsPaged(t *testing.T) {
	var starts []string
	page := func(item, userData string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("limit") != "500" || q.Get("enableUserData") != userData {
				http.Error(w, "", 400)
				return
			}
			starts = append(starts, q.Get("startIndex"))
			start, _ := strconv.Atoi(q.Get("startIndex"))
			var items []string
			for i := start; i < min(start+500, 1203); i++ {
				items = append(items, fmt.Sprintf(item, i, i))
			}
			w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `],"TotalRecordCount":1203,"StartIndex":` + q.Get("startIndex") + `}`))
		}
	}
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/pl1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"pl1","Name":"Mix","Type":"Playlist","MediaType":"Audio"}`))
		},
		"/Playlists/pl1/Items":  page(`{"Id":"s%d","Type":"Audio","Path":"/music/A/%d.flac"}`, "true"),
		"/Artists/AlbumArtists": page(`{"Id":"ar%d","Name":"Artist %d","Type":"MusicArtist"}`, "false"),
	})
	c := signedIn(t, srv)
	_, ss, err := c.Playlist(context.Background(), "pl1")
	if err != nil || len(ss) != 1203 || strings.Join(starts, ",") != "0,500,1000" {
		t.Fatalf("%d songs, starts %v, %v", len(ss), starts, err)
	}
	for i, s := range ss {
		if s.ID != fmt.Sprint("s", i) {
			t.Fatalf("song %d is %s", i, s.ID)
		}
	}
	starts = nil
	ar, err := c.Artists(context.Background(), "name", false)
	if err != nil || len(ar) != 1203 || strings.Join(starts, ",") != "0,500,1000" {
		t.Fatalf("%d artists, starts %v, %v", len(ar), starts, err)
	}
	for i, a := range ar {
		if a.ID != fmt.Sprint("ar", i) {
			t.Fatalf("artist %d is %s", i, a.ID)
		}
	}
}

// Lyrics become LRC: synced lines with their times, unsynced ones as plain lines; none is "".
func TestLyricsToLRC(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Audio/s1/Lyrics": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Lyrics":[{"Text":"First","Start":0},{"Text":"Second line","Start":630100000},{"Text":"Long\nline","Start":36000000000}]}`))
		},
		"/Audio/s2/Lyrics": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Lyrics":[{"Text":"Plain one"},{"Text":""},{"Text":"Plain two"}]}`))
		},
	})
	c := signedIn(t, srv)
	got, err := c.Lyrics(context.Background(), "s1")
	if want := "[00:00.00]First\n[01:03.01]Second line\n[60:00.00]Long line\n"; err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := c.Lyrics(context.Background(), "s2"); got != "Plain one\nPlain two\n" {
		t.Fatalf("%q", got)
	}
	if got, err := c.Lyrics(context.Background(), "s9"); got != "" || err != nil { // 404: none
		t.Fatalf("%q %v", got, err)
	}
}

// Lyrics stop at the 64 KB Q2 Pod reads: a server's 8 MB of them would fill /tmp, which is RAM.
func TestLyricsLimit(t *testing.T) {
	line := strings.Repeat("x", 999)
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Audio/s1/Lyrics": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Lyrics":[` + strings.Repeat(`{"Text":"`+line+`"},`, 100) + `{"Text":"end"}]}`))
		},
	})
	got, err := signedIn(t, srv).Lyrics(context.Background(), "s1")
	if want := strings.Repeat(line+"\n", 64<<10/1000); err != nil || got != want {
		t.Fatalf("%d bytes, want %d: %v", len(got), len(want), err)
	}
}

// A refused sign-in (401) is recorded as not connected, any other answer as connected (B9).
func TestLastRecordsRefusedSignIn(t *testing.T) {
	code := http.StatusOK
	_, srv := newFake(t, map[string]http.HandlerFunc{"/Users/Me": func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		w.Write([]byte(`{"Id":"u1","Name":"u"}`))
	}})
	c := signedIn(t, srv)
	c.Ping(context.Background())
	if at, err := c.Last(); at.IsZero() || err != nil {
		t.Fatalf("after an answer: %v %v", at, err)
	}
	code = http.StatusUnauthorized
	c.Ping(context.Background())
	if _, err := c.Last(); err == nil {
		t.Fatal("a 401 counts as connected")
	}
}

// A cover or lyrics that came writes no log line; one that failed does (B6).
func TestNoLogLineForCoverOrLyricsThatCame(t *testing.T) {
	lyrics := http.StatusOK
	_, srv := newFake(t, map[string]http.HandlerFunc{"/Audio/s1/Lyrics": func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(lyrics)
		w.Write([]byte(`{"Lyrics":[{"Text":"la"}]}`))
	}})
	c := signedIn(t, srv)
	logFile := filepath.Join(t.TempDir(), "log.txt")
	c.Log = applog.Open(logFile, 1<<20)
	if _, err := c.Lyrics(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(logFile); strings.Contains(string(b), "Lyrics") {
		t.Errorf("logged a lyrics fetch that worked: %q", b)
	}
	lyrics = http.StatusInternalServerError
	c.Lyrics(context.Background(), "s1")
	if b, _ := os.ReadFile(logFile); !strings.Contains(string(b), "-> 500") {
		t.Errorf("a failed lyrics fetch not logged: %q", b)
	}
}

// A server that doesn't answer before the request's deadline is not connected; only the app
// cancelling a request (a newer one, the user leaving) records nothing (B9).
func TestLastRecordsATimeoutNotACancel(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{"/Users/Me": func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}})
	c := signedIn(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	c.Ping(ctx)
	if at, _ := c.Last(); !at.IsZero() {
		t.Error("a cancelled request was recorded")
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	c.Ping(ctx)
	if at, err := c.Last(); at.IsZero() || err == nil {
		t.Errorf("a timed-out request not recorded as not connected: %v %v", at, err)
	}
}

// A cover that misses its own short budget doesn't make the app "Not connected" (B9).
func TestLastIgnoresACoverTimeout(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{"/Items/al1/Images/Primary": func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}})
	c := signedIn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if resp, err := c.Get(ctx, c.ThumbURL("al1", 64), 0); err == nil {
		resp.Body.Close()
	}
	if at, _ := c.Last(); !at.IsZero() {
		t.Error("a cover's timeout was recorded as the connection's state")
	}
}

// An answer within maxBody made of tiny array values (0, null) would decode to over 1 GB: it is
// refused as too large. A real-shaped 500-song page, its commas in strings too, still loads.
func TestTinyArrayValuesRefused(t *testing.T) {
	var body string
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) },
	})
	c := signedIn(t, srv)
	for _, v := range []string{"0,", "null,"} {
		body = `{"Items":[` + strings.Repeat(v, (maxBody-40)/len(v)) + `0],"TotalRecordCount":1}`
		_, err := c.Albums(context.Background(), 0, 500, "name", false)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Msg != "Answer too large for the Q2" {
			t.Fatalf("%q: %v", v, err)
		}
	}
	var items []string
	for i := range 500 {
		items = append(items, fmt.Sprintf(`{"Id":"al%d","Name":"A, \"B\" [%d,%d,%d]","Type":"MusicAlbum",`+
			`"Artists":["X","Y","Z"],"ArtistItems":[{"Id":"x","Name":"X"},{"Id":"y","Name":"Y"}],"Genres":["Rock","Pop"]}`, i, i, i, i))
	}
	body = `{"Items":[` + strings.Join(items, ",") + `],"TotalRecordCount":500}`
	if got, err := c.Albums(context.Background(), 0, 500, "name", false); err != nil || len(got) != 500 {
		t.Fatalf("%d albums, %v", len(got), err)
	}
}

// ImageTags keeps only Primary: a hostile answer's ImageTags of 100,000 keys is skipped, not
// kept in a map, and the cover tag still comes through.
func TestImageTagsOnlyPrimary(t *testing.T) {
	keys := make([]string, 100_000)
	for i := range keys {
		keys[i] = fmt.Sprintf(`"%d":""`, i)
	}
	body := []byte(`{"Id":"al1","ImageTags":{` + strings.Join(keys, ",") + `,"Primary":"t"}}`)
	var w wireItem
	allocs := testing.AllocsPerRun(1, func() {
		w = wireItem{}
		if err := json.Unmarshal(body, &w); err != nil {
			t.Fatal(err)
		}
	})
	if (allocs > 100 && !raceOn) || w.album().CoverArt != "al1" {
		t.Fatalf("%v allocations, cover %q", allocs, w.album().CoverArt)
	}
	w = wireItem{}
	json.Unmarshal([]byte(`{"Id":"s1","ImageTags":{"Primary":"t","Logo":"l"},"BackdropImageTags":[]}`), &w)
	if w.song().CoverArt != "s1" {
		t.Errorf("real-shaped answer: cover %q", w.song().CoverArt)
	}
}

// Favourites asks for the songs' user data (it sends no enableUserData), so each song keeps its
// heart: Jellyfin leaves UserData out when told enableUserData=false.
func TestFavoritesKeepHearts(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			ud := `,"UserData":{"IsFavorite":true}`
			if r.URL.Query().Get("enableUserData") == "false" {
				ud = ""
			}
			fmt.Fprintf(w, `{"Items":[{"Id":"s1","Type":"Audio"%s}],"TotalRecordCount":1}`, ud)
		},
	})
	got, err := signedIn(t, srv).Favorites(context.Background())
	if err != nil || len(got) != 1 || !got[0].Favorite {
		t.Fatalf("%+v %v", got, err)
	}
}

// An artist's album list that answers 404 is an HTTP error, not the artist deleted: only the
// artist's own lookup says that.
func TestArtistAlbumListNotFound(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/ar1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"ar1","Type":"MusicArtist","Name":"A"}`))
		},
		"/Items": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
	})
	_, _, err := signedIn(t, srv).Artist(context.Background(), "ar1")
	var ae *APIError
	if !errors.As(err, &ae) || ae.HTTP != 404 || ae.Code != 0 || !errors.Is(err, ErrUnreachable) ||
		Message(err, "h") != "The server answered HTTP 404" {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// A Favourites or Entire Library song list that answers 404 is "not found" (CodeGone); the Sync
// preview decides what that means for each (preview.go).
func TestSongListsNotFound(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
	})
	c := signedIn(t, srv)
	_, err1 := c.Favorites(context.Background())
	_, err2 := c.AllSongs(context.Background())
	for _, err := range []error{err1, err2} {
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != CodeGone || Message(err, "h") != "Not found on the server" {
			t.Fatalf("%v / %q", err, Message(err, "h"))
		}
	}
}

// The music folders scan (when a playlist or a song is checked) asks for no user data.
func TestFolderScanAsksNoUserData(t *testing.T) {
	f, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items/s1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"s1","Type":"Audio","Path":"/music/Artist/1.flac"}`))
		},
	})
	if _, err := signedIn(t, srv).Song(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if folderScans(f) == 0 {
		t.Fatal("no folder scan")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.seen {
		if isFolderScan(r) && r.URL.Query().Get("enableUserData") != "false" {
			t.Errorf("folder scan %s", r.URL)
		}
	}
}

// twoLibraries is a fake whose account sees two music libraries, lib1 and lib2.
func twoLibraries(t *testing.T, h map[string]http.HandlerFunc) *httptest.Server {
	f := &fake{h: h}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/UserViews" {
			w.Write([]byte(`{"Items":[{"Id":"lib1","CollectionType":"music"},{"Id":"films","CollectionType":"movies"},{"Id":"lib2","CollectionType":"music"}]}`))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// With two music libraries: an artist's album in both comes once (and is no skipped album),
// Favourites lists lib1's songs before lib2's, and Artists by name is one list A to Z, any case,
// while any other order keeps each library's own.
func TestTwoMusicLibraries(t *testing.T) {
	srv := twoLibraries(t, map[string]http.HandlerFunc{
		"/Items/ar1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"ar1","Type":"MusicArtist","Name":"A"}`))
		},
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			lib := q.Get("parentId")
			switch q.Get("includeItemTypes") {
			case "MusicAlbum":
				items := map[string]string{"lib1": `{"Id":"al1","Type":"MusicAlbum"},{"Id":"al2","Type":"MusicAlbum"}`,
					"lib2": `{"Id":"al1","Type":"MusicAlbum"},{"Id":"al3","Type":"MusicAlbum"}`}[lib]
				fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":2}`, items)
			case "Audio":
				fmt.Fprintf(w, `{"Items":[{"Id":"s-%s","Type":"Audio"}],"TotalRecordCount":1}`, lib)
			}
		},
		"/Artists/AlbumArtists": func(w http.ResponseWriter, r *http.Request) {
			items := map[string]string{"lib1": `{"Id":"a1","Name":"beta"},{"Id":"a2","Name":"Delta"}`,
				"lib2": `{"Id":"a3","Name":"Alpha"},{"Id":"a4","Name":"charlie"}`}[r.URL.Query().Get("parentId")]
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":2}`, items)
		},
	})
	c := signedIn(t, srv)
	ctx := context.Background()
	_, als, err := c.Artist(ctx, "ar1")
	var ids []string
	for _, a := range als {
		ids = append(ids, a.ID)
	}
	if err != nil || strings.Join(ids, ",") != "al1,al2,al3" {
		t.Fatalf("artist's albums %v, %v", ids, err)
	}
	fav, err := c.Favorites(ctx)
	if err != nil || len(fav) != 2 || fav[0].ID != "s-lib1" || fav[1].ID != "s-lib2" {
		t.Fatalf("favourites %+v, %v", fav, err)
	}
	for _, o := range []struct {
		order   string
		reverse bool
		want    string
	}{
		{"name", false, "Alpha,beta,charlie,Delta"},
		{"name", true, "beta,Delta,Alpha,charlie"},
		{"added", false, "beta,Delta,Alpha,charlie"},
	} {
		ar, err := c.Artists(ctx, o.order, o.reverse)
		var names []string
		for _, a := range ar {
			names = append(names, a.Name)
		}
		if err != nil || strings.Join(names, ",") != o.want {
			t.Errorf("%s %v: %v, %v", o.order, o.reverse, names, err)
		}
	}
}

// Each sign-in and Quick Connect request gives its own meaning to the answers it expects: a
// refused password or key, Quick Connect off, an expired code, a server that isn't Jellyfin. Any
// other status keeps its usual meaning.
func TestStatusMeaningPerRequest(t *testing.T) {
	ctx := context.Background()
	for _, s := range []struct {
		path string
		kind error
		on   string // which of 400 401 403 404 500 502 are kind
		run  func(c *Client) error
	}{
		{"/System/Info/Public", ErrNotJellyfin, "404", func(c *Client) error { return c.CheckServer(ctx) }},
		{"/Users/AuthenticateByName", ErrAuth, "400 401 403", func(c *Client) error {
			_, err := c.SignIn(ctx)
			return err
		}},
		{"/Users", ErrBadKey, "401 403", func(c *Client) error {
			_, err := c.SignInWithKey(ctx, "u")
			return err
		}},
		{"/QuickConnect/Initiate", ErrQuickConnectOff, "401 403", func(c *Client) error {
			_, _, err := c.QuickConnectInitiate(ctx)
			return err
		}},
		{"/QuickConnect/Connect", ErrQuickConnectExpired, "404", func(c *Client) error {
			_, err := c.QuickConnectPoll(ctx, "s")
			return err
		}},
		{"/QuickConnect/Connect", ErrQuickConnectOff, "401 403", func(c *Client) error { // turned off while the code is on screen
			_, err := c.QuickConnectPoll(ctx, "s")
			return err
		}},
		{"/Users/AuthenticateWithQuickConnect", ErrQuickConnectExpired, "401", func(c *Client) error {
			_, err := c.QuickConnectSignIn(ctx, "s")
			return err
		}},
	} {
		var on []string
		for _, st := range []int{400, 401, 403, 404, 500, 502} {
			_, srv := newFake(t, map[string]http.HandlerFunc{
				s.path: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", st) },
			})
			c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
			if errors.Is(s.run(c), s.kind) {
				on = append(on, strconv.Itoa(st))
			}
		}
		if got := strings.Join(on, " "); got != s.on {
			t.Errorf("%s: %s on %q, want %q", s.path, s.kind, got, s.on)
		}
	}
}

// Plain http is taken only for a host's IPv4 address with its port (which ones, the app decides).
func TestNewPlainHTTPAddressOnly(t *testing.T) {
	for _, s := range []string{"http://10.0.0.5:8096", "http://172.32.0.1:8096", "http://192.168.111.111:8096"} {
		if _, err := New(Creds{Server: s}, http.DefaultClient); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"http://192.168.1.10", "http://music.example:8096", "http://127.0.0.1:8096", "http://0.0.0.0:80", "http://239.1.1.1:80"} {
		if _, err := New(Creds{Server: s}, http.DefaultClient); !errors.Is(err, ErrInsecure) {
			t.Errorf("%s: %v", s, err)
		}
	}
}

// A plain http server may redirect only to itself over http (or anywhere over https); an https
// server never to http.
func TestRedirectPlainHTTP(t *testing.T) {
	c, err := New(Creds{Server: "http://192.168.1.10:8096"}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	from := &http.Request{URL: &url.URL{Scheme: "http", Host: "192.168.1.10:8096"}}
	for to, ok := range map[string]bool{
		"http://192.168.1.10:8096/x": true,
		"https://music.example/x":    true,
		"http://192.168.1.11:8096/x": false,
		"http://192.168.1.10:8097/x": false,
	} {
		u, _ := url.Parse(to)
		if err := c.hc.CheckRedirect(&http.Request{Method: "GET", URL: u, Header: http.Header{}}, []*http.Request{from}); (err == nil) != ok {
			t.Errorf("%s: %v", to, err)
		}
	}
	s, _ := New(Creds{Server: "https://music.example"}, http.DefaultClient)
	u, _ := url.Parse("http://music.example/x")
	from = &http.Request{URL: &url.URL{Scheme: "https", Host: "music.example"}}
	if err := s.hc.CheckRedirect(&http.Request{URL: u, Header: http.Header{}}, []*http.Request{from}); !errors.Is(err, ErrRedirect) {
		t.Errorf("https to http: %v", err)
	}
}

// Review 0.8.2 #2: a self-redirect written without the default port (nginx's) is the server
// itself, typed with :80 or https's :443; another host or port still isn't.
func TestRedirectDefaultPort(t *testing.T) {
	for server, cases := range map[string]map[string]bool{
		"http://192.168.1.10:80": {
			"http://192.168.1.10/x":      true,
			"http://192.168.1.10:80/x":   true,
			"http://192.168.1.10:8096/x": false,
			"http://192.168.1.11/x":      false,
		},
		"https://music.example": {
			"https://music.example:443/x":  true,
			"https://music.example:8443/x": false,
		},
	} {
		c, err := New(Creds{Server: server}, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		for to, same := range cases {
			u, _ := url.Parse(to)
			req := &http.Request{URL: u, Header: http.Header{"Authorization": {"t"}}}
			err := c.hc.CheckRedirect(req, []*http.Request{{URL: c.base}})
			if u.Scheme == "http" && (err == nil) != same {
				t.Errorf("%s to %s: %v", server, to, err)
			}
			if err == nil && (req.Header.Get("Authorization") != "") != same {
				t.Errorf("%s to %s: token kept %v", server, to, !same)
			}
		}
	}
}

// A library over 100,000 items (a real one of 120,000, or a server that would page for ever under
// a huge total) is refused on its first page, not read 200 pages deep or until the Q2 runs out of
// memory. A total that changes between pages is ErrChanged, so the total is all there is to check.
func TestPagingStopsAtLibraryCap(t *testing.T) {
	for _, total := range []int{120000, 2000000000} {
		var pages atomic.Int32
		_, srv := newFake(t, map[string]http.HandlerFunc{
			"/Items": func(w http.ResponseWriter, r *http.Request) {
				start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
				pages.Add(1)
				items := strings.TrimSuffix(strings.Repeat(`{"Id":"s"},`, 500), ",")
				fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":%d,"StartIndex":%d}`, items, total, start)
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, err := signedIn(t, srv).AllSongs(ctx)
		cancel()
		if Message(err, "h") != "Library too large for the Q2" || pages.Load() != 1 {
			t.Errorf("total %d: %d pages: %v", total, pages.Load(), err)
		}
	}
}

// The cap is for the whole list, not each music library: two libraries of 60,000 songs (or album
// artists) are refused, not read as 120,000, and so is a second library whose total is the largest
// int (it must not wrap the sum negative and page on): each on the second library's first page.
func TestPagingCapCountsEveryLibrary(t *testing.T) {
	for _, total2 := range []int{60000, math.MaxInt} {
		var lib2 atomic.Int32
		page := func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			start, _ := strconv.Atoi(q.Get("startIndex"))
			total := 60000
			if q.Get("parentId") == "lib2" {
				lib2.Add(1)
				total = total2
			}
			var b strings.Builder
			for i := start; i < start+500; i++ {
				fmt.Fprintf(&b, `{"Id":"%s-%d","Type":"Audio","Path":"/music/A/%d.flac"},`, q.Get("parentId"), i, i)
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":%d,"StartIndex":%d}`, strings.TrimSuffix(b.String(), ","), total, start)
		}
		srv := twoLibraries(t, map[string]http.HandlerFunc{"/Items": page, "/Artists/AlbumArtists": page})
		c := signedIn(t, srv)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if songs, err := c.AllSongs(ctx); Message(err, "h") != "Library too large for the Q2" || lib2.Load() != 1 {
			t.Errorf("lib2 total %d: songs: %d, %d lib2 pages, %v", total2, len(songs), lib2.Load(), err)
		}
		lib2.Store(0)
		if artists, err := c.Artists(ctx, "name", false); Message(err, "h") != "Library too large for the Q2" || lib2.Load() != 1 {
			t.Errorf("lib2 total %d: artists: %d, %d lib2 pages, %v", total2, len(artists), lib2.Load(), err)
		}
		cancel()
	}
}

// A server that ignores limit but honours startIndex: a playlist of 1,200 songs comes whole,
// 500 at a time, not cut to its first 500.
func TestPlaylistServerIgnoresLimit(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Playlists/p1/Items": func(w http.ResponseWriter, r *http.Request) {
			start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
			var b strings.Builder
			for i := start; i < 1200; i++ {
				fmt.Fprintf(&b, `{"Id":"s%d","Type":"Audio","Path":"/music/Artist/%d.flac"},`, i, i)
			}
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1200,"StartIndex":%d}`, strings.TrimSuffix(b.String(), ","), start)
		},
	})
	songs, err := signedIn(t, srv).PlaylistSongs(context.Background(), "p1", false)
	if err != nil || len(songs) != 1200 || songs[1199].ID != "s1199" {
		t.Fatalf("%d songs, %v", len(songs), err)
	}
}

// A page that doesn't say where it starts is taken only as the first: a server that ignores
// startIndex and leaves StartIndex out stops at its second page.
func TestPagingStopsWithoutStartIndex(t *testing.T) {
	items := strings.TrimSuffix(strings.Repeat(`{"Id":"s","Type":"Audio","Path":"/music/A/1.flac"},`, 500), ",")
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":50000}`, items)
		},
		"/Playlists/p1/Items": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":50000}`, items)
		},
	})
	c := signedIn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.AllSongs(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("songs: %v", err)
	}
	if _, err := c.PlaylistSongs(ctx, "p1", false); !errors.Is(err, ErrChanged) {
		t.Fatalf("playlist: %v", err)
	}
}

// A Windows server whose music is on a network share (\\nas\music) lists its songs as music.
func TestMusicOnWindowsShare(t *testing.T) {
	f := &fake{h: map[string]http.HandlerFunc{
		"/Items/s1": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Id":"s1","Type":"Audio","Path":"\\\\nas\\music\\Artist\\A\\01.flac"}`))
		},
		"/Playlists/p1/Items": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Items":[{"Id":"s1","Type":"Audio","Path":"\\\\nas\\music\\Artist\\A\\01.flac"}],"TotalRecordCount":1}`))
		},
	}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isFolderScan(r) {
			w.Write([]byte(`{"Items":[{"Id":"ar","Type":"MusicArtist","Path":"\\\\nas\\music\\Artist"}],"TotalRecordCount":1}`))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := signedIn(t, srv)
	if ss, err := c.PlaylistSongs(context.Background(), "p1", false); err != nil || len(ss) != 1 {
		t.Fatalf("playlist: %+v %v", ss, err)
	}
	if _, err := c.Song(context.Background(), "s1"); err != nil {
		t.Fatalf("song: %v", err)
	}
}

// Jellyfin answers a right password for a disabled or locked-out account (or one not allowed
// from this network) with 403: that is not "Wrong username or password".
func TestSignInRefusedAccount(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Users/AuthenticateByName": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusForbidden) },
	})
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	_, err := c.SignIn(context.Background())
	if want := "The server refused this account (disabled, locked, or not allowed from here). Ask its admin"; !errors.Is(err, ErrAuth) || Message(err, "h") != want {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// A 403 at Quick Connect's last step is the account refused, not "Sign in again ... Sign Out" to
// someone not signed in.
func TestQuickConnectRefusedAccount(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Users/AuthenticateWithQuickConnect": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusForbidden) },
	})
	c, _ := New(Creds{Server: srv.URL}, srv.Client())
	_, err := c.QuickConnectSignIn(context.Background(), "secret")
	if want := "The server refused this account (disabled, locked, or not allowed from here). Ask its admin"; Message(err, "h") != want {
		t.Fatalf("%v / %q", err, Message(err, "h"))
	}
}

// A proxy in front of the server (basic auth, Authelia, Cloudflare Access) that refuses the server
// check says what it answered, not "Sign in again ... Sign Out" to someone not signed in.
func TestServerCheckRefusedByProxy(t *testing.T) {
	for _, st := range []int{401, 403} {
		_, srv := newFake(t, map[string]http.HandlerFunc{
			"/System/Info/Public": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", st) },
		})
		c, _ := New(Creds{Server: srv.URL}, srv.Client())
		err := c.CheckServer(context.Background())
		if want := fmt.Sprintf("The server answered HTTP %d", st); Message(err, "h") != want {
			t.Errorf("%d: %v / %q", st, err, Message(err, "h"))
		}
	}
}

// A server that ignores limit sends no more rows than were asked into a page of albums.
func TestAlbumsPageNoMoreThanAsked(t *testing.T) {
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Items": func(w http.ResponseWriter, r *http.Request) {
			items := strings.TrimSuffix(strings.Repeat(`{"Id":"al","Type":"MusicAlbum"},`, 300), ",")
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":300,"StartIndex":0}`, items)
		},
	})
	if al, err := signedIn(t, srv).Albums(context.Background(), 0, 25, "name", false); err != nil || len(al) != 25 {
		t.Fatalf("%d albums, %v", len(al), err)
	}
}

// A server that drips its JSON answer, a byte at a time, is cut off at the request's limit: the
// idle limit alone never fires while bytes keep coming.
func TestCallLimitCutsDrip(t *testing.T) {
	old := callTimeout
	callTimeout = 50 * time.Millisecond
	t.Cleanup(func() { callTimeout = old })
	_, srv := newFake(t, map[string]http.HandlerFunc{
		"/Users/Me": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("{"))
			for range 1000 { // ends after 10 s, so the test fails rather than hangs without the limit
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
				w.Write([]byte(" "))
			}
			w.Write([]byte("}"))
		},
	})
	if err := signedIn(t, srv).Ping(context.Background()); err == nil {
		t.Fatal("a dripping answer was read to its end")
	}
}
