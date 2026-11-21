package main

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

// Sign-in on the Q2, in this order:
//
//  1. Quick Connect: the Q2 shows a code, the user approves it in any Jellyfin app where they are
//     signed in, and the Q2 swaps the secret for a token. No password reaches the Q2 at all.
//  2. User name and password typed on the Q2 (Q2 Pod's T9 box): the password goes only over HTTPS
//     to the server, is swapped for a token and never stored. Q2 Pod's box has no masked mode, so
//     it shows while typed.
//  3. An API key typed on the Q2, with the user whose library to show. Jellyfin's API keys are
//     admin-level and belong to no user, so the rows say so and the key is the last choice. Off
//     for now (apiKeySignIn).
//
// Everything is typed on the Q2, the server's address first: the app serves no page, and its one
// port (the player's relay, relay.go) is on the loopback only, so nothing crosses the Wi-Fi except
// HTTPS to the server. Typed text reaches the app as
// Q2SPOT/1 "search" requests (addon.c opens its T9 box for a row of kind "search" and sends what
// was typed); login.pending says which field the next one fills. The signed-out root's only box is
// the server's, so the root sets pending to it. Requests are not logged, so a typed password never
// reaches log.txt.
//
// Q2 Pod titles the page of a typed text with the text, and Return asks again for each page below
// it, typed ones included. So a password's or key's page always fails (a failed page is titled with
// its message): the secret is never a title. A user name's page asked again is told apart from a
// password by being the user name typed (typed), and once signed in, a sign-in page asked again is
// the signed-in root's rows (home), never a sign-in page or a search of what was typed.

const (
	qcPoll    = 3 * time.Second
	qcTimeout = 10 * time.Minute // Quick Connect requests expire on the server well within this
	loginIn   = 20 * time.Second // one sign-in request's limit in the background; a reply has d.answerIn
)

// The fields a typed text can fill, in login.pending.
const (
	fieldServer   = "server"
	fieldUser     = "user"
	fieldPassword = "password"
	fieldKeyUser  = "keyuser"
	fieldKey      = "key"
	// After the server refused the password or key: Q2 Pod's Retry sends it again, and Jellyfin
	// may lock a user out after failed sign-ins, so it is not asked again (typed).
	fieldPasswordRefused = "password refused"
	fieldKeyRefused      = "key refused"
)

// apiKeySignIn turns API key sign-in on: its row on the signed-out root, its pages (keylogin) and
// apikey= in the sign-in file. Off for now (TODO.md); a key already stored keeps working. A var,
// so tests can turn it on.
var apiKeySignIn = false

// keyOff is why an API key sign-in is refused while apiKeySignIn is off.
const keyOff = "API key sign-in is turned off for now"

// loginSuccess titles a typed password's or key's page once it has signed in (a failed reply's
// title: see above). Q2 Pod adds Retry, which asks again and gets this again.
const loginSuccess = "Login Success"

type login struct {
	mu      sync.Mutex
	server  string // the server address typed on the Q2, kept in .jellyfin/server
	pending string // the field the next typed text fills
	user    string // the user name typed for the password or key sign-in, kept once signed in (typedSignedIn)
	msg     string // the last sign-in's outcome, for the root's rows
	file    string // the sign-in file's outcome, shown on the signed-in root too (signinfile.go)

	fileMu sync.Mutex // one root request at a time reads the sign-in file
	stuck  time.Time  // the sign-in file that couldn't be deleted (its time): not read again

	// The password page's field rows (Q2 Pod's field cap): what was typed, in memory only, never
	// on the card. The password is forgotten after one try, both once signed in or back on the root.
	formUser, formPw string
	formMsg          string // why the last Sign In failed, the page's next answer (once)

	ask  asked // the plain http question waiting for its answer (httpask.go)
	askN int   // the last question's number, from a random one (newDaemon)

	tried  string // the address a field row's check failed on, for Try Again (typedServerField)
	triedN int    // its Try Again's number, from a random one (newDaemon): only that page's Try Again checks it

	qcCode   string             // the Quick Connect code on screen, "" when none is waiting
	qcCancel context.CancelFunc // ends the poll
	poll     time.Duration      // qcPoll unless a test sets it
}

const serverFile = "server"

// loadServer reads the saved server address (not a secret: the token is in credentials). It is
// the clock's fallback until a sign-in's own server is loaded (client).
func (d *daemon) loadServer() {
	b, _ := os.ReadFile(filepath.Join(d.dir, serverFile))
	server := strings.TrimSpace(string(b))
	d.login.mu.Lock()
	d.login.server = server
	d.login.mu.Unlock()
	if server != "" {
		tlsroots.SetDateURL(server)
	}
}

