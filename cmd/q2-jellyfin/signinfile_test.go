package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
)

// TestMain keeps the credentials' key (C3) in a temp folder: no test writes the machine's /data.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "q2-jellyfin-key")
	if err != nil {
		panic(err)
	}
	creds.KeyPath = filepath.Join(dir, "q2-jellyfin.key")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fileDaemon is a daemon whose clients reach authServer (login_test.go), and its address.
func fileDaemon(t *testing.T, signedIn bool) (*tdaemon, string) {
	t.Helper()
	srv := httptest.NewTLSServer(&authServer{})
	t.Cleanup(srv.Close)
	d := newTestDaemon(t, &nav{}, "", signedIn)
	d.newClient = func(c jellyfin.Creds) (*jellyfin.Client, error) { return jellyfin.New(c, srv.Client()) }
	return d, srv.URL
}

func writeSignIn(t *testing.T, d *tdaemon, s string) string {
	t.Helper()
	p := filepath.Join(d.card, signInFile)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("%s still there: %v", p, err)
	}
}

// noSecretAnywhere fails when s is in any file on the card (credentials, log.txt, server...).
func noSecretAnywhere(t *testing.T, d *tdaemon, s string) {
	t.Helper()
	filepath.Walk(d.card, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(s)) {
				t.Errorf("%q is in %s", s, p)
			}
		}
		return nil
	})
}

