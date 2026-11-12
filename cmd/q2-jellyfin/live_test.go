package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

// End-to-end tests of the app (daemon, sync engine, card layout) against a real Jellyfin server
// made by contrib/testserver/setup.py. They run when JELLYFIN_TEST_SERVER names its server.json;
// the daemon reaches the server through a TLS proxy of the test's own, as it only speaks HTTPS.

type liveConf struct {
	URL   string                          `json:"url"`
	User  struct{ Name, Password string } `json:"user"`
	Lists map[string]string               `json:"playlists"`
}

// liveDaemon is a daemon signed in to the test server as its music user, on an empty card with
// room bytes above Space to Keep Free (0: plenty).
func liveDaemon(t *testing.T, room int64) (*tdaemon, *liveConf) {
	t.Helper()
	f := os.Getenv("JELLYFIN_TEST_SERVER")
	if f == "" {
		t.Skip("set JELLYFIN_TEST_SERVER to contrib/testserver's server.json to run")
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var conf liveConf
	if err := json.Unmarshal(b, &conf); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(conf.URL)
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	card, tmp := t.TempDir(), t.TempDir()
	d := newDaemon(filepath.Join(card, ".jellyfin"), card, tmp)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, proxy.Client()) }
	d.runner.CardMounted = func() bool { return true }
	d.localIP = func() string { return "192.168.1.20" }
	td := &tdaemon{d, tmp}
	if room > 0 {
		cardOf(td, room)
	} else {
		d.free = func(string) int64 { return 1 << 40 }
	}
	cl, _ := jellyfin.New(jellyfin.Creds{Server: proxy.URL, User: conf.User.Name, Password: conf.User.Password}, proxy.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cr, err := cl.SignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Save(d.dir, cr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.stream.stop()
		d.runner.Cancel()
		proxy.CloseClientConnections()
		proxy.Close()
		d.bg.Wait()
	})
	return td, &conf
}

// onCard is the songs synced, as "<folder>/<file>" under Jellyfin/, sorted.
func onCard(t *testing.T, d *tdaemon) []string {
	t.Helper()
	var out []string
	root := filepath.Join(d.card, "Jellyfin")
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.HasSuffix(p, ".jpg") && !strings.HasSuffix(p, ".lrc") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// m3uTitles is a playlist file's songs, by the title in each line's file name, in order.
func m3uTitles(t *testing.T, d *tdaemon, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(d.card, "_explaylist_data", name+".m3u"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(l), filepath.Ext(l))
		_, title, _ := strings.Cut(base, " - ")
		out = append(out, title)
	}
	return out
}