// setServer keeps the address of a server already checked to be Jellyfin, and for a plain http
// one the network it is on now, a network its secrets go to (homenet.go).
func (d *daemon) setServer(server string) error {
	if err := d.addHome(server); err != nil {
		return err
	}
	if err := q2.WriteFile(filepath.Join(d.dir, serverFile), []byte(server+"\n")); err != nil {
		return err
	}
	d.login.mu.Lock()
	d.login.server, d.login.msg = server, ""
	d.login.mu.Unlock()
	d.cancelQC()
	return nil
}

// loginServer is the saved server, or the signed-in one when none is saved (a sign-in from before
// the server was kept on its own).
func (d *daemon) loginServer() string {
	d.login.mu.Lock()
	server := d.login.server
	d.login.mu.Unlock()
	if server == "" && d.client() != nil {
		d.mu.Lock()
		server = d.server
		d.mu.Unlock()
	}
	return server
}

// waiting reports whether a Quick Connect code is on screen (the app stays up and awake for it).
func (d *daemon) qcWaiting() bool {
	d.login.mu.Lock()
	defer d.login.mu.Unlock()
	return d.login.qcCode != ""
}

const noHTTPSIP = "No HTTPS on this port. Type http:// first"

const plainOff = "Plain HTTP is turned off in http-allow.txt"

// ipv4Host reports whether server's host is an IPv4 address.
func ipv4Host(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Is4()
}

// hostPort is server's host and port as typed (no user:password@), or hostOf's fallback.
func hostPort(server string) string {
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return hostOf(server)
}

func hostOf(server string) string {
	if u, err := url.Parse(server); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "the server"
}

// serverAddress turns what was typed into the server's address: a bare host (with an optional port
// and path) gets https://, but an IP and port in a range of the allow list gets http://
// (httpallow.go); an http:// typed in full may be any IPv4 address and port (jellyfin.PlainOK),
// unless the allow list turns plain http off; any other http:// and anything without a host are
// refused. The error's text never repeats what was typed, which may hold a user:password@.
func (d *daemon) serverAddress(typed string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(typed), "/")
	if !strings.Contains(s, "://") {
		s = "https://" + s
		if u, err := url.Parse("http" + s[5:]); err == nil && jellyfin.PlainOK(u) {
			if ap, ok := addrPort(u.String()); ok && d.allowList().inRange(ap) {
				s = u.String()
			}
		}
	}
	if u, err := url.Parse(s); err == nil && u.Port() == "" && d.wantsPort(u, typed) {
		return "", errors.New(addPort)
	}
	if u, err := url.Parse(s); err == nil && u.Scheme == "http" && jellyfin.PlainOK(u) && d.allowList().off() {
		return "", errors.New(plainOff)
	}
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Hostname() == "" || strings.ContainsAny(s, " \t"):
		return "", errors.New("That isn't a server address")
	case u.Scheme != "https" && !(u.Scheme == "http" && jellyfin.PlainOK(u)):
		return "", errors.New("HTTPS is required. Use an https:// address")
	case u.User != nil:
		return "", errors.New("Leave the user name and password out of the address")
	}
	return s, nil
}

// setTypedServer checks the typed address is a Jellyfin server over HTTPS (sending it no
// credentials) and keeps it. signOut is the Server page's box while signed in: only once the new
// server answers, so a mistyped address or a text typed in another box (searchIn) changes nothing,
// does it sign out (the token or API key deleted) and switch. The same address keeps the sign-in.
//
// A failed address offers no box to type another: Q2 Pod keeps a page per text typed, so Return
// goes back to the page with the box however many tries were made.
func (d *daemon) setTypedServer(text string, signOut bool) q2spot.Reply {
	if err := d.changeServer(text, signOut); errors.Is(err, errAskHTTP) {
		return d.askPage()
	} else if errors.Is(err, errSameServer) {
		return q2spot.Reply{Rows: []q2spot.Row{info(err.Error())}}
	} else if err != nil {
		return q2spot.Reply{Rows: []q2spot.Row{info(err.Error()), info("Press Return to try another address")}}
	}
	// The way on at once: the sign-in choices, with Change Server (its address beneath) to type
	// it again, rather than a line of text to press Return past.
	return q2spot.Reply{Rows: d.signedOutRows()}
}

var errSameServer = errors.New("That's the server you're on")

