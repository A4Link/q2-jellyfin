package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2"
)

// Q2 Pod's "now" (U31): a URL queue whose header holds now=1 (stream_urls.go) is told what plays.
// While it is the queue playing, the firmware keeps /tmp/q2-jellyfin.now, one line
//
//	<seq><TAB><n><TAB><playing|paused|stopped><TAB><sec><TAB><length>
//
// seq the queue's (its .play's), n its song's line, sec how far into the song; written whole and
// only on a change, whether the app runs or not (Q2 Pod's docs/librespot.md). The app reads it at
// each tick, its first one too, and tells the server as Jellyfin's own apps do:
//
//   - another seq or n: the song read before stopped, at the position last read (Stopped), and
//     this one started (Playing, which counts it; a start the server didn't take goes in the
//     outbox, as live reports do: liveStart);
//   - a pause or play: Progress at once, paused or not; another sec: Progress at most every nowEvery;
//     paused, Progress again every nowPaused, or the server drops the session;
//   - the same song from its last seconds back to its first (repeat one): Stopped and Playing
//     again, a play more, as Jellyfin's apps count it; a seek back mid-song is Progress;
//   - stopped: Stopped at sec.
//
// Q2 Pod also writes stopped on Stop Streaming Services, Rockbox and after its own restart (seq, n
// and sec kept): only a song whose start was sent gets a Stopped.
//
// A synced song in the queue is reported by neither, as before: Q2 Pod's log is read for those
// (report.go). The queue's songs by line, the line read last and the song last started are kept
// in /tmp/q2-jellyfin.queue (ids and numbers: no URL, no token; /tmp goes at power-off, as .now
// does), so an app closed and opened again carries on: the song it read last stops if another
// plays now, and the one playing starts. A song played whole while the app was closed is not
// known: the firmware tells only the song playing, and Q2 Pod's log has no streamed song; so the
// app stays up while its queue plays (nowKeeps), and that happens only if it is closed otherwise
// (Stop Streaming Services, signed out, reporting off, or idleAfter paused or stopped). With
// reporting off, or signed out, nothing is sent; a song playing when reporting comes on again is
// reported at the next line. Signing out forgets the queue.

const (
	nowEvery  = 20 * time.Second // the least time between two Progress reports of a position
	nowPaused = 4 * time.Minute  // paused, Progress goes again this often: the server drops a session silent for 5
	nowEdge   = 15               // seconds: the end and start a repeat goes between (repeated)
)

// repeated is whether cur, the same song as prev, is it played again: from its last seconds back
// to its first (repeat one), not a seek back mid-song.
func repeated(prev, cur nowLine) bool {
	return prev.length > 0 && prev.sec >= prev.length-nowEdge && cur.sec < nowEdge &&
		prev.state != "stopped" && cur.state != "stopped"
}

// nowState is what is known of the URL queue Q2 Pod tells of; d.rep.mu guards it.
type nowState struct {
	loaded     bool // the queue file was read, once a run
	q          nowQueue
	progAt     time.Time // the last Progress (or start) sent: when, the position and the pause
	progSec    int
	progPaused bool
	calls      []nowCall // to send, in order
	sending    bool
	playedAt   time.Time // the last line read that said the app's queue plays (nowKeeps)
}

// nowQueue is /tmp/q2-jellyfin.queue.
type nowQueue struct {
	Seq     string   `json:"seq"`
	IDs     []string `json:"ids,omitempty"`     // the queue's songs by line; "" for a synced one
	Last    string   `json:"last,omitempty"`    // the .now line read last
	Started string   `json:"started,omitempty"` // Last's song, once its start is sent
}

type nowLine struct {
	seq, state     string
	n, sec, length int // length 0 while the player doesn't know it
}

// parseNow reads a .now line; false for one that isn't.
func parseNow(s string) (nowLine, bool) {
	f := strings.Split(s, "\t")
	if len(f) < 4 || f[0] == "" || f[2] != "playing" && f[2] != "paused" && f[2] != "stopped" {
		return nowLine{}, false
	}
	n, err := strconv.Atoi(f[1])
	sec, err2 := strconv.Atoi(f[3])
	if err != nil || err2 != nil || n < 0 || sec < 0 {
		return nowLine{}, false
	}
	length := 0
	if len(f) > 4 {
		length, _ = strconv.Atoi(f[4])
	}
	return nowLine{seq: f[0], state: f[2], n: n, sec: sec, length: length}, true
}

// nowCall is a report to send: a start (at when the song began), Progress or Stopped (at sec).
type nowCall struct {
	kind   string // "start", "progress" or "stop"
	id     string
	sec    int
	paused bool
	at     time.Time
}

func (d *daemon) nowFile() string      { return filepath.Join(d.tmp, "q2-jellyfin.now") }
func (d *daemon) nowQueueFile() string { return filepath.Join(d.tmp, "q2-jellyfin.queue") }

// nowLoad is the state, with the queue file read the first time. d.rep.mu is held.
func (d *daemon) nowLoad() *nowState {
	st := &d.rep.now
	if !st.loaded {
		st.loaded = true
		var q nowQueue
		if b, err := os.ReadFile(d.nowQueueFile()); err == nil && json.Unmarshal(b, &q) == nil {
			st.q = q // a damaged one is none
		}
	}
	return st
}

// nowSave writes the queue file whole. d.rep.mu is held.
func (d *daemon) nowSave(q nowQueue) {
	b, _ := json.Marshal(q)
	if err := q2.WriteFile(d.nowQueueFile(), b); err != nil {
		d.log.Printf("reports: %s: %v", filepath.Base(d.nowQueueFile()), err)
	}
}

