package main

import (
	"io"
	"net"
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

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

// relayDaemon is a daemon signed in (token "tok") to a server answering with up, and the URL of
// its relay. The server's certificate is trusted only when trusted: else the client is the app's
// own, which checks it against the embedded roots.
func relayDaemon(t *testing.T, up http.HandlerFunc, trusted, signedIn bool) (*tdaemon, string) {
	t.Helper()
	srv := httptest.NewTLSServer(up)
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "urls", false)
	hc := srv.Client()
	if !trusted {
		hc = tlsroots.Client(5 * time.Second)
	}
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, hc) }
	if signedIn {
		if err := creds.Save(d.dir, jellyfin.Creds{Server: srv.URL, User: "u", Token: "tok", UserID: "u1", DeviceID: "dev1"}); err != nil {
			t.Fatal(err)
		}
	}
	front := httptest.NewServer(http.HandlerFunc(d.serveRelay))
	t.Cleanup(front.Close)
	return d, front.URL
}

// The relay listens on the Q2's loopback only, on its fixed port: the Wi-Fi address doesn't
// answer. A second app finding the port taken logs it and runs on without the relay.
func TestRelayLoopbackOnly(t *testing.T) {
	if relayAddr != "127.0.0.1:2317" {
		t.Fatalf("relayAddr %q", relayAddr)
	}
	d := newTestDaemon(t, &nav{}, "urls", true)
	d.startRelay(relayAddr)
	if d.relay == nil {
		t.Fatal("not listening")
	}
	t.Cleanup(func() { d.relay.Close() })
	c, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && !ip.IP.IsLoopback() && ip.IP.To4() != nil {
			if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip.IP.String(), "2317"), time.Second); err == nil {
				c.Close()
				t.Errorf("answers on %s", ip.IP)
			}
		}
	}
	d2 := newTestDaemon(t, &nav{}, "urls", true)
	d2.startRelay(relayAddr)
	if d2.relay != nil {
		d2.relay.Close()
		t.Fatal("a second relay on the same port")
	}
	if b, _ := os.ReadFile(filepath.Join(d2.dir, "log.txt")); !strings.Contains(string(b), "relay: ") {
		t.Errorf("not logged: %q", b)
	}
}

// A song reaches the server at the URL the app streams it from itself, with the app's sign-in in
// the Authorization header and nothing in the URL; of the player's headers only Range goes, and
// the server's partial answer comes back (seeking). A cover is the list's 320 px square.
func TestRelaySong(t *testing.T) {
	var got atomic.Value
	d, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Clone(r.Context()))
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader("0123456789abcdefghij"))
	}, true, true)
	cl := d.client()
	for _, c := range []struct{ path, want string }{
		{"/song/0123abcd-ef?q=opus-128", cl.PlayURL("0123abcd-ef", "opus-128")},
		{"/song/0123abcd?q=original", cl.PlayURL("0123abcd", "original")},
		{"/cover/beef", cl.ThumbURL("beef", 320)},
	} {
		req, _ := http.NewRequest("GET", front+c.path, nil)
		req.Header.Set("Range", "bytes=10-14")
		req.Header.Set("Cookie", "player=1")
		req.Header.Set("Authorization", "Bearer someone-else")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 10-14/20" || string(body) != "abcde" {
			t.Errorf("%s: %d %q %q", c.path, resp.StatusCode, resp.Header.Get("Content-Range"), body)
		}
		r, _ := got.Load().(*http.Request)
		if r == nil {
			t.Fatalf("%s: never reached the server", c.path)
		}
		if want := strings.TrimPrefix(c.want, "https://"+r.Host); r.URL.RequestURI() != want {
			t.Errorf("%s: server got %s, want %s", c.path, r.URL.RequestURI(), want)
		}
		if strings.Contains(strings.ToLower(r.URL.RawQuery), "apikey") || strings.Contains(r.URL.RawQuery, "tok") {
			t.Errorf("%s: a token in the URL: %s", c.path, r.URL)
		}
		if a := r.Header.Get("Authorization"); !strings.Contains(a, `Token="tok"`) || !strings.HasPrefix(a, "MediaBrowser ") {
			t.Errorf("%s: Authorization %q", c.path, a)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Range") != "bytes=10-14" {
			t.Errorf("%s: headers %v", c.path, r.Header)
		}
	}
}

