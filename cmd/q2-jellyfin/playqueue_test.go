package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// queueSeq is the seq the latest Play named in .play, once the start has run.
func (d *tdaemon) queueSeq(t *testing.T) string {
	t.Helper()
	d.bg.Wait() // the start runs behind the reply
	b, err := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.play"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))[0]
}

// subtitles is the rows' subtitles in order.
func subtitles(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Subtitle)
	}
	return out
}

// U28: Now Playing's ⋯ (q2app's more=npqueue) is the queue the firmware plays, opening at the song
// playing: it and the songs after it, then the earlier songs under an "Earlier songs" row, in the
// queue's order. The song is the one pressed, "Started here", until Q2 Pod's .now names a line of
// this queue ("now"), which is "Now Playing"; .now is not read without "now".
func TestPlayQueuePage(t *testing.T) {
	for _, now := range []bool{false, true} {
		caps := map[bool]string{false: "urls", true: "urls now"}[now]
		d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, caps, true)
		d.do("play", "album:al0 1")
		seq := d.queueSeq(t)
		pressed := func(what string) {
			t.Helper()
			r := d.do("npqueue")
			if got := strings.Join(titles(r), "|"); got != "Song 1|Song 2|Earlier songs|Song 0" || r.Rows[2].Kind != "info" {
				t.Fatalf("%s %s: rows %q", caps, what, got)
			}
			if got := subtitles(r); !reflect.DeepEqual(got, []string{"Started here · Artist", "Artist", "", "Artist"}) {
				t.Fatalf("%s %s: subtitles %q", caps, what, got)
			}
		}
		pressed("before .now")
		os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.now"), []byte(seq+"\t2\tplaying\t5\t100\n"), 0o644)
		if now {
			r := d.do("npqueue")
			if got := strings.Join(titles(r), "|"); got != "Song 2|Earlier songs|Song 0|Song 1" {
				t.Fatalf("after .now: rows %q", got)
			}
			if got := subtitles(r); !reflect.DeepEqual(got, []string{"Now Playing · Artist", "", "Artist", "Artist"}) {
				t.Fatalf("after .now: subtitles %q", got)
			}
		} else {
			pressed("with a .now")
		}
		// another queue's line (one played before) marks the song pressed
		os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.now"), []byte("1\t0\tplaying\t5\t100\n"), 0o644)
		pressed("another queue's .now")
	}
}

// A URL queue holds 100 songs before the one pressed and 400 from it on: the page lists them all
// over as many replies as they take, from the one pressed, the earlier ones last.
func TestPlayQueueLong(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 520}, "urls now", true)
	d.do("play", "album:al0 150")
	d.queueSeq(t)
	rows := walkWindow(t, d, "npqueue", "")
	if len(rows) != 471 || rows[370].Title != "Earlier songs" || rows[0].Subtitle != "Started here · Artist" {
		t.Fatalf("%d rows", len(rows))
	}
	for i, r := range rows {
		want := 150 + i // Song 150 … Song 519, then Song 50 … Song 149
		if i > 370 {
			want = 50 + i - 371
		}
		if i != 370 && (r.Title != "Song "+strconv.Itoa(want) || r.Context != strings.Fields(r.Context)[0]+" "+strconv.Itoa(want-50)) {
			t.Fatalf("row %d: %+v", i, r)
		}
	}
}

// Pressing a row, an earlier song's too, plays the same queue from that song, as a Play does: a
// new queue (the reports see a new seq, so no song counts twice), the page now opening at it.
// Without "now" the song pressed is counted once, as a Play's is.
func TestPlayQueuePress(t *testing.T) {
	for _, now := range []bool{false, true} {
		caps := map[bool]string{false: "urls", true: "urls now"}[now]
		n := &nav{albums: 1, songsPer: 3}
		d := newTestDaemon(t, n, caps, true)
		d.do("play", "album:al0 1")
		seq := d.queueSeq(t)
		row, _ := find(d.do("npqueue"), "Song 0") // an earlier song
		if r := d.do(row.URI, row.Context); r.Status != "" || r.Message != "" {
			t.Fatalf("%s: press %+v", caps, r)
		}
		if d.queueSeq(t) == seq {
			t.Fatalf("%s: no new queue", caps)
		}
		urls := d.urlLines(t)
		if urls[0] != "#Q2URLS/1\tstart=0"+map[bool]string{false: "", true: "\tnow=1"}[now] || len(urls) != 4 ||
			!strings.Contains(urls[1], "\tSong 0\t") || !strings.Contains(urls[3], "\tSong 2\t") {
			t.Fatalf("%s: queue %q", caps, urls)
		}
		r := d.do("npqueue")
		if got := strings.Join(titles(r), "|"); got != "Song 0|Song 1|Song 2" || r.Rows[0].Subtitle != "Started here · Artist" {
			t.Fatalf("%s: after the press %q %q", caps, got, subtitles(r))
		}
		var starts []string
		for _, s := range n.reported() {
			if strings.HasPrefix(s, "start ") {
				starts = append(starts, s)
			}
		}
		want := []string{"start al0.1", "start al0.0"}
		if now {
			want = nil // the .now line says when each plays (now.go)
		}
		if !reflect.DeepEqual(starts, want) {
			t.Fatalf("%s: reported %q", caps, starts)
		}
	}
}

