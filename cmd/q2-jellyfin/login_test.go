package main

import (
	"encoding/json"
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
	if row := r.Rows[0]; row.Kind != "list" || row.URI != "server" || row.Subtitle != "127.0.0.1" {
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
		row.Subtitle != "https:// is added for you, e.g. music.example.com" {
		t.Fatalf("root %+v", r.Rows)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	r = d.do("search", host) // no scheme typed
	// The sign-in choices at once, after Change Server (its address beneath) to type it again.
	want := "Change Server|Sign In with Quick Connect|Sign In with Password"
	if got := strings.Join(titles(r), "|"); got != want {
		t.Fatalf("%q", got)
	}
	if row := r.Rows[0]; row.Kind != "list" || row.URI != "server" || row.Subtitle != "127.0.0.1" {
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
	d.do("signout")
	if r := d.do("menu"); r.Rows[len(r.Rows)-1].Subtitle != serverHint {
		t.Fatalf("stale message %+v", r.Rows)
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
	d.do("signout")
	eventually(t, "the logout", func() bool { return a.logouts.Load() == 1 })

	d.do("keylogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", "KEY"))
	d.do("account")
	d.do("signout")
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
	if r := d.do("signout"); r.Status == "" || r.Status == "ok" || r.Message != "Couldn't sign out: the card can't be written" {
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
	if r := d.do("signout"); r.Message != "Signed out" {
		t.Fatalf("%+v", r)
	}
	r := d.do("account")
	if got := strings.Join(titles(r), "|"); r.Message != "refresh" || got != "Change Server|Sign In with Quick Connect|Sign In with Password" {
		t.Fatalf("%q %+v", got, r)
	}
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

// With field rows, the server's address is a field row on the first page and on the Server page:
// an address that works shows the page as it now stands; one that doesn't is Q2 Pod's error page
// (the reason, Retry and Cancel).
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
	if r := d.do("setserver", "menu", "http://music.example"); r.Status == "" || r.Status == q2spot.OK ||
		r.Message != "HTTPS is required. Use an https:// address" {
		t.Fatalf("%+v", r)
	}
	r = d.do("setserver", "menu", strings.TrimPrefix(srv.URL, "https://"))
	if got := strings.Join(titles(r), "|"); got != "Change Server|Sign In with Quick Connect|Sign In with Password" {
		t.Fatalf("%q", got)
	}
	r = d.do("server")
	if row := r.Rows[0]; row.Kind != "field" || row.URI != "setserver" || row.Context != "server" || row.Subtitle != srv.URL {
		t.Fatalf("Server page %+v", r.Rows)
	}
	if r := d.do("setserver", "server", "http://elsewhere.example"); r.Message != "HTTPS is required. Use an https:// address" {
		t.Fatalf("%+v", r)
	}
	if r := d.do("setserver", "server", srv.URL); r.Rows[0].Kind != "field" || r.Rows[0].Subtitle != srv.URL {
		t.Fatalf("%+v", r)
	}
}