// A redirect is followed by the app, as its own requests are: the token never goes to another host.
func TestRelayRedirect(t *testing.T) {
	var auth atomic.Value
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.Write([]byte("song"))
	}))
	t.Cleanup(other.Close)
	_, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/file", http.StatusFound)
	}, true, true)
	resp, err := http.Get(front + "/song/abc?q=original")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "song" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if a, _ := auth.Load().(string); a != "" {
		t.Errorf("the other host got Authorization %q", a)
	}

	// A redirect to plain http on the same host is refused: the token would go in clear text.
	_, front = relayDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/file", http.StatusFound)
	}, true, true)
	resp, err = http.Get(front + "/song/abc?q=original")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirect to http: %d, want 502", resp.StatusCode)
	}
}

// Only a song id with a known quality, or a cover id, is relayed: anything else is 404 and never
// reaches the server. Signed out, 503.
func TestRelayRefuses(t *testing.T) {
	var hits atomic.Int32
	_, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }, true, true)
	for _, p := range []string{
		"/song/abc", "/song/abc?q=", "/song/abc?q=opus-999", "/song/abc?q=opus-0128", "/song/abc?q=flac",
		"/song/../Users?q=original", "/song/ab%2F..%2FUsers?q=original", "/song/xyz?q=original",
		"/song/" + strings.Repeat("a", 65) + "?q=original", "/song/?q=original", "/song/abc/def?q=original",
		"/cover/", "/cover/abc/x", "/cover/ab%3Fc", "/Audio/abc/stream", "/", "/songs/abc?q=original",
	} {
		resp, err := http.Get(front + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	resp, err := http.Post(front+"/song/abc?q=original", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("POST: %d", resp.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d reached the server", n)
	}

	_, front = relayDaemon(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }, true, false)
	if resp, err := http.Get(front + "/song/abc?q=original"); err != nil || resp.StatusCode != 503 {
		t.Fatalf("signed out: %v %v", resp, err)
	}
}

// A server whose certificate the app doesn't trust gets nothing, and the player nothing from it;
// the server shows as not connected.
func TestRelayUntrusted(t *testing.T) {
	var hits atomic.Int32
	d, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }, false, true)
	resp, err := http.Get(front + "/song/abc?q=original")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || len(body) != 0 || hits.Load() != 0 {
		t.Fatalf("%d %q, %d requests served", resp.StatusCode, body, hits.Load())
	}
	if _, err := d.client().Last(); err == nil { // Settings says Not connected, as for a list
		t.Error("a song the relay couldn't fetch left the server connected")
	}
}

// The URL queue holds the relay's addresses and no token: each song and cover is fetched through
// the app.
func TestURLQueueNoToken(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "urls", true)
	d.do("play", "album:al0 0")
	d.queueSeq(t)
	lines := d.urlLines(t)
	if len(lines) != 3 {
		t.Fatalf("%q", lines)
	}
	for i, l := range lines[1:] {
		f := strings.Split(l, "\t")
		if f[0] != "http://127.0.0.1:2317/song/al0."+string(rune('0'+i))+"?q=original" {
			t.Errorf("line %d: %q", i, l)
		}
		if strings.Contains(l, "tok") || strings.Contains(strings.ToLower(l), "apikey") {
			t.Errorf("line %d holds the token: %q", i, l)
		}
	}
	if u := (src{d.daemon}).PlayerCoverURL("al0"); u != "http://127.0.0.1:2317/cover/al0" { // nav's songs have no cover
		t.Errorf("cover %q", u)
	}
}

