package main

import (
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Without Wi-Fi a page that needs the server is live and says so: "Connecting to Wi-Fi…" while
// Q2 Pod's Smart Wi-Fi may still bring the radio up, then "Network Disconnected". An action toasts
// it. Settings, built from the card, still opens, and the root keeps a way to it. With Wi-Fi back
// the page is the page again.
func TestNoWiFi(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.localIP = func() string { return "" }
	if r := d.do("albums"); r.Message != "refresh" || strings.Join(titles(r), "|") != "Connecting to Wi-Fi…" {
		t.Fatalf("%q %q", r.Message, titles(r))
	}
	d.mu.Lock()
	d.noWiFiAt = d.noWiFiAt.Add(-wifiWait)
	d.mu.Unlock()
	if r := d.do("albums"); r.Message != "refresh" || strings.Join(titles(r), "|") != "Network Disconnected|No Wi-Fi network in reach" {
		t.Fatalf("%q %q", r.Message, titles(r))
	}
	if r := d.do("menu"); r.Message != "refresh" || strings.Join(titles(r), "|") != "Network Disconnected|No Wi-Fi network in reach|Settings" {
		t.Fatalf("root %q %q", r.Message, titles(r))
	}
	if r := d.do("toggle", "album:al0"); r.Status != q2spot.Offline || r.Message != "Network Disconnected" {
		t.Fatalf("toggle %+v", r)
	}
	if row, ok := find(d.do("settings"), "Sync"); !ok || row.Subtitle != "Not connected · No Wi-Fi" {
		t.Fatalf("settings %+v", row)
	}
	if _, ok := find(d.do("account"), "Sign Out"); !ok {
		t.Fatal("account")
	}

	d.localIP = func() string { return "192.168.1.20" }
	if _, ok := find(d.do("albums"), "Album 000"); !ok {
		t.Fatal("albums with Wi-Fi back")
	}
	d.localIP = func() string { return "" } // gone again: Connecting… again
	if r := d.do("albums"); r.Rows[0].Title != "Connecting to Wi-Fi…" {
		t.Fatalf("%q", titles(r))
	}
}

// With the user's Wi-Fi switch off, opening Jellyfin says "Network Disconnected" at once: nothing
// is coming to wait for. Turned on again, it waits as above.
func TestWiFiSwitchedOff(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.localIP = func() string { return "" }
	off := true
	d.wifiOff = func() bool { return off }
	if r := d.do("menu"); r.Message != "refresh" || strings.Join(titles(r), "|") != "Network Disconnected|Turn on Wi-Fi in Settings|Settings" {
		t.Fatalf("root %q %q", r.Message, titles(r))
	}
	if r := d.do("play", "album:al0 0"); r.Status != q2spot.Offline || r.Message != "Network Disconnected" {
		t.Fatalf("play %+v", r)
	}
	d.mu.Lock()
	d.noWiFiAt = time.Now().Add(-time.Hour) // as if a request had found no Wi-Fi long ago
	d.mu.Unlock()
	d.do("menu")
	off = false // switched on: the radio is coming, and the wait counts from now
	if r := d.do("albums"); r.Rows[0].Title != "Connecting to Wi-Fi…" {
		t.Fatalf("%q", titles(r))
	}
}

// Signed out, the steps that ask the server wait for Wi-Fi too; the root and typing a user name don't.
func TestNoWiFiSignIn(t *testing.T) {
	d, _ := loginDaemon(t)
	d.localIP = func() string { return "" }
	if _, ok := find(d.do("menu"), "Sign In with Password"); !ok {
		t.Fatal("root")
	}
	if r := d.do("qc"); r.Status != q2spot.Offline || r.Message != "Connecting to Wi-Fi…" {
		t.Fatalf("qc %+v", r)
	}
	d.do("pwlogin")
	if r := d.do("search", "u"); r.Rows[len(r.Rows)-1].Title != "Type Your Password" {
		t.Fatalf("user %q", titles(r))
	}
	// A failed page, titled with its message: a live one would be titled with the password.
	if r := d.do("search", typedPassword); r.Status != q2spot.Offline || r.Message != "Connecting to Wi-Fi…" {
		t.Fatalf("password %+v", r)
	}
	d.localIP = func() string { return "192.168.1.20" }
	loginSuccessPage(t, d.do("search", typedPassword)) // its Retry

}
