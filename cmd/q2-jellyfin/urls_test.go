package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
)

// urlSrc is fakeSrc with player URLs.
type urlSrc struct{ *fakeSrc }

func (urlSrc) PlayerURL(id, q string) string {
	return "https://srv/Audio/" + id + "/" + q + "?ApiKey=k"
}
func (urlSrc) PlayerCoverURL(id string) string { return "https://srv/Items/" + id + "/cover?ApiKey=k" }

// With "urls", Play writes the Q2URLS/1 queue and names it in .play; nothing downloads; a song on
// the card stays its card path; tabs in titles become spaces.
func TestURLQueue(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	e.s.src = urlSrc{e.src}
	ss := songs(3, "flac")
	ss[1].Title, ss[1].CoverArt, ss[1].Duration = "Two\tWords", "al9", 61
	e.man.Songs["s0"] = &library.Entry{ID: "s0", Path: e.root + "/Jellyfin/s0.flac"}
	os.MkdirAll(e.root+"/Jellyfin", 0o755)
	os.WriteFile(e.root+"/Jellyfin/s0.flac", []byte("x"), 0o644)
	e.s.start(ss, 1)
	play, _ := os.ReadFile(e.play)
	f := strings.Fields(string(play))
	if len(f) != 2 || !strings.HasSuffix(f[1], "q2-jellyfin.urls") {
		t.Fatalf(".play %q", play)
	}
	b, err := os.ReadFile(f[1])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if lines[0] != "#Q2URLS/1\tstart=1" || len(lines) != 4 {
		t.Fatalf("%q", lines)
	}
	if got := strings.Split(lines[1], "\t")[0]; got != e.root+"/Jellyfin/s0.flac" {
		t.Errorf("synced song: %q", got)
	}
	if want := "https://srv/Audio/s1/original?ApiKey=k\tTwo Words\t\t\thttps://srv/Items/al9/cover?ApiKey=k\t61"; lines[2] != want {
		t.Errorf("song 1:\n%q\n%q", lines[2], want)
	}
	if len(e.src.urls) != 0 {
		t.Errorf("downloaded %q", e.src.urls)
	}
	if e.s.active() {
		t.Error("a fetch run for a URL queue")
	}
	_ = jellyfin.Song{}
}

// U30: with the firmware's "fav", every line carries the song's item and heart (a card path's
// too): the server's favourite, unless a press not sent yet, or taken in this run, says otherwise.
// Without it, six fields as before.
func TestURLQueueHearts(t *testing.T) {
	for _, c := range []struct {
		caps fw.Caps
		want []string
	}{
		{fw.Caps{Playlist: true, URLs: true, Fav: true}, []string{"\t0\ts0\t1", "\t0\ts1\t0", "\t0\ts2\t1"}},
		{fw.Caps{Playlist: true, URLs: true}, []string{"\t\t0", "\t\t0", "\t\t0"}},
	} {
		e := newEnv(t, c.caps)
		e.s.src = urlSrc{e.src}
		e.s.hearts = func() map[string]bool { return map[string]bool{"s1": false, "s2": true} }
		ss := songs(3, "flac")
		ss[0].Favorite, ss[1].Favorite = true, true
		e.man.Songs["s0"] = &library.Entry{ID: "s0", Path: e.root + "/Jellyfin/s0.flac"}
		os.MkdirAll(e.root+"/Jellyfin", 0o755)
		os.WriteFile(e.root+"/Jellyfin/s0.flac", []byte("x"), 0o644)
		e.s.start(ss, 0)
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(e.play), "q2-jellyfin.urls"))
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")[1:]
		if len(lines) != 3 || !strings.HasPrefix(lines[0], e.root+"/Jellyfin/s0.flac\t") {
			t.Fatalf("%q", b)
		}
		for i, l := range lines {
			if n := strings.Count(l, "\t"); !strings.HasSuffix(l, c.want[i]) || c.caps.Fav != (n == 7) || n != 5 && n != 7 {
				t.Errorf("fav %v: line %d %q, want …%q", c.caps.Fav, i, l, c.want[i])
			}
		}
	}
}

// lyricSrc is urlSrc with lyrics for every song.
type lyricSrc struct{ urlSrc }

func (lyricSrc) Lyrics(_ context.Context, id string) (string, error) { return "[00:01.00]" + id, nil }

