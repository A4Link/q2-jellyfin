package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Review round 2 #1: a sign-in file never keeps a Wi-Fi for a plain server: on a Wi-Fi not kept
// for it, the file is refused, nothing reaches the server and the network record is unchanged.
func TestSignInFilePlainHTTPOtherWiFi(t *testing.T) {
	a := &authServer{}
	d := privateHTTPDaemon(t, a)
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096") // at home
	rec := filepath.Join(d.dir, networkFile)
	before, _ := os.ReadFile(rec)
	d.netNow = func() q2.Network { return q2.Network{SSID: "Hotel", Gateway: "11:22:33:44:55:66"} }
	d.netAt = time.Time{}
	writeSignIn(t, d, "server=192.168.111.111:8096\nuser=u\npassword="+typedPassword+"\n")
	n := a.signIns.Load()
	d.do("menu")
	d.bg.Wait()
	after, _ := os.ReadFile(rec)
	if _, ok := creds.Load(d.dir); ok || a.signIns.Load() != n || string(after) != string(before) ||
		d.login.msg != "Jellyfin sign-in.txt: type this address on the Q2 first, on this Wi-Fi" {
		t.Fatalf("%q, %d sign-ins, record %q", d.login.msg, a.signIns.Load()-n, after)
	}
}

// Review round 2 #2: without field rows the question is a search level; Yes's page asks for
// nothing again (a search level asked again would be the address typed again).
func TestPlainHTTPYesNoRefreshOnSearch(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	if r := allowPlain(t, d, "192.168.111.111:8096"); r.Message != "" || r.Rows[0].Title != "Change Server" {
		t.Fatalf("%+v", r)
	}
}

// Review round 2 #4: a name with a plain http port (Jellyfin's 8096) means https://, which that
// port doesn't speak: the check says so, with the port typed.
func TestNoHTTPSOnThisPort(t *testing.T) {
	srv := httptest.NewServer(&authServer{})
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, &http.Client{}) }
	d.do("menu")
	host := strings.TrimPrefix(srv.URL, "http://")
	r := d.do("search", host)                                           // 127.0.0.1:port typed bare: not in a range, so https://
	if r.Rows[0].Title != "No HTTPS on this port. Type http:// first" { // r3 #1: an IP: http:// works
		t.Fatalf("%q", titles(r))
	}
	d.do("menu")
	r = d.do("search", "localhost:"+srv.URL[strings.LastIndex(srv.URL, ":")+1:]) // a name
	if r.Rows[0].Title != "This port has no HTTPS. At home, type the server's IP and port" {
		t.Fatalf("name: %q", titles(r))
	}
	if err := d.checkServer("https://nas.invalid:1", time.Second); err == nil || !strings.Contains(err.Error(), "nas.invalid:1") {
		t.Fatalf("the port in the message: %v", err)
	}
}

// Review round 2 #6: Yes while the gateway's MAC can't be read keeps the old sign-in: the
// network is read before the sign-out, not after.
func TestPlainHTTPYesKeepsSignInWhenUnsure(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	d.do("pwlogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", typedPassword))
	d.do("server")
	r := d.do("search", "192.168.111.112:8096") // another server: asked
	yes, ok := find(r, "Yes, allow HTTP")
	if !ok {
		t.Fatalf("%q", titles(r))
	}
	d.netNow = func() q2.Network { return q2.Network{SSID: "HomeNet"} } // the ARP answer late
	d.netAt = time.Time{}
	r = d.do("search", "192.168.111.112:8096", yes.Context)
	if r.Message != "Can't check this Wi-Fi yet. Try again in a moment" || d.client() == nil {
		t.Fatalf("%+v, signed in %v", r, d.client() != nil)
	}
	if r.Status == "" || r.Status == q2spot.OK {
		t.Fatalf("%+v", r)
	}
	if d.allowed("http://192.168.111.112:8096") { // r3 #4: nothing written before the network read
		t.Fatal("on the allow list after a failed Yes")
	}
	d.netNow = func() q2.Network { return homeNet } // r3 #5: Retry (the same allow:n) answers Yes again
	d.netAt = time.Time{}
	r = d.do("search", "192.168.111.112:8096", yes.Context)
	if r.Rows[0].Title != "Change Server" || d.loginServer() != "http://192.168.111.112:8096" {
		t.Fatalf("Retry: %q, kept %q", titles(r), d.loginServer())
	}
}