// changeServer is setTypedServer's work; the error is for the screen.
func (d *daemon) changeServer(text string, signOut bool) error {
	server, err := d.serverAddress(text)
	same, plain := false, plainHTTP(server)
	if err == nil && signOut {
		d.mu.Lock()
		same = shownServer(server) == shownServer(d.server)
		d.mu.Unlock()
		if same && (!plain || d.knownHere(server) && d.allowed(server)) {
			return errSameServer // a plain one off the allow list is asked about again
		}
	}
	if err == nil {
		err = d.checkServer(server, d.answerIn*7/8)
	}
	if err == nil && plain && d.freshNetwork().Gateway == "" { // a Wi-Fi name alone is anyone's;
		err = errNetworkUnsure // read after the check (which may take seconds), before any sign-out
	}
	if err == nil && plain && (same || !d.allowed(server) || !d.knownHere(server)) {
		return d.askHTTP(server, signOut, same) // httpask.go: Yes adds it, then carries on here
	}
	if err == nil && signOut && d.signOut() != nil {
		err = errors.New(signOutFailed)
	}
	if err == nil {
		err = d.setServer(server)
	}
	if err != nil && !signOut && !errors.Is(err, errAskHTTP) { // still signed in, the root never shows it
		d.setLoginMsg(err.Error())
	}
	return err
}

const addPort = "Add the port, e.g. 192.168.1.10:8096"

// wantsPort reports whether u, an address with no port, is an IPv4 address meant for plain http:
// http:// typed in full, or typed bare in a range of the allow list. Plain http needs its port.
func (d *daemon) wantsPort(u *url.URL, typed string) bool {
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.Is4() {
		return false
	}
	return u.Scheme == "http" || !strings.Contains(typed, "://") && d.allowList().inRangeAddr(ip)
}

// typedServerField is an address typed in a field row (Q2 Pod's field cap), on the first page
// (in "menu") or the Server page. One that works shows the sign-in page in the page's place (it is
// signed out by then). An address that isn't right shows why, with the box to fix it (holding the
// text on fieldtext); only the same server, a failed sign-out, or text with '@' is Q2 Pod's error
// page (the reason, Retry and Cancel), whose Retry asks for the page again, to type another. A server
// that fails its check is kept (tried) for this page's Try Again, which checks it again: Q2 Pod's
// own Retry never sends a field's text again, as it may be a password; an address isn't secret.
func (d *daemon) typedServerField(in, text string) q2spot.Reply {
	if r, off := d.noWiFi(false); off {
		return r
	}
	server, err := d.serverAddress(text)
	if err == nil {
		err = d.changeServer(server, d.client() != nil)
	}
	if err == nil || errors.Is(err, errAskHTTP) || server == "" || errors.Is(err, errSameServer) || err.Error() == signOutFailed {
		d.setTried("") // Q2 Pod's Retry asks for the page again: the page as it stands (review #9)
	}
	switch {
	case err == nil:
		return q2spot.Reply{Message: "refresh", Rows: d.signedOutRows()}
	case errors.Is(err, errAskHTTP):
		return d.askPage() // httpask.go: its Yes carries on as here
	case server == "" && !strings.Contains(text, "@"): // not an address: fix it, not start again
		sub := "Press to type it again" // the box opens empty: the whole address
		if d.caps.FieldText {
			sub = "Press to fix it" // the box holds what was typed
		}
		if err.Error() == addPort {
			sub = "Press to type it again with the port"
			if d.caps.FieldText {
				sub = "Press to add the port"
			}
		}
		return q2spot.Reply{Rows: []q2spot.Row{info(err.Error()), d.addressField(in, sub, strings.TrimSpace(text))}}
	case server == "" || errors.Is(err, errSameServer) || err.Error() == signOutFailed:
		return q2spot.Fail(q2spot.Error, err.Error())
	}
	n := d.setTried(server)
	box, sub := shownServer(server), "Press to type another address"
	if err.Error() == noHTTPSIP { // the box opens with the http:// the message asks for (review loop r2)
		box = "http://" + strings.TrimPrefix(box, "https://")
		if d.caps.FieldText {
			sub = "Press, then OK: http:// is in"
		}
	}
	return q2spot.Reply{Rows: []q2spot.Row{info(err.Error()),
		// Q2 Pod asks for this page again with retryArg:n last (a page row, caps word window)
		{Kind: "page", URI: retryArg, Context: retryArg + ":" + strconv.Itoa(n), Title: "Try Again", Subtitle: shownServer(server)},
		d.addressField(in, sub, box)}}
}

// addressField is a Server Address: field row on page in, sub beneath it; its box opens holding
// address to edit where Q2 Pod can (caps word fieldtext), and empty elsewhere. An address only:
// never a password, which no row puts there.
func (d *daemon) addressField(in, sub, address string) q2spot.Row {
	row := q2spot.Row{Kind: "field", URI: "setserver", Context: in, Title: "Server Address:", Subtitle: sub}
	if d.caps.FieldText {
		row.Position = address
	}
	return row
}

