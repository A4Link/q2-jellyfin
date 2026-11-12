package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Now Playing's ⋯ (q2app's more=npqueue, Q2 Pod's card-app-more) is the play queue: the songs the
// latest Play handed the firmware, the one playing marked; pressing one plays the
// same queue from it, as a Play does. Each start keeps its queue in /tmp/q2-jellyfin.playq: a URL
// queue's lines (up to 100 songs before the one pressed and 400 from it on), or in a folder or
// playlist the songs from the one pressed on, as the firmware has them. It holds the songs as the
// server listed them, no URL and no token, so the page needs no server, and an app closed and
// opened again still shows it; /tmp goes at power-off, and Sign Out removes it.
//
// The page opens at the song playing, the songs after it following, then an "Earlier songs" row
// and the songs before it, all in the queue's order. The song playing is the one pressed, "Started
// here", until Q2 Pod's .now (its "now") names a line of this queue, "Now Playing" (now.go); a
// folder or playlist, and firmware without "now", get no .now line for it.

// playQueue is /tmp/q2-jellyfin.playq.
type playQueue struct {
	Seq   string          `json:"seq"`   // the .play's, which .now names
	Start int             `json:"start"` // the song pressed
	Songs []jellyfin.Song `json:"songs"`
}

// queueMax is the most songs a folder or playlist's queue keeps: a URL queue's most.
const queueMax = urlsBefore + urlsAfter

func (s *streamer) queueFile() string {
	return filepath.Join(filepath.Dir(s.playFile), "q2-jellyfin.playq")
}

// saveQueue keeps the queue a start hands the firmware, before its .play.
func (s *streamer) saveQueue(seq string, songs []jellyfin.Song, start int) {
	b, _ := json.Marshal(playQueue{Seq: seq, Start: start, Songs: songs})
	if err := q2.WriteFile(s.queueFile(), b); err != nil {
		s.logf("stream: play queue: %v", err)
	}
	s.queueGen.Add(1) // after the write: a page read before it decodes it again
}

// queueCache is the queue file as last decoded, and the file's time and size and the saves made then.
type queueCache struct {
	mu   sync.Mutex
	gen  int64
	mod  time.Time
	size int64
	q    playQueue
	ok   bool
}

// loadQueue is the queue kept; false for none (or a damaged one). The file is decoded again only
// when this app saved it since, or its time or size changed (another run's): a page of 500 songs
// takes several replies. A file's time alone can't tell: it moves with the kernel's coarse clock,
// so two saves of the same size within a tick look the same.
func (d *daemon) loadQueue() (playQueue, bool) {
	c := &d.queue
	c.mu.Lock()
	defer c.mu.Unlock()
	gen := d.stream.queueGen.Load()
	fi, err := os.Stat(d.stream.queueFile())
	if err != nil {
		c.mod, c.q, c.ok = time.Time{}, playQueue{}, false
		return playQueue{}, false
	}
	if gen == c.gen && fi.ModTime().Equal(c.mod) && fi.Size() == c.size {
		return c.q, c.ok
	}
	var q playQueue
	b, err := os.ReadFile(d.stream.queueFile())
	ok := err == nil && json.Unmarshal(b, &q) == nil && q.Start >= 0 && q.Start < len(q.Songs)
	if !ok {
		q = playQueue{}
	}
	c.gen, c.mod, c.size, c.q, c.ok = gen, fi.ModTime(), fi.Size(), q, ok
	return q, ok
}

// queuePage is ⋯'s page from cursor on, opening at the song playing: it and the songs after it,
// then "Earlier songs" and the ones before it. A song's row has its Context "<seq> <index>".
func (d *daemon) queuePage(cursor string) q2spot.Reply {
	q, ok := d.loadQueue()
	if !ok {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing from Jellyfin is playing")}}
	}
	at, mark := q.Start, "Started here"
	if d.caps.Now {
		if b, err := os.ReadFile(d.nowFile()); err == nil {
			if l, ok := parseNow(strings.TrimRight(string(b), "\n")); ok && l.seq == q.Seq && l.n < len(q.Songs) {
				at, mark = l.n, "Now Playing"
			}
		}
	}
	row := func(i int) q2spot.Row {
		s := q.Songs[i]
		sub := s.Artist
		if i == at {
			sub = join(mark, s.Artist)
		}
		return q2spot.Row{Kind: "action", URI: "npplay", Title: s.Title, Subtitle: sub, Context: q.Seq + " " + strconv.Itoa(i)}
	}
	rows := make([]q2spot.Row, 0, len(q.Songs)+1)
	for i := at; i < len(q.Songs); i++ {
		rows = append(rows, row(i))
	}
	if at > 0 {
		rows = append(rows, info("Earlier songs"))
	}
	for i := range at {
		rows = append(rows, row(i))
	}
	return d.window(page(rows, cursor), cursor)
}

// queuePlay is a row's press: the queue kept, played from that song.
func (d *daemon) queuePlay(arg string) q2spot.Reply {
	q, ok := d.loadQueue()
	seq, n, _ := strings.Cut(arg, " ")
	i, err := strconv.Atoi(n)
	if !ok || seq != q.Seq || err != nil || i < 0 || i >= len(q.Songs) {
		return q2spot.Fail(q2spot.Unavailable, "The queue has changed")
	}
	d.startSongs(q.Songs, i, "queue")
	return d.playing(q.Songs[i].Title)
}