// r3 #2: "Add the port" on a field row is the app's page, its box holding what was typed (fieldtext),
// so only the port is typed; never a user:password@.
func TestAddPortKeepsTyped(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.caps.Field, d.caps.FieldText = true, true
	d.do("menu")
	r := d.do("setserver", "menu", "192.168.1.10/jellyfin")
	if r.Status != "" && r.Status != q2spot.OK || r.Rows[0].Title != "Add the port, e.g. 192.168.1.10:8096" ||
		r.Rows[1].Title != "Server Address:" || r.Rows[1].Position != "192.168.1.10/jellyfin" {
		t.Fatalf("%+v", r)
	}
	if r := d.do("setserver", "menu", "http://u:p@192.168.1.10"); r.Status == "" || r.Status == q2spot.OK {
		t.Fatalf("user:password@ in the box: %+v", r)
	}
}

// r3 #3: signed in, a plain server taken off the allow list (the file deleted) is asked about again
// when its address is typed again, not "That's the server you're on"; Yes puts it back.
func TestSameServerOffTheListAskedAgain(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	allowPlain(t, d, "192.168.111.111:8096")
	d.do("pwlogin")
	d.do("search", "u")
	loginSuccessPage(t, d.do("search", typedPassword))
	os.Remove(filepath.Join(d.dir, allowFile))
	d.allow = nil
	d.do("server")
	r := d.do("search", "192.168.111.111:8096")
	yes, ok := find(r, "Yes, allow HTTP")
	if !ok {
		t.Fatalf("%q", titles(r))
	}
	r = d.do("search", "192.168.111.111:8096", yes.Context)
	if r.Rows[0].Title != "Saved for this Wi-Fi: 192.168.111.111" || !d.allowed("http://192.168.111.111:8096") || d.client() == nil {
		t.Fatalf("%q", titles(r))
	}
}

// Q2 Pod keeps a level's allow:/deny: across an app restart: the Yes of an earlier run's question
// never answers this run's, even for the same address.
func TestPlainHTTPAnswerFromEarlierRun(t *testing.T) {
	d1 := privateHTTPDaemon(t, &authServer{})
	d1.caps.Field = true
	d1.do("menu")
	yes := d1.do("setserver", "menu", "192.168.111.111:8096").Rows[3].Context
	d2 := privateHTTPDaemon(t, &authServer{}) // the app started again
	d2.caps.Field = true
	d2.do("menu")
	d2.do("setserver", "menu", "192.168.111.111:8096") // its own question waits
	d2.do("menu", yes)
	if d2.loginServer() != "" || d2.allowed("http://192.168.111.111:8096") {
		t.Fatalf("an earlier run's Yes taken: kept %q", d2.loginServer())
	}
}

// Q2 Pod keeps a level's retry: across an app restart: an earlier run's Try Again never checks
// this run's failed address.
func TestServerFieldTryAgainFromEarlierRun(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	failed := func() (*tdaemon, q2spot.Reply) {
		d := newTestDaemon(t, &nav{}, "field", false)
		d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
		d.do("menu")
		return d, d.do("setserver", "menu", srv.URL)
	}
	_, r := failed()
	old := r.Rows[1].Context
	d2, r := failed() // the app started again, its own page failed too
	if r.Rows[1].Title != "Try Again" {
		t.Fatalf("%q", titles(r))
	}
	if r := d2.do("menu", old); r.Rows[0].Title == "The server answered HTTP 503" {
		t.Fatalf("an earlier run's Try Again checked: %q", titles(r))
	}
}

// Sign Out's page (Account, signed out) is the sign-in list: a plain address typed there is asked
// about, and its Yes, asked for with "account", keeps the server.
func TestPlainHTTPAskedAfterSignOut(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.caps.Field = true
	r := d.do("account")
	if last := r.Rows[len(r.Rows)-1]; last.Title != "Server Address:" {
		t.Fatalf("%q", titles(r))
	}
	r = d.do("setserver", "menu", "192.168.111.111:8096")
	if r.Rows[0].Title != "192.168.111.111:8096 is served over HTTP" {
		t.Fatalf("%q", titles(r))
	}
	r = d.do("account", r.Rows[3].Context)
	if d.loginServer() != "http://192.168.111.111:8096" || titles(r)[0] != "Server Address:" {
		t.Fatalf("%q, kept %q", titles(r), d.loginServer())
	}
}

// Signed out, the Account level's Try Again checks the address again too.
func TestServerFieldTryAgainAfterSignOut(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "field", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	d.do("account")
	r := d.do("setserver", "menu", srv.URL)
	if r = d.do("account", r.Rows[1].Context); r.Rows[0].Title != "The server answered HTTP 503" {
		t.Fatalf("%q", titles(r))
	}
}

// An IP address typed bare on a port with no HTTPS says to type http:// first; on Q2 Pod with
// fieldtext the Server Address: box then opens holding the address with that http://.
func TestNoHTTPSFieldOpensWithHTTP(t *testing.T) {
	srv := httptest.NewServer(&authServer{})
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "field fieldtext", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, &http.Client{}) }
	d.do("menu")
	host := strings.TrimPrefix(srv.URL, "http://")
	r := d.do("setserver", "menu", host)
	if r.Rows[0].Title != noHTTPSIP {
		t.Fatalf("%+v", r)
	}
	if row := r.Rows[len(r.Rows)-1]; row.Title != "Server Address:" || row.Position != "http://"+host {
		t.Fatalf("%+v", row)
	}
}

