package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestSuffixFromContainer(t *testing.T) {
	for in, want := range map[string]string{"mov,mp4,m4a": "m4a", "mp3": "mp3", "ogg": "ogg"} {
		if got := (wireItem{Container: in}).suffix(); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
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
			w.Write([]byte(`{"Items":[{"Id":"al1","Name":"Tender","AlbumArtist":"Broadcast","ProductionYear":2005,"ChildCount":12,"ImageTags":{"Primary":"t"}}],"TotalRecordCount":1}`))
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
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":%d}`, strings.Join(items, ","), total)
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
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1000}`, strings.Join(items, ","))
		},
	})
	if _, err := signedIn(t, srv).AllSongs(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("%v", err)
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
			fmt.Fprintf(w, `{"Items":[%s],"TotalRecordCount":1200}`, strings.Join(items, ","))
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
			w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `],"TotalRecordCount":503}`))
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
			w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `],"TotalRecordCount":1203}`))
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