// A queue playing through the relay keeps the app up by the data it moves: a request, then each
// read of the server's answer, counts as a press. A song that stops sending (a hung server, or a
// player paused with its connection open) lets the app exit idleAfter after its last byte.
func TestRelayKeepsAppUp(t *testing.T) {
	more, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(release) }) }
	d, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		select {
		case <-more:
			w.Write([]byte("more"))
			w.(http.Flusher).Flush()
		case <-release:
		}
		<-release // then nothing: a stalled server
	}, true, true)
	t.Cleanup(end) // before the servers close: a failure leaves no handler waiting
	d.mu.Lock()
	d.lastReq = time.Now().Add(-2 * idleAfter) // long after the last press
	d.mu.Unlock()
	if !d.idle(time.Now()) {
		t.Fatal("not idle before the song")
	}
	resp, err := http.Get(front + "/song/abc?q=original")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 5)
	io.ReadFull(resp.Body, buf)
	t1 := time.Now()
	if d.idle(t1) {
		t.Fatal("exits while a song streams")
	}
	if !d.idle(t1.Add(idleAfter)) {
		t.Fatal("no data for idleAfter: still up")
	}
	d.relayAt.Store(-int64(2 * idleAfter)) // as if the last byte came long ago
	if !d.idle(time.Now()) {
		t.Fatal("not idle with no data for 2 idleAfter")
	}
	close(more) // the song goes on
	io.ReadFull(resp.Body, buf[:4])
	if d.idle(time.Now()) {
		t.Fatal("data read later didn't keep the app up")
	}
	if !d.idle(time.Now().Add(idleAfter + time.Second)) {
		t.Fatal("stalled: still up idleAfter after the last byte")
	}
}

// A song's failed answer reaches the player as it is, and is logged, with no token, and sets
// Settings' status: 401 the sign-in refused, 502-504 a server down, anything else reached. A 416
// (a seek past the end) isn't logged.
func TestRelayFailedLogged(t *testing.T) {
	var code atomic.Int32
	d, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(code.Load())) }, true, true)
	for _, c := range []struct {
		code      int
		logged    bool
		reachable bool
	}{{401, true, false}, {500, true, true}, {503, true, false}, {404, true, true}, {416, false, true}, {200, false, true}} {
		code.Store(int32(c.code))
		before, _ := os.ReadFile(filepath.Join(d.dir, "log.txt"))
		resp, err := http.Get(front + "/song/abc?q=original")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%d: player got %d", c.code, resp.StatusCode)
		}
		after, _ := os.ReadFile(filepath.Join(d.dir, "log.txt"))
		line := string(after[len(before):])
		if strings.Contains(line, "-> "+strconv.Itoa(c.code)) != c.logged {
			t.Errorf("%d: log %q", c.code, line)
		}
		if strings.Contains(line, "tok") {
			t.Errorf("%d: the token logged: %q", c.code, line)
		}
		if _, err := d.client().Last(); (err == nil) != c.reachable {
			t.Errorf("%d: last %v", c.code, err)
		}
	}
}

// A Now Playing cover is at most artMax, as a list row's: a bigger one with its length is refused
// (502), one without a length fails once past artMax (the connection drops). A song has no limit.
func TestRelayCoverLimit(t *testing.T) {
	var size, chunked atomic.Int32
	_, front := relayDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, size.Load())
		if chunked.Load() == 0 {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		}
		w.Write(b)
	}, true, true)
	for _, c := range []struct {
		path     string
		size     int
		chunked  bool
		wantCode int
		wantLen  int // -1: the body fails
	}{
		{"/cover/beef", artMax, false, 200, artMax},
		{"/cover/beef", artMax, true, 200, artMax},
		{"/cover/beef", artMax + 1, false, 502, 0},
		{"/cover/beef", artMax + 100, true, 200, -1},
		{"/song/abc?q=original", artMax + 100, false, 200, artMax + 100},
	} {
		size.Store(int32(c.size))
		chunked.Store(0)
		if c.chunked {
			chunked.Store(1)
		}
		resp, err := http.Get(front + c.path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := len(b)
		if err != nil && got <= artMax {
			got = -1
		}
		if resp.StatusCode != c.wantCode || got != c.wantLen {
			t.Errorf("%s %d bytes (chunked %v): %d, %d bytes, %v", c.path, c.size, c.chunked, resp.StatusCode, len(b), err)
		}
	}
}