// With plain HTTP turned off in http-allow.txt, an IP address on a port with no HTTPS says so,
// not to type http:// first, and the box never opens with http://.
func TestNoHTTPSPlainOff(t *testing.T) {
	srv := httptest.NewServer(&authServer{})
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "field fieldtext", false)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, &http.Client{}) }
	if err := os.WriteFile(filepath.Join(d.dir, allowFile), []byte("# nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.allow = nil
	d.do("menu")
	host := strings.TrimPrefix(srv.URL, "http://")
	r := d.do("setserver", "menu", host)
	if r.Rows[0].Title != "No HTTPS on this port. Plain HTTP is turned off in http-allow.txt" {
		t.Fatalf("%q", titles(r))
	}
	for _, row := range r.Rows {
		if strings.HasPrefix(row.Position, "http://") {
			t.Fatalf("%+v", row)
		}
	}
}

// An address that isn't right shows why, with the Server Address: box holding it to fix (on
// fieldtext); one with a user name and password is Q2 Pod's error page, never echoed in a box.
func TestServerFieldTypoKeptInBox(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "field fieldtext", false)
	d.do("menu")
	r := d.do("setserver", "menu", "http://nas.example:8096")
	if len(r.Rows) < 2 || r.Rows[1].Position != "http://nas.example:8096" || r.Rows[1].Subtitle != "Press to fix it" {
		t.Fatalf("%+v", r)
	}
	r = d.do("setserver", "menu", "http://u:p@192.168.1.10:8096")
	if r.Message != "Leave the user name and password out of the address" {
		t.Fatalf("%+v", r)
	}
	for _, row := range r.Rows {
		if strings.Contains(row.Position, "p@") {
			t.Fatalf("%+v", row)
		}
	}
}

// Review 0.8.2 #1: an allow list saved with a byte-order mark (Notepad's "UTF-8 with BOM") and
// CRLF keeps its first line.
func TestAllowListBOM(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	os.WriteFile(filepath.Join(d.dir, allowFile), []byte("\xef\xbb\xbf192.168.0.0/16\r\n"), 0o644)
	d.allow = nil
	if a := d.allowList(); len(a.ranges) != 1 || a.ranges[0].String() != "192.168.0.0/16" || a.off() {
		t.Fatalf("%+v", a)
	}
}

// Review 0.8.2 #5: the allow list is read again when the file changes while the app runs: a line
// changed on a computer stops HTTP to the old server; a server said Yes to stays allowed. The edit
// keeps the size, so its time alone tells.
func TestAllowListEditedWhileRunning(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	d.do("menu")
	if err := d.allowServer("http://192.168.1.10:8096"); err != nil || !d.allowed("http://192.168.1.10:8096") {
		t.Fatalf("not allowed after Yes: %v", err)
	}
	p := filepath.Join(d.dir, allowFile)
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "\n192.168.1.10:8096\n", "\n192.168.1.11:8096\n", 1)), 0o644)
	os.Chtimes(p, time.Now(), time.Now().Add(time.Minute)) // FAT's times are to 2 s
	if d.allowed("http://192.168.1.10:8096") || !d.allowed("http://192.168.1.11:8096") {
		t.Fatal("the edit wasn't read")
	}
}

// Review 0.8.2 #5: a failed read (plain HTTP off) isn't kept: once the file reads, it is used.
func TestAllowListReadFailed(t *testing.T) {
	d := privateHTTPDaemon(t, &authServer{})
	p := filepath.Join(d.dir, allowFile)
	os.Remove(p)
	os.MkdirAll(p, 0o755) // a directory: the read fails
	d.allow = nil
	if !d.allowList().off() {
		t.Fatal("a failed read allows")
	}
	os.Remove(p)
	os.WriteFile(p, []byte("192.168.0.0/16\n"), 0o644)
	if d.allowList().off() {
		t.Fatal("the failed read was kept")
	}
}

