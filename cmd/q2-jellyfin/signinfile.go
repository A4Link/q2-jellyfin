package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

// The sign-in file (C4): from a computer, the user puts the server's address and a user name and
// password (or an API key, with the user whose library to show, while apiKeySignIn is on) in
// "Jellyfin sign-in.txt" in the card's top folder:
//
//	server=music.example.com
//	user=alice
//	password=…
//
// The app looks for it when it starts and at every root request (a stat), deletes it before
// anything else, whether the sign-in then works or not, and keeps only the encrypted token (creds,
// C3). A file found while signed in wins: sign out, then sign in with it, which also changes the
// server. The outcome shows on the root. Nothing of the file is logged or written anywhere.

const (
	signInFile    = "Jellyfin sign-in.txt"
	signInFileMax = 4 << 10 // more than any sign-in: something else, never read whole into the Q2's RAM
)

type fileSignIn struct {
	server, user, password, key string
	hasPassword, hasKey         bool
}

// parseSignIn reads key=value lines: LF or CRLF, a UTF-8 BOM, keys in any case, spaces around
// trimmed, blank lines and # comments skipped. Errors never repeat a value.
func parseSignIn(b []byte) (f fileSignIn, err error) {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // a UTF-8 BOM, as Notepad may write
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line) // also CRLF's \r
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "server":
			f.server = v
		case "user":
			f.user = v
		case "password": // may be empty: a Jellyfin user can have none
			f.password, f.hasPassword = v, true
		case "apikey":
			f.key, f.hasKey = v, true
		default:
			return f, errors.New("a line isn't server=, user=, password= or apikey=")
		}
	}
	switch {
	case f.server == "":
		return f, errors.New("no server= line")
	case f.hasPassword && f.hasKey:
		return f, errors.New("both password= and apikey=: keep one")
	case f.user == "":
		return f, errors.New("no user= line")
	case !f.hasPassword && !f.hasKey:
		return f, errors.New("no password= line (or apikey=)")
	case f.hasKey && f.key == "":
		return f, errors.New("apikey= is empty")
	case f.hasKey && !apiKeySignIn:
		return f, errors.New(keyOff)
	}
	return f, nil
}

// signInFromFile takes the sign-in file, if there is one. Without Wi-Fi it stays for later:
// deleted unused, the user would have to write it again. The sign-in itself, two requests to the
// server, runs in the background, as the root must answer within answerIn; the root is live, so
// it shows the outcome when it comes.
func (d *daemon) signInFromFile() {
	p := filepath.Join(d.card, signInFile)
	d.login.fileMu.Lock()
	defer d.login.fileMu.Unlock()
	fi, err := os.Stat(p)
	if err != nil || d.localIP() == "" {
		return
	}
	d.login.mu.Lock()
	stuck := d.login.stuck
	d.login.mu.Unlock()
	if fi.ModTime().Equal(stuck) {
		return
	}
	var b []byte
	rerr := errors.New("too big to be a sign-in file")
	if fi.Size() <= signInFileMax {
		b, rerr = os.ReadFile(p)
		q2.Wipe(p) // FAT keeps a deleted file's bytes: the password goes first
	}
	if err := os.Remove(p); err != nil { // a read-only card: then the sign-in couldn't be kept either
		d.log.Printf("sign-in file: can't delete it: %v", err)
		d.login.mu.Lock()
		d.login.stuck = fi.ModTime()
		d.login.mu.Unlock()
		d.fileOutcome("Couldn't delete " + signInFile + " — delete it from a computer")
		return
	}
	f, err := parseSignIn(b)
	if rerr != nil {
		err = rerr
	}
	var server string
	if err == nil {
		server, err = serverAddress(f.server)
	}
	if err != nil {
		d.fileOutcome(signInFile + ": " + err.Error())
		return
	}
	d.log.Printf("sign-in file: signing in to %s", hostOf(server))
	d.fileOutcome("Signing in from " + signInFile + "…")
	d.spawn(func() { d.signInWith(server, f) })
}

// signInWith signs in as a typed password or API key does (typed), the server checked first. Only a
// sign-in that worked replaces the one on the Q2 (signed out first, as the Server page does), so a
// file with a typo or a wrong password changes nothing (audit 2026-11-10 #6). Signed out, a server
// found to be Jellyfin is kept even when the sign-in fails, for the sign-ins on the Q2.
func (d *daemon) signInWith(server string, f fileSignIn) {
	c := jellyfin.Creds{Server: server, User: f.user, Password: f.password}
	if f.hasKey {
		c = jellyfin.Creds{Server: server, User: f.user, Token: f.key, Kind: jellyfin.KindAPIKey}
	}
	err := d.checkServer(server, loginIn)
	if err == nil {
		var cl *jellyfin.Client
		if cl, err = d.clientFor(c); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), loginIn)
			if f.hasKey {
				c, err = cl.SignInWithKey(ctx, f.user)
			} else {
				c, err = cl.SignIn(ctx) // the password is swapped for a token and dropped
			}
			cancel()
		}
		if err == nil && d.client() != nil && d.signOut() != nil {
			err = errors.New(signOutFailed)
		}
		if d.client() == nil {
			if serr := d.setServer(server); err == nil {
				err = serr
			}
		} else { // still signed in (the sign-in failed): the clock's fallback back on its server
			d.mu.Lock()
			cur := d.server
			d.mu.Unlock()
			tlsroots.SetDateURL(cur)
		}
	}
	if err == nil {
		err = d.signedIn(c)
	}
	if err != nil {
		d.fileOutcome(signInFile + ": " + jellyfin.Message(err, hostOf(server)))
		return
	}
	d.fileOutcome("Signed in as " + c.User + " (from " + signInFile + ")")
}

// fileOutcome is the sign-in file's outcome, for the root signed out (msg) or in (file).
func (d *daemon) fileOutcome(m string) {
	d.login.mu.Lock()
	d.login.msg, d.login.file = m, m
	d.login.mu.Unlock()
}

// fileRows is the sign-in file's outcome on the signed-in root (or Settings, with sections).
func (d *daemon) fileRows() []q2spot.Row {
	d.login.mu.Lock()
	defer d.login.mu.Unlock()
	if d.login.file == "" {
		return nil
	}
	return []q2spot.Row{info(d.login.file)}
}