// A URL queue's songs with server lyrics get <line>.lrc beside the queue for Q2 Pod's Now Playing:
// the first song's before .play names the queue, the rest after; a new queue starts afresh.
func TestURLQueueLyrics(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	e.s.src = lyricSrc{urlSrc{e.src}}
	ss := songs(3, "flac")
	ss[0].HasLyrics, ss[2].HasLyrics = true, true
	e.s.start(ss, 2)
	dir := filepath.Join(filepath.Dir(e.play), "q2-jellyfin.lyrics")
	if b, err := os.ReadFile(filepath.Join(dir, "2.lrc")); err != nil || string(b) != "[00:01.00]s2" {
		t.Fatalf("first song's %q %v", b, err)
	}
	eventually(t, "the other lyrics", func() bool { _, err := os.Stat(filepath.Join(dir, "0.lrc")); return err == nil })
	if _, err := os.Stat(filepath.Join(dir, "1.lrc")); err == nil {
		t.Error("lyrics for a song without any")
	}
	ss[2].HasLyrics = false
	e.s.start(ss, 1)
	if _, err := os.Stat(filepath.Join(dir, "2.lrc")); err == nil {
		t.Error("the last queue's lyrics left")
	}
}

// Stopping the stream (Sign Out, the app's exit) removes the URL queue: each of its lines carries
// the token, and the firmware has read it already.
func TestURLQueueRemovedOnStop(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	e.s.src = urlSrc{e.src}
	e.s.start(songs(2, "flac"), 0)
	if _, err := os.Stat(e.s.urlsFile()); err != nil {
		t.Fatal("no queue written:", err)
	}
	e.s.stop()
	if _, err := os.Stat(e.s.urlsFile()); !os.IsNotExist(err) {
		t.Fatal("queue left after stop:", err)
	}
}

// urlsFile is the URL queue e's .play names, a line a string, the header first.
func (e *env) urlsFile(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(e.play), "q2-jellyfin.urls"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > urlsMax {
		t.Fatalf("queue of %d bytes: Q2 Pod reads %d", len(b), urlsMax)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// A queue started partway into a long list keeps urlsBefore songs before the one started, and
// start= is its line.
func TestURLQueueStartsPartway(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	e.s.src = urlSrc{e.src}
	e.s.start(songs(300, "flac"), 250)
	lines := e.urlsFile(t)
	if lines[0] != "#Q2URLS/1\tstart=100" || len(lines) != 151 {
		t.Fatalf("%q, %d lines", lines[0], len(lines))
	}
	for i, id := range map[int]string{1: "s150", 101: "s250", 150: "s299"} {
		if !strings.HasPrefix(lines[i], "https://srv/Audio/"+id+"/") {
			t.Errorf("line %d %q, want %s", i, lines[i], id)
		}
	}
}

// Lines too long for 500 of them in urlsMax: the song started and those after it are kept first,
// then those before it as room allows, and the lyrics' <n> is each song's line.
func TestURLQueueFitsFirmware(t *testing.T) {
	for _, c := range []struct {
		n, from, title int
		start, lo      int // start= and the first song written
	}{
		{600, 150, 600, 0, 150},   // the ones after fill it: none before
		{600, 590, 3000, 75, 515}, // ten after, then as many before as fit
	} {
		e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
		e.s.src = lyricSrc{urlSrc{e.src}}
		ss := songs(c.n, "flac")
		for i := range ss {
			ss[i].Title, ss[i].CoverArt, ss[i].HasLyrics = strings.Repeat("x", c.title), "c", true
		}
		e.s.start(ss, c.from)
		lines := e.urlsFile(t)
		if want := fmt.Sprintf("#Q2URLS/1\tstart=%d", c.start); lines[0] != want {
			t.Fatalf("%q, want %q", lines[0], want)
		}
		if !strings.HasPrefix(lines[1], fmt.Sprintf("https://srv/Audio/s%d/", c.lo)) ||
			!strings.HasPrefix(lines[1+c.start], fmt.Sprintf("https://srv/Audio/s%d/", c.from)) {
			t.Fatalf("first %.30q, started %.30q", lines[1], lines[1+c.start])
		}
		last := len(lines) - 2
		dir := filepath.Join(filepath.Dir(e.play), "q2-jellyfin.lyrics")
		lrc := func(n int) string { b, _ := os.ReadFile(filepath.Join(dir, fmt.Sprint(n, ".lrc"))); return string(b) }
		if got := lrc(c.start); got != fmt.Sprint("[00:01.00]s", c.from) {
			t.Fatalf("started song's lyrics %q", got)
		}
		// line files only: each song's lyrics are also kept as id-<song>.lrc for the next Play
		eventually(t, "every line's lyrics", func() bool { m, _ := filepath.Glob(dir + "/[0-9]*.lrc"); return len(m) == last+1 })
		if got := lrc(last); got != fmt.Sprint("[00:01.00]s", c.lo+last) {
			t.Errorf("last line's lyrics %q", got)
		}
	}
}

// gatedLyrics is a URL-queue source that records its lyrics requests in order; a song with a gate
// waits for it (or its request's time limit), and fail, when set, is every answer's error.
type gatedLyrics struct {
	urlSrc
	mu    sync.Mutex
	asked []string
	gates map[string]chan struct{}
	fail  error
}

func (g *gatedLyrics) Lyrics(ctx context.Context, id string) (string, error) {
	g.mu.Lock()
	g.asked = append(g.asked, id)
	gate, fail := g.gates[id], g.fail
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail != nil {
		return "", fail
	}
	return "[00:01.00]" + id, nil
}

func (g *gatedLyrics) askedNow() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

// lyricSongs is n songs <prefix>0.. that all have lyrics on the server.
func lyricSongs(prefix string, n int) []jellyfin.Song {
	var out []jellyfin.Song
	for i := 0; i < n; i++ {
		out = append(out, jellyfin.Song{ID: fmt.Sprint(prefix, i), Title: fmt.Sprint("T", i), Suffix: "flac", HasLyrics: true})
	}
	return out
}

func lyricsDirOf(e *env) string { return filepath.Join(filepath.Dir(e.play), "q2-jellyfin.lyrics") }

// lyricLines counts a queue's line files (<n>.lrc) in dir.
func lyricLines(dir string) int { m, _ := filepath.Glob(dir + "/[0-9]*.lrc"); return len(m) }

// neverWithin fails if cond turns true within d, looking every 10 ms as eventually does.
func neverWithin(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			t.Fatalf("%s happened", what)
		}
	}
}