// Review 0.8.2 pass 2: an allow list with lines but no address the app can read (saved as UTF-16,
// or a typo in its only line) says so, not that plain HTTP was turned off; one with only comments
// still says it is off. Plain HTTP is off either way.
func TestAllowListUnreadable(t *testing.T) {
	utf16 := []byte{0xff, 0xfe}
	for _, c := range "192.168.0.0/16\r\n" {
		utf16 = append(utf16, byte(c), 0)
	}
	for file, want := range map[string]string{
		string(utf16):      "http-allow.txt has no address the app can read",
		"192.168.0.0/33\n": "http-allow.txt has no address the app can read",
		"# nothing\n":      "Plain HTTP is turned off in http-allow.txt",
	} {
		d := privateHTTPDaemon(t, &authServer{})
		os.WriteFile(filepath.Join(d.dir, allowFile), []byte(file), 0o644)
		d.allow = nil
		d.do("menu")
		if r := d.do("search", "192.168.111.111:8096"); r.Rows[0].Title != "No HTTPS on this port. "+want {
			t.Errorf("%q: %q", file, titles(r))
		}
		if _, err := d.serverAddress("http://192.168.111.111:8096"); err == nil || err.Error() != want {
			t.Errorf("%q: http:// %v", file, err)
		}
	}
}

// Review 0.8.3 p2: a missing allow list that can't be written back (a full card, a read-only
// FAT) is tried once: the defaults stay in effect and the failure is logged once, not on every
// check.
func TestAllowListMissingUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	d := newTestDaemon(t, &nav{}, "", false)
	os.MkdirAll(d.dir, 0o755)
	lp := filepath.Join(t.TempDir(), "log.txt")
	d.log = applog.Open(lp, 1<<20)
	os.Remove(filepath.Join(d.dir, allowFile))
	d.allow = nil
	os.Chmod(d.dir, 0o555)
	t.Cleanup(func() { os.Chmod(d.dir, 0o755) })
	for range 3 {
		if err := d.onHomeNetwork("http://192.168.1.10:8096"); err != jellyfin.ErrNotAllowed {
			t.Fatalf("refused with %v", err)
		}
		if a := d.allowList(); a.off() || !a.inRangeAddr(netip.MustParseAddr("192.168.1.10")) {
			t.Fatalf("not the defaults: %+v", a)
		}
	}
	b, _ := os.ReadFile(lp)
	if n := strings.Count(string(b), "can't write the defaults"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
}

// Review 0.8.3 p3: an allow list deleted again after the defaults were written back is written
// again, and a Yes keeps the default ranges: a bare IP and port in them still means http://.
// It covers allowList's re-read (allowList writes the file back before allowServer reads it);
// allowServer's own missing-file branch is covered by TestAllowListUnwritableThenYes.
func TestAllowListDeletedTwice(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	os.MkdirAll(d.dir, 0o755)
	p := filepath.Join(d.dir, allowFile)
	for range 2 {
		os.Remove(p)
		d.allowList()
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("not written back: %v", err)
		}
	}
	os.Remove(p)
	if err := d.allowServer("http://192.168.1.10:8096"); err != nil {
		t.Fatal(err)
	}
	if a := d.allowList(); !a.inRangeAddr(netip.MustParseAddr("192.168.1.20")) || !a.has("http://192.168.1.10:8096") {
		t.Fatalf("%+v", a)
	}
}

// Review 0.8.3 p3: a Yes after a missing allow list couldn't be written back, once the card is
// writable again, writes the defaults with the server.
func TestAllowListUnwritableThenYes(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	d := newTestDaemon(t, &nav{}, "", false)
	os.MkdirAll(d.dir, 0o755)
	os.Remove(filepath.Join(d.dir, allowFile))
	d.allow = nil
	os.Chmod(d.dir, 0o555)
	t.Cleanup(func() { os.Chmod(d.dir, 0o755) })
	d.allowList() // the defaults can't be written
	os.Chmod(d.dir, 0o755)
	if err := d.allowServer("http://192.168.1.10:8096"); err != nil {
		t.Fatal(err)
	}
	if a := d.allowList(); !a.inRangeAddr(netip.MustParseAddr("192.168.1.20")) || !a.has("http://192.168.1.10:8096") {
		t.Fatalf("%+v", a)
	}
}

// Review 0.8.3 p4: a Yes while the allow list is there but can't be read (a card read error)
// leaves it untouched and reports the failure: the ranges and earlier servers aren't replaced
// by the one server line.
func TestAllowListUnreadableThenYes(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file with no permissions")
	}
	d := newTestDaemon(t, &nav{}, "", false)
	os.MkdirAll(d.dir, 0o755)
	p := filepath.Join(d.dir, allowFile)
	want := "10.0.0.0/8\n192.168.0.0/16\n192.168.1.50:8096\n"
	os.WriteFile(p, []byte(want), 0o644)
	d.allowList()
	os.Chmod(p, 0o000)
	t.Cleanup(func() { os.Chmod(p, 0o644) })
	if err := d.allowServer("http://192.168.1.10:8096"); err == nil {
		t.Error("no error")
	}
	os.Chmod(p, 0o644)
	if b, _ := os.ReadFile(p); string(b) != want {
		t.Fatalf("file now %q", b)
	}
}
