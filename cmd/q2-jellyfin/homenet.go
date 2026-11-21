package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
)

// A plain http server (an address on the allow list, httpallow.go) is used with a secret, the
// sign-in's token or a password, only on a network it was set up on: the Wi-Fi's name and the
// gateway's MAC kept in networkFile when its address was kept (setServer), up to maxHomes of them
// (typing the address again on another Wi-Fi at home adds it, after a warning). A Q2 taken to
// another network whose 192.168.x address is some other device's sends that device nothing.
// Checking a server sends no secret, so it works anywhere. Nothing kept, or a file that can't be
// read, sends nothing: the check fails closed.

const networkFile = "network"

// maxHomes is how many networks a server keeps: a home's main Wi-Fi, its 5 GHz or extender's name.
const maxHomes = 4

// homeNetCache is how long a read of the network stands: a page's requests run together.
const homeNetCache = 2 * time.Second

// homes is networkFile: the plain http server they belong to and its networks.
type homes struct {
	Server   string       `json:"server"`
	Networks []q2.Network `json:"networks"`
}

// errNetworkUnsure is a network that can't be told yet: a part kept for the server (its gateway's
// MAC, the Wi-Fi's name) can't be read now, and every part read matches; or, keeping an address,
// the gateway's MAC can't be read. Nothing is sent; on a list it reads as a server that can't be
// reached, so nothing advises typing the address again.
var errNetworkUnsure = errors.New("Can't check this Wi-Fi yet. Try again in a moment")

// homeOnly is hc, or for a plain http server's client with a secret, hc checking every request's
// network first (onHomeNetwork).
func (d *daemon) homeOnly(c jellyfin.Creds, hc *http.Client) *http.Client {
	if !plainHTTP(c.Server) || c.Token == "" && c.Password == "" {
		return hc
	}
	h := *hc
	server := c.Server
	h.Transport = homeGate{base: orDefault(hc.Transport), ok: func() error { return d.onHomeNetwork(server) }}
	return &h
}

func orDefault(t http.RoundTripper) http.RoundTripper {
	if t == nil {
		return http.DefaultTransport
	}
	return t
}

// homeGate sends a request only once ok says the network is the server's.
type homeGate struct {
	base http.RoundTripper
	ok   func() error
}

func (g homeGate) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := g.ok(); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	return g.base.RoundTrip(req)
}

// onHomeNetwork is nil when the network now is one kept for server, errNetworkUnsure when that
// can't be told yet, or jellyfin.ErrOtherNetwork. A refusal is logged once, until one is allowed.
func (d *daemon) onHomeNetwork(server string) error {
	if a := d.allowList(); !a.has(server) { // taken off the allow list on a computer since it was said Yes to
		d.logRefusal(func() { d.log.Printf("plain http: nothing sent: the server isn't on %s", allowFile) })
		if a.off() && a.skipped {
			return jellyfin.ErrAllowUnread
		}
		return jellyfin.ErrNotAllowed
	}
	now := d.network()
	h, err := d.loadHomes()
	if err == nil && h.Server != shownServer(server) {
		err = errors.New("kept for another server")
	}
	verdict := jellyfin.ErrOtherNetwork
	if err == nil {
		for _, n := range h.Networks {
			if n.Same(now) {
				d.logRefusal(nil)
				return nil
			}
			if n.Unsure(now) {
				verdict = errNetworkUnsure
			}
		}
	}
	d.logRefusal(func() {
		d.log.Printf("plain http: nothing sent: not a network kept for the server (Wi-Fi name read: %v, gateway read: %v, kept: %v)",
			now.SSID != "", now.Gateway != "", err == nil)
	})
	return verdict
}

// logRefusal runs logf on the first refusal after an allowed request (nil: allowed), so a page
// of refused covers writes one line, not one each.
func (d *daemon) logRefusal(logf func()) {
	d.netMu.Lock()
	first := logf != nil && !d.netRefused
	d.netRefused = logf != nil
	d.netMu.Unlock()
	if first {
		logf()
	}
}

func (d *daemon) loadHomes() (homes, error) {
	var h homes
	b, err := os.ReadFile(filepath.Join(d.dir, networkFile))
	if err == nil {
		err = json.Unmarshal(b, &h)
	}
	return h, err
}

// knownHere reports whether the network now is one kept for server.
func (d *daemon) knownHere(server string) bool {
	h, err := d.loadHomes()
	if err != nil || h.Server != shownServer(server) {
		return false
	}
	now := d.network()
	for _, n := range h.Networks {
		if n.Same(now) {
			return true
		}
	}
	return false
}

// addHome keeps the network now for plain http server: the first of a new server's, or one more
// of the same server's (the oldest dropped past maxHomes). An https server keeps none.
func (d *daemon) addHome(server string) error {
	p := filepath.Join(d.dir, networkFile)
	if !plainHTTP(server) {
		os.Remove(p)
		return nil
	}
	now := d.freshNetwork()
	if now.Gateway == "" { // a Wi-Fi name alone is anyone's to copy (setServer refuses it first)
		return errNetworkUnsure
	}
	h, err := d.loadHomes()
	if err != nil || h.Server != shownServer(server) {
		h = homes{Server: shownServer(server)}
	}
	for _, n := range h.Networks {
		if n.Same(now) {
			return nil
		}
	}
	h.Networks = append(h.Networks, now)
	if len(h.Networks) > maxHomes {
		h.Networks = h.Networks[len(h.Networks)-maxHomes:]
	}
	b, _ := json.Marshal(h)
	return q2.WriteFile(p, b)
}

// freshNetwork is the network now, read now rather than from homeNetCache.
func (d *daemon) freshNetwork() q2.Network {
	d.netMu.Lock()
	d.netAt = time.Time{}
	d.netMu.Unlock()
	return d.network()
}

// network is the network now (d.netNow), read again at most every homeNetCache.
// shortcut: for up to homeNetCache after a switch to another network the old read stands; joining
// one (association, DHCP) takes longer than that.
func (d *daemon) network() q2.Network {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if time.Since(d.netAt) > homeNetCache {
		d.net, d.netAt = d.netNow(), time.Now()
	}
	return d.net
}