// retryArg, with its page's number, is the last argument of a page asked again by Try Again.
const retryArg = "retry"

// setTried keeps the address whose check failed, for its page's Try Again, and returns that page's
// number; "" forgets it.
func (d *daemon) setTried(server string) int {
	d.login.mu.Lock()
	defer d.login.mu.Unlock()
	d.login.tried = server
	if server != "" {
		d.login.triedN++
	}
	return d.login.triedN
}

// retryServer is the address Try Again checks again when a page is asked for with arg, or "". Q2
// Pod keeps arg for every later request of that page (Back, a refresh), so only the newest failed
// page's number counts (review #1): any other page asked with an older one is the page as usual.
func (d *daemon) retryServer(arg string) string {
	verb, n, _ := strings.Cut(arg, ":")
	d.login.mu.Lock()
	defer d.login.mu.Unlock()
	if verb != retryArg || d.login.tried == "" || n != strconv.Itoa(d.login.triedN) {
		return ""
	}
	return d.login.tried
}

// checkServer checks server (an https:// address, or a private network's http:// one) is
// Jellyfin within in, sending no credentials; the error is for the screen. A server that fails
// gives the clock's fallback back to the last one; a plain http:// one never touches it.
func (d *daemon) checkServer(server string, in time.Duration) error {
	plain := plainHTTP(server) // no certificate to check, and tlsroots takes a Date over https only
	prev := tlsroots.DateURL()
	if !plain {
		tlsroots.SetDateURL(server) // the certificate check needs a real clock (an unset Q2's isn't)
	}
	cl, err := d.clientFor(jellyfin.Creds{Server: server})
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), in)
		defer cancel()
		if err = cl.CheckServer(ctx); errors.Is(err, jellyfin.ErrNoHTTPS) && ipv4Host(server) && !d.allowList().off() {
			err = errors.New(noHTTPSIP) // typed bare outside the allow list's ranges: http:// in full works
		} else if errors.Is(err, jellyfin.ErrNoHTTPS) && ipv4Host(server) {
			err = errors.New("No HTTPS on this port. " + plainOff) // http:// would be refused too
		} else if err != nil {
			err = errors.New(jellyfin.Message(err, hostPort(server))) // the port typed may be the wrong one
		}
	}
	if err != nil && !plain {
		tlsroots.SetDateURL(prev)
	}
	return err
}

// clientFor is a client for c, logging to log.txt (redacted).
func (d *daemon) clientFor(c jellyfin.Creds) (*jellyfin.Client, error) {
	cl, err := d.newClient(c)
	if cl != nil {
		cl.Log = d.log
	}
	return cl, err
}

// signedIn keeps c on the card and makes it the app's sign-in.
func (d *daemon) signedIn(c jellyfin.Creds) error {
	if err := creds.Save(d.dir, c); err != nil {
		return err
	}
	d.mu.Lock()
	d.cl = nil // client() loads the new sign-in
	d.dropAlbumsFetch()
	d.mu.Unlock()
	d.login.mu.Lock()
	d.login.pending, d.login.msg, d.login.file, d.login.user = "", "Signed in as "+c.User, "", c.User
	d.login.tried = ""                                             // a Try Again from before checks nothing now (review #1)
	d.login.formUser, d.login.formPw, d.login.formMsg = "", "", "" // whichever way it signed in (clearForm)
	d.login.mu.Unlock()
	return nil
}

// ---- Quick Connect ----