func TestParseSignIn(t *testing.T) {
	f, err := parseSignIn(append([]byte{0xEF, 0xBB, 0xBF}, "# from my laptop\r\n\r\n  SERVER = music.example.com \r\nUser=alice\r\n# comment\r\nPassword = a b=c \r\n"...))
	if err != nil || f.server != "music.example.com" || f.user != "alice" || f.password != "a b=c" || !f.hasPassword || f.hasKey {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := parseSignIn([]byte("server=s\nuser=u\napikey=K\n")); err == nil || err.Error() != keyOff {
		t.Fatalf("API key while it is off: %v", err)
	}
	keysOn(t)
	if f, err := parseSignIn([]byte("server=s\nuser=u\napikey=K\n")); err != nil || f.key != "K" || !f.hasKey {
		t.Fatalf("%+v %v", f, err)
	}
	for in, want := range map[string]string{
		"user=u\npassword=p":                     "no server= line",
		"server=s\nuser=u":                       "no password= line (or apikey=)",
		"server=s\npassword=p":                   "no user= line",
		"server=s\nuser=u\npassword=p\napikey=k": "both password= and apikey=: keep one",
		"server=s\nuser=u\napikey=":              "apikey= is empty",
		"server=s\nuser=u\nhunter2":              "a line isn't server=, user=, password= or apikey=",
	} {
		if _, err := parseSignIn([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

// C4: the file signs in, is gone before the server is asked, and only the encrypted token is kept.
func TestSignInFile(t *testing.T) {
	d, srv := fileDaemon(t, false)
	p := writeSignIn(t, d, "server="+srv+"\nuser=u\npassword="+typedPassword+"\n")
	d.do("menu")
	fileGone(t, p)
	d.bg.Wait()
	c, ok := creds.Load(d.dir)
	if !ok || c.Server != srv || c.Token != "tok" || c.Password != "" {
		t.Fatalf("%+v %v", c, ok)
	}
	b, _ := os.ReadFile(filepath.Join(d.dir, "credentials"))
	if json.Valid(b) || bytes.Contains(b, []byte("tok")) {
		t.Fatalf("not encrypted: %q", b)
	}
	if _, ok := find(d.do("menu"), "Signed in as u (from Jellyfin sign-in.txt)"); !ok {
		t.Fatalf("root %q", titles(d.do("menu")))
	}
	if d.loginServer() != srv {
		t.Fatalf("server %q", d.loginServer())
	}
	noSecretAnywhere(t, d, typedPassword)
}

// An API key from the file, while it is off: the file is deleted all the same, the root says why,
// and nothing is signed in or kept.
func TestSignInFileKeyOff(t *testing.T) {
	d, srv := fileDaemon(t, false)
	p := writeSignIn(t, d, "server="+srv+"\nuser=andreas\napikey=KEY\n")
	r := d.do("menu")
	fileGone(t, p)
	d.bg.Wait()
	if _, ok := creds.Load(d.dir); ok {
		t.Fatal("signed in")
	}
	if row, _ := find(r, "Type Your Server Address"); row.Subtitle != "Jellyfin sign-in.txt: API key sign-in is turned off for now" {
		t.Fatalf("%+v", r.Rows)
	}
	noSecretAnywhere(t, d, "KEY")
}

// An API key from the file, while it is on: the user named is whose library shows.
func TestSignInFileKey(t *testing.T) {
	keysOn(t)
	d, srv := fileDaemon(t, false)
	writeSignIn(t, d, "server="+srv+"\nuser=andreas\napikey=KEY\n")
	d.do("menu")
	d.bg.Wait()
	if c, ok := creds.Load(d.dir); !ok || c.User != "Andreas" || c.Kind != jellyfin.KindAPIKey {
		t.Fatalf("%+v %v", c, ok)
	}
	noSecretAnywhere(t, d, "KEY") // kept, but only encrypted
}

// A wrong password: the file is deleted all the same, the root says why, still signed out, and
// the server is kept for the sign-ins on the Q2.
func TestSignInFileWrongPassword(t *testing.T) {
	d, srv := fileDaemon(t, false)
	p := writeSignIn(t, d, "server="+srv+"\nuser=u\npassword=nope-Pw\n")
	d.do("menu")
	fileGone(t, p)
	d.bg.Wait()
	if _, ok := creds.Load(d.dir); ok {
		t.Fatal("signed in")
	}
	row, _ := find(d.do("menu"), "Sign In with Quick Connect")
	if row.Subtitle != "Jellyfin sign-in.txt: Wrong username or password" || d.loginServer() != srv {
		t.Fatalf("%+v, server %q", row, d.loginServer())
	}
	noSecretAnywhere(t, d, "nope-Pw")
}

// A file with no server line: deleted, the root says so, nothing else happens.
func TestSignInFileNoServer(t *testing.T) {
	d, _ := fileDaemon(t, false)
	p := writeSignIn(t, d, "user=u\npassword="+typedPassword+"\n")
	r := d.do("menu")
	fileGone(t, p)
	if row, _ := find(r, "Type Your Server Address"); row.Subtitle != "Jellyfin sign-in.txt: no server= line" {
		t.Fatalf("%+v", r.Rows)
	}
	noSecretAnywhere(t, d, typedPassword)
}

// Signed in, the file wins: signed out, then signed in to the file's server.
func TestSignInFileWhileSignedIn(t *testing.T) {
	d, srv := fileDaemon(t, true)
	if c, _ := creds.Load(d.dir); c.Server == srv {
		t.Fatal("already on the file's server")
	}
	p := writeSignIn(t, d, "server="+srv+"\nuser=u\npassword="+typedPassword+"\n")
	d.do("menu")
	fileGone(t, p)
	d.bg.Wait()
	if c, ok := creds.Load(d.dir); !ok || c.Server != srv || c.User != "u" {
		t.Fatalf("%+v %v", c, ok)
	}
	if d.client() == nil || d.hostName() != "127.0.0.1" {
		t.Fatal("not signed in")
	}
}

// Signed in, a file whose sign-in fails (a wrong password) changes nothing: still signed in as
// before, on the same server, and the root says why (audit 2026-11-10 #6).
func TestSignInFileFailsWhileSignedIn(t *testing.T) {
	d, srv := fileDaemon(t, true)
	before, _ := creds.Load(d.dir)
	p := writeSignIn(t, d, "server="+srv+"\nuser=u\npassword=wrong\n")
	d.do("menu")
	fileGone(t, p)
	d.bg.Wait()
	if c, ok := creds.Load(d.dir); !ok || c.Server != before.Server || c.Token != before.Token {
		t.Fatalf("sign-in changed: %+v %v", c, ok)
	}
	if d.client() == nil || d.loginServer() == srv {
		t.Fatalf("signed out, or server %q", d.loginServer())
	}
	if rows := d.fileRows(); len(rows) != 1 || !strings.HasPrefix(rows[0].Title, signInFile+": ") {
		t.Fatalf("%+v", rows)
	}
}

// Without Wi-Fi the file is left as it is, and taken once Wi-Fi is back.
func TestSignInFileWaitsForWiFi(t *testing.T) {
	d, srv := fileDaemon(t, false)
	p := writeSignIn(t, d, "server="+srv+"\nuser=u\npassword="+typedPassword+"\n")
	d.localIP = func() string { return "" }
	d.do("menu")
	if b, err := os.ReadFile(p); err != nil || !strings.Contains(string(b), typedPassword) {
		t.Fatalf("file touched: %v", err)
	}
	d.localIP = func() string { return "192.168.1.20" }
	d.do("menu")
	fileGone(t, p)
	d.bg.Wait()
	if _, ok := creds.Load(d.dir); !ok {
		t.Fatal("not signed in")
	}
}

// A file the card won't delete isn't used, and the root says to delete it from a computer.
func TestSignInFileUndeletable(t *testing.T) {
	d, _ := fileDaemon(t, false)
	p := filepath.Join(d.card, signInFile)
	os.MkdirAll(filepath.Join(p, "x"), 0o755) // a folder with something in it: os.Remove fails
	r := d.do("menu")
	if row, _ := find(r, "Type Your Server Address"); row.Subtitle != "Couldn't delete Jellyfin sign-in.txt — delete it from a computer" {
		t.Fatalf("%+v", r.Rows)
	}
	d.setLoginMsg("")
	d.do("menu") // not read again
	if d.login.msg != "" {
		t.Fatalf("read again: %q", d.login.msg)
	}
}