func waitSync(t *testing.T, d *tdaemon, state string) syncer.Snapshot {
	t.Helper()
	var s syncer.Snapshot
	for i := 0; i < 600; i++ {
		if s = d.runner.Snapshot(); s.State == state {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %s: %+v", state, s)
	return s
}

// Sync Favourites in Original: the hearts' songs only (no movie, audiobook or mixed-library file),
// each the server's own file, and a Favourites playlist.
func TestLiveSyncFavourites(t *testing.T) {
	d, _ := liveDaemon(t, 0)
	if m := d.do("syncfav").Message; m != "Added to Sync · 3 songs · starting" {
		t.Fatalf("%q", m)
	}
	waitSync(t, d, "done")
	want := []string{"Second Artist/2010 - Second Album/01 - Delta.mp3", "Test Artist/2001 - First Album/01 - Alpha.flac",
		"Various Artists/2015 - Compilation/01 - Zeta.opus"}
	if got := onCard(t, d); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("on the card %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(d.card, "Jellyfin", want[1]))
	if !bytes.HasPrefix(b, []byte("fLaC")) {
		t.Fatal("Alpha is not the server's FLAC")
	}
	if got := strings.Join(m3uTitles(t, d, "Favourites"), ","); got != "Delta,Alpha,Zeta" && got != "Alpha,Delta,Zeta" {
		t.Fatalf("Favourites.m3u %q", got)
	}
	// Alpha's lyrics, from the server, beside it for Q2 Pod's player; none for the others.
	lrc, err := os.ReadFile(filepath.Join(d.card, "Jellyfin", "Test Artist/2001 - First Album/01 - Alpha.lrc"))
	if want := "[00:00.50]First line of Alpha\n[00:02.25]Second line of Alpha\n[00:04.00]Third line of Alpha\n"; err != nil || string(lrc) != want {
		t.Fatalf("Alpha.lrc %q %v", lrc, err)
	}
	if m, _ := filepath.Glob(filepath.Join(d.card, "Jellyfin", "*", "*", "*.lrc")); len(m) != 1 {
		t.Fatalf("lyrics files %v", m)
	}
	// A song synced without its lyrics (as 0.1 did) gets them when Sync is next opened.
	lp := filepath.Join(d.card, "Jellyfin", "Test Artist/2001 - First Album/01 - Alpha.lrc")
	os.Remove(lp)
	d.do("check")
	d.preview(t)
	for i := 0; i < 100 && !exists(lp); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if b, _ := os.ReadFile(lp); !strings.Contains(string(b), "First line of Alpha") {
		t.Fatalf("lyrics not restored: %q", b)
	}
}

// At an Opus Sync Quality, the same sync brings Ogg Opus files, tagged, at that quality.
func TestLiveSyncFavouritesOpus(t *testing.T) {
	d, _ := liveDaemon(t, 0)
	d.do("setdefq", "opus-96")
	d.do("syncfav")
	waitSync(t, d, "done")
	got := onCard(t, d)
	if len(got) != 3 {
		t.Fatalf("%q", got)
	}
	for _, f := range got {
		b, _ := os.ReadFile(filepath.Join(d.card, "Jellyfin", f))
		if !strings.HasSuffix(f, ".opus") || !bytes.HasPrefix(b, []byte("OggS")) || !bytes.Contains(b, []byte("OpusTags")) {
			t.Errorf("%s: not Ogg Opus", f)
		}
		title := strings.TrimSuffix(f[strings.LastIndex(f, " - ")+3:], ".opus")
		if !bytes.Contains(bytes.ToUpper(b), []byte("TITLE="+strings.ToUpper(title))) {
			t.Errorf("%s: no TITLE tag", f)
		}
	}
}

// Choose Playlists: a playlist's songs in its order, duplicates kept, music only.
func TestLiveSyncPlaylists(t *testing.T) {
	d, conf := liveDaemon(t, 0)
	r := d.do("syncpl")
	if got := strings.Join(titles(r), "|"); got != "Mixed Bag|Road Trip" {
		t.Fatalf("playlists %q", got)
	}
	for _, row := range r.Rows {
		d.do(row.URI, row.Context)
	}
	d.preview(t)
	d.do("start")
	waitSync(t, d, "done")
	if got := strings.Join(m3uTitles(t, d, "Road Trip"), ","); got != "Beta,Alpha,Beta,Eta" {
		t.Errorf("Road Trip %q", got)
	}
	if got := strings.Join(m3uTitles(t, d, "Mixed Bag"), ","); got != "Gamma,Epsilon" {
		t.Errorf("Mixed Bag %q", got)
	}
	for _, f := range onCard(t, d) {
		if strings.Contains(f, "Memo") || strings.Contains(f, "Book") || strings.Contains(f, "Movie") {
			t.Errorf("not music: %s", f)
		}
	}
	_ = conf
}

// With little room, the sync stops before the card falls under Space to Keep Free, says so, and
// what it brought plays from the Favourites playlist.
func TestLiveSyncStopsAtReserve(t *testing.T) {
	d, _ := liveDaemon(t, 160_000) // room for about one of the test songs
	d.do("syncfav")
	s := waitSync(t, d, "paused")
	var sl *syncer.SpaceLowError
	if !errors.As(s.Err, &sl) || s.Done >= 3 {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(strings.Join(titles(d.do("sync")), "|"), "Stopped: the card is nearly full") {
		t.Fatalf("%q", titles(d.do("sync")))
	}
	if n, have := len(m3uTitles(t, d, "Favourites")), len(onCard(t, d)); n != have {
		t.Fatalf("playlist %d songs, card %d", n, have)
	}
}