// startQC asks the server for a code and polls until the user approves it, it times out, or it is
// cancelled. The code shows on the root (signedOutRows).
func (d *daemon) startQC() q2spot.Reply {
	server := d.loginServer()
	if server == "" {
		return q2spot.Fail(q2spot.Error, "Set the server first")
	}
	if plainHTTP(server) { // its token would be refused here at once (homenet.go)
		if err := d.onHomeNetwork(server); err != nil {
			return q2spot.Fail(q2spot.Error, jellyfin.Message(err, hostOf(server)))
		}
	}
	d.cancelQC()
	cl, err := d.clientFor(jellyfin.Creds{Server: d.loginServer()})
	if err != nil {
		return q2spot.Fail(q2spot.Error, jellyfin.Message(err, hostOf(server)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.answerIn*7/8)
	code, secret, err := cl.QuickConnectInitiate(ctx)
	cancel()
	if err != nil {
		d.setLoginMsg(jellyfin.Message(err, hostOf(server)))
		return d.errToast(err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), qcTimeout)
	d.login.mu.Lock()
	d.login.qcCode, d.login.qcCancel, d.login.msg = code, pcancel, ""
	poll := d.login.poll
	d.login.mu.Unlock()
	if poll == 0 {
		poll = qcPoll
	}
	d.spawn(func() { d.pollQC(pctx, cl, secret, poll) })
	return q2spot.Reply{Message: "Approve code " + code + " in a Jellyfin app"}
}

func (d *daemon) pollQC(ctx context.Context, cl *jellyfin.Client, secret string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	defer d.endQC()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				d.setLoginMsg("Quick Connect timed out")
			}
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, loginIn)
		ok, err := cl.QuickConnectPoll(rctx, secret)
		if err == nil && ok {
			var c jellyfin.Creds
			if c, err = cl.QuickConnectSignIn(rctx, secret); err == nil {
				err = d.signedIn(c)
			}
			cancel()
			if err != nil {
				d.setLoginMsg(jellyfin.Message(err, hostOf(d.loginServer())))
			}
			return
		}
		cancel()
		if err != nil && !errors.Is(err, jellyfin.ErrUnreachable) { // a Wi-Fi blip: keep polling
			d.setLoginMsg(jellyfin.Message(err, hostOf(d.loginServer())))
			return
		}
	}
}

func (d *daemon) endQC() {
	d.login.mu.Lock()
	d.login.qcCode, d.login.qcCancel = "", nil
	d.login.mu.Unlock()
}

func (d *daemon) cancelQC() {
	d.login.mu.Lock()
	cancel := d.login.qcCancel
	d.login.qcCode, d.login.qcCancel = "", nil
	d.login.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *daemon) setLoginMsg(m string) {
	d.login.mu.Lock()
	d.login.msg = m
	d.login.mu.Unlock()
}

// ---- typed sign-ins ----

// typePage starts a typed sign-in: "pwlogin" (user name, then password) or "keylogin" (user name,
// then API key).
func (d *daemon) typePage(op string) q2spot.Reply {
	if d.loginServer() == "" {
		return q2spot.Fail(q2spot.Error, "Set the server first")
	}
	d.cancelQC()
	if op == "pwlogin" && d.caps.Field { // one page (pwForm), what was typed kept
		return d.pwForm()
	}
	d.login.mu.Lock()
	defer d.login.mu.Unlock()
	d.login.user = ""
	if op == "keylogin" {
		d.login.pending = fieldKeyUser
		return q2spot.Reply{Rows: append(keyWarning(), info("Whose library should the Q2 show?"),
			q2spot.Row{Kind: "search", Title: "Type the User Name"})}
	}
	d.login.pending = fieldUser
	return q2spot.Reply{Rows: []q2spot.Row{info(passwordGoes(d.login.server)),
		q2spot.Row{Kind: "search", Title: "Type Your User Name"}}}
}

// keyWarning is what every API-key row says first.
func keyWarning() []q2spot.Row {
	return []q2spot.Row{info("An API key is admin-level: it opens every library"),
		info("on the server. Prefer Quick Connect."),
		info("Make one in Dashboard → API Keys")}
}

