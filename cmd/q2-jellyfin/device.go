package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// The Q2's Jellyfin page over Q2SPOT/1 (q2-pod patch/addon.c draws it). A list row's URI is the op it
// opens and its Context the argument; an action row sends URI with Context and toasts the reply's
// message; a held Play/Pause sends the row's Position ("toggle") with its Context ("kind:id").
// Item pages take "kind:id" too, since one Context serves both press and hold. A song row's Context
// is "song:<id> <list kind:id> <index>": toggle reads the first field, play the rest.

func info(title string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title} }
func act(op, title, ctx string) q2spot.Row {
	return q2spot.Row{Kind: "action", URI: op, Title: title, Context: ctx}
}
func list(op, title, sub, ctx string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: op, Title: title, Subtitle: sub, Context: ctx}
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func size(b int64) string {
	if b >= 1e9 {
		return fmt.Sprintf("%.1f GB", float64(b)/1e9)
	}
	return fmt.Sprintf("%d MB", (b+5e5)/1e6)
}

// page is rows from cursor (an offset) on, as many as one reply carries (fit), with the cursor of
// the next page.
func page(rows []q2spot.Row, cursor string) q2spot.Reply {
	off, _ := strconv.Atoi(cursor)
	off = max(0, min(off, len(rows)))
	end := off + fit(rows[off:min(off+q2spot.MaxRows, len(rows))])
	r := q2spot.Reply{Rows: rows[off:end]}
	if end < len(rows) {
		r.Cursor = strconv.Itoa(end)
	}
	return r
}

// windowRows is how many rows of a long list show at a time with the firmware's "window" cap: the
// firmware asks for them a reply (at most q2spot.MaxRows) at a time and appends them, then a Next
// page row asks for the next window in the list's place, and the window after the first starts with
// a Previous page row. The firmware keeps no more than one window's replies.
const windowRows = 200

// window cuts r, the reply for the rows from cursor on, at the end of cursor's window: a Next page
// row in place of the cursor, and a Previous page row first on a later window's first reply, after
// top (the section's search box, which Q2 Pod shows in its title bar only when it is the first row).
// Rows that would no longer fit one reply go to the next. A cursor counts rows, as page()'s does.
func (d *daemon) window(r q2spot.Reply, cursor string, top ...q2spot.Row) q2spot.Reply {
	if !d.caps.Window || r.Status != "" && r.Status != "ok" {
		return r
	}
	c, _ := strconv.Atoi(cursor)
	c = max(c, 0)
	start := c - c%windowRows
	end := start + windowRows
	next := -1 // where the next reply starts, -1 at the list's end
	if n, err := strconv.Atoi(r.Cursor); err == nil && r.Cursor != "" {
		next = n
	}
	rows, reached := r.Rows, false
	if next >= end { // the window ends in this reply
		if keep := end - c; keep < len(rows) {
			rows = rows[:keep]
		}
		next, reached = end, true
	}
	var prev []q2spot.Row
	if c == start && start > 0 {
		prev = append(top, q2spot.Row{Kind: "page", URI: "prev", Title: "Previous page", Context: strconv.Itoa(start - windowRows)})
	}
	for {
		out := append(append([]q2spot.Row{}, prev...), rows...)
		if reached {
			out = append(out, q2spot.Row{Kind: "page", URI: "next", Title: "Next page", Context: strconv.Itoa(end)})
		}
		if fit(out) == len(out) || len(rows) <= 1 {
			r.Rows, r.Cursor = out, ""
			if !reached && next >= 0 {
				r.Cursor = strconv.Itoa(next)
			}
			return r
		}
		// one row too many: the last goes to the next reply, which then ends the window
		rows, reached = rows[:len(rows)-1], false
		if next < 0 {
			next = c + len(r.Rows)
		}
		next--
	}
}