// nowQueued is the streamer's queued: a URL queue with now=1 is named in .play under seq.
func (d *daemon) nowQueued(seq string, ids []string) {
	d.rep.mu.Lock()
	defer d.rep.mu.Unlock()
	st := d.nowLoad()
	st.q.Seq, st.q.IDs = seq, ids // Last and Started stay: the song before stops at the next line
	d.nowSave(st.q)
}

// nowForget is a sign-out: the queue and the reports not sent are the account's.
func (d *daemon) nowForget() {
	d.rep.mu.Lock()
	defer d.rep.mu.Unlock()
	st := d.nowLoad()
	st.q, st.calls = nowQueue{}, nil
	os.Remove(d.nowQueueFile())
}

// nowTick, from tick, reads .now and queues what changed since the line read last for the server.
func (d *daemon) nowTick(now time.Time) {
	if !d.caps.Now {
		return
	}
	b, err := os.ReadFile(d.nowFile())
	if err != nil {
		return // no URL queue with now=1 has played yet
	}
	line := strings.TrimRight(string(b), "\n")
	cur, ok := parseNow(line)
	if !ok {
		return
	}
	on := d.reportsOn() && d.client() != nil
	d.rep.mu.Lock()
	defer d.rep.mu.Unlock()
	st := d.nowLoad()
	if line == st.q.Last {
		// Paused, nothing is written: Progress again now and then keeps the server's session.
		if on && cur.state == "paused" && st.q.Started != "" && now.Sub(st.progAt) >= nowPaused {
			st.progAt = now
			d.nowQueue(nowCall{kind: "progress", id: st.q.Started, sec: cur.sec, paused: true})
		}
		return
	}
	prev, had := parseNow(st.q.Last)
	id := "" // cur's song; "" for a synced one, or of a queue not known
	if cur.seq == st.q.Seq && cur.n < len(st.q.IDs) {
		id = st.q.IDs[cur.n]
	}
	var calls []nowCall
	stop := func(sec int) {
		if st.q.Started != "" && (!had || prev.state != "stopped") {
			calls = append(calls, nowCall{kind: "stop", id: st.q.Started, sec: sec})
		}
	}
	switch {
	case !had || prev.seq != cur.seq || prev.n != cur.n || repeated(prev, cur): // another song, or again
		stop(prev.sec)
		st.q.Started = ""
	case cur.state == "stopped":
		stop(cur.sec)
	}
	paused := cur.state == "paused"
	switch {
	case cur.state == "stopped":
	case st.q.Started == "": // a song not reported yet starts, here or already playing
		if on && id != "" {
			st.q.Started = id
			calls = append(calls, nowCall{kind: "start", id: id, at: clock().Add(-time.Duration(cur.sec) * time.Second)})
			st.progAt, st.progSec, st.progPaused = now, cur.sec, false
			if paused {
				calls = append(calls, nowCall{kind: "progress", id: id, sec: cur.sec, paused: true})
				st.progPaused = true
			}
		}
	case paused != st.progPaused || prev.state == "stopped" || cur.sec != st.progSec && now.Sub(st.progAt) >= nowEvery:
		calls = append(calls, nowCall{kind: "progress", id: st.q.Started, sec: cur.sec, paused: paused})
		st.progAt, st.progSec, st.progPaused = now, cur.sec, paused
	}
	if cur.state == "playing" && cur.seq == st.q.Seq && on {
		st.playedAt = now
	}
	st.q.Last = line
	d.nowSave(st.q)
	d.nowQueue(calls...)
}

// nowQueue queues calls to send, in order. d.rep.mu is held.
func (d *daemon) nowQueue(calls ...nowCall) {
	st := &d.rep.now
	st.calls = append(st.calls, calls...)
	if !st.sending && len(st.calls) > 0 {
		st.sending = true
		d.spawn(d.nowSend)
	}
}

// nowKeeps is whether the app stays up for its URL queue (idle): a line saying it plays was read
// less than idleAfter ago, with reporting on and signed in, so every song of it is reported. While
// it plays the line changes every 5 s; paused or stopped, nothing is written, so the app closes
// idleAfter after the queue last played, as after a press.
func (d *daemon) nowKeeps(now time.Time) bool {
	if !d.caps.Now || !d.reportsOn() || d.client() == nil {
		return false
	}
	d.rep.mu.Lock()
	defer d.rep.mu.Unlock()
	return !d.rep.now.playedAt.IsZero() && now.Sub(d.rep.now.playedAt) < idleAfter
}

// nowSend sends the reports queued, one at a time in their order, in the background.
func (d *daemon) nowSend() {
	for {
		d.rep.mu.Lock()
		st := &d.rep.now
		if len(st.calls) == 0 {
			st.sending = false
			d.rep.mu.Unlock()
			return
		}
		c := st.calls[0]
		st.calls = st.calls[1:]
		d.rep.mu.Unlock()
		if !d.reportsOn() {
			continue
		}
		if c.kind == "start" {
			d.rep.mu.Lock()
			prev := d.rep.live // a song .playing told of (report.go) has stopped too
			d.rep.live = nil
			if prev != nil {
				prev.stop.Stop()
			}
			d.rep.mu.Unlock()
			if prev != nil {
				d.liveStopped(prev, clock())
			}
			d.liveStart(c.id, c.at, false) // no mark: Q2 Pod's log has no streamed song
			continue
		}
		cl := d.client()
		if cl == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), reportWait)
		pos := time.Duration(c.sec) * time.Second
		if c.kind == "stop" {
			cl.PlaybackStopped(ctx, c.id, pos) // best effort: the client logs a failure
		} else {
			cl.PlaybackProgress(ctx, c.id, pos, c.paused)
		}
		cancel()
	}
}
