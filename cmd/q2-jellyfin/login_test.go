package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

const typedPassword = "s3cret-Pw!" // checked never to reach the card or the log

const wrongPassword = "wr0ng-Pw!" // a refused one, checked the same way

// authServer is a Jellyfin server for sign-in only: user "u" / typedPassword, Quick Connect code
// 123456 (approved when approved is set, or answering 401 when qcOff), API key "KEY" (admin).
type authServer struct {
	approved, qcOff, qcGone atomic.Bool // qcGone: the server forgot the secret (404)
	logouts                 atomic.Int32
	requests, signIns       atomic.Int32 // every request; /Users/AuthenticateByName and /Users (the key's check)
}

func (a *authServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	auth := r.Header.Get("Authorization")
	tok := `{"AccessToken":"tok","User":{"Id":"u1","Name":"u"}}`
	switch r.URL.Path {
	case "/Sessions/Logout":
		if r.Method == "POST" {
			a.logouts.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	case "/System/Info/Public":
		w.Write([]byte(`{"ProductName":"Jellyfin Server","Version":"10.10.7"}`))
	case "/QuickConnect/Initiate":
		if a.qcOff.Load() {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"Code":"123456","Secret":"sec"}`))
	case "/QuickConnect/Connect":
		if a.qcGone.Load() {
			http.Error(w, "Unknown quick connect secret", http.StatusNotFound)
			return
		}
		if a.approved.Load() {
			w.Write([]byte(`{"Authenticated":true}`))
		} else {
			w.Write([]byte(`{"Authenticated":false}`))
		}
	case "/Users/AuthenticateWithQuickConnect":
		if !a.approved.Load() {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(tok))
	case "/Users/AuthenticateByName":
		a.signIns.Add(1)
		var in struct{ Username, Pw string }
		json.NewDecoder(r.Body).Decode(&in)
		if in.Username != "u" || in.Pw != typedPassword {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(tok))
	case "/Users":
		a.signIns.Add(1)
		if !strings.Contains(auth, `Token="KEY"`) {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`[{"Id":"u1","Name":"u"},{"Id":"u2","Name":"Andreas"}]`))
	default:
		http.NotFound(w, r)
	}
}

// loginDaemon is a signed-out daemon with authServer already typed in as its server.
func loginDaemon(t *testing.T) (*tdaemon, *authServer) {
	t.Helper()
	a := &authServer{}
	srv := httptest.NewTLSServer(a)
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	d.login.poll = 5 * time.Millisecond
	if err := d.setServer(srv.URL); err != nil {
		t.Fatal(err)
	}
	return d, a
}

// noSecretOnCard fails when the typed password (or one of others) is in any file the app wrote.
func noSecretOnCard(t *testing.T, d *tdaemon, others ...string) {
	t.Helper()
	filepath.Walk(d.dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			for _, pw := range append(others, typedPassword) {
				if strings.Contains(string(b), pw) {
					t.Errorf("the password %q is in %s", pw, p)
				}
			}
		}
		return nil
	})
}

// With a server set, the root offers Change Server first, opening the Server page, then the three
// sign-ins in order.
func TestLoginRootOrder(t *testing.T) {
	d, _ := loginDaemon(t)
	r := d.do("menu")
	got := titles(r)
	want := []string{"Change Server", "Sign In with Quick Connect", "Sign In with Password"} // no API key while it is off
	if r := d.do("keylogin"); r.Status == "" || r.Status == q2spot.OK || r.Message != keyOff {
		t.Fatalf("keylogin %+v", r)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("root %q", got)
	}
	if row := r.Rows[0]; row.Kind != "list" || row.URI != "server" || row.Subtitle != d.loginServer() { // the full address
		t.Fatalf("Change Server %+v", row)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, serverFile)); !strings.HasPrefix(string(b), "https://") {
		t.Fatalf("server not kept: %q", b)
	}
}

// Without a server, no sign-in is offered or started.
func TestLoginNeedsServer(t *testing.T) {
	keysOn(t)
	d := newTestDaemon(t, &nav{}, "", false)
	for _, op := range []string{"qc", "pwlogin", "keylogin"} {
		if r := d.do(op); r.Message != "Set the server first" {
			t.Errorf("%s: %+v", op, r)
		}
	}
}

// Quick Connect: the code shows on the root while the server waits for approval; once approved,
// the token is kept and the root turns into the signed-in menu. No password is involved.
func TestQuickConnectSignsIn(t *testing.T) {
	d, a := loginDaemon(t)
	if r := d.do("qc"); !strings.Contains(r.Message, "123456") {
		t.Fatalf("qc %+v", r)
	}
	// While it waits: the code, how to approve it beneath, and Cancel; no other way to sign in.
	r := d.do("menu")
	if got := strings.Join(titles(r), "|"); got != "Quick Connect Code: 123456|Approve it in a Jellyfin app:|Cancel Quick Connect" {
		t.Fatalf("root while waiting %q", got)
	}
	if r.Rows[1].Subtitle != "User Menu → Quick Connect" {
		t.Fatalf("how to approve %+v", r.Rows[1])
	}
	if !d.qcWaiting() {
		t.Fatal("not waiting")
	}
	a.approved.Store(true)
	eventually(t, "the sign-in", func() bool { _, ok := creds.Load(d.dir); return ok })
	c, _ := creds.Load(d.dir)
	if c.Token != "tok" || c.Password != "" || c.Kind != "" || c.User != "u" {
		t.Fatalf("%+v", c)
	}
	eventually(t, "the code to go", func() bool { return !d.qcWaiting() })
	if _, ok := find(d.do("menu"), "Settings"); !ok {
		t.Fatalf("root after sign-in %q", titles(d.do("menu")))
	}
}

func TestQuickConnectCancel(t *testing.T) {
	d, _ := loginDaemon(t)
	d.do("qc")
	if r := d.do("qccancel"); r.Message != "Quick Connect cancelled" || d.qcWaiting() {
		t.Fatalf("%+v waiting=%v", r, d.qcWaiting())
	}
	want := "Change Server|Sign In with Quick Connect|Sign In with Password"
	if got := strings.Join(titles(d.do("menu")), "|"); got != want {
		t.Fatalf("after cancel %q", got)
	}
}

func TestQuickConnectOff(t *testing.T) {
	d, a := loginDaemon(t)
	a.qcOff.Store(true)
	if r := d.do("qc"); !strings.Contains(r.Message, "Quick Connect is off on this server") {
		t.Fatalf("%+v", r)
	}
	row, _ := find(d.do("menu"), "Sign In with Quick Connect")
	if !strings.Contains(row.Subtitle, "Quick Connect is off") {
		t.Fatalf("row %+v", row)
	}
}

// loginSuccessPage is a typed secret's page once signed in: a failed reply, which Q2 Pod titles
// with its message instead of the typed text.
func loginSuccessPage(t *testing.T, r q2spot.Reply) {
	t.Helper()
	if r.Status == "" || r.Status == q2spot.OK || r.Message != "Login Success" {
		t.Fatalf("%+v", r)
	}
}

// Password typed on the Q2: user name, then password, each through Q2 Pod's box (a "search"
// request). Only the token is kept; the password is in no file the app wrote, log included.
func TestPasswordTypedOnQ2(t *testing.T) {
	d, _ := loginDaemon(t)
	r := d.do("pwlogin")
	if row := r.Rows[len(r.Rows)-1]; row.Kind != "search" || row.Title != "Type Your User Name" {
		t.Fatalf("%+v", r.Rows)
	}
	r = d.do("search", "u")
	if row := r.Rows[len(r.Rows)-1]; row.Kind != "search" || row.Title != "Type Your Password" {
		t.Fatalf("%+v", r.Rows)
	}
	if _, ok := find(r, "The password shows on screen as you type"); !ok {
		t.Error("no warning that the password shows")
	}
	loginSuccessPage(t, d.do("search", typedPassword))
	c, ok := creds.Load(d.dir)
	if !ok || c.Token != "tok" || c.Password != "" {
		t.Fatalf("%+v", c)
	}
	noSecretOnCard(t, d)
}

// A wrong password says so as the page's title, not the password; nothing is saved. Return asks
// for the user name's page again, which asks for the password again.
func TestPasswordWrongRetries(t *testing.T) {
	d, _ := loginDaemon(t)
	d.do("pwlogin")
	d.do("search", "u")
	r := d.do("search", "nope")
	if r.Status == "" || r.Status == q2spot.OK || r.Message != "Wrong username or password" {
		t.Fatalf("%+v", r)
	}
	if _, ok := creds.Load(d.dir); ok {
		t.Fatal("saved")
	}
	if r = d.do("search", "u"); r.Rows[len(r.Rows)-1].Title != "Type Your Password" { // Return
		t.Fatalf("user name's page again %q", titles(r))
	}
	loginSuccessPage(t, d.do("search", typedPassword))
}

// Retry on a refused password's or key's page sends nothing: the server is asked once, however
// often Retry is pressed (Jellyfin may lock a user out after failed sign-ins). Return, the user
// name's page again, asks for another.
func TestRefusedSecretNotResent(t *testing.T) {
	keysOn(t)
	for _, c := range []struct{ op, wrong, right, msg string }{
		{"pwlogin", "nope", typedPassword, "Wrong username or password"},
		{"keylogin", "WRONG", "KEY", "API key not accepted, or not an admin key"},
	} {
		d, a := loginDaemon(t)
		d.do(c.op)
		d.do("search", "u")
		for range 3 { // typed, then Retry twice
			if r := d.do("search", c.wrong); r.Status == "" || r.Status == q2spot.OK || r.Message != c.msg {
				t.Fatalf("%s: %+v", c.op, r)
			}
		}
		if n := a.signIns.Load(); n != 1 {
			t.Fatalf("%s: asked the server %d times", c.op, n)
		}
		if r := d.do("search", "u"); !strings.HasPrefix(r.Rows[len(r.Rows)-1].Title, "Type ") { // Return
			t.Fatalf("%s: %q", c.op, titles(r))
		}
		loginSuccessPage(t, d.do("search", c.right))
	}
}

// Q2 Pod keeps a typed secret's page when the app exits; its Retry starts a new app and sends the
// text again. A new app has offered no box yet, so the text is neither searched for nor a title,
// and the server is not asked at all.
func TestSecretReplayedAfterRestart(t *testing.T) {
	keysOn(t)
	for _, c := range []struct{ op, secret string }{{"pwlogin", typedPassword}, {"keylogin", "KEY"}} {
		d, a := loginDaemon(t)
		d.do(c.op)
		d.do("search", "u")
		loginSuccessPage(t, d.do("search", c.secret))
		d2 := newTestDaemonOn(t, d.card, &nav{}, "", false) // the same card, after the app exited
		d2.newClient = d.newClient
		if d2.client() == nil {
			t.Fatal("not signed in after a restart")
		}
		for _, wifi := range []bool{true, false} { // with none, never the Wi-Fi page titled with it
			if !wifi {
				d2.localIP = func() string { return "" }
			}
			n := a.requests.Load()
			r := d2.do("search", c.secret)
			if r.Status == "" || r.Status == q2spot.OK || strings.Contains(r.Message, c.secret) {
				t.Fatalf("%s, Wi-Fi %v: %+v", c.op, wifi, r)
			}
			noSecretShown(t, r)
			if got := a.requests.Load(); got != n {
				t.Fatalf("%s: %d requests to the server", c.op, got-n)
			}
		}
	}
}

// keysOn turns API key sign-in on for one test (it is off for now): its code stays tested.
func keysOn(t *testing.T) {
	apiKeySignIn = true
	t.Cleanup(func() { apiKeySignIn = false })
}

// API key typed on the Q2, while it is on: its row is last on the root; every step says it is
// admin-level; the user is picked by name; the key is kept marked as one.
func TestAPIKeyTypedOnQ2(t *testing.T) {
	keysOn(t)
	d, _ := loginDaemon(t)
	if got := strings.Join(titles(d.do("menu")), "|"); got != "Change Server|Sign In with Quick Connect|Sign In with Password|Sign In with an API Key" {
		t.Fatalf("root %q", got)
	}
	r := d.do("keylogin")
	if _, ok := find(r, "An API key is admin-level: it opens every library"); !ok {
		t.Fatalf("no warning: %q", titles(r))
	}
	r = d.do("search", "andreas") // any case
	if _, ok := find(r, "An API key is admin-level: it opens every library"); !ok || r.Rows[len(r.Rows)-1].Title != "Type the API Key" {
		t.Fatalf("%q", titles(r))
	}
	loginSuccessPage(t, d.do("search", "KEY"))
	c, _ := creds.Load(d.dir)
	if c.Kind != jellyfin.KindAPIKey || c.UserID != "u2" || c.Token != "KEY" {
		t.Fatalf("%+v", c)
	}
}

func TestAPIKeyRefusals(t *testing.T) {
	keysOn(t)
	d, _ := loginDaemon(t)
	d.do("keylogin")
	d.do("search", "u")
	if r := d.do("search", "WRONG"); r.Message != "API key not accepted, or not an admin key" {
		t.Fatalf("%+v", r)
	}
	d.do("keylogin")
	d.do("search", "nobody")
	if r := d.do("search", "KEY"); r.Message != "No user by that name on the server" {
		t.Fatalf("%+v", r)
	}
	if _, ok := creds.Load(d.dir); ok {
		t.Fatal("saved")
	}
}

// A search signed out with no sign-in field pending is refused, not run.
func TestSearchSignedOutRefused(t *testing.T) {
	d, _ := loginDaemon(t)
	if r := d.do("search", "x"); r.Message != "Sign in first" {
		t.Fatalf("%+v", r)
	}
}

// Signed out with no server, the root asks only for the server's address, typed on the Q2. A bare
// host gets https://; the server is checked to be Jellyfin before it is kept. No port is opened.
func TestServerTypedOnQ2(t *testing.T) {
	a := &authServer{}
	srv := httptest.NewTLSServer(a)
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	r := d.do("menu")
	if row := r.Rows[len(r.Rows)-1]; len(r.Rows) != 2 || row.Kind != "search" || row.Title != "Type Your Server Address" ||
		row.Subtitle != "e.g. music.example.com or 192.168.1.10:8096" {
		t.Fatalf("root %+v", r.Rows)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	r = d.do("search", host) // no scheme typed
	// The sign-in choices at once, after Change Server (its address beneath) to type it again.
	want := "Change Server|Sign In with Quick Connect|Sign In with Password"
	if got := strings.Join(titles(r), "|"); got != want {
		t.Fatalf("%q", got)
	}
	if row := r.Rows[0]; row.Kind != "list" || row.URI != "server" || row.Subtitle != d.loginServer() { // the full address
		t.Fatalf("Change Server %+v", row)
	}
	if d.loginServer() != srv.URL {
		t.Fatalf("kept %q", d.loginServer())
	}
	if _, ok := find(d.do("menu"), "Sign In with Quick Connect"); !ok {
		t.Fatal("no sign-in offered once the server is set")
	}
}

func TestServerAddressRefusals(t *testing.T) {
	other := httptest.NewTLSServer(http.NotFoundHandler()) // not Jellyfin
	t.Cleanup(other.Close)
	d := newTestDaemon(t, &nav{}, "", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, other.Client()) }
	for typed, want := range map[string]string{
		"http://music.example":                    "HTTPS is required. Use an https:// address",
		"https://alice:my pass@music.example":     "That isn't a server address", // never echoed: it may hold a password
		"https://alice:secret@music.example":      "Leave the user name and password out of the address",
		strings.TrimPrefix(other.URL, "https://"): "Not a Jellyfin server",
	} {
		d.do("menu") // the root's box is the server's
		r := d.do("search", typed)
		if r.Rows[0].Title != want || len(r.Rows) != 2 || r.Rows[1].Title != "Press Return to try another address" {
			t.Errorf("%q: %q", typed, titles(r))
		}
		for _, row := range r.Rows {
			if strings.Contains(row.Title+row.Subtitle, "secret") || strings.Contains(row.Title+row.Subtitle, "my pass") {
				t.Errorf("%q echoed: %+v", typed, row)
			}
		}
	}
	if d.loginServer() != "" {
		t.Fatalf("kept %q", d.loginServer())
	}
}

// The certificate check runs with the clock-fallback URL set to the typed server, so a Q2 with an
// unset clock can still check it.
func TestServerSetsDateURL(t *testing.T) {
	d, _ := loginDaemon(t)
	tlsroots.SetDateURL("")
	srv := d.loginServer()
	d.do("menu")
	d.do("search", srv)
	if tlsroots.DateURL() != srv {
		t.Fatalf("date URL %q, want %q", tlsroots.DateURL(), srv)
	}
}

// The saved server is the clock's fallback from the start, so a sign-in after a restart can check
// its certificate on an unset clock.
func TestLoadServerSetsDateURL(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	os.WriteFile(filepath.Join(d.dir, serverFile), []byte("https://music.example\n"), 0o644)
	tlsroots.SetDateURL("")
	d.loadServer()
	if tlsroots.DateURL() != "https://music.example" {
		t.Fatalf("date URL %q", tlsroots.DateURL())
	}
}

// A new server address that fails, while signed in, leaves the clock's fallback on the
// signed-in server, and no message for the signed-out root once the user signs out later.
func TestChangeServerFailKeepsDateURL(t *testing.T) {
	other := httptest.NewTLSServer(http.NotFoundHandler()) // not Jellyfin
	t.Cleanup(other.Close)
	d := newTestDaemon(t, &nav{}, "", true)
	d.do("server")
	want := tlsroots.DateURL()
	if want == "" {
		t.Fatal("no date URL while signed in")
	}
	if r := d.do("search", strings.TrimPrefix(other.URL, "https://")); r.Rows[0].Title != "Not a Jellyfin server" || d.client() == nil {
		t.Fatalf("%q, signed in %v", titles(r), d.client() != nil)
	}
	if tlsroots.DateURL() != want {
		t.Fatalf("date URL %q, want %q", tlsroots.DateURL(), want)
	}
	pressSignOut(d)
	if r := d.do("menu"); r.Rows[len(r.Rows)-1].Subtitle != serverHint {
		t.Fatalf("stale message %+v", r.Rows)
	}
}

// A plain server's check never moves the clock's fallback: a question answered No leaves it where
// it was (tlsroots takes a Date over https only).
func TestPlainHTTPCheckKeepsDateURL(t *testing.T) {
	t.Cleanup(func() { tlsroots.SetDateURL("") })
	d := privateHTTPDaemon(t, &authServer{})
	tlsroots.SetDateURL("https://music.example")
	d.do("menu")
	r := d.do("search", "192.168.111.111:8096") // it passed the check: the question waits
	if r.Rows[0].Title != "192.168.111.111:8096 is served over HTTP" {
		t.Fatalf("%q", titles(r))
	}
	d.do("menu", r.Rows[4].Context) // No
	if tlsroots.DateURL() != "https://music.example" {
		t.Fatalf("date URL %q", tlsroots.DateURL())
	}
}

// Sign Out ends the session on the server; an API key has none to end.
func TestSignOutEndsSession(t *testing.T) {
	keysOn(t)
	d, a := loginDaemon(t)
	d.do("pwlogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", typedPassword))
	d.do("account")
	pressSignOut(d)
	eventually(t, "the logout", func() bool { return a.logouts.Load() == 1 })

	d.do("keylogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", "KEY"))
	d.do("account")
	pressSignOut(d)
	d.bg.Wait()
	if n := a.logouts.Load(); n != 1 {
		t.Fatalf("%d logouts", n)
	}
}

// Credentials the card won't delete leave the sign-in as it was, and Sign Out says so.
func TestSignOutUnwritable(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.do("menu") // signed in
	p := filepath.Join(d.dir, "credentials")
	os.Remove(p)
	os.MkdirAll(filepath.Join(p, "x"), 0o755) // a folder with something in it: os.Remove fails
	if r := pressSignOut(d); r.Status == "" || r.Status == "ok" || r.Message != "Couldn't sign out: the card can't be written" {
		t.Fatalf("%+v", r)
	}
	if d.client() == nil {
		t.Fatal("signed out")
	}
}

// Account's first row is Server, its address beneath, opening the Server page; a new address typed
// there signs out (the token or API key deleted) and sets the typed server as the signed-out root
// does, leading to its sign-in choices. A text that isn't a server's address (a search typed in
// another box drawn earlier) changes nothing, and the same address keeps the sign-in.
func TestChangeServerSignedIn(t *testing.T) {
	a := httptest.NewTLSServer(&authServer{})
	t.Cleanup(a.Close)
	d := newTestDaemon(t, &nav{}, "", true)
	r := d.do("account")
	if row := r.Rows[0]; row.Kind != "list" || row.URI != "server" || row.Title != "Server" || row.Subtitle != d.server {
		t.Fatalf("%+v", row)
	}
	if row := r.Rows[1]; row.Kind != "search" || row.Title != "Signed in as:" || row.Subtitle != "u" {
		t.Fatalf("%+v", row)
	}
	if got := strings.Join(titles(r), "|"); strings.Contains(got, "Change Server") || !strings.Contains(got, "Sign Out") {
		t.Fatalf("%q", got)
	}
	// The address is the page's only row (a first box with others would be the title bar's icon),
	// pressed to type another; what that does beneath it.
	r = d.do("server")
	if len(r.Rows) != 1 || r.Rows[0].Kind != "search" || r.Rows[0].Title != d.server || r.Rows[0].Subtitle != "A new address signs you out" {
		t.Fatalf("%+v", r.Rows)
	}
	same := strings.ToUpper(strings.TrimPrefix(d.server, "https://")) // as T9 may type it
	if r := d.do("search", same); len(r.Rows) != 1 || r.Rows[0].Title != "That's the server you're on" || d.client() == nil {
		t.Fatalf("%q, signed in %v", titles(r), d.client() != nil)
	}
	// A failed address offers no box: Return goes back to the Server page whatever the number of tries.
	for _, typed := range []string{"http://elsewhere.example", "http://again.example"} {
		r := d.do("search", typed)
		if got := strings.Join(titles(r), "|"); got != "HTTPS is required. Use an https:// address|Press Return to try another address" || d.client() == nil {
			t.Fatalf("%q, signed in %v", got, d.client() != nil)
		}
	}
	r = d.do("search", strings.TrimPrefix(a.URL, "https://"))
	want := "Change Server|Sign In with Quick Connect|Sign In with Password"
	if got := strings.Join(titles(r), "|"); got != want {
		t.Fatalf("%q", got)
	}
	if _, ok := creds.Load(d.dir); ok || d.client() != nil || d.loginServer() != a.URL {
		t.Fatalf("still signed in, or server %q", d.loginServer())
	}
	if r := d.do("pwlogin"); r.Rows[0].Title != "Your password goes only to 127.0.0.1, over HTTPS" {
		t.Fatalf("%q", titles(r))
	}
}

// A server that can't be reached offers the Server row under its message.
func TestUnreachableOffersChangeServer(t *testing.T) {
	d := newTestDaemon(t, &nav{stall: true}, "", true)
	d.answerIn = 400 * time.Millisecond
	r := d.do("latest")
	if r.Rows[0].Title != "Can't reach 127.0.0.1" {
		t.Fatalf("%q", titles(r))
	}
	if row := r.Rows[len(r.Rows)-1]; row.Kind != "list" || row.URI != "server" || row.Title != "Server" {
		t.Fatalf("%+v", row)
	}
}

// A code the server has forgotten ends the wait at once and says so, instead of polling for the
// whole ten minutes.
func TestQuickConnectExpiredEndsWait(t *testing.T) {
	d, a := loginDaemon(t)
	d.do("qc")
	a.qcGone.Store(true)
	eventually(t, "the wait to end", func() bool { return !d.qcWaiting() })
	row, _ := find(d.do("menu"), "Sign In with Quick Connect")
	if row.Subtitle != "The Quick Connect code expired. Try again" {
		t.Fatalf("row %+v", row)
	}
}

// signInWithPassword signs loginDaemon's user in as typed on the Q2.
func signInWithPassword(t *testing.T, d *tdaemon) {
	t.Helper()
	d.do("pwlogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", typedPassword))
}

// noSecretShown fails when the typed password is in any of r's texts.
func noSecretShown(t *testing.T, r q2spot.Reply) {
	t.Helper()
	for _, row := range r.Rows {
		if strings.Contains(row.Title+row.Subtitle+row.Context, typedPassword) {
			t.Fatalf("password shown: %+v", row)
		}
	}
	if strings.Contains(r.Message, typedPassword) {
		t.Fatalf("password shown: %+v", r)
	}
}

// Once signed in, Return asks Q2 Pod's pages below again, typed ones included: the user name's
// page and Sign In with Password are then the signed-in root's rows (never "No playlists called
// that", a search of the user name), and the password's page asked again (its Retry) is Login
// Success again, never a search of the password.
func TestReturnAfterSignIn(t *testing.T) {
	d, _ := loginDaemon(t)
	d.do("playlists") // a section's search box was the last asked for before signing out
	signInWithPassword(t, d)
	for _, req := range [][]string{{"search", "u"}, {"pwlogin"}, {"keylogin"}} {
		r := d.do(req[0], req[1:]...)
		if r.Status != "" && r.Status != q2spot.OK || len(r.Rows) == 0 || r.Rows[0].Title != "Newly Added" {
			t.Fatalf("%v: %+v", req, r)
		}
	}
	r := d.do("search", typedPassword)
	loginSuccessPage(t, r)
	noSecretShown(t, r)
	noSecretOnCard(t, d)
}

// Sign Out's page, once it has said so, is the sign-in, live so Quick Connect's approval shows.
func TestSignOutShowsSignIn(t *testing.T) {
	d, _ := loginDaemon(t)
	signInWithPassword(t, d)
	d.do("account")
	if r := pressSignOut(d); r.Message != "Signed out" {
		t.Fatalf("%+v", r)
	}
	r := d.do("account")
	if got := strings.Join(titles(r), "|"); r.Message != "refresh" || got != "Change Server|Sign In with Quick Connect|Sign In with Password" {
		t.Fatalf("%q %+v", got, r)
	}
	if subs := strings.Join(subtitles(r), "|"); strings.Contains(subs, "Signed in as") {
		t.Fatalf("stale subtitle %q", subs)
	}
	if row, _ := find(d.do("settings"), "Account"); row.Subtitle != "" { // nobody is signed in
		t.Fatalf("Settings' Account row %+v", row)
	}
}

// Settings as the first request after a restart (Back from Quality and Storage once the app had
// exited) names the user under Account, as it does later.
func TestSettingsFirstRequestShowsUser(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	if row, _ := find(d.do("settings"), "Account"); row.Subtitle != "u" {
		t.Fatalf("Settings' Account row %+v", row)
	}
}

// Sign Out asks first, as Clear Playback Cache does: the first press only says to press again, the
// page asked again after the toast keeps it, and any other press drops it. Swipe on or off alike.
func TestSignOutAsksFirst(t *testing.T) {
	for _, caps := range []string{"", "sections"} {
		t.Run("caps="+caps, func(t *testing.T) {
			d := newTestDaemon(t, &nav{}, caps, true)
			d.do("menu")
			d.do("settings")
			d.do("account")
			if r := d.do("signout"); r.Message != "Press again to sign out" || d.client() == nil {
				t.Fatalf("first press %+v, signed in %v", r, d.client() != nil)
			}
			d.do("settings") // another press: the first is dropped
			d.do("account")
			if r := d.do("signout"); r.Message != "Press again to sign out" || d.client() == nil {
				t.Fatalf("after another press %+v, signed in %v", r, d.client() != nil)
			}
			d.do("account") // Q2 Pod asks the page again after the toast
			if r := d.do("signout"); r.Message != "Signed out" || d.client() != nil {
				t.Fatalf("second press %+v, signed in %v", r, d.client() != nil)
			}
		})
	}
}

// pressSignOut presses Sign Out twice, as it asks, and returns the second reply.
func pressSignOut(d *tdaemon) q2spot.Reply {
	d.do("signout")
	return d.do("signout")
}

// Signed out, Change Server opens the Server page: the address, pressed to type another.
func TestServerPageSignedOut(t *testing.T) {
	d, _ := loginDaemon(t)
	srv := d.loginServer()
	r := d.do("server")
	if len(r.Rows) != 1 || r.Rows[0].Kind != "search" || r.Rows[0].Title != srv {
		t.Fatalf("%+v", r.Rows)
	}
	if r := d.do("search", "http://elsewhere.example"); r.Rows[0].Title != "HTTPS is required. Use an https:// address" || len(r.Rows) != 2 {
		t.Fatalf("%q", titles(r))
	}
	d.do("server")
	if r := d.do("search", srv); r.Rows[0].Title != "Change Server" || d.loginServer() != srv {
		t.Fatalf("%q", titles(r))
	}
}

// Account's Signed in as is the box for another user's name, then the password, as a first
// sign-in. A wrong password keeps the sign-in; one that works replaces it, ending the old session.
func TestSignInAsAnotherUser(t *testing.T) {
	d, a := loginDaemon(t)
	signInWithPassword(t, d)
	d.do("account")
	r := d.do("search", "u")
	if r.Rows[len(r.Rows)-1].Title != "Type Your Password" {
		t.Fatalf("%q", titles(r))
	}
	if r = d.do("search", "nope"); r.Message != "Wrong username or password" || d.client() == nil {
		t.Fatalf("%+v, signed in %v", r, d.client() != nil)
	}
	d.do("search", "u") // Return: the user name's page again
	r = d.do("search", typedPassword)
	loginSuccessPage(t, r)
	noSecretShown(t, r)
	if d.client() == nil {
		t.Fatal("signed out")
	}
	eventually(t, "the old session's logout", func() bool { return a.logouts.Load() == 1 })
	noSecretOnCard(t, d)
}

// Signed in as another user when the old sign-in can't be signed out (the card can't be written):
// the typed sign-in fails, saying so, and the old one stays.
func TestSignInAsAnotherUserSignOutFails(t *testing.T) {
	d, _ := loginDaemon(t)
	signInWithPassword(t, d)
	d.do("account") // the sign-in loaded, so the next one must sign it out
	p := filepath.Join(d.dir, "credentials")
	os.Remove(p)
	os.MkdirAll(filepath.Join(p, "x"), 0o755) // a folder with something in it: os.Remove fails
	d.do("search", "u")
	if r := d.do("search", typedPassword); r.Message != "Couldn't sign out: the card can't be written" {
		t.Fatalf("%+v", r)
	}
	if d.client() == nil {
		t.Fatal("signed out")
	}
}

// Signed in as another user with no Wi-Fi: the password typed fails, never the Wi-Fi page (live,
// so Q2 Pod would keep it titled with the password).
func TestSignInAsAnotherUserNoWiFi(t *testing.T) {
	d, _ := loginDaemon(t)
	signInWithPassword(t, d)
	d.do("account")
	d.do("search", "u")
	d.localIP = func() string { return "" }
	r := d.do("search", typedPassword)
	if r.Status == "" || r.Status == q2spot.OK {
		t.Fatalf("%+v", r)
	}
	noSecretShown(t, r)
}

// Signed out, Return from the user name's box asks for the server's page again, with the address
// typed there: the sign-in choices, never "User: https://…" (a password then would be sent as it).
func TestReturnToServerWhileSigningIn(t *testing.T) {
	keysOn(t)
	d, _ := loginDaemon(t)
	host := strings.TrimPrefix(d.loginServer(), "https://")
	for _, op := range []string{"pwlogin", "keylogin"} {
		d.do(op)
		r := d.do("search", host)
		if got := strings.Join(titles(r), "|"); got != "Change Server|Sign In with Quick Connect|Sign In with Password|Sign In with an API Key" {
			t.Fatalf("%s: %q", op, got)
		}
	}
}

// fieldDaemon is loginDaemon on firmware with Q2 Pod's field rows.
func fieldDaemon(t *testing.T) (*tdaemon, *authServer) {
	t.Helper()
	d, a := loginDaemon(t)
	d.caps.Field = true
	return d, a
}

// pwForm checks the password page's rows: Username: and Password:, each a field row showing
// user and pw beneath, then Sign In; the password never.
func pwForm(t *testing.T, r q2spot.Reply, user, pw string) {
	t.Helper()
	if r.Status != "" && r.Status != q2spot.OK || len(r.Rows) < 3 {
		t.Fatalf("%+v", r)
	}
	u, p, in := r.Rows[0], r.Rows[1], r.Rows[2]
	if u.Kind != "field" || u.Title != "Username:" || u.URI != "pwfield" || u.Context != "user" || u.Subtitle != user {
		t.Fatalf("user row %+v", u)
	}
	if p.Kind != "field" || p.Title != "Password:" || p.URI != "pwfield" || p.Context != "password" || p.Subtitle != pw {
		t.Fatalf("password row %+v", p)
	}
	if in.Kind != "action" || in.URI != "pwsignin" || in.Title != "Sign In" {
		t.Fatalf("sign-in row %+v", in)
	}
	noSecretShown(t, r)
}

// With field rows, Sign In with Password is one page: the user name and password typed in their
// rows (the name shown, the password as ***), then Sign In. Only the token is kept; the password
// is in no file and is gone from memory once used.
func TestPasswordFormOnQ2(t *testing.T) {
	d, a := fieldDaemon(t)
	pwForm(t, d.do("pwlogin"), typeHint, typeHint)
	pwForm(t, d.do("pwfield", "user", "u"), "u", typeHint)
	r := d.do("pwfield", "password", typedPassword)
	pwForm(t, r, "u", "***")
	if strings.Contains(r.Message, typedPassword) {
		t.Fatalf("%+v", r)
	}
	pwForm(t, d.do("pwlogin"), "u", "***") // the page asked again keeps what was typed
	if r := d.do("pwsignin"); r.Status != "" && r.Status != q2spot.OK || r.Message != "Signed in as u" {
		t.Fatalf("%+v", r)
	}
	c, ok := creds.Load(d.dir)
	if !ok || c.Token != "tok" || c.Password != "" || a.signIns.Load() != 1 {
		t.Fatalf("%+v, %d sign-ins", c, a.signIns.Load())
	}
	// asked again after the action: the page, emptied, saying who is signed in
	r = d.do("pwlogin")
	pwForm(t, r, typeHint, typeHint)
	if _, ok := find(r, "Signed in as u"); !ok {
		t.Fatalf("%q", titles(r))
	}
	if d.login.formPw != "" || d.login.formUser != "" {
		t.Fatal("kept after signing in")
	}
	noSecretOnCard(t, d)
}

// A refused password: the page asked again after Sign In fails with why (Q2 Pod's error page, Retry
// and Cancel); Retry, the page asked again, shows the page with the password gone, so it is never
// sent twice; Sign In then asks for one. Leaving to the root forgets the user name too.
func TestPasswordFormWrong(t *testing.T) {
	d, a := fieldDaemon(t)
	d.do("pwlogin")
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", wrongPassword)
	if r := d.do("pwsignin"); r.Status != "" && r.Status != q2spot.OK || r.Message != "" {
		t.Fatalf("sign in %+v", r)
	}
	if r := d.do("pwlogin"); r.Status == "" || r.Status == q2spot.OK || r.Message != "Wrong username or password" {
		t.Fatalf("after %+v", r)
	}
	pwForm(t, d.do("pwlogin"), "u", typeHint) // Retry
	if r := d.do("pwsignin"); r.Message != "Type your password first" || a.signIns.Load() != 1 {
		t.Fatalf("%+v, %d sign-ins", r, a.signIns.Load())
	}
	pwForm(t, d.do("pwlogin"), "u", typeHint)
	if _, ok := creds.Load(d.dir); ok {
		t.Fatal("saved")
	}
	d.do("menu") // Cancel back to the root
	pwForm(t, d.do("pwlogin"), typeHint, typeHint)
	if r := d.do("pwsignin"); r.Message != "Type your user name first" {
		t.Fatalf("%+v", r)
	}
	noSecretOnCard(t, d, wrongPassword)
}

// Sign In with no Wi-Fi sends nothing: the page says why, and keeps the password for a Sign In
// once Wi-Fi is back.
func TestPasswordFormNoWiFi(t *testing.T) {
	d, a := fieldDaemon(t)
	d.do("pwlogin")
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", typedPassword)
	d.localIP = func() string { return "" }
	d.do("pwsignin")
	r := d.do("pwlogin")
	if r.Status == "" || r.Status == q2spot.OK || !strings.Contains(r.Message, "Wi-Fi") || a.signIns.Load() != 0 {
		t.Fatalf("%+v, %d sign-ins", r, a.signIns.Load())
	}
	pwForm(t, d.do("pwlogin"), "u", "***")
	d.localIP = func() string { return "192.168.1.20" }
	d.do("pwsignin")
	if _, ok := creds.Load(d.dir); !ok || a.signIns.Load() != 1 {
		t.Fatalf("signed in %v, %d sign-ins", ok, a.signIns.Load())
	}
	noSecretOnCard(t, d)
}

// With field rows, Account's Signed in as opens the password page too (the old boxes could title
// a page with a typed text there). A refused password keeps the sign-in; one that works replaces
// it, ending the old session.
func TestAccountPasswordForm(t *testing.T) {
	d, a := fieldDaemon(t)
	d.caps.Field = false
	signInWithPassword(t, d)
	d.caps.Field = true
	old, _ := creds.Load(d.dir) // told apart from the new sign-in's "tok"
	old.Token = "old-tok"
	if err := creds.Save(d.dir, old); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.cl = nil // loads it
	d.mu.Unlock()
	r := d.do("account")
	if row, _ := find(r, "Signed in as:"); row.Kind != "list" || row.URI != "pwlogin" || row.Subtitle != "u" {
		t.Fatalf("%+v", r.Rows)
	}
	pwForm(t, d.do("pwlogin"), typeHint, typeHint)
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", wrongPassword)
	d.do("pwsignin")
	if r := d.do("pwlogin"); r.Message != "Wrong username or password" || d.client() == nil {
		t.Fatalf("%+v, signed in %v", r, d.client() != nil)
	}
	if c, _ := creds.Load(d.dir); c.Token != "old-tok" || a.logouts.Load() != 0 { // the sign-in kept
		t.Fatalf("token %q, %d logouts", c.Token, a.logouts.Load())
	}
	pwForm(t, d.do("pwlogin"), "u", typeHint) // Retry
	pwForm(t, d.do("pwfield", "password", typedPassword), "u", "***")
	if r := d.do("pwsignin"); r.Message != "Signed in as u" || d.client() == nil {
		t.Fatalf("%+v", r)
	}
	eventually(t, "the old session's logout", func() bool { return a.logouts.Load() == 1 })
	if c, _ := creds.Load(d.dir); c.Token != "tok" {
		t.Fatalf("token %q", c.Token)
	}
	d.do("pwfield", "user", "someone")
	d.do("account") // Return to Account forgets what was typed
	pwForm(t, d.do("pwlogin"), typeHint, typeHint)
	noSecretOnCard(t, d, wrongPassword)
}

// Without field rows the page's own requests are refused (only the field cap's page sends them).
func TestPasswordFormNeedsField(t *testing.T) {
	d, a := loginDaemon(t)
	for _, op := range [][]string{{"pwfield", "user", "u"}, {"pwsignin"}} {
		if r := d.do(op[0], op[1:]...); r.Status != q2spot.Unsupported {
			t.Errorf("%s: %+v", op[0], r)
		}
	}
	if d.login.formUser != "" || a.requests.Load() != 0 {
		t.Fatal("taken")
	}
}

// Signed in from before the server was kept on its own, the page still names the server's host.
func TestPasswordFormHost(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "field", true)
	if _, ok := find(d.do("pwlogin"), "Your password goes only to 127.0.0.1, over HTTPS"); !ok {
		t.Fatalf("%q", titles(d.do("pwlogin")))
	}
}

// Any sign-in (Quick Connect, the sign-in file, the page) forgets what the password page holds.
func TestSignedInClearsForm(t *testing.T) {
	d, _ := fieldDaemon(t)
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", typedPassword)
	if err := d.signedIn(jellyfin.Creds{Server: d.loginServer(), User: "u", Token: "tok", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if d.login.formUser != "" || d.login.formPw != "" || d.login.formMsg != "" {
		t.Fatal("kept")
	}
}

// With field rows, the server's address is a field row on the first page, and signed out the
// sign-in page holds it, the Server page being that page too: an address that works shows the
// sign-in page; one that isn't right says why, with the Server Address: box to type it again.
func TestServerField(t *testing.T) {
	a := &authServer{}
	srv := httptest.NewTLSServer(a)
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "field", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	r := d.do("menu")
	if row := r.Rows[len(r.Rows)-1]; row.Kind != "field" || row.URI != "setserver" || row.Context != "menu" ||
		row.Title != "Server Address:" || row.Subtitle != serverHint {
		t.Fatalf("root %+v", r.Rows)
	}
	if r := d.do("setserver", "menu", "http://music.example"); r.Rows[0].Title != "HTTPS is required. Use an https:// address" ||
		r.Rows[1].Kind != "field" || r.Rows[1].Title != "Server Address:" || r.Rows[1].Position != "" {
		t.Fatalf("%+v", r)
	}
	r = d.do("setserver", "menu", strings.TrimPrefix(srv.URL, "https://"))
	if got := strings.Join(titles(r), "|"); got != "Server Address:|Sign In with Quick Connect|Sign In with Password" {
		t.Fatalf("%q", got)
	}
	r = d.do("server")
	if row := r.Rows[0]; row.Kind != "field" || row.URI != "setserver" || row.Subtitle != srv.URL || len(r.Rows) != 3 {
		t.Fatalf("Server page %+v", r.Rows)
	}
	if r := d.do("setserver", "server", "http://elsewhere.example"); r.Rows[0].Title != "HTTPS is required. Use an https:// address" ||
		r.Rows[1].Kind != "field" || r.Rows[1].Title != "Server Address:" || r.Rows[1].Position != "" {
		t.Fatalf("%+v", r)
	}
	if r := d.do("setserver", "server", srv.URL); r.Rows[0].Kind != "field" || r.Rows[0].Subtitle != srv.URL {
		t.Fatalf("%+v", r)
	}
}

// A private network's IP and port, typed bare, is kept as plain http:// and is checked, signed in
// to and used as any server; every page that sends the password or uses the sign-in warns it isn't
// encrypted. The test server is reached at a private address by a dialer that connects to it.
func TestPrivateHTTPServer(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	r := allowPlain(t, d, "192.168.111.111:8096")
	want := "Change Server|Sign In with Quick Connect|Sign In with Password|Warning: HTTP, not encrypted"
	if got := strings.Join(titles(r), "|"); got != want {
		t.Fatalf("%q", got)
	}
	if d.loginServer() != "http://192.168.111.111:8096" {
		t.Fatalf("kept %q", d.loginServer())
	}
	d.do("pwlogin")
	r = d.do("search", "u")
	if _, ok := find(r, "Warning: HTTP, not encrypted"); !ok {
		t.Fatalf("password page %q", titles(r))
	}
	loginSuccessPage(t, d.do("search", typedPassword))
	if c, ok := creds.Load(d.dir); !ok || c.Token != "tok" {
		t.Fatalf("%+v", c)
	}
	if _, ok := find(d.do("account"), "Warning: HTTP, not encrypted"); !ok {
		t.Fatal("Account has no warning")
	}
}

// The password page (field cap) says HTTP and warns for a plain server, and says HTTPS otherwise.
func TestPasswordFormPlainHTTP(t *testing.T) {
	d, _ := fieldDaemon(t)
	if _, ok := find(d.do("pwlogin"), "Warning: HTTP, not encrypted"); ok {
		t.Fatal("warned for an https server")
	}
	d.login.server = "http://10.1.2.3:8096"
	r := d.do("pwlogin")
	if _, ok := find(r, "Your password goes only to 10.1.2.3, over HTTP"); !ok {
		t.Fatalf("%q", titles(r))
	}
	if _, ok := find(r, "Warning: HTTP, not encrypted"); !ok {
		t.Fatalf("%q", titles(r))
	}
}

// privateHTTPDaemon is a signed-out daemon whose clients reach h over plain http at any address:
// every connection goes to h's test server, so a private address can stand for it. Its clients
// check the network as the app's do (homeOnly); the Q2 is on homeNet.
func privateHTTPDaemon(t *testing.T, h http.Handler) *tdaemon {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}}}
	d := newTestDaemon(t, &nav{}, "", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, d.homeOnly(c, hc)) }
	d.netNow = func() q2.Network { return homeNet }
	return d
}

var homeNet = q2.Network{SSID: "HomeNet", Gateway: "aa:bb:cc:dd:ee:ff"}

// A plain server's redirect to another address is refused with its own message, not taken for
// the address check's "HTTPS is required" (a wrong port answered by another service did this).
func TestPrivateHTTPRedirectRefused(t *testing.T) {
	d := privateHTTPDaemon(t, http.RedirectHandler("http://192.168.1.99:8096/web/", http.StatusFound))
	d.do("menu")
	r := d.do("search", "192.168.111.111:8096")
	if r.Rows[0].Title != "The server redirected to another address. Check the address and port" {
		t.Fatalf("%q", titles(r))
	}
}

// The password page that asks for the user name first (no field cap) says HTTP for a plain server.
func TestPasswordTypedPlainHTTP(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	if _, ok := find(d.do("pwlogin"), "Your password goes only to 192.168.111.111, over HTTP"); !ok {
		t.Fatalf("%q", titles(d.do("pwlogin")))
	}
}

// An https server's client is never wrapped, nor a plain one's without a secret.
func TestHomeOnlyScope(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	hc := &http.Client{}
	if d.homeOnly(jellyfin.Creds{Server: "https://music.example", Token: "tok"}, hc) != hc ||
		d.homeOnly(jellyfin.Creds{Server: "http://192.168.1.10:8096"}, hc) != hc {
		t.Fatal("wrapped")
	}
	if d.homeOnly(jellyfin.Creds{Server: "http://192.168.1.10:8096", Password: "p"}, hc) == hc {
		t.Fatal("a password not checked")
	}
}

// A1-P1-1: Quick Connect to a plain http server polls with its secret, and swaps it for a token,
// only on the server's home Wi-Fi. A Q2 moved to another network before the first poll sends
// nothing more: the wait ends with the network's message and no sign-in is kept.
func TestQuickConnectPlainHTTPOtherNetwork(t *testing.T) {
	a := &authServer{}
	d := privateHTTPDaemon(t, a)
	d.login.poll = 200 * time.Millisecond
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	if r := d.do("qc"); !strings.Contains(r.Message, "123456") {
		t.Fatalf("qc at home %+v", r)
	}
	t.Cleanup(d.cancelQC)
	before := a.requests.Load()
	d.setNet(q2.Network{SSID: "HomeNet", Gateway: "11:22:33:44:55:66"}) // an evil twin
	eventually(t, "the wait to end", func() bool { return !d.qcWaiting() })
	a.approved.Store(true)
	time.Sleep(3 * d.login.poll)
	if n := a.requests.Load() - before; n != 0 {
		t.Errorf("%d requests sent off the home Wi-Fi", n)
	}
	if _, ok := creds.Load(d.dir); ok {
		t.Error("a sign-in kept")
	}
	if qc, _ := find(d.do("menu"), "Sign In with Quick Connect"); qc.Subtitle != "Not this server's home Wi-Fi. New Wi-Fi at home? Type its address again there" {
		t.Errorf("message %q", qc.Subtitle)
	}
}

// qcMover answers like authServer, but its first approved Quick Connect poll moves the Q2 to net
// first (the Q2 changed Wi-Fi while the poll was on its way), and it counts the token swaps. The
// Quick Connect loop polls one at a time, so moved needs no compare-and-swap.
type qcMover struct {
	authServer
	d     *tdaemon
	net   q2.Network
	moved atomic.Bool
	swaps atomic.Int32
}

func (m *qcMover) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/Users/AuthenticateWithQuickConnect":
		m.swaps.Add(1)
	case r.URL.Path == "/QuickConnect/Connect" && m.approved.Load() && !m.moved.Load():
		m.d.setNet(m.net)
		m.moved.Store(true) // after the move, so a test that waits for moved sees the new Wi-Fi
	}
	m.authServer.ServeHTTP(w, r)
}

func (d *tdaemon) setNet(n q2.Network) {
	d.netMu.Lock()
	d.netNow = func() q2.Network { return n }
	d.netAt = time.Time{}
	d.netMu.Unlock()
}

// startQCMover starts Quick Connect at home on a plain server whose approval moves the Q2 to net.
func startQCMover(t *testing.T, net q2.Network) (*tdaemon, *qcMover) {
	t.Helper()
	m := &qcMover{net: net}
	m.d = privateHTTPDaemon(t, m)
	d := m.d
	d.login.poll = 100 * time.Millisecond
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	if r := d.do("qc"); !strings.Contains(r.Message, "123456") {
		t.Fatalf("qc at home %+v", r)
	}
	t.Cleanup(d.cancelQC)
	m.approved.Store(true)
	return d, m
}

// A1-P2-2: the Q2 leaves the home Wi-Fi while the approved poll is on its way: the token isn't
// asked for.
func TestQuickConnectPlainHTTPMovedBeforeSwap(t *testing.T) {
	d, m := startQCMover(t, q2.Network{SSID: "HomeNet", Gateway: "11:22:33:44:55:66"}) // an evil twin
	eventually(t, "the wait to end", func() bool { return !d.qcWaiting() })
	if n := m.swaps.Load(); n != 0 {
		t.Errorf("%d token swaps sent off the home Wi-Fi", n)
	}
	if _, ok := creds.Load(d.dir); ok {
		t.Error("a sign-in kept")
	}
	if qc, _ := find(d.do("menu"), "Sign In with Quick Connect"); qc.Subtitle != "Not this server's home Wi-Fi. New Wi-Fi at home? Type its address again there" {
		t.Errorf("message %q", qc.Subtitle)
	}
}

// A1-P2-1: the Wi-Fi can't be told for a moment (no gateway yet) between the approved poll and
// the token swap: the wait goes on, and the approval signs in once the home Wi-Fi is back.
func TestQuickConnectPlainHTTPUnsureBeforeSwap(t *testing.T) {
	d, m := startQCMover(t, q2.Network{SSID: "HomeNet"})
	eventually(t, "the approved poll", m.moved.Load)
	time.Sleep(3 * d.login.poll)
	if !d.qcWaiting() || m.swaps.Load() != 0 {
		qc, _ := find(d.do("menu"), "Sign In with Quick Connect")
		t.Fatalf("waiting %v, swaps %d; message %q", d.qcWaiting(), m.swaps.Load(), qc.Subtitle)
	}
	d.setNet(homeNet)
	eventually(t, "the sign-in", func() bool { _, ok := creds.Load(d.dir); return ok })
}

// allowPlain types addr in the T9 box (signed out), answers Yes on the plain http question that
// comes in the page's place (Q2 Pod asks the page again with the Yes row's context last), and
// returns that answer's page.
func allowPlain(t *testing.T, d *tdaemon, addr string) q2spot.Reply {
	t.Helper()
	r := d.do("search", addr)
	yes, ok := find(r, "Yes, allow HTTP")
	if !ok {
		if yes, ok = find(r, "Yes, this is my home Wi-Fi"); !ok {
			t.Fatalf("no question: %q", titles(r))
		}
	}
	if yes.Kind != "page" || !strings.HasPrefix(yes.Context, "allow:") {
		t.Fatalf("Yes %+v", yes)
	}
	return d.do("search", addr, yes.Context)
}

// U54: a bare IP and port in a range of the allow list means http://; outside it, https://. An
// http:// typed in full may be any IPv4 address and port, never a name, loopback or an odd
// spelling. An allow list with no address turns plain http off.
func TestServerAddressPlainHTTP(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	for typed, want := range map[string]string{
		"192.168.111.111:8096":        "http://192.168.111.111:8096",
		"10.0.0.5:8096":               "http://10.0.0.5:8096",
		"192.168.1.10:30013":          "http://192.168.1.10:30013", // any port: a range covers them all
		"172.31.255.255:8096":         "http://172.31.255.255:8096",
		"192.168.1.10:8096/jellyfin":  "http://192.168.1.10:8096/jellyfin",
		"http://192.168.1.10:8096/jf": "http://192.168.1.10:8096/jf",
		"http://172.32.0.1:8096":      "http://172.32.0.1:8096", // in full: anywhere, asked first
		"https://192.168.1.10:8920":   "https://192.168.1.10:8920",
		"8.8.8.8":                     "https://8.8.8.8",         // no port, outside the ranges: https as ever
		"172.32.0.1:8096":             "https://172.32.0.1:8096", // bare, outside the ranges
		"100.100.1.2:8096":            "https://100.100.1.2:8096",
		"music.example:8096":          "https://music.example:8096",
	} {
		if got, err := d.serverAddress(typed); err != nil || got != want {
			t.Errorf("%q: %q, %v", typed, got, err)
		}
	}
	for _, typed := range []string{"http://192.168.1.10", "192.168.1.10", "192.168.1.10/jellyfin"} { // r2 #3
		if _, err := d.serverAddress(typed); err == nil || err.Error() != "Add the port, e.g. 192.168.1.10:8096" {
			t.Errorf("%q: %v", typed, err)
		}
	}
	for _, typed := range []string{":8096", "https://:8096"} { // no host name: not the Q2 itself
		if _, err := d.serverAddress(typed); err == nil || err.Error() != "That isn't a server address" {
			t.Errorf("%q: %v", typed, err)
		}
	}
	for _, typed := range []string{"http://music.example:8096", "http://192.168.001.010:8096",
		"http://[::ffff:192.168.1.1]:8096", "http://localhost:8096", "http://127.0.0.1:8096", "http://0.0.0.0:8096",
		"http://224.0.0.1:8096", "http://255.255.255.255:8096"} {
		if _, err := d.serverAddress(typed); err == nil || err.Error() != "HTTPS is required. Use an https:// address" {
			t.Errorf("%q: %v", typed, err)
		}
	}
	if _, err := d.serverAddress("http://u:p@192.168.1.10:8096"); err == nil || err.Error() != "Leave the user name and password out of the address" {
		t.Errorf("user:password@ %v", err)
	}
	// edited on a computer: Tailscale's range added; then no address at all: plain http off
	os.WriteFile(filepath.Join(d.dir, allowFile), []byte("# mine\n100.64.0.0/10\n"), 0o644)
	d.allow = nil
	if got, _ := d.serverAddress("100.100.1.2:8096"); got != "http://100.100.1.2:8096" {
		t.Errorf("Tailscale: %q", got)
	}
	if got, _ := d.serverAddress("192.168.1.10:8096"); got != "https://192.168.1.10:8096" {
		t.Errorf("not listed: %q", got)
	}
	os.WriteFile(filepath.Join(d.dir, allowFile), []byte("# nothing\n"), 0o644)
	d.allow = nil
	if _, err := d.serverAddress("http://192.168.1.10:8096"); err == nil || err.Error() != "Plain HTTP is turned off in http-allow.txt" {
		t.Errorf("off: %v", err)
	}
}

// U54: the allow list is written with the defaults when missing, and again when deleted.
func TestAllowListDefaults(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	p := filepath.Join(d.dir, allowFile)
	os.Remove(p)
	d.allow = nil
	a := d.allowList()
	b, _ := os.ReadFile(p)
	if string(b) != allowDefaults || len(a.ranges) != 3 || len(a.servers) != 0 || a.off() {
		t.Fatalf("%q %+v", b, a)
	}
}

// U54: each new plain server is asked about once: served over HTTP, the risk, Yes or No. No keeps
// nothing and adds nothing; Yes adds its IP and port to the allow list and keeps it, and typed
// again it isn't asked. A public address gets the stronger warning. An answer kept by an earlier
// page (Q2 Pod keeps a level's arguments) never answers a later question.
func TestPlainHTTPAsked(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	r := d.do("search", "192.168.111.111:8096")
	if got := strings.Join(titles(r), "|"); got != "192.168.111.111:8096 is served over HTTP|Not encrypted|Say Yes only at home|Yes, allow HTTP|No" {
		t.Fatalf("%q", got)
	}
	if d.loginServer() != "" {
		t.Fatal("kept before Yes")
	}
	no := r.Rows[4] // find matches by prefix: "Not encrypted" starts with "No"
	if no.URI != "deny" {
		t.Fatalf("No %+v", no)
	}
	r = d.do("search", "192.168.111.111:8096", no.Context)
	if r.Rows[0].Title != "Not used: 192.168.111.111" || d.loginServer() != "" || d.allowed("http://192.168.111.111:8096") {
		t.Fatalf("%q", titles(r))
	}
	d.do("search", "192.168.111.111:8096")
	r = d.do("search", "192.168.111.111:8096", no.Context) // the first question's No: asked anew
	yes, ok := find(r, "Yes, allow HTTP")
	if !ok || d.loginServer() != "" {
		t.Fatalf("the first question's No answered the second: %q", titles(r))
	}
	r = d.do("search", "192.168.111.111:8096", yes.Context)
	if r.Rows[0].Title != "Change Server" || d.loginServer() != "http://192.168.111.111:8096" {
		t.Fatalf("%q, kept %q", titles(r), d.loginServer())
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, allowFile)); !strings.HasSuffix(string(b), "\n192.168.111.111:8096\n") {
		t.Fatalf("allow list %q", b)
	}
	if r := d.do("search", "192.168.111.111:8096", yes.Context); r.Rows[0].Title == "Not used: 192.168.111.111" {
		t.Fatal("answered twice")
	}
	if r := d.do("search", "192.168.111.111:8096"); r.Rows[0].Title != "Change Server" { // said Yes to: not asked
		t.Fatalf("asked again %q", titles(r))
	}
	r = d.do("search", "http://203.0.113.9:8096")
	if r.Rows[1].Title != "On the internet, NOT encrypted" {
		t.Fatalf("public %q", titles(r))
	}
}

// Review 2026-11-20 #2: the check fails closed: no network file (or one for another server, or a
// broken one) sends nothing; an address isn't kept while the gateway's MAC can't be read (#5),
// and a Wi-Fi name alone never matches. #3: a kept part that can't be read now is "can't reach",
// not another network. #4: Quick Connect is refused on another network. #6 and #7: typed again
// on a new Wi-Fi of the same home, signed in, the Q2 asks first and keeps both, signed in still.
func TestHomeNetworkOnly(t *testing.T) {
	a := &authServer{}
	d := privateHTTPDaemon(t, a)
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	d.do("pwlogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", typedPassword))
	ctx := context.Background()
	if err := d.client().Logout(ctx); err != nil {
		t.Fatal(err)
	}
	at := d.setNet // under netMu: Sign Out's background Logout reads the network too (go test -race)
	hotel := q2.Network{SSID: "HomeNet", Gateway: "11:22:33:44:55:66"}
	at(hotel)
	before := a.requests.Load()
	err := d.client().Logout(ctx)
	if !errors.Is(err, jellyfin.ErrOtherNetwork) || a.requests.Load() != before {
		t.Fatalf("%v, %d requests sent", err, a.requests.Load()-before)
	}
	if m := jellyfin.Message(err, "x"); m != "Not this server's home Wi-Fi. New Wi-Fi at home? Type its address again there" {
		t.Fatal(m)
	}
	at(q2.Network{SSID: "HomeNet"}) // the gateway's ARP answer late: not another network
	if err := d.client().Logout(ctx); errors.Is(err, jellyfin.ErrOtherNetwork) || err == nil {
		t.Fatalf("unsure: %v", err)
	}
	if !errors.Is(d.homeOnly(jellyfin.Creds{Server: "http://192.168.111.111:8096", Token: "t"}, &http.Client{}).Transport.(homeGate).ok(), errNetworkUnsure) {
		t.Fatal("unsure isn't errNetworkUnsure")
	}
	// #6: the same home's 5 GHz name, typed again signed in: asked, kept, both work, still signed in
	fiveG := q2.Network{SSID: "HomeNet-5G", Gateway: "aa:bb:cc:dd:ee:ff"}
	at(fiveG)
	r := d.do("server")
	r = d.do("search", "192.168.111.111:8096")
	yes, ok := find(r, "Yes, this is my home Wi-Fi")
	if !ok || r.Rows[0].Title != "This Wi-Fi isn't saved for 192.168.111.111:8096" {
		t.Fatalf("%q", titles(r))
	}
	d.do("search", "192.168.111.111:8096", yes.Context)
	if d.client() == nil {
		t.Fatal("signed out")
	}
	for _, n := range []q2.Network{homeNet, fiveG} {
		at(n)
		if err := d.client().Logout(ctx); err != nil {
			t.Fatalf("%+v: %v", n, err)
		}
	}
	// #4: Quick Connect, signed out: refused at the hotel, a code at home (the record still there)
	d.signOut()
	d.do("menu")
	at(hotel)
	if r := d.do("qc"); r.Message != "Not this server's home Wi-Fi. New Wi-Fi at home? Type its address again there" {
		t.Fatalf("qc at the hotel %+v", r)
	}
	at(homeNet)
	r = d.do("qc")
	d.do("qccancel") // before any Fatal: its poll would outlive the test
	if r.Status != "" && r.Status != q2spot.OK || !strings.Contains(r.Message, "123456") {
		t.Fatalf("qc at home %+v", r)
	}
	// #2: no network file sends nothing, even on a network that was home
	os.Remove(filepath.Join(d.dir, networkFile))
	if err := d.onHomeNetwork("http://192.168.111.111:8096"); !errors.Is(err, jellyfin.ErrOtherNetwork) {
		t.Fatalf("no file: %v", err)
	}
}

// Review 2026-11-20 #5: an address isn't kept while the gateway's MAC can't be read: the Wi-Fi's
// name alone is anyone's to copy. A kept Wi-Fi name alone matches nothing anyway.
func TestHomeNetworkNeedsGateway(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.netNow = func() q2.Network { return q2.Network{SSID: "HomeNet"} }
	d.do("menu")
	r := d.do("search", "192.168.111.111:8096")
	if r.Rows[0].Title != "Can't check this Wi-Fi yet. Try again in a moment" || d.loginServer() != "" {
		t.Fatalf("%q, kept %q", titles(r), d.loginServer())
	}
}

// U54: taken off the allow list on a computer, a server said Yes to gets no secret.
func TestHomeNetworkNotAllowed(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	os.WriteFile(filepath.Join(d.dir, allowFile), []byte(allowDefaults), 0o644)
	d.allow = nil
	err := d.onHomeNetwork("http://192.168.111.111:8096")
	if !errors.Is(err, jellyfin.ErrNotAllowed) || jellyfin.Message(err, "x") != "Not on http-allow.txt: nothing sent" {
		t.Fatal(err)
	}
}

// Review 0.8.3 p1: signed in, an allow list re-saved as UTF-16 on a computer (still holding the
// server) has no address the app can read: nothing is sent, and the message says why.
func TestHomeNetworkAllowListUnreadable(t *testing.T) {
	a := &authServer{}
	d := privateHTTPDaemon(t, a)
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	d.do("pwlogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", typedPassword))
	p := filepath.Join(d.dir, allowFile)
	b, _ := os.ReadFile(p)
	utf16 := []byte{0xff, 0xfe}
	for _, c := range string(b) {
		utf16 = append(utf16, byte(c), 0)
	}
	os.WriteFile(p, utf16, 0o644)
	os.Chtimes(p, time.Now(), time.Now().Add(time.Minute)) // FAT's times are to 2 s
	before := a.requests.Load()
	err := d.client().Logout(context.Background())
	if !errors.Is(err, jellyfin.ErrNotAllowed) || a.requests.Load() != before {
		t.Fatalf("%v, %d requests sent", err, a.requests.Load()-before)
	}
	if m := jellyfin.Message(err, "x"); m != "http-allow.txt has no address the app can read" {
		t.Fatal(m)
	}
}

// A typed address whose server fails its check (field rows) gets the app's own page, not Q2 Pod's
// error page, whose Retry never sends the text again: Try Again (a page row) asks for the page
// again with retryArg, which checks the same address again; once it works the page is the sign-in
// page, and the page asked again with retryArg after that (Q2 Pod keeps it) is the page as usual.
func TestServerFieldTryAgain(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	a := &authServer{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}
		a.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "field", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	d.do("menu")
	r := d.do("setserver", "menu", srv.URL)
	if got := strings.Join(titles(r), "|"); r.Status != "" && r.Status != q2spot.OK ||
		got != "The server answered HTTP 503|Try Again|Server Address:" {
		t.Fatalf("%+v", r)
	}
	row := r.Rows[1]
	if row.Kind != "page" || !strings.HasPrefix(row.Context, retryArg+":") || row.Subtitle != srv.URL {
		t.Fatalf("Try Again %+v", row)
	}
	r = d.do("menu", row.Context) // still down: the page again, with a Try Again of its own
	if r.Rows[0].Title != "The server answered HTTP 503" || r.Rows[1].Context == row.Context {
		t.Fatalf("%+v", r.Rows)
	}
	again := r.Rows[1].Context
	if r := d.do("menu", row.Context); r.Rows[0].Title == "The server answered HTTP 503" { // the older page's: none
		t.Fatalf("an older Try Again checked: %q", titles(r))
	}
	down.Store(false)
	r = d.do("menu", again)
	if got := strings.Join(titles(r), "|"); got != "Server Address:|Sign In with Quick Connect|Sign In with Password" || d.loginServer() != srv.URL {
		t.Fatalf("%q, kept %q", got, d.loginServer())
	}
	if got := strings.Join(titles(d.do("menu", again)), "|"); got != "Server Address:|Sign In with Quick Connect|Sign In with Password" {
		t.Fatalf("asked again %q", got)
	}
}

// Review 2026-11-20 #1: a Try Again kept by the first page (Q2 Pod keeps a level's arguments)
// never checks an address that failed later on another page, nor once signed in: Back there is the
// page as usual, with no check and no sign-out. #9: after "That isn't a server address" (with the
// box to type it again), the old Try Again asks for the page as it stands, never the address.
func TestServerFieldOldTryAgain(t *testing.T) {
	d, _ := fieldDaemon(t)
	good := d.loginServer()
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	d.do("menu")
	r := d.do("setserver", "menu", bad.URL)
	first := r.Rows[1].Context
	if r := d.do("setserver", "menu", "not an address"); r.Rows[0].Title != "That isn't a server address" || r.Rows[1].Kind != "field" {
		t.Fatalf("%+v", r)
	}
	if r := d.do("menu", first); r.Rows[0].Title == "The server answered HTTP 503" { // #9: Retry
		t.Fatalf("Retry checked the old address: %q", titles(r))
	}
	r = d.do("setserver", "menu", bad.URL)
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", typedPassword)
	if r := d.do("pwsignin"); r.Message != "Signed in as u" {
		t.Fatalf("%+v", r)
	}
	later := d.do("setserver", "server", bad.URL) // fails on the Server page, signed in
	if later.Rows[0].Title != "The server answered HTTP 503" {
		t.Fatalf("%q", titles(later))
	}
	if r := d.do("menu", first); r.Message != "sections" && r.Message != "refresh" || d.client() == nil || d.loginServer() != good {
		t.Fatalf("Back to the first page checked or signed out: %+v", r)
	}
}

// Review 2026-11-20 #8: after the app starts again (the sign-in loaded from the card), a new
// address that signs out still fills in the last user name.
func TestPrefillAfterRestart(t *testing.T) {
	d, _ := fieldDaemon(t)
	d.do("menu")
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", typedPassword)
	d.do("pwsignin")
	d.login.user = "" // as after a restart: only the sign-in on the card knows it
	d.mu.Lock()
	d.cl = nil
	d.mu.Unlock()
	d.client()
	other := httptest.NewTLSServer(&authServer{})
	t.Cleanup(other.Close)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, other.Client()) }
	d.do("setserver", "server", other.URL)
	pwForm(t, d.do("pwlogin"), "u", typeHint)
}

// Signed in, a new address on the Server page that works signs out and shows the sign-in page in
// its place, the password page's Username: already the last user's.
func TestServerFieldSignedInLandsOnSignIn(t *testing.T) {
	d, _ := fieldDaemon(t)
	d.do("menu")
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", typedPassword)
	if r := d.do("pwsignin"); r.Message != "Signed in as u" {
		t.Fatalf("%+v", r)
	}
	other := httptest.NewTLSServer(&authServer{})
	t.Cleanup(other.Close)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, other.Client()) }
	r := d.do("setserver", "server", other.URL)
	if got := strings.Join(titles(r), "|"); got != "Server Address:|Sign In with Quick Connect|Sign In with Password" || d.client() != nil {
		t.Fatalf("%q", got)
	}
	pwForm(t, d.do("pwlogin"), "u", typeHint)
}

// U50: with Q2 Pod's fieldtext, every Server Address: row with an address opens its box holding it
// (the row's position), to edit; without it, position stays empty (it would be a held Play/Pause
// op there), and the password rows never carry one.
func TestServerFieldOpensWithAddress(t *testing.T) {
	d, _ := fieldDaemon(t)
	d.caps.FieldText = true
	r := d.do("menu")
	if row := r.Rows[0]; row.Title != "Server Address:" || row.Position != d.loginServer() || row.Position == "" {
		t.Fatalf("sign-in list %+v", row)
	}
	for _, row := range d.do("pwlogin").Rows {
		if row.Position != "" {
			t.Fatalf("password page %+v", row)
		}
	}
	d.do("menu")
	d.do("pwfield", "user", "u")
	d.do("pwfield", "password", typedPassword)
	d.do("pwsignin")
	if row := d.do("server").Rows[0]; row.Kind != "field" || row.Position != d.loginServer() {
		t.Fatalf("Server page %+v", row)
	}
	d.caps.FieldText = false
	if row := d.do("server").Rows[0]; row.Position != "" {
		t.Fatalf("without fieldtext %+v", row)
	}
}

// A failed address's page opens its box holding the address that failed.
func TestServerFieldFailedOpensWithIt(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "field fieldtext", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	d.do("menu")
	r := d.do("setserver", "menu", srv.URL)
	if row := r.Rows[len(r.Rows)-1]; row.Title != "Server Address:" || row.Position != srv.URL {
		t.Fatalf("%+v", r.Rows)
	}
}

// U54 with field rows: the question comes in the page's place; No shows the page as it stands
// (Return on the first page would leave the app); Yes keeps the server and shows the sign-in list.
func TestPlainHTTPAskedField(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.caps.Field = true
	d.do("menu")
	r := d.do("setserver", "menu", "192.168.111.111:8096")
	if r.Rows[0].Title != "192.168.111.111:8096 is served over HTTP" || r.Rows[4].URI != "deny" {
		t.Fatalf("%q", titles(r))
	}
	r = d.do("menu", r.Rows[4].Context)
	if row := r.Rows[len(r.Rows)-1]; row.Title != "Server Address:" || d.loginServer() != "" {
		t.Fatalf("No: %q", titles(r))
	}
	r = d.do("setserver", "menu", "192.168.111.111:8096")
	r = d.do("menu", r.Rows[3].Context)
	if got := strings.Join(titles(r), "|"); got != "Server Address:|Sign In with Quick Connect|Sign In with Password|Warning: HTTP, not encrypted" {
		t.Fatalf("Yes: %q", got)
	}
}

// Without Q2 Pod's fieldtext the box opens empty: Add the port asks for the whole address again;
// with it, the box holds what was typed.
func TestAddPortWithoutFieldText(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.caps.Field = true
	d.do("menu")
	if r := d.do("setserver", "menu", "192.168.111.111"); r.Rows[1].Subtitle != "Press to type it again with the port" || r.Rows[1].Position != "" {
		t.Fatalf("%+v", r.Rows[1])
	}
	d.caps.FieldText = true
	if r := d.do("setserver", "menu", "192.168.111.111"); r.Rows[1].Subtitle != "Press to add the port" || r.Rows[1].Position != "192.168.111.111" {
		t.Fatalf("%+v", r.Rows[1])
	}
}