// sectionCursor is the cursor a paged section is answered from. Q2 Pod asks for a section it swipes
// back to without a cursor, and selects the row that was selected when it was left, so the section
// comes back in the window it was left in, row for row, and that row is the one it was on. The
// first window is always answered as the first ("0", Previous page's, as none), so it has the same
// rows however it is reached. The main list (asked for when Back reaches it, and when Jellyfin
// opens) forgets the windows: Q2 Pod opens a section from it at its first row.
// shortcut: held in memory, so an app restart while the page stays open (idle exit, a crash) forgets
// it, and the section comes back at its first window with the saved row's index, as before this.
func (d *daemon) sectionCursor(op, cursor string) string {
	if !d.caps.Window {
		return cursor
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cursor == "" {
		if at := d.windowAt[op]; at > 0 {
			return strconv.Itoa(at)
		}
		return ""
	}
	c, _ := strconv.Atoi(cursor)
	if d.windowAt == nil {
		d.windowAt = map[string]int{}
	}
	d.windowAt[op] = max(c, 0) - max(c, 0)%windowRows
	if cursor == "0" {
		return ""
	}
	return cursor
}

// fit is how many of rows (at most 50) one reply carries with room for a Sync Now row: a row that
// Encode left out for 64 KiB would never be shown, as the cursor would go on past it (M28).
func fit(rows []q2spot.Row) int {
	// The longest message stands in for "pages" and the Sync Now row's bytes.
	r := q2spot.Reply{Message: strings.Repeat("x", q2spot.MaxMessage), Cursor: "9999999999", Rows: rows}
	_, kept := r.Encode("99999999999999999999")
	return min(len(rows), max(kept, 1))
}

// listErr is a list that failed: on its first page the message and Details; on a later one, which
// the firmware adds to the list while it loads it page by page, a failed reply, so the list keeps
// More… to try again (M30).
func (d *daemon) listErr(err error, cursor string) q2spot.Reply {
	if cursor != "" {
		return d.errToast(err)
	}
	r := d.errRows(err, "")
	if errors.Is(err, jellyfin.ErrUnreachable) { // maybe the server moved
		r.Rows = append(r.Rows, d.serverRow())
	}
	return r
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
	server, note := d.serverRow().Subtitle, "A new address signs you out"
	if d.client() == nil {
		server, note = shownServer(d.loginServer()), "Press to type another address"
		d.login.mu.Lock()
		d.login.pending = fieldServer
		d.login.mu.Unlock()
	}
	if d.caps.Field { // Q2 Pod sends a text typed in it to setserver: the reply is this page's (login.go)
		return q2spot.Reply{Rows: []q2spot.Row{{Kind: "field", URI: "setserver", Context: "server", Title: "Server Address:",
			Subtitle: server}, info(note)}}
	}
	return q2spot.Reply{Rows: []q2spot.Row{{Kind: "search", Title: server, Subtitle: note}}}
}

// home is the signed-in root's rows, for a sign-in page Q2 Pod asks for again once signed in
// (Return goes back a page at a time): Newly Added is a press away. Sections are the first page's.
func (d *daemon) home() q2spot.Reply {
	r := d.menu()
	if r.Message == "sections" {
		r.Message = ""
	}
	return r
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

// wifiWait is how long a request without Wi-Fi says "Connecting to Wi-Fi…" while the user's switch
// is on: Q2 Pod's Smart Wi-Fi turns the radio off when idle and on again 3-8 s after Streaming opens.
const wifiWait = 15 * time.Second

// netOps are the signed-in requests only the server can answer, true for an action (toasted).
var netOps = map[string]bool{"latest": false, "playlists": false, "artists": false, "albums": false, "artist": false,
	"album": false, "playlist": false, "favorites": false, "search": false, "syncpl": false, "play": true, "npplay": true, "syncfav": true}

// noWiFi is the page in place of one that needs the server while the Q2 has no Wi-Fi, as stock
// Tidal's "Network Disconnected", or for an action its toast; ok is false with Wi-Fi. With the
// user's Wi-Fi switch off it says so at once; with it on, "Connecting to Wi-Fi…" for wifiWait
// first. Live, so it turns into the page asked for once Wi-Fi is back.
func (d *daemon) noWiFi(action bool) (r q2spot.Reply, ok bool) {
	up := d.localIP() != ""
	off := !up && d.wifiOff()
	d.mu.Lock()
	switch {
	case up || off: // the wait counts from the switch turned on
		d.noWiFiAt = time.Time{}
	case d.noWiFiAt.IsZero():
		d.noWiFiAt = time.Now()
	}
	since := time.Since(d.noWiFiAt)
	d.mu.Unlock()
	switch {
	case up:
		return r, false
	case !off && since < wifiWait:
		r = q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Connecting to Wi-Fi…")}}
	case off:
		r = q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Network Disconnected"), info("Turn on Wi-Fi in Settings")}}
	default: // the switch is on: no network in reach, or it won't connect
		r = q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Network Disconnected"), info("No Wi-Fi network in reach")}}
	}
	if action {
		r = q2spot.Fail(q2spot.Offline, r.Rows[0].Title)
	}
	return r, true
}

// parseKey splits "kind:id" (or a song row's Context, whose first field it is).
func parseKey(arg string) (selection.Kind, string) {
	f, _, _ := strings.Cut(arg, " ")
	k, id, _ := strings.Cut(f, ":")
	return selection.Kind(k), id
}

func (d *daemon) device(req q2spot.Request) q2spot.Reply {
	start := time.Now()
	if in, ok := searchSections[req.Op]; ok { // as asked, not as answered: see searchSections
		d.mu.Lock()
		d.searchIn = in
		d.mu.Unlock()
	}
	var r q2spot.Reply
	if _, box := d.boxSearch(req.Op); songListOps[req.Op] && d.client() != nil && !box {
		r = d.songList(req) // status.go: live while a song pressed in it buffers
	} else {
		r = d.reply(req)
	}
	if artOps[req.Op] && (r.Status == "" || r.Status == "ok") {
		r = d.withArt(d.client(), r, start.Add(d.answerIn-time.Second)) // art.go: covers, before the reply
	}
	// F-R8: the firmware auto-pages a list only when its Message is "pages" and it has a cursor;
	// otherwise it shows a More… row. Refresh levels keep "refresh" and never page.
	if r.Cursor != "" && r.Message == "" {
		r.Message = "pages"
	}
	return r
}

// boxSearch reports whether op is text typed in a box that isn't a search's: a sign-in page's, the
// Server page's, or none offered since the app started (in ""). It may be a secret, so it is
// never a song list (kept for polls) nor a page that needs the server; in is the box.
func (d *daemon) boxSearch(op string) (in string, box bool) {
	if op != "search" {
		return "", false
	}
	d.mu.Lock()
	in = d.searchIn
	d.mu.Unlock()
	return in, in == "" || in == fieldServer || in == searchLogin
}

func (d *daemon) reply(req q2spot.Request) q2spot.Reply {
	d.mu.Lock()
	d.lastReq = time.Now()
	if req.Op != "clearcache" {
		d.clearArmed = time.Time{} // any other press drops Clear Playback Cache's first press
	}
	d.mu.Unlock()
	switch req.Op {
	case "menu": // the main list, or Jellyfin opened: each section from its start again (sectionCursor)
		d.mu.Lock()
		d.windowAt = nil
		d.mu.Unlock()
		return d.menu()
	case "details":
		return details(req.Arg(0))
	case "settings", "storage", "defq", "streamq", "setdefq", "setstreamq", "cache", "clearcache", "keepfree", "swipe", "reportplays":
		return d.settingsOp(req.Op, req.Arg(0))
	case "sort", "sortby", "setsort", "sortorder", "setorder": // settings on the card: no server needed
		return d.sortOp(req.Op, req.Arg(0))
	case "onq2":
		return d.onQ2(req.Arg(0))
	case "npqueue": // Now Playing's ⋯ (q2app's more=): from /tmp, no server needed (playqueue.go)
		return d.queuePage(req.Arg(0))
	case "resetsel":
		return d.resetSelection()
	case "qc":
		if r, off := d.noWiFi(true); off {
			return r
		}
		return d.startQC()
	case "qccancel":
		d.cancelQC()
		return q2spot.Reply{Message: "Quick Connect cancelled"}
	case "pwlogin", "keylogin":
		// asked again by Return once signed in: never a sign-in page, but the one-page form (also
		// Account's Signed in as), which pushes no page for Return to ask again
		if d.client() != nil && !(req.Op == "pwlogin" && d.caps.Field) {
			return d.home()
		}
		if req.Op == "keylogin" && !apiKeySignIn { // no row opens it; a page kept from before may
			return q2spot.Fail(q2spot.Error, keyOff)
		}
		return d.typePage(req.Op)
	case "server":
		return d.serverPage()
	case "setserver": // a field row's text (Q2 Pod's field cap): the page it is on, its argument
		return d.typedServerField(req.Arg(0), req.Arg(1))
	case "pwfield", "pwsignin": // the password page's rows (field cap), signed in too (Account)
		if !d.caps.Field { // no such page on this firmware
			return q2spot.Fail(q2spot.Unsupported, "Unknown request")
		}
		if req.Op == "pwsignin" {
			return d.pwSignIn()
		}
		return d.pwField(req.Arg(0), req.Arg(1))
	}
	cl := d.client()
	if cl == nil {
		if req.Op == "search" { // text typed in Q2 Pod's box for a sign-in field (login.go)
			return d.typed(req.Arg(0))
		}
		if req.Op == "account" { // Sign Out's page, once it has toasted: the sign-in, live for Quick Connect
			return q2spot.Reply{Message: "refresh", Rows: d.signedOutRows()}
		}
		return q2spot.Fail(q2spot.Login, "Sign in first")
	}
	if in, box := d.boxSearch(req.Op); box { // before netOps: its Wi-Fi page would be titled with the text
		switch in {
		case "": // no box offered since the app started: Q2 Pod's Retry of a page kept from before,
			// which may be a password's or key's, so neither searched for nor a title
			return q2spot.Fail(q2spot.Error, searchAgain)
		case fieldServer: // typed in the Server page's box
			if r, off := d.noWiFi(false); off {
				return r
			}
			return d.setTypedServer(req.Arg(0), true)
		}
		return d.typedSignedIn(req.Arg(0)) // in a sign-in page's box (login.go): a secret's fails with no Wi-Fi
	}
	if action, ok := netOps[req.Op]; ok {
		if r, off := d.noWiFi(action); off {
			return r
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.answerIn*7/8)
	defer cancel()
	switch req.Op {
	case "latest":
		return d.withSyncNow(d.latest(ctx, cl, req.Arg(0)), req.Arg(0))
	case "playlists", "artists", "albums":
		s, cursor := d.sortNow(req.Op), d.sectionCursor(req.Op, req.Arg(0))
		top := d.listTop(req.Op, s) // every window has its search and sort; first, so a failed page's box wins
		r := d.withSyncNow(d.browse(ctx, cl, req.Op, cursor, s), cursor)
		return d.window(r, cursor, top...)
	case "artist", "album", "playlist", "favorites":
		return d.window(d.withSyncNow(d.itemPage(ctx, cl, req.Arg(0), req.Arg(1)), req.Arg(1)), req.Arg(1))
	case "search":
		return d.window(d.withSyncNow(d.search(ctx, cl, req.Arg(0), req.Arg(1)), req.Arg(1)), req.Arg(1))
	case "toggle":
		return d.toggle(ctx, cl, req.Arg(0))
	case "play":
		return d.play(ctx, cl, req.Arg(0))
	case "npplay":
		return d.queuePlay(req.Arg(0))
	case "account":
		return d.account(cl)
	case "signout":
		if d.signOut() != nil {
			return q2spot.Fail(q2spot.Error, signOutFailed)
		}
		return q2spot.Reply{Message: "Signed out"}
	case "sync":
		return d.syncPage(cl)
	case "syncfav":
		return d.syncFavourites(ctx, cl)
	case "syncpl":
		return d.syncPlaylists(ctx, cl, req.Arg(0))
	case "jobs":
		return d.jobs(req.Arg(0))
	case "hold":
		return d.hold(cl, req.Arg(0))
	case "start", "pause", "resume", "cancel", "retry", "check":
		return d.syncAction(req.Op)
	case "removed":
		return d.removed(req.Arg(0))
	case "added":
		return d.added(req.Arg(0))
	case "failed":
		return d.failed(req.Arg(0))
	case "library":
		return d.library()
	}
	return q2spot.Fail(q2spot.Unsupported, "Unknown request")
}

// errRows is a failed list: the message and a Details row. The status stays ok because the
// firmware shows only the message and Retry, never the rows, for any other status.
func (d *daemon) errRows(err error, song string) q2spot.Reply {
	host := d.hostName()
	d.log.Printf("error: %v", err)
	var ae *jellyfin.APIError
	op, h, code := "", 0, 0
	if errors.As(err, &ae) {
		op, h, code = ae.Op, ae.HTTP, ae.Code
	}
	return q2spot.Reply{Rows: []q2spot.Row{info(jellyfin.Message(err, host)),
		list("details", "Details", "", detailsCtx(op, h, code, time.Now(), song))}}
}

// errToast is a failed action's reply: the firmware toasts its message.
func (d *daemon) errToast(err error) q2spot.Reply {
	host := d.hostName()
	d.log.Printf("error: %v", err)
	st := q2spot.Error
	if errors.Is(err, jellyfin.ErrUnreachable) {
		st = q2spot.Offline
	}
	return q2spot.Fail(st, jellyfin.Message(err, host))
}

// detailsCtx carries what the Details page shows, so it needs no state: op|http|code|unix|song,
// then |error when withErr adds one.
func detailsCtx(op string, h, code int, at time.Time, song string) string {
	return fmt.Sprintf("%s|%d|%d|%d|%s", op, h, code, at.Unix(), song)
}

// withErr adds err's text to a Details context, for a failure no op, HTTP status or code explains.
func withErr(ctx string, err error) string {
	return ctx + "|" + strings.ReplaceAll(applog.Redact(err.Error()), "|", "/")
}

func details(ctx string) q2spot.Reply {
	f := strings.SplitN(ctx, "|", 6)
	for len(f) < 6 {
		f = append(f, "")
	}
	unix, _ := strconv.ParseInt(f[3], 10, 64)
	rows := []q2spot.Row{info("Op: " + f[0]), info("HTTP: " + f[1]), info("Code: " + f[2]),
		info("Time: " + time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC"))}
	if f[4] != "" {
		rows = append(rows, info("Song: "+f[4]))
	}
	if f[5] != "" {
		rows = append(rows, info("Error: "+f[5]))
	}
	return q2spot.Reply{Rows: rows}
}

// menu is the root. Signed out it is the sign-in on the Q2 (login.go: the server's address, then
// Quick Connect, password or API key), kept live so a Quick Connect approval turns it into the
// signed-in menu. The app opens no port and serves no page for signing in.
func (d *daemon) menu() q2spot.Reply {
	d.signInFromFile() // signinfile.go: a sign-in file put on the card from a computer
	cl := d.client()
	if cl == nil {
		return q2spot.Reply{Message: "refresh", Rows: d.signedOutRows()}
	}
	if r, off := d.noWiFi(false); off { // Settings stays in reach: it, Sync's On This Q2 and Account need no server
		r.Rows = append(r.Rows, list("settings", "Settings", "", ""))
		return r
	}
	// The sections; Sync (with On This Q2 and Sync Entire Library) is under Settings, its subtitle
	// the sync's state. Search is at the top of Albums, Artists and Playlists, for that section.
	rows := []q2spot.Row{list("latest", "Newly Added", "", ""), list("albums", "Albums", "", ""),
		list("artists", "Artists", "", ""), list("playlists", "Playlists", "", ""),
		list("settings", "Settings", d.syncSubtitle(cl), "")}
	on := d.swipeOn() // swipe.go: the firmware offers sections and the setting is on
	d.mu.Lock()
	d.sectionsShown = on
	d.mu.Unlock()
	if on { // Q2 Pod shows them as pages swiped between; a stream's failure shows in Settings
		return q2spot.Reply{Message: "sections", Rows: rows}
	}
	// Last, so the rows above keep their places under the cursor (M33).
	return q2spot.Reply{Message: "refresh", Rows: append(append(append(rows, d.streamErrRows()...), d.favRows()...), d.fileRows()...)}
}

// streamErrRows are a stream's failure and its Details, when one stopped or skipped a song.
func (d *daemon) streamErrRows() []q2spot.Row {
	e := d.stream.Err()
	if e == nil {
		return nil
	}
	op, h, code := "stream", 0, 0
	var ae *jellyfin.APIError
	if errors.As(e, &ae) {
		op, h, code = ae.Op, ae.HTTP, ae.Code
	}
	return []q2spot.Row{info(e.Error()), list("details", "Details", "", detailsCtx(op, h, code, e.at, e.id))}
}

func (d *daemon) syncSubtitle(cl *jellyfin.Client) string {
	snap := d.runner.Snapshot()
	switch snap.State {
	case "running":
		return fmt.Sprintf("Syncing · %d %%", snap.Permille/10)
	case "paused":
		return fmt.Sprintf("Paused · %d %%", snap.Permille/10)
	}
	// Connected comes from the app's latest real request, not a ping on a timer (audit B9): a list,
	// a Play or a sync that reached the server says so, one that couldn't says why.
	asked, err := cl.Last()
	d.mu.Lock()
	host := d.host
	d.mu.Unlock()
	last := "Not synced yet"
	// From the last sync that ran to its end; an older build's card says "Not synced yet" until the
	// next. The file's time can't be used: previews and imports save it too.
	if at := d.manifest().SyncedAt; !at.IsZero() {
		last = "Last synced " + when(at, time.Now())
	}
	switch {
	case d.localIP() == "":
		return "Not connected · No Wi-Fi"
	case asked.IsZero(): // nothing asked of the server yet
		return last
	case err != nil:
		return "Not connected · " + jellyfin.Message(err, host)
	}
	return "Connected · " + last
}

func when(t, now time.Time) string {
	y, m, dd := now.Date()
	today := time.Date(y, m, dd, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "today " + t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// libraryRow is Sync Entire Library: Off, On, or On with the counts once the preview has resolved it.
func (d *daemon) libraryRow(sel selection.Set) q2spot.Row {
	sub := "Off"
	if sel.Has(selection.KindLibrary, "all") {
		sub = "On"
		d.mu.Lock()
		c := d.libCount
		_, ok := d.resolved[selection.Key(selection.KindLibrary, "all")]
		d.mu.Unlock()
		if ok {
			sub = fmt.Sprintf("On · %s · %s", plural(c[0], "album"), plural(c[1], "playlist"))
		}
	}
	return q2spot.Row{Kind: "action", URI: "toggle", Title: "Sync Entire Library", Subtitle: sub,
		Context: "library:all", Position: "toggle"}
}

// onQ2 is the selection, each with its status and size on the card; a hold takes one out. Entire
// Library is switched from the Sync page; here it shows only while on, as one of the items.
func (d *daemon) onQ2(cursor string) q2spot.Reply {
	if _, err := d.selection(); err != nil {
		return damagedSelection()
	}
	v := d.view()
	var rows []q2spot.Row
	if len(v.sel.Items) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{{Kind: "info", Title: "Nothing chosen yet",
			Subtitle: "Hold Play/Pause on a playlist, album or artist"}}}
	}
	for _, it := range v.sel.Items {
		key := selection.Key(it.Kind, it.ID)
		if it.Kind == selection.KindLibrary {
			rows = append([]q2spot.Row{d.libraryRow(v.sel)}, rows...)
			continue
		}
		var b int64
		for _, sid := range v.keyed[key] {
			b += v.m.Songs[sid].Size
		}
		total := 0
		if it.Kind == selection.KindSong {
			total = 1
		}
		row := v.item(it.Kind, it.ID, it.Name, "", total)
		row.Subtitle = join(row.Subtitle, size(b))
		rows = append(rows, row)
		if it.Kind == selection.KindSong {
			rows[len(rows)-1].Kind, rows[len(rows)-1].URI = "info", ""
		}
	}
	return page(rows, cursor)
}

var settingsMu sync.Mutex

const settingNotSaved = "Couldn't save the setting"

func (d *daemon) settingsOp(op, arg string) q2spot.Reply {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := d.settings()
	switch op {
	case "defq":
		return syncQualityPage(s)
	case "streamq":
		return streamQualityPage(s)
	case "setdefq": // every synced song's: the next sync brings those at another quality again
		if quality.Normal(arg) == s.def() { // as it is: a sync under way goes on
			return q2spot.Reply{Message: "Sync Quality: " + quality.Name(s.def())}
		}
		s.StreamQuality = s.stream() // "" followed Sync Quality: it keeps the value it had
		s.DefaultQuality = quality.Normal(arg)
		if d.saveSettings(s) != nil {
			return q2spot.Fail(q2spot.Error, settingNotSaved)
		}
		d.changed()
		// a sync under way goes on at the new quality
		return q2spot.Reply{Message: "Sync Quality: " + quality.Name(s.def()) + d.restartSync(d.client())}
	case "setstreamq":
		s.StreamQuality = quality.Normal(arg)
		if d.saveSettings(s) != nil {
			return q2spot.Fail(q2spot.Error, settingNotSaved)
		}
		return q2spot.Reply{Message: "Streaming Quality: " + quality.Name(s.stream())}
	case "keepfree":
		s.KeepFree = nextKeepFree(s.KeepFree)
		if d.saveSettings(s) != nil {
			return q2spot.Fail(q2spot.Error, settingNotSaved)
		}
		return q2spot.Reply{Message: "Space to Keep Free: " + keepFreeName(s.KeepFree)}
	case "cache":
		s.PlaybackCache = nextCache(s.PlaybackCache)
		if d.saveSettings(s) != nil {
			return q2spot.Fail(q2spot.Error, settingNotSaved)
		}
		d.trimSoon() // a smaller cap applies once the presses stop, never to a size merely passed through
		return q2spot.Reply{Message: "Playback Cache: " + cacheName(s.PlaybackCache)}
	case "clearcache": // two presses in a row, as Q2 Pod's Clear All Downloads asks first
		n := d.stream.cacheSize()
		d.mu.Lock()
		armed := !d.clearArmed.IsZero() && time.Since(d.clearArmed) < clearArmFor
		d.clearArmed = time.Time{}
		if !armed && n > 0 {
			d.clearArmed = time.Now() // only a cache with songs in it asks to be confirmed
		}
		d.mu.Unlock()
		switch {
		case n == 0:
			return q2spot.Reply{Message: "The cache is empty"}
		case !armed:
			return q2spot.Reply{Message: "Press again to remove " + size(n) + " of cached songs"}
		}
		return q2spot.Reply{Message: "Removed " + size(d.stream.clearCache()) + " of cached songs"}
	case "swipe":
		return d.toggleSwipe(s)
	case "reportplays":
		return d.toggleReports(s)
	case "storage": // Quality and Storage: what is synced, streamed and kept on the card
		return q2spot.Reply{Rows: []q2spot.Row{list("defq", "Sync Quality: "+quality.Name(s.def()), "For every synced song", ""),
			list("streamq", "Streaming Quality: "+quality.Name(s.stream()), "For songs streamed", ""),
			{Kind: "action", URI: "keepfree", Title: "Space to Keep Free: " + keepFreeName(s.KeepFree), Subtitle: "A sync stops before the card has less"},
			act("cache", "Playback Cache: "+cacheName(s.PlaybackCache), ""),
			{Kind: "action", URI: "clearcache", Title: "Clear Playback Cache", Subtitle: size(d.stream.cacheSize()) + " of played songs"}}}
	}
	if reconcileSwipe(&s, d.caps.Sections) {
		d.saveSettings(s)
	}
	d.mu.Lock()
	user, shown := d.user, d.sectionsShown
	d.mu.Unlock()
	var syncRows []q2spot.Row // the sync, first
	if shown {                // no root to show a stream's failure: here, first
		syncRows = append(append(d.fileRows(), d.streamErrRows()...), d.favRows()...)
	}
	if cl := d.client(); cl != nil {
		syncRows = append(syncRows, list("sync", "Sync", d.syncSubtitle(cl), ""))
	}
	return q2spot.Reply{Rows: append(syncRows, []q2spot.Row{list("storage", "Quality and Storage", "", ""),
		d.swipeRow(s),
		reportRow(s),
		list("account", "Account", user, ""),
		info("Version " + strings.TrimPrefix(version, "q2-v"))}...)}
}

func (d *daemon) account(cl *jellyfin.Client) q2spot.Reply {
	d.mu.Lock()
	user := d.user
	d.mu.Unlock()
	rows := []q2spot.Row{d.serverRow(), {Kind: "search", Title: "Signed in as:", Subtitle: user}}
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
