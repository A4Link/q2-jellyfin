package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// Integration tests against a real Jellyfin server, made by contrib/testserver/setup.py:
//
//	python3 contrib/testserver/setup.py ../jellyfin-test
//	JELLYFIN_TEST_SERVER=../jellyfin-test/server.json go test ./internal/jellyfin -run Live -v
//
// The client only speaks HTTPS, so each test reaches the server through a TLS proxy of its own.

type liveServer struct {
	URL     string                              `json:"url"`
	Version string                              `json:"version"`
	User    struct{ Name, Password, ID string } `json:"user"`
	NoDL    struct{ Name, Password, ID string } `json:"nodl"`
	APIKey  string                              `json:"apiKey"`
	Items   map[string]string                   `json:"items"`
	Lists   map[string]string                   `json:"playlists"`
}

func live(t *testing.T) (*liveServer, *httptest.Server) {
	t.Helper()
	f := os.Getenv("JELLYFIN_TEST_SERVER")
	if f == "" {
		t.Skip("set JELLYFIN_TEST_SERVER to contrib/testserver's server.json to run")
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var s liveServer
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(s.URL)
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)
	return &s, proxy
}

func liveSignIn(t *testing.T, s *liveServer, proxy *httptest.Server, name, pw string) *Client {
	t.Helper()
	c, err := New(Creds{Server: proxy.URL, User: name, Password: pw}, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cr, err := c.SignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := New(cr, proxy.Client())
	return k
}

func names[T any](xs []T, f func(T) string) string {
	var out []string
	for _, x := range xs {
		out = append(out, f(x))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestLiveSignIn(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	if c.c.Token == "" || c.c.Password != "" || c.c.UserID != s.User.ID {
		t.Fatalf("%+v", c.c)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad, _ := New(Creds{Server: proxy.URL, User: s.User.Name, Password: "wrong"}, proxy.Client())
	if _, err := bad.SignIn(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong password: %v", err)
	}
}

// Quick Connect end to end: the Q2 asks for a code, the user approves it as Jellyfin's apps do
// (POST /QuickConnect/Authorize with their own token), and the Q2's poll and sign-in follow.
func TestLiveQuickConnect(t *testing.T) {
	s, proxy := live(t)
	ctx := context.Background()
	q2, _ := New(Creds{Server: proxy.URL}, proxy.Client())
	code, secret, err := q2.QuickConnectInitiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := q2.QuickConnectPoll(ctx, secret); ok || err != nil {
		t.Fatalf("approved before approval: %v %v", ok, err)
	}
	phone := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	if err := phone.call(ctx, "POST", "/QuickConnect/Authorize", url.Values{"code": {code}, "userId": {s.User.ID}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := q2.QuickConnectPoll(ctx, secret); !ok || err != nil {
		t.Fatalf("after approval: %v %v", ok, err)
	}
	cr, err := q2.QuickConnectSignIn(ctx, secret)
	if err != nil || cr.Token == "" || cr.UserID != s.User.ID {
		t.Fatalf("%+v %v", cr, err)
	}
	if _, err := q2.QuickConnectPoll(ctx, "not-a-secret"); !errors.Is(err, ErrQuickConnectExpired) {
		t.Fatalf("unknown secret: %v", err)
	}
}

func TestLiveAPIKey(t *testing.T) {
	s, proxy := live(t)
	c, _ := New(Creds{Server: proxy.URL, Token: s.APIKey, Kind: KindAPIKey}, proxy.Client())
	cr, err := c.SignInWithKey(context.Background(), strings.ToUpper(s.User.Name))
	if err != nil || cr.UserID != s.User.ID {
		t.Fatalf("%+v %v", cr, err)
	}
	k, _ := New(cr, proxy.Client())
	if err := k.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if al, err := k.Albums(context.Background(), 0, 50, "name", false); err != nil || len(al) == 0 {
		t.Fatalf("browse with the key: %d %v", len(al), err)
	}
}

// Each album order, and its reverse, as the server sorts them (years 2001, 2010, 2015, 2020).
func TestLiveAlbumOrders(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	get := func(order string, reverse bool) string {
		al, err := c.Albums(context.Background(), 0, 50, order, reverse)
		if err != nil {
			t.Fatal(err)
		}
		var out []string // in the server's order (names sorts)
		for _, a := range al {
			out = append(out, a.Name)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		order         string
		want, reverse string
	}{
		{"name", "Compilation,First Album,Odd Album,Second Album", "Second Album,Odd Album,First Album,Compilation"},
		{"released", "Odd Album,Compilation,Second Album,First Album", "First Album,Second Album,Compilation,Odd Album"},
		{"albumartist", "Odd Album,Second Album,First Album,Compilation", "Compilation,First Album,Second Album,Odd Album"},
	} {
		if got := get(tc.order, false); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.order, got, tc.want)
		}
		if got := get(tc.order, true); got != tc.reverse {
			t.Errorf("%s reversed: %q, want %q", tc.order, got, tc.reverse)
		}
	}
	added, back := strings.Split(get("added", false), ","), strings.Split(get("added", true), ",")
	slices.Reverse(back)
	if len(added) != 4 || !slices.Equal(added, back) {
		t.Errorf("added %q, reversed %q", added, back)
	}
}

// Artists and playlists in each order, as the server sorts them: the test server's artists were
// added Odd, Second, Test, Various; its playlists' run times are Mixed Bag 27 s, Road Trip 26 s,
// Movie Night 0 (a movie only, so it isn't listed: video playlists stay out).
func TestLiveArtistAndPlaylistOrders(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	ctx := context.Background()
	inOrder := func(xs []string, err error) string {
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(xs, ",")
	}
	artists := func(o string, r bool) string {
		ar, err := c.Artists(ctx, o, r)
		var out []string
		for _, a := range ar {
			out = append(out, a.Name)
		}
		return inOrder(out, err)
	}
	playlists := func(o string, r bool) string {
		pls, err := c.Playlists(ctx, o, r)
		var out []string
		for _, p := range pls {
			out = append(out, p.Name)
		}
		return inOrder(out, err)
	}
	for _, tc := range []struct {
		got, want string
	}{
		{artists("name", false), "Odd Artist,Second Artist,Test Artist,Various Artists"},
		{artists("name", true), "Various Artists,Test Artist,Second Artist,Odd Artist"},
		{artists("added", false), "Various Artists,Test Artist,Second Artist,Odd Artist"},
		{artists("added", true), "Odd Artist,Second Artist,Test Artist,Various Artists"},
		{playlists("duration", false), "Mixed Bag,Road Trip"},
		{playlists("duration", true), "Road Trip,Mixed Bag"},
		{playlists("name", true), "Road Trip,Mixed Bag"},
	} {
		if tc.got != tc.want {
			t.Errorf("%q, want %q", tc.got, tc.want)
		}
	}
}

// Music only: no movie, audiobook or mixed-library file anywhere the app lists songs.
func TestLiveMusicOnly(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	ctx := context.Background()

	al, err := c.Albums(ctx, 0, 50, "name", false)
	if got := names(al, func(a Album) string { return a.Name }); err != nil || got != "Compilation,First Album,Odd Album,Second Album" {
		t.Errorf("albums %q %v", got, err)
	}
	ar, err := c.Artists(ctx, "name", false)
	if got := names(ar, func(a Artist) string { return a.Name }); err != nil || got != "Odd Artist,Second Artist,Test Artist,Various Artists" {
		t.Errorf("artists %q %v", got, err)
	}
	for _, a := range ar { // each has one album: AlbumCount comes with ItemCounts only
		if a.AlbumCount != 1 {
			t.Errorf("%s: %d albums", a.Name, a.AlbumCount)
		}
	}
	if ars, _, _, err := c.Search(ctx, "Test Artist", 0); err != nil || len(ars) != 1 || ars[0].AlbumCount != 1 {
		t.Errorf("search Test Artist: %+v %v", ars, err)
	}
	fav, err := c.Favorites(ctx)
	if got := names(fav, func(x Song) string { return x.Title }); err != nil || got != "Alpha,Delta,Zeta" {
		t.Errorf("favourites %q %v", got, err)
	}
	for _, term := range []string{"Memo", "Test Book", "Test Movie"} {
		ars, als, ss, err := c.Search(ctx, term, 0)
		if err != nil || len(ss) != 0 || len(als) != 0 || len(ars) != 0 {
			t.Errorf("search %q: artists %d albums %d songs %v %v", term, len(ars), len(als),
				names(ss, func(x Song) string { return x.Title }), err)
		}
	}
	pls, err := c.Playlists(ctx, "name", false)
	if got := names(pls, func(p Playlist) string { return p.Name }); err != nil || got != "Mixed Bag,Road Trip" {
		t.Errorf("playlists %q %v", got, err)
	}
	_, mixed, err := c.Playlist(ctx, s.Lists["Mixed Bag"])
	var titles []string
	for _, x := range mixed {
		titles = append(titles, x.Title)
	}
	if got := strings.Join(titles, ","); err != nil || got != "Gamma,Epsilon" {
		t.Errorf("Mixed Bag %q %v", got, err)
	}
}

func TestLivePlaylistOrderAndDuplicates(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	p, songs, err := c.Playlist(context.Background(), s.Lists["Road Trip"])
	var titles []string
	for _, x := range songs {
		titles = append(titles, x.Title)
	}
	if got := strings.Join(titles, ","); err != nil || got != "Beta,Alpha,Beta,Eta" {
		t.Fatalf("%s: %q %v", p.Name, got, err)
	}
}

func TestLiveSongMetadata(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	sg, err := c.Song(context.Background(), s.Items["Zeta"])
	if err != nil {
		t.Fatal(err)
	}
	if sg.Title != "Zeta" || sg.Artist != "Guest One" || sg.AlbumArtist != "Various Artists" || sg.Album != "Compilation" ||
		sg.Track != 1 || sg.Year != 2015 || sg.Suffix != "opus" || sg.Size <= 0 || sg.Duration < 5 {
		t.Fatalf("%+v", sg)
	}
	if _, err := c.Song(context.Background(), "00000000000000000000000000000000"); err == nil {
		t.Fatal("a missing song came back")
	}
}

// fetch reads a song's bytes through the client.
func fetch(t *testing.T, c *Client, u string) []byte {
	t.Helper()
	resp, err := c.Get(context.Background(), u, 0)
	if err != nil {
		t.Fatalf("%s: %v", u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Original is the server's file byte for byte; a user without the download permission is told so
// for a sync, and can still stream.
func TestLiveOriginal(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	sg, _ := c.Song(context.Background(), s.Items["Alpha"])
	b := fetch(t, c, c.MediaURL(sg.ID, "original"))
	if int64(len(b)) != sg.Size || !bytes.HasPrefix(b, []byte("fLaC")) {
		t.Fatalf("download %d bytes (server says %d), head %q", len(b), sg.Size, b[:min(4, len(b))])
	}
	if p := fetch(t, c, c.PlayURL(sg.ID, "original")); !bytes.Equal(p, b) {
		t.Fatal("stream differs from download")
	}
	n := liveSignIn(t, s, proxy, s.NoDL.Name, s.NoDL.Password)
	_, err := n.Get(context.Background(), n.MediaURL(sg.ID, "original"), 0)
	if !errors.Is(err, ErrDownloadsOff) {
		t.Fatalf("no-download user's download: %v", err)
	}
	if p := fetch(t, n, n.PlayURL(sg.ID, "original")); !bytes.Equal(p, b) {
		t.Fatal("no-download user can't stream")
	}
}

// An Opus transcode is Ogg Opus at about the bitrate asked, with the song's tags.
func TestLiveOpus(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	sg, _ := c.Song(context.Background(), s.Items["Beta"])
	for _, kbps := range []int{64, 320} {
		q := "opus-" + map[int]string{64: "64", 320: "320"}[kbps]
		b := fetch(t, c, c.MediaURL(sg.ID, q))
		if !bytes.HasPrefix(b, []byte("OggS")) || !bytes.Contains(b, []byte("OpusHead")) {
			t.Fatalf("%s: not Ogg Opus: %q", q, b[:min(40, len(b))])
		}
		kbit := float64(len(b)*8) / float64(sg.Duration) / 1000
		t.Logf("%s: %d bytes for %d s = %.0f kbit/s", q, len(b), sg.Duration, kbit)
		if kbit > float64(kbps)*1.6+16 {
			t.Errorf("%s: %.0f kbit/s, far above the %d asked", q, kbit, kbps)
		}
		// A streamed transcode, which the playback cache keeps and a sync may take (U32), is the
		// same request and carries the same tags.
		p := fetch(t, c, c.PlayURL(sg.ID, q))
		for _, tag := range []string{"TITLE=Beta", "ARTIST=Test Artist", "ALBUM=First Album"} {
			if !bytes.Contains(bytes.ToUpper(b), bytes.ToUpper([]byte(tag))) {
				t.Errorf("%s: tag %s missing", q, tag)
			}
			if !bytes.Contains(bytes.ToUpper(p), bytes.ToUpper([]byte(tag))) {
				t.Errorf("%s: tag %s missing from the stream", q, tag)
			}
		}
	}
	// The old-firmware fallback: MP3 at the same rate.
	b := fetch(t, c, c.MediaURL(sg.ID, "mp3-128"))
	if !bytes.HasPrefix(b, []byte("ID3")) && !(len(b) > 1 && b[0] == 0xFF && b[1]&0xE0 == 0xE0) {
		t.Fatalf("mp3-128: not MP3: %q", b[:min(8, len(b))])
	}
}

// Lyrics come from the server: Alpha's .lrc, synced, as LRC; Beta has none.
func TestLiveLyrics(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	ctx := context.Background()
	sg, err := c.Song(ctx, s.Items["Alpha"])
	if err != nil || !sg.HasLyrics {
		t.Fatalf("HasLyrics %v %v", sg.HasLyrics, err)
	}
	got, err := c.Lyrics(ctx, s.Items["Alpha"])
	want := "[00:00.50]First line of Alpha\n[00:02.25]Second line of Alpha\n[00:04.00]Third line of Alpha\n"
	if err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}
	if b, _ := c.Song(ctx, s.Items["Beta"]); b.HasLyrics {
		t.Fatal("Beta has lyrics")
	}
	if got, err := c.Lyrics(ctx, s.Items["Beta"]); got != "" || err != nil {
		t.Fatalf("Beta %q %v", got, err)
	}
}

func TestLiveLatest(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	al, err := c.Latest(context.Background())
	if got := names(al, func(a Album) string { return a.Name }); err != nil || got != "Compilation,First Album,Odd Album,Second Album" {
		t.Fatalf("%q %v", got, err)
	}
}

// Playback reporting (U31) on a real server: a start counts the play, progress and a stop change no count, a
// play marked later counts one more with its own date, and an unknown item is refused for good.
func TestLiveReport(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	ctx := context.Background()
	id := s.Items["Gamma"]
	type userData struct {
		PlayCount      int
		Played         bool
		LastPlayedDate time.Time
	}
	state := func() userData {
		t.Helper()
		var it struct{ UserData userData }
		if err := c.call(ctx, "GET", "/Items/"+id, url.Values{"userId": {s.User.ID}}, nil, &it); err != nil {
			t.Fatal(err)
		}
		return it.UserData
	}
	t.Cleanup(func() { c.call(ctx, "DELETE", "/UserPlayedItems/"+id, nil, nil, nil) })
	before := state()
	if err := c.PlaybackStarted(ctx, id); err != nil {
		t.Fatal(err)
	}
	if u := state(); u.PlayCount != before.PlayCount+1 || !u.Played {
		t.Fatalf("after start: %+v, was %+v", u, before)
	}
	// Progress (Q2 Pod's now: position and pause) is taken and changes no count.
	for _, paused := range []bool{false, true} {
		if err := c.PlaybackProgress(ctx, id, 2*time.Second, paused); err != nil {
			t.Fatal(err)
		}
	}
	if u := state(); u.PlayCount != before.PlayCount+1 {
		t.Fatalf("after progress: %+v", u)
	}
	if err := c.PlaybackStopped(ctx, id, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if u := state(); u.PlayCount != before.PlayCount+1 {
		t.Fatalf("after stop: %+v", u)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := c.MarkPlayed(ctx, id, at); err != nil {
		t.Fatal(err)
	}
	if u := state(); u.PlayCount != before.PlayCount+2 || !u.LastPlayedDate.Equal(at) {
		t.Fatalf("after mark: %+v", u)
	}
	// What the outbox asks before sending again a play whose answer was lost: exactly datePlayed.
	if last, err := c.LastPlayed(ctx, id); err != nil || !last.Equal(at) {
		t.Fatalf("last played %v %v", last, err)
	}
	err := c.MarkPlayed(ctx, "0123456789abcdef0123456789abcdef", at)
	if !Refused(err) {
		t.Fatalf("unknown item: %v", err)
	}
	t.Logf("unknown item: %v", err)
}

// U30: a heart sent is the server's favourite, read back as UserData.IsFavorite (Song, a
// playlist's songs and the Favourites list); the same state sent again is no error, and an item
// the server doesn't know is refused for good.
func TestLiveFavorite(t *testing.T) {
	s, proxy := live(t)
	c := liveSignIn(t, s, proxy, s.User.Name, s.User.Password)
	ctx := context.Background()
	id := s.Items["Beta"] // not one of setup.py's favourites
	t.Cleanup(func() { c.SetFavorite(ctx, id, false) })
	fav := func() bool {
		t.Helper()
		sg, err := c.Song(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return sg.Favorite
	}
	if fav() {
		t.Fatal("Beta is a favourite before the test")
	}
	for _, on := range []bool{true, true, false, false, true} {
		if err := c.SetFavorite(ctx, id, on); err != nil {
			t.Fatalf("set %v: %v", on, err)
		}
		if got := fav(); got != on {
			t.Fatalf("set %v, server has %v", on, got)
		}
	}
	favs, err := c.Favorites(ctx)
	if err != nil || !slices.ContainsFunc(favs, func(x Song) bool { return x.ID == id && x.Favorite }) {
		t.Fatalf("Favourites %+v %v", favs, err)
	}
	// /Items gives user data without enableUserData: an album's songs and a search's carry hearts.
	sg, _ := c.Song(ctx, id)
	_, al, err := c.Album(ctx, sg.AlbumID)
	if err != nil || !slices.ContainsFunc(al, func(x Song) bool { return x.ID == id && x.Favorite }) {
		t.Fatalf("album %+v %v", al, err)
	}
	_, _, found, err := c.Search(ctx, "Beta", 0)
	if err != nil || !slices.ContainsFunc(found, func(x Song) bool { return x.ID == id && x.Favorite }) {
		t.Fatalf("search %+v %v", found, err)
	}
	_, pl, err := c.Playlist(ctx, s.Lists["Road Trip"]) // Beta, Alpha (a favourite), Beta, Eta
	if err != nil || len(pl) != 4 || !pl[0].Favorite || !pl[1].Favorite || pl[3].Favorite {
		t.Fatalf("playlist %+v %v", pl, err)
	}
	if err := c.SetFavorite(ctx, "0123456789abcdef0123456789abcdef", true); !Refused(err) {
		t.Fatalf("unknown item: %v", err)
	}
}