// typed takes text typed in Q2 Pod's T9 box for the pending field, and answers the next step.
func (d *daemon) typed(text string) q2spot.Reply {
	d.login.mu.Lock()
	field, user, server := d.login.pending, d.login.user, d.login.server
	d.login.mu.Unlock()
	text = strings.TrimSpace(text)
	// shortcut: a password (or key) the same as the user name is taken for Return here and asks
	// for the password again; such a user signs in with Quick Connect (README).
	if back, ok := map[string]string{fieldPassword: fieldUser, fieldPasswordRefused: fieldUser,
		fieldKey: fieldKeyUser, fieldKeyRefused: fieldKeyUser}[field]; ok && text == user { // Return asked for its page again
		field = back
	} else if field == fieldPasswordRefused || field == fieldKeyRefused { // Retry: not sent again
		d.login.mu.Lock()
		msg := d.login.msg
		d.login.mu.Unlock()
		return q2spot.Fail(q2spot.Error, msg)
	}
	if field == fieldServer || field == fieldPassword || field == fieldKey { // the server is asked
		if r, off := d.noWiFi(field != fieldServer); off { // a secret's page fails: see above
			return r
		}
	}
	switch field {
	case fieldServer:
		return d.setTypedServer(text, false)
	case fieldUser, fieldKeyUser:
		if s, err := d.serverAddress(text); err == nil && shownServer(s) == shownServer(server) {
			return q2spot.Reply{Rows: d.signedOutRows()} // Return asked for the server's page again
		}
		next, title := fieldPassword, "Type Your Password"
		rows := []q2spot.Row{info("User: " + text), info("The password shows on screen as you type")}
		if field == fieldKeyUser {
			next, title, rows = fieldKey, "Type the API Key", append(keyWarning(), info("User: "+text))
		}
		rows = append(rows, plainWarning(server)...)
		d.login.mu.Lock()
		d.login.user, d.login.pending = text, next
		d.login.mu.Unlock()
		return q2spot.Reply{Rows: append(rows, q2spot.Row{Kind: "search", Title: title})}
	case fieldPassword, fieldKey:
		c := jellyfin.Creds{User: user}
		if field == fieldPassword {
			c.Password = text
		} else {
			c.Token, c.Kind = text, jellyfin.KindAPIKey
		}
		if err := d.typedSignIn(c); err != nil { // Return goes back to the box: the user name's page, asked again
			msg := jellyfin.Message(err, hostOf(cmp.Or(server, d.loginServer())))
			d.login.mu.Lock()
			d.login.msg = msg
			if errors.Is(err, jellyfin.ErrAuth) || errors.Is(err, jellyfin.ErrNoSuchUser) { // refused, not unreachable
				d.login.pending = map[string]string{fieldPassword: fieldPasswordRefused, fieldKey: fieldKeyRefused}[field]
			}
			d.login.mu.Unlock()
			return q2spot.Fail(q2spot.Error, msg)
		}
		return q2spot.Fail(q2spot.Error, loginSuccess)
	}
	return q2spot.Fail(q2spot.Login, "Sign in first")
}

// typedSignIn signs in with c, a user and a password or API key; a sign-in already made (Account's
// Signed in as) is replaced only by one that worked.
func (d *daemon) typedSignIn(c jellyfin.Creds) error {
	c.Server = d.loginServer()
	c, err := d.trySignIn(c, d.answerIn*7/8)
	if err == nil {
		err = d.signedIn(c)
	}
	return err
}

// trySignIn swaps c's password or API key for a sign-in to c.Server within in. Only one that worked
// signs out the sign-in on the Q2 (as the Server page does), so a typo or a wrong password changes
// nothing (audit 2026-11-10 #6). The caller keeps the new one (signedIn).
func (d *daemon) trySignIn(c jellyfin.Creds, in time.Duration) (jellyfin.Creds, error) {
	cl, err := d.clientFor(c)
	if err != nil {
		return c, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), in)
	if c.Kind == jellyfin.KindAPIKey {
		c, err = cl.SignInWithKey(ctx, c.User)
	} else {
		c, err = cl.SignIn(ctx) // the password is swapped for a token and dropped
	}
	cancel()
	if err == nil && d.client() != nil && d.signOut() != nil {
		err = errors.New(signOutFailed)
	}
	return c, err
}

// ---- the password page with field rows (Q2 Pod's field cap) ----
//
// One page: Username: and Password:, each a field row (Q2 Pod sends the text typed in its box to
// "pwfield" and shows the reply, the page with the row's new value, in the page's place: nothing is
// titled with it, and nothing is pushed for Return to ask again), then Sign In. What was typed is
// kept in memory only (login.formUser, formPw) and shown as the name and ***. Sign In, an action,
// tries the password once and forgets it; Q2 Pod then asks for the page again, which answers a
// failure as a failed reply: its error page, the reason with Retry and Cancel. Retry asks for the
// page again (never Sign In), so a password is never sent twice. The root forgets both.

// pwForm is the password page as it stands.
func (d *daemon) pwForm() q2spot.Reply {
	signedOut := d.client() == nil
	d.login.mu.Lock()
	if signedOut && d.login.formUser == "" { // the last user name known, as typed or signed in
		d.mu.Lock()
		d.login.formUser = cmp.Or(d.login.user, d.user) // d.user: a sign-in loaded at start (review #8)
		d.mu.Unlock()
	}
	user, pw, msg := d.login.formUser, d.login.formPw, d.login.formMsg
	d.login.formMsg = ""
	server := d.login.server
	d.login.mu.Unlock()
	if msg != "" { // the last Sign In's failure, once
		return q2spot.Fail(q2spot.Error, msg)
	}
	if pw != "" {
		pw = "***"
	}
	// signed in from before the server was kept on its own: the signed-in one
	server = cmp.Or(server, d.loginServer())
	var now []q2spot.Row // signed in (Account's Signed in as, or a sign-in just made): who
	if d.client() != nil {
		d.mu.Lock()
		now = []q2spot.Row{info("Signed in as " + d.user)}
		d.mu.Unlock()
	}
	return q2spot.Reply{Rows: append([]q2spot.Row{
		{Kind: "field", URI: "pwfield", Context: "user", Title: "Username:", Subtitle: cmp.Or(user, typeHint)},
		{Kind: "field", URI: "pwfield", Context: "password", Title: "Password:", Subtitle: cmp.Or(pw, typeHint)},
		act("pwsignin", "Sign In", ""),
		info(passwordGoes(server)),
		info("It shows on screen as you type it")}, append(plainWarning(server), now...)...)}
}

