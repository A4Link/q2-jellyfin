package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// The relay. Q2 Pod's player (hciplayer, through ffmpeg) and its cover fetch (curl) open a URL
// queue's songs and covers themselves, with no headers and no certificate check (Q2 Pod's
// patch/tidal.c). So the queue (stream_urls.go) names the app instead, on the Q2's loopback, and
// the app fetches each from the server as it fetches everything else: the token in the
// Authorization header, over a connection whose certificate is checked (jellyfin.Client.RoundTrip).
// The queue holds no token, and nothing on the Wi-Fi can reach the relay.
//
//	GET or HEAD http://127.0.0.1:2317/song/<id>?q=<quality>   the song, as PlayURL streams it
//	GET or HEAD http://127.0.0.1:2317/cover/<id>              its cover, as a list row's (320 px)
//
// The URLs hold no state: each request takes the client signed in then, so an app opened again
// serves a queue an earlier run wrote. A queue playing keeps the app up by the data it moves: each
// request and each read of the server's answer counts as a press (relayAt, idle), so a song still
// streaming keeps it up, and one paused or stalled lets it exit idleAfter after its last byte.
// shortcut: nothing more keeps it up, as staying up would hold the app in RAM while paused; so a
// queue paused for idleAfter, or resumed after the app exited, fails to play until Jellyfin is
// opened again. A Q2 Pod hook is planned that starts
// the app when one of its songs starts and it isn't running.
// TODO(maintainer): if that hook needs the app to name its relay (a q2app key, a caps word), add
// it here; no such contract exists yet.

// relayAddr is where the relay listens: the loopback only, never 0.0.0.0, on a fixed port so a
// queue's URLs stay good from one run of the app to the next.
const relayAddr = "127.0.0.1:2317"

// playerURL is song id at quality q for the firmware's player: the relay's URL for it.
func playerURL(id, q string) string {
	return "http://" + relayAddr + "/song/" + url.PathEscape(id) + "?q=" + url.QueryEscape(q)
}

// playerCoverURL is id's cover for Now Playing, through the relay.
func playerCoverURL(id string) string {
	return "http://" + relayAddr + "/cover/" + url.PathEscape(id)
}

// startRelay serves the relay on addr in the background. A port already taken (another copy of
// the app) is logged and the app runs on without it: its queues still name the relay, never the
// server with the token, and their songs play once the port is the app's again.
func (d *daemon) startRelay(addr string) {
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		d.log.Printf("relay: %v", err)
		return
	}
	d.relay = &http.Server{Handler: http.HandlerFunc(d.serveRelay), ReadHeaderTimeout: 10 * time.Second}
	go d.relay.Serve(ln)
}

// serveRelay answers the firmware: 404 for anything but a song id with a quality the app knows, or
// a cover id; 503 signed out. Of the player's headers only Range goes on, for seeking.
func (d *daemon) serveRelay(w http.ResponseWriter, r *http.Request) {
	kind, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query().Get("q")
	song := kind == "song" && quality.Normal(q) == q
	if r.Method != "GET" && r.Method != "HEAD" || !itemID(id) || !song && kind != "cover" {
		http.NotFound(w, r)
		return
	}
	cl := d.client()
	if cl == nil {
		http.Error(w, "signed out", http.StatusServiceUnavailable)
		return
	}
	d.relayAt.Store(int64(time.Since(relayEpoch))) // a press (idle)
	up := cl.ThumbURL(id, 320)
	if song {
		up = cl.PlayURL(id, q)
	}
	u, _ := url.Parse(up) // the client's own URL: it parses
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL, pr.Out.Host = u, ""
			pr.Out.Header = http.Header{}
			if v := pr.In.Header.Get("Range"); v != "" {
				pr.Out.Header.Set("Range", v)
			}
		},
		Transport: cl, // logs an answer that failed and sets Settings' status
		ModifyResponse: func(resp *http.Response) error {
			b := &relayBody{ReadCloser: resp.Body, at: &d.relayAt}
			if !song { // a cover, at most artMax as a list row's (art.go)
				if resp.ContentLength > artMax {
					return errBigCover
				}
				b.max = artMax
			}
			resp.Body = b
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil { // not the player hanging up (a seek, the next song)
				d.log.Printf("relay %s %s: %v", kind, id, err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

var errBigCover = errors.New("cover over 1 MB")

// relayEpoch is what relayAt counts from, on the monotonic clock: the app may set the wall clock.
var relayEpoch = time.Now()

// relayBody is a relayed answer whose reads each count as a press (idle). A cover's fails once
// over max bytes, so the player's connection is dropped rather than given a cut image.
type relayBody struct {
	io.ReadCloser
	n, max int64 // bytes read; a cover's limit, 0 for a song
	at     *atomic.Int64
}

func (b *relayBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.at.Store(int64(time.Since(relayEpoch)))
	}
	if b.n += int64(n); b.max > 0 && b.n > b.max {
		return 0, errBigCover
	}
	return n, err
}

// itemID is whether id can be a Jellyfin item id, hex digits and dashes (a GUID), so that nothing
// else reaches the server's path.
func itemID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdefABCDEF-", c) {
			return false
		}
	}
	return true
}
