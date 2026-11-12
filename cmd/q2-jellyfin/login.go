package main

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
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
// Everything is typed on the Q2, the server's address first: the app opens no port and serves no
// page, so nothing crosses the Wi-Fi except HTTPS to the server. Typed text reaches the app as
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

// setServer keeps the address of a server already checked to be Jellyfin.
func (d *daemon) setServer(server string) error {
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

func hostOf(server string) string {
	if u, err := url.Parse(server); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "the server"
}

// serverAddress turns what was typed into the server's https:// address: a bare host (with an
// optional port and path) gets https://; http:// and anything without a host are refused. The
// error's text never repeats what was typed, which may hold a user:password@.
func serverAddress(typed string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(typed), "/")
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "" || strings.ContainsAny(s, " \t"):
		return "", errors.New("That isn't a server address")
	case u.Scheme != "https":
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
	if err := d.changeServer(text, signOut); errors.Is(err, errSameServer) {
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
	server, err := serverAddress(text)
	if err == nil && signOut {
		d.mu.Lock()
		same := shownServer(server) == shownServer(d.server)
		d.mu.Unlock()
		if same {
			return errSameServer
		}
	}
	if err == nil {
		err = d.checkServer(server, d.answerIn*7/8)
	}
	if err == nil && signOut && d.signOut() != nil {
		err = errors.New(signOutFailed)
	}
	if err == nil {
		err = d.setServer(server)
	}
	if err != nil && !signOut { // still signed in, the root never shows it
		d.setLoginMsg(err.Error())
	}
	return err
}

// typedServerField is an address typed in a field row (Q2 Pod's field cap), on the first page
// (in "menu") or the Server page: the page as it now stands, or a failure, which Q2 Pod shows as
// the page's error page (the reason, Retry and Cancel), its Retry asking for the page again.
func (d *daemon) typedServerField(in, text string) q2spot.Reply {
	if r, off := d.noWiFi(false); off {
		return r
	}
	if err := d.changeServer(text, d.client() != nil); err != nil {
		return q2spot.Fail(q2spot.Error, err.Error())
	}
	if in == "server" {
		return d.serverPage()
	}
	return d.menu()
}

// checkServer checks server (an https:// address) is Jellyfin within in, sending no credentials;
// the error is for the screen. A server that fails gives the clock's fallback back to the last one.
func (d *daemon) checkServer(server string, in time.Duration) error {
	prev := tlsroots.DateURL()
	tlsroots.SetDateURL(server) // the certificate check needs a real clock (an unset Q2's isn't)
	cl, err := d.clientFor(jellyfin.Creds{Server: server})
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), in)
		defer cancel()
		if err = cl.CheckServer(ctx); err != nil {
			err = errors.New(jellyfin.Message(err, hostOf(server)))
		}
	}
	if err != nil {
		tlsroots.SetDateURL(prev)
	}
	return err
}

// loginClient is a client for the saved server with c's sign-in (c.Server is filled in).
func (d *daemon) loginClient(c jellyfin.Creds) (*jellyfin.Client, error) {
	c.Server = d.loginServer()
	return d.clientFor(c)
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
	d.login.pending, d.login.msg, d.login.file = "", "Signed in as "+c.User, ""
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
	d.cancelQC()
	cl, err := d.loginClient(jellyfin.Creds{})
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
	return q2spot.Reply{Rows: []q2spot.Row{info("Your password goes only to " + hostOf(d.login.server) + ", over HTTPS"),
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
		if s, err := serverAddress(text); err == nil && shownServer(s) == shownServer(server) {
			return q2spot.Reply{Rows: d.signedOutRows()} // Return asked for the server's page again
		}
		next, title := fieldPassword, "Type Your Password"
		rows := []q2spot.Row{info("User: " + text), info("The password shows on screen as you type")}
		if field == fieldKeyUser {
			next, title, rows = fieldKey, "Type the API Key", append(keyWarning(), info("User: "+text))
		}
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
	user := c.User
	cl, err := d.loginClient(c)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), d.answerIn*7/8)
		if c.Kind == jellyfin.KindAPIKey {
			c, err = cl.SignInWithKey(ctx, user)
		} else {
			c, err = cl.SignIn(ctx) // the password is swapped for a token and dropped
		}
		cancel()
	}
	if err == nil && d.client() != nil && d.signOut() != nil {
		err = errors.New(signOutFailed)
	}
	if err == nil {
		err = d.signedIn(c)
	}
	return err
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
	d.login.mu.Lock()
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
		info("Your password goes only to " + hostOf(server) + ", over HTTPS"),
		info("It shows on screen as you type it")}, now...)}
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
	if s, err := serverAddress(text); text == user || err == nil && shownServer(s) == shownServer(server) {
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
	rows := []q2spot.Row{list("server", "Change Server", hostOf(server), ""),
		{Kind: "action", URI: "qc", Title: "Sign In with Quick Connect", Subtitle: msg},
		list("pwlogin", "Sign In with Password", "", "")}
	if apiKeySignIn {
		rows = append(rows, list("keylogin", "Sign In with an API Key", "Admin-level: not recommended", ""))
	}
	return rows
}

// typeHint is under a field row with nothing typed yet.
const typeHint = "Press to type"

// serverHint is under a box for the server's address: Q2 Pod's box can't start with text in it.
const serverHint = "https:// is added for you, e.g. music.example.com"
