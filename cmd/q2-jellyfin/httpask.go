package main

import (
	"errors"
	"strconv"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// The question before plain http (U54): a server checked to be Jellyfin over plain http, that the
// user hasn't said Yes to, or on a Wi-Fi not yet kept for it (review 2026-11-20 #6), is asked
// about first: it is served over HTTP, what that risks, Yes or No. Both are page rows (Q2 Pod's
// window word): Q2 Pod asks for the page again with "allow:<n>" or "deny:<n>" last, its answer in
// the page's place. The level keeps that argument for every later request (Back, a refresh), so
// each question has its own n, and only its own page's answer counts, once.

var errAskHTTP = errors.New("ask before plain http")

// asked is the question waiting for its answer: n 0 when none waits.
type asked struct {
	n       int
	server  string
	signOut bool // signed in on another server: Yes signs out first, as a new address does
	same    bool // the server signed in on: Yes keeps this Wi-Fi as a home too, signed in still
	newWiFi bool // a server said Yes to before, on a Wi-Fi not kept for it
}

// askHTTP keeps the question for server and returns errAskHTTP, for the caller to show askPage.
func (d *daemon) askHTTP(server string, signOut, same bool) error {
	newWiFi := d.allowed(server)
	d.login.mu.Lock()
	d.login.askN++
	d.login.ask = asked{n: d.login.askN, server: server, signOut: signOut, same: same, newWiFi: newWiFi}
	d.login.mu.Unlock()
	return errAskHTTP
}

// askPage is the waiting question.
func (d *daemon) askPage() q2spot.Reply {
	d.login.mu.Lock()
	a := d.login.ask
	d.login.mu.Unlock()
	addr := hostOf(a.server)
	if ap, ok := addrPort(a.server); ok {
		addr = ap.String()
	}
	n := strconv.Itoa(a.n)
	yes := q2spot.Row{Kind: "page", URI: "allow", Context: "allow:" + n, Title: "Yes, allow HTTP", Subtitle: "Adds it to " + allowFile}
	no := q2spot.Row{Kind: "page", URI: "deny", Context: "deny:" + n, Title: "No"}
	// Both questions' Yes keeps the Wi-Fi the Q2 is on as one the sign-in may be sent on.
	home := q2spot.Row{Kind: "info", Title: "Say Yes only at home", Subtitle: "Elsewhere a device could pose as your server"}
	if a.newWiFi {
		yes.Title, yes.Subtitle = "Yes, this is my home Wi-Fi", "Its sign-in is sent on this Wi-Fi too"
		return q2spot.Reply{Rows: []q2spot.Row{info("This Wi-Fi isn't saved for " + addr), home, yes, no}}
	}
	risk := q2spot.Row{Kind: "info", Title: "Not encrypted", Subtitle: "Others on this network can read your password and sign-in"}
	if onInternet(a.server) {
		risk = q2spot.Row{Kind: "info", Title: "On the internet, NOT encrypted", Subtitle: "Anyone on the way can read your password and sign-in"}
	}
	return q2spot.Reply{Rows: []q2spot.Row{info(addr + " is served over HTTP"), risk, home, yes, no}}
}

// answerHTTP is a page asked again with its question's answer last: Yes adds the server to the
// allow list (and this Wi-Fi to its networks) and carries on as the typed address would have; No
// keeps nothing. ok is false for any other request, or an answer that isn't the waiting
// question's (an earlier page's, kept by its level): that page is then answered as usual.
func (d *daemon) answerHTTP(req q2spot.Request) (q2spot.Reply, bool) {
	if len(req.Args) == 0 || req.Op != "menu" && req.Op != "server" && req.Op != "search" {
		return q2spot.Reply{}, false
	}
	verb, n, _ := strings.Cut(req.Args[len(req.Args)-1], ":")
	d.login.mu.Lock()
	a := d.login.ask
	match := a.n != 0 && n == strconv.Itoa(a.n) && (verb == "allow" || verb == "deny")
	if match {
		d.login.ask = asked{}
	}
	d.login.mu.Unlock()
	if !match {
		return q2spot.Reply{}, false
	}
	if verb == "deny" { // nothing kept: a field row's page as it stands (Return there may leave the app)
		switch req.Op {
		case "menu":
			return d.menu(), true
		case "server":
			return d.serverPage(), true
		}
		return q2spot.Reply{Rows: []q2spot.Row{info("Not used: " + hostOf(a.server)), info("Press Return to type another address")}}, true
	}
	if d.freshNetwork().Gateway == "" { // first: nothing written, nobody signed out (addHome refuses it)
		d.login.mu.Lock()
		d.login.ask = a // Retry asks with the same allow:n: Yes again, once the network reads
		d.login.mu.Unlock()
		return q2spot.Fail(q2spot.Error, errNetworkUnsure.Error()), true
	}
	if err := d.allowServer(a.server); err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't write "+allowFile), true
	}
	if a.same {
		if err := d.addHome(a.server); err != nil {
			return q2spot.Fail(q2spot.Error, err.Error()), true
		}
		return q2spot.Reply{Rows: []q2spot.Row{info("Saved for this Wi-Fi: " + hostOf(a.server)),
			info("Press Return to go back")}}, true
	}
	if a.signOut && d.client() != nil && d.signOut() != nil {
		return q2spot.Fail(q2spot.Error, signOutFailed), true
	}
	if err := d.setServer(a.server); err != nil {
		return q2spot.Fail(q2spot.Error, err.Error()), true
	}
	r := q2spot.Reply{Rows: d.signedOutRows()}
	if req.Op != "search" { // a search level asked again would be the address typed again
		r.Message = "refresh"
	}
	return r, true
}