// A second Play of the same songs asks each song's lyrics once in all; past 100 songs' lyrics kept,
// the next Play keeps no more than 100; the prefetch stops after two unreachable errors.
func TestURLQueueLyricsCapAndUnreachableStop(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	src := &gatedLyrics{urlSrc: urlSrc{e.src}}
	e.s.src = src
	dir := lyricsDirOf(e)
	five := lyricSongs("s", 5)
	e.s.start(five, 0)
	eventually(t, "every line's lyrics", func() bool { return lyricLines(dir) == 5 })
	e.s.start(five, 2)
	eventually(t, "every line's lyrics again", func() bool { return lyricLines(dir) == 5 })
	if got := src.askedNow(); len(got) != 5 {
		t.Errorf("asked %q over two Plays, want each of the five once", got)
	}

	e.s.start(lyricSongs("h", 150), 0)
	eventually(t, "the 150 lines", func() bool { return lyricLines(dir) == 150 })
	e.s.start(lyricSongs("x", 3), 0)
	eventually(t, "the next list's lines", func() bool { return lyricLines(dir) == 3 })
	if kept, _ := filepath.Glob(filepath.Join(dir, "id-*.lrc")); len(kept) > lyricsKeep {
		t.Errorf("%d songs' lyrics kept, want at most %d", len(kept), lyricsKeep)
	}

	down := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	unreachable := &gatedLyrics{urlSrc: urlSrc{down.src}, fail: fmt.Errorf("lyrics: %w", jellyfin.ErrUnreachable)}
	down.s.src = unreachable
	down.s.start(lyricSongs("u", 10), 0)
	eventually(t, "the second lyrics request", func() bool { return len(unreachable.askedNow()) >= 2 })
	neverWithin(t, "a third request after two unreachable errors", time.Second, func() bool { return len(unreachable.askedNow()) > 2 })
}

// With "now", the queue's header asks Q2 Pod to say which song plays (now=1), and the reporter is
// handed the queue's seq (the .play's) and its songs by line, "" for a synced one, which Q2 Pod's
// log has; the song pressed is no longer reported at once: the .now line says when it plays
// (now.go). Without "now" the header has no field and the song pressed is reported.
func TestURLQueueNow(t *testing.T) {
	for _, now := range []bool{false, true} {
		e := newEnv(t, fw.Caps{Playlist: true, URLs: true, Now: now})
		e.s.src = urlSrc{e.src}
		var seq string
		var ids, started []string
		e.s.queued = func(s string, i []string) { seq, ids = s, i }
		e.s.started = func(sg jellyfin.Song) { started = append(started, sg.ID) }
		e.man.Songs["s0"] = &library.Entry{ID: "s0", Path: e.root + "/Jellyfin/s0.flac"}
		os.MkdirAll(e.root+"/Jellyfin", 0o755)
		os.WriteFile(e.root+"/Jellyfin/s0.flac", []byte("x"), 0o644)
		e.s.start(songs(3, "flac"), 1)
		play, _ := os.ReadFile(e.play)
		f := strings.Fields(string(play))
		b, _ := os.ReadFile(f[1])
		header, _, _ := strings.Cut(string(b), "\n")
		if now {
			if header != "#Q2URLS/1\tstart=1\tnow=1" || seq != f[0] || !slices.Equal(ids, []string{"", "s1", "s2"}) || len(started) != 0 {
				t.Errorf("now: header %q, seq %q of %q, ids %q, started %q", header, seq, f[0], ids, started)
			}
		} else if header != "#Q2URLS/1\tstart=1" || seq != "" || !slices.Equal(started, []string{"s1"}) {
			t.Errorf("no now: header %q, seq %q, started %q", header, seq, started)
		}
	}
}