// A row of a queue replaced since the page was drawn, or past the queue's end, plays nothing.
func TestPlayQueueStalePress(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "urls now", true)
	d.do("play", "album:al0 1")
	old := d.do("npqueue").Rows[0]
	d.do("play", "album:al0 2")
	seq := d.queueSeq(t)
	d.mu.Lock()
	ps := d.playSeq
	d.mu.Unlock()
	for _, ctx := range []string{old.Context, seq + " 3", seq + " -1", seq + " x", seq} {
		r := d.do("npplay", ctx)
		d.bg.Wait()
		d.mu.Lock()
		after := d.playSeq
		d.mu.Unlock()
		if r.Status != q2spot.Unavailable || r.Message != "The queue has changed" || after != ps || d.queueSeq(t) != seq {
			t.Fatalf("%q: %+v, playSeq %d → %d", ctx, r, ps, after)
		}
	}
}

// The queue is decoded again only when its file changes: a new Play's queue shows at once.
func TestPlayQueueNewSave(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "urls now", true)
	d.do("play", "album:al0 0")
	d.queueSeq(t)
	d.do("npqueue")
	d.do("play", "album:al1 1")
	seq := d.queueSeq(t)
	r := d.do("npqueue")
	if got := strings.Join(titles(r), "|"); got != "Song 1|Earlier songs|Song 0" || !strings.HasPrefix(r.Rows[0].Context, seq+" ") {
		t.Fatalf("rows %q, %q", got, r.Rows[0].Context)
	}
}

// urlLines is the URL queue file's lines.
func (d *tdaemon) urlLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.urls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// Nothing of Jellyfin's queued (the app's first run since power-on, or signed out since): one
// plain row. An app closed and opened again still has the queue, from /tmp; the file holds no
// URL and no token.
func TestPlayQueueEmptyAndRestart(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "urls now", true)
	r := d.do("npqueue")
	if len(r.Rows) != 1 || r.Rows[0].Kind != "info" || r.Rows[0].Title != "Nothing from Jellyfin is playing" || r.Status != "" {
		t.Fatalf("empty %+v", r)
	}
	d.do("play", "album:al0 0")
	d.queueSeq(t)
	b, err := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.playq"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "tok") || strings.Contains(s, "http") || strings.Contains(s, "ApiKey") {
		t.Fatalf("the queue file holds %s", s)
	}
	d2 := newTestDaemon(t, n, "urls now", true)
	os.WriteFile(filepath.Join(d2.tmp, "q2-jellyfin.playq"), b, 0o644)
	if got := strings.Join(titles(d2.do("npqueue")), "|"); got != "Song 0|Song 1" {
		t.Fatalf("after a restart %q", got)
	}
	d.do("signout")
	d.bg.Wait()
	if got := titles(d.do("npqueue")); !reflect.DeepEqual(got, []string{"Nothing from Jellyfin is playing"}) {
		t.Fatalf("signed out %q", got)
	}
}

// The page is the same with Swipe Between Sections on and off.
func TestPlayQueueSwipeParity(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "urls now sections window", true)
	d.do("play", "album:al0 1")
	d.queueSeq(t)
	if !d.swipeOn() {
		t.Fatal("swipe is off") // the premise
	}
	on := d.do("npqueue")
	d.do("swipe")
	if d.swipeOn() {
		t.Fatal("swipe stayed on")
	}
	if off := d.do("npqueue"); !reflect.DeepEqual(on, off) || len(on.Rows) != 4 {
		t.Fatalf("swipe on %+v, off %+v", on, off)
	}
}

// Streamed to a playlist (firmware without "urls"), the firmware's queue is the songs from the
// one pressed on: those are the page, and a row pressed plays them from it.
func TestPlayQueuePlaylist(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 4}, "playlist", true)
	d.do("play", "album:al0 1")
	seq := d.queueSeq(t)
	r := d.do("npqueue")
	if got := strings.Join(titles(r), "|"); got != "Song 1|Song 2|Song 3" || r.Rows[0].Subtitle != "Started here · Artist" {
		t.Fatalf("rows %q %q", got, subtitles(r))
	}
	d.do(r.Rows[1].URI, r.Rows[1].Context)
	if d.queueSeq(t) == seq {
		t.Fatal("no new queue")
	}
	if got := strings.Join(titles(d.do("npqueue")), "|"); got != "Song 2|Song 3" {
		t.Fatalf("after the press %q", got)
	}
}