// passwordGoes is what a password page says about where the password goes, and how.
func passwordGoes(server string) string {
	over := "HTTPS"
	if plainHTTP(server) {
		over = "HTTP"
	}
	return "Your password goes only to " + hostOf(server) + ", over " + over
}

// plainHTTP reports whether server is a plain http:// address (a private network's, serverAddress).
func plainHTTP(server string) bool {
	u, err := url.Parse(server)
	return err == nil && u.Scheme == "http"
}

// plainWarning is the row under every page that sends the server a password, or shows its token
// in use, while the server is plain http: nothing is encrypted on the network.
func plainWarning(server string) []q2spot.Row {
	if !plainHTTP(server) {
		return nil
	}
	return []q2spot.Row{{Kind: "info", Title: "Warning: HTTP, not encrypted",
		Subtitle: "Others on this network can read your password and sign-in"}}
}

// pwField keeps a text typed in the password page's row in (user or password).
func (d *daemon) pwField(in, text string) q2spot.Reply {
	d.login.mu.Lock()
	if in == "password" {
		d.login.formPw = text // as typed: a password may have spaces
	} else {
		d.login.formUser = strings.TrimSpace(text)
	}
	d.login.mu.Unlock()
	return d.pwForm()
}

// pwSignIn is the password page's Sign In. Its reply is toasted; the page, asked again next, says
// why it failed.
func (d *daemon) pwSignIn() q2spot.Reply {
	if r, off := d.noWiFi(true); off { // nothing sent: the password stays for another try
		d.login.mu.Lock()
		d.login.formMsg = r.Message
		d.login.mu.Unlock()
		return q2spot.Reply{}
	}
	d.login.mu.Lock()
	user, pw, server := d.login.formUser, d.login.formPw, d.login.server
	if user != "" {
		d.login.formPw = "" // taken: tried once, never sent again
	}
	d.login.mu.Unlock()
	switch {
	case user == "":
		return q2spot.Fail(q2spot.Error, "Type your user name first")
	case pw == "":
		return q2spot.Fail(q2spot.Error, "Type your password first")
	}
	err := d.typedSignIn(jellyfin.Creds{User: user, Password: pw})
	if err == nil {
		return q2spot.Reply{Message: "Signed in as " + user}
	}
	msg := jellyfin.Message(err, hostOf(cmp.Or(server, d.loginServer())))
	d.login.mu.Lock()
	d.login.formMsg = msg
	d.login.mu.Unlock()
	return q2spot.Reply{}
}

// clearForm forgets what the password page holds.
func (d *daemon) clearForm() {
	d.login.mu.Lock()
	d.login.formUser, d.login.formPw, d.login.formMsg = "", "", ""
	d.login.mu.Unlock()
}

// typedSignedIn is a text typed in a sign-in page's box while signed in: Account's Signed in as,
// starting a password sign-in as another user, or a page of a sign-in just made that Return asked
// for again. The user name's or server's page is the signed-in root's rows; anything else may be a
// password, so it is neither searched for nor a title.
func (d *daemon) typedSignedIn(text string) q2spot.Reply {
	d.login.mu.Lock()
	field, user, server := d.login.pending, d.login.user, d.login.server
	d.login.mu.Unlock()
	if field != "" && field != fieldServer { // a sign-in under way
		return d.typed(text)
	}
	text = strings.TrimSpace(text)
	if s, err := d.serverAddress(text); text == user || err == nil && shownServer(s) == shownServer(server) {
		return d.home()
	}
	return q2spot.Fail(q2spot.Error, loginSuccess)
}

