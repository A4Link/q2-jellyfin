package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Buffering feedback. A card app can't open or close a page itself, and Now Playing is the
// firmware's, so the feedback lives where the user already is: pressing a song toasts "Buffering…"
// when it has to arrive first, and the list it was pressed in turns live (refresh) until the song
// starts, its row's subtitle saying what the stream does (connecting, buffering with a
// percentage, downloading whole). Once the song plays the list is a plain list again, polled no
// more, and Return from Now Playing comes back to it as it was.

type streamPhase int

const (
	phaseConnecting streamPhase = iota // asked the server, nothing yet
	phaseBuffering                     // arriving; plays at want bytes (early.go)
	phaseWhole                         // arriving; plays once whole (a format that can't start early, or no bitrate)
	phaseReady                         // the player has it: on the card whole, or started early
)

// streamStatus is the first song of the latest Play: what holds it up until it plays.
type streamStatus struct {
	seq              int64 // the run's; -1 a start not yet running
	index            int
	id, title        string
	phase            streamPhase
	got, want, total int64 // bytes arrived, needed to start, expected
	playing          bool  // .playing has named it
	at               time.Time
}

func (s *streamer) setStatus(st streamStatus) {
	s.statMu.Lock()
	s.stat = st
	s.statMu.Unlock()
}

func (s *streamer) status() streamStatus {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return s.stat
}

// fetching records song i's download when it is the run's first: got of total bytes, playing at
// want (0: it can play now, -1: once whole).
func (s *streamer) fetching(seq int64, i int, id, title string, got, want, total int64) {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	st := &s.stat
	// -1: a Play not yet running, whose song is known; a stopped run's late news isn't it
	if (st.seq != seq && (st.seq != -1 || st.id != id)) || (st.seq == seq && st.index != i) || st.playing {
		return
	}
	st.seq, st.index, st.id, st.title = seq, i, id, title
	st.got, st.want, st.total, st.at = got, want, total, time.Now()
	switch {
	case want == 0:
		st.phase = phaseReady
	case want < 0:
		st.phase = phaseWhole
	default:
		st.phase = phaseBuffering
	}
}

// playingNow records that .playing names song i.
func (s *streamer) playingNow(seq int64, i int, id, title string) {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	if st := &s.stat; st.seq == seq || st.seq == -1 && st.id == id {
		st.seq, st.playing, st.phase, st.at = seq, true, phaseReady, time.Now()
		if st.id == "" {
			st.index, st.id, st.title = i, id, title
		}
	}
}

// waitingNow is the latest Play's first song while it waits to play, and whether there is one.
// A stream that stopped (failed, or another list played) waits for nothing.
func (d *daemon) waitingNow() (streamStatus, bool) {
	st := d.stream.status()
	if st.playing || st.phase == phaseReady && time.Since(st.at) > 10*time.Second {
		return st, false // ready this long and never named in .playing: it isn't waited for
	}
	if e := d.stream.Err(); e != nil && !e.skipped && e.at.After(st.at) || !d.stream.active() && st.seq != -1 {
		return st, false
	}
	return st, st.id != ""
}

// phaseText is a waiting song's row subtitle.
func phaseText(st streamStatus) string {
	pct := func(num, den int64) int {
		if den <= 0 {
			return 0
		}
		return int(min(max(num*100/den, 0), 100))
	}
	switch st.phase {
	case phaseBuffering:
		if st.total > 0 && st.want*4 > st.total {
			return fmt.Sprintf("Slow connection · buffering %d %%", pct(st.got, st.want))
		}
		return fmt.Sprintf("Buffering %d %%", pct(st.got, st.want))
	case phaseWhole:
		return fmt.Sprintf("Downloading %d %%", pct(st.got, st.total))
	case phaseReady:
		return "Starting…"
	}
	return "Connecting…"
}

// songListOps are the ops whose replies hold song rows.
var songListOps = map[string]bool{"album": true, "artist": true, "playlist": true, "favorites": true, "search": true}

// pollCache keeps song lists while a song buffers, so the list's refreshes don't ask the server
// again every 2 s.
type pollCache struct {
	mu sync.Mutex
	m  map[string]pollEntry
}

type pollEntry struct {
	at time.Time
	r  q2spot.Reply
}

const pollFor = 30 * time.Second

func (c *pollCache) get(key string) (q2spot.Reply, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > pollFor {
		return q2spot.Reply{}, false
	}
	e.r.Rows = slices.Clone(e.r.Rows)
	return e.r, true
}

func (c *pollCache) put(key string, r q2spot.Reply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 32 {
		c.m = map[string]pollEntry{}
	}
	r.Rows = slices.Clone(r.Rows)
	c.m[key] = pollEntry{time.Now(), r}
}

// songList answers a song list, from the poll cache while a song waits to play, and marks the
// waiting song's row; the list is live (refresh) while it waits, unless it is paged.
func (d *daemon) songList(req q2spot.Request) q2spot.Reply {
	key := req.Op + "\t" + strings.Join(req.Args, "\t")
	st, waiting := d.waitingNow()
	r, ok := q2spot.Reply{}, false
	if waiting {
		r, ok = d.polls.get(key)
	}
	if !ok {
		r = d.reply(req)
		if r.Status == "" || r.Status == "ok" {
			d.polls.put(key, r)
		}
	}
	if !waiting || r.Cursor != "" || (r.Message != "" && r.Message != "refresh") {
		return r
	}
	d.mu.Lock()
	playKey := d.playKey
	d.mu.Unlock()
	marked := false
	for i := range r.Rows {
		row := &r.Rows[i]
		f := strings.Fields(row.Context)
		if row.URI == "play" && len(f) == 3 && f[0] == "song:"+st.id && f[1] == playKey {
			row.Subtitle = phaseText(st)
			marked = true
		}
	}
	if marked {
		r.Message = "refresh"
	}
	return r
}