// signedOutRows is the root while signed out: with no server yet, the box to type its address
// (https:// is added to it); then Change Server, opening the Server page (a first box would be Q2
// Pod's title bar icon), and the ways to sign in (an API key's only with apiKeySignIn). While a
// Quick Connect code waits, only it and Cancel, which brings the rest back. The root's only box is
// the server's, so a text typed from here fills it.
func (d *daemon) signedOutRows() []q2spot.Row {
	d.login.mu.Lock()
	d.login.pending = fieldServer
	d.login.formUser, d.login.formPw, d.login.formMsg = "", "", "" // the password page left (clearForm)
	server, code, msg := d.login.server, d.login.qcCode, d.login.msg
	d.login.mu.Unlock()
	if server == "" {
		box := q2spot.Row{Kind: "search", Title: "Type Your Server Address", Subtitle: cmp.Or(msg, serverHint)}
		if d.caps.Field { // Q2 Pod sends its text to setserver and shows the reply in this page's place
			box = q2spot.Row{Kind: "field", URI: "setserver", Context: "menu", Title: "Server Address:", Subtitle: box.Subtitle}
		}
		return []q2spot.Row{info("First, your Jellyfin server's address"), box}
	}
	if code != "" {
		return []q2spot.Row{info("Quick Connect Code: " + code),
			{Kind: "info", Title: "Approve it in a Jellyfin app:", Subtitle: "User Menu → Quick Connect"},
			act("qccancel", "Cancel Quick Connect", "")}
	}
	change := list("server", "Change Server", shownServer(server), "")
	if d.caps.Field { // typed here, the reply is this page as it then stands (typedServerField)
		change = d.addressField("menu", shownServer(server), shownServer(server))
	}
	rows := []q2spot.Row{change,
		{Kind: "action", URI: "qc", Title: "Sign In with Quick Connect", Subtitle: msg},
		list("pwlogin", "Sign In with Password", "", "")}
	if apiKeySignIn {
		rows = append(rows, list("keylogin", "Sign In with an API Key", "Admin-level: not recommended", ""))
	}
	return append(rows, plainWarning(server)...)
}

// serverRow opens the Server page, the signed-in server's address beneath.
func (d *daemon) serverRow() q2spot.Row {
	d.mu.Lock()
	server := d.server
	d.mu.Unlock()
	return list("server", "Server", shownServer(server), "")
}

// serverPage is the server's address, pressed to type another; a text typed in it comes back as a
// search, which goes to setTypedServer as this page was asked for last (searchSections), or signed
// out as the root's box does. It is the page's only row, what a new address does beneath it: Q2 Pod
// makes a first search row the title bar's search icon unless it is the only one.
func (d *daemon) serverPage() q2spot.Reply {
	signedOut := d.client() == nil // first: it loads the sign-in, and with it d.server
	server, note := d.serverRow().Subtitle, "A new address signs you out"
	if signedOut && d.caps.Field { // the sign-in page holds the address (signedOutRows)
		return q2spot.Reply{Message: "refresh", Rows: d.signedOutRows()}
	}
	if signedOut {
		server, note = shownServer(d.loginServer()), "Press to type another address"
		d.login.mu.Lock()
		d.login.pending = fieldServer
		d.login.mu.Unlock()
	}
	if d.caps.Field { // Q2 Pod sends a text typed in it to setserver: the reply is this page's (login.go)
		return q2spot.Reply{Rows: []q2spot.Row{d.addressField("server", server, server), info(note)}}
	}
	return q2spot.Reply{Rows: []q2spot.Row{{Kind: "search", Title: server, Subtitle: note}}}
}

// shownServer is a server's address for the screen: never a proxy's user:password@, the host in
// lower case as typed on the T9 box it may not be.
func shownServer(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	u.User, u.Host = nil, strings.ToLower(u.Host)
	return u.String()
}

func (d *daemon) account(cl *jellyfin.Client) q2spot.Reply {
	d.mu.Lock()
	user, server := d.user, d.server
	d.mu.Unlock()
	rows := []q2spot.Row{d.serverRow(), {Kind: "search", Title: "Signed in as:", Subtitle: user}}
	rows = append(rows, plainWarning(server)...)
	if d.caps.Field { // the password page (login.go pwForm), what was typed there forgotten
		rows[1] = list("pwlogin", "Signed in as:", user, "")
		d.clearForm()
	}
	if cl.KeyAuth() { // signed in with a key before API key sign-in was turned off
		rows = append(rows, info("An admin-level API key is stored on this card"))
	}
	// Signed in as is the box for another user's name, then a password, as a first sign-in
	// (typedSignedIn); the sign-in now is replaced only once the new one works.
	d.login.mu.Lock()
	d.login.pending = fieldUser
	d.login.mu.Unlock()
	return q2spot.Reply{Rows: append(rows, act("signout", "Sign Out", ""))}
}

// typeHint is under a field row with nothing typed yet.
const typeHint = "Press to type"

// serverHint is under a box for the server's address: Q2 Pod's box can't start with text in it.
const serverHint = "e.g. music.example.com or 192.168.1.10:8096"
