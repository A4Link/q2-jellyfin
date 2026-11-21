package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
)

// urlSrc is fakeSrc with player URLs: the relay's (relay.go).
type urlSrc struct{ *fakeSrc }

func (urlSrc) PlayerURL(id, q string) string   { return playerURL(id, q) }
func (urlSrc) PlayerCoverURL(id string) string { return playerCoverURL(id) }

// With "urls", Play writes the Q2URLS/1 queue and names it in .play; nothing downloads; a song on
// the card stays its card path; tabs in titles become spaces.
func TestURLQueue(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
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
	if want := "http://127.0.0.1:2317/song/s1?q=original\tTwo Words\t\t\thttp://127.0.0.1:2317/cover/al9\t61"; lines[2] != want {
		t.Errorf("song 1:\n%q\n%q", lines[2], want)
	}
	_ = jellyfin.Song{}
}

// U37: under an Opus Streaming Quality a lossy song is queued as it is, a lossless one transcoded;
// Always Re-encode transcodes both.
func TestURLQueueKeepsLossy(t *testing.T) {
	for _, re := range []bool{false, true} {
		e := newEnv(t, fw.Caps{URLs: true})
		e.s.src = urlSrc{e.src}
		e.s.quality = func() string { return "opus-128" }
		e.s.reencode = func() bool { return re }
		ss := songs(2, "flac")
		ss[1].Suffix, ss[1].Codec = "m4a", "aac"
		e.s.start(ss, 0)
		play, _ := os.ReadFile(e.play)
		b, err := os.ReadFile(strings.Fields(string(play))[1])
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		want := []string{"opus-128", "original"}
		if re {
			want[1] = "opus-128"
		}
		for i, w := range want {
			if got := strings.Split(lines[i+1], "\t")[0]; got != "http://127.0.0.1:2317/song/s"+strconv.Itoa(i)+"?q="+w {
				t.Errorf("Always Re-encode %v, song %d: %q, want %s", re, i, got, w)
			}
		}
	}
}

// U30: with the firmware's "fav", every line carries the song's item and heart (a card path's
// too): the server's favourite, unless a press not sent yet, or taken in this run, says otherwise.
// Without it, six fields as before.
func TestURLQueueHearts(t *testing.T) {
	for _, c := range []struct {
		caps fw.Caps
		want []string
	}{
		{fw.Caps{URLs: true, Fav: true}, []string{"\t0\ts0\t1", "\t0\ts1\t0", "\t0\ts2\t1"}},
		{fw.Caps{URLs: true}, []string{"\t\t0", "\t\t0", "\t\t0"}},
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
	e := newEnv(t, fw.Caps{URLs: true})
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
	e := newEnv(t, fw.Caps{URLs: true})
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
	e := newEnv(t, fw.Caps{URLs: true})
	e.s.src = urlSrc{e.src}
	e.s.start(songs(300, "flac"), 250)
	lines := e.urlsFile(t)
	if lines[0] != "#Q2URLS/1\tstart=100" || len(lines) != 151 {
		t.Fatalf("%q, %d lines", lines[0], len(lines))
	}
	for i, id := range map[int]string{1: "s150", 101: "s250", 150: "s299"} {
		if !strings.HasPrefix(lines[i], "http://127.0.0.1:2317/song/"+id+"?") {
			t.Errorf("line %d %q, want %s", i, lines[i], id)
		}
	}
}

// Lines too long for 500 of them in urlsMax: the song started and those after it are kept first,
// then those before it as room allows, and the lyrics' <n> is each song's line (lyricsAhead of
// them from the song started, back to the first line past the last).
func TestURLQueueFitsFirmware(t *testing.T) {
	for _, c := range []struct {
		n, from, text int // text: bytes of title, artist and album together (a field is cut at 1,000)
		start, lo     int // start= and the first song written
	}{
		{600, 150, 600, 0, 150},   // the ones after fill it: none before
		{600, 590, 3000, 75, 515}, // ten after, then as many before as fit
	} {
		e := newEnv(t, fw.Caps{URLs: true})
		e.s.src = lyricSrc{urlSrc{e.src}}
		ss := songs(c.n, "flac")
		for i := range ss {
			f := strings.Repeat("x", c.text/3)
			ss[i].Title, ss[i].Artist, ss[i].Album, ss[i].CoverArt, ss[i].HasLyrics = f, f, f, "c", true
		}
		e.s.start(ss, c.from)
		lines := e.urlsFile(t)
		if want := fmt.Sprintf("#Q2URLS/1\tstart=%d", c.start); lines[0] != want {
			t.Fatalf("%q, want %q", lines[0], want)
		}
		if !strings.HasPrefix(lines[1], fmt.Sprintf("http://127.0.0.1:2317/song/s%d?", c.lo)) ||
			!strings.HasPrefix(lines[1+c.start], fmt.Sprintf("http://127.0.0.1:2317/song/s%d?", c.from)) {
			t.Fatalf("first %.30q, started %.30q", lines[1], lines[1+c.start])
		}
		last := len(lines) - 2
		dir := filepath.Join(filepath.Dir(e.play), "q2-jellyfin.lyrics")
		lrc := func(n int) string { b, _ := os.ReadFile(filepath.Join(dir, fmt.Sprint(n, ".lrc"))); return string(b) }
		if got := lrc(c.start); got != fmt.Sprint("[00:01.00]s", c.from) {
			t.Fatalf("started song's lyrics %q", got)
		}
		// line files only: each song's lyrics are also kept as id-<song>.lrc for the next Play
		eventually(t, "the window's lyrics", func() bool { return lyricLines(dir) == lyricsAhead })
		end := (c.start + lyricsAhead - 1) % (last + 1)
		if got := lrc(end); got != fmt.Sprint("[00:01.00]s", c.lo+end) {
			t.Errorf("window's last line's lyrics %q", got)
		}
	}
}

// A field over 1,000 bytes is cut on a character, so the started song's line always fits in
// urlsMax: a 300 KB title from the server plays instead of crashing Play.
func TestURLQueueLongFieldCut(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
	e.s.src = lyricSrc{urlSrc{e.src}}
	ss := songs(2, "flac")
	ss[0].Title, ss[0].HasLyrics = strings.Repeat("€", 100_000), true
	e.s.start(ss, 0)
	if play, _ := os.ReadFile(e.play); !strings.HasSuffix(strings.TrimSpace(string(play)), "q2-jellyfin.urls") {
		t.Fatalf(".play %q", play)
	}
	lines := e.urlsFile(t)
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	if got := strings.Split(lines[1], "\t")[1]; got != strings.Repeat("€", 333) {
		t.Errorf("title of %d bytes", len(got))
	}
}

// A control character in a field, a NUL from the server's JSON ("\u0000") above all, is a space:
// the firmware stops reading the queue at a NUL, and would play another song than the one pressed.
func TestURLQueueControlChars(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
	e.s.src = urlSrc{e.src}
	ss := songs(3, "flac")
	ss[0].Title, ss[0].Artist = "Intro\x00(live)", "A\x1fB\x7f"
	e.s.start(ss, 2)
	lines := e.urlsFile(t)
	if len(lines) != 4 || strings.ContainsAny(strings.Join(lines, ""), "\x00\x1f\x7f") {
		t.Fatalf("%q", lines)
	}
	if f := strings.Split(lines[1], "\t"); f[1] != "Intro (live)" || f[2] != "A B" {
		t.Errorf("song 0: %q", f)
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
// the next Play keeps 100; the prefetch stops after two unreachable errors.
func TestURLQueueLyricsCapAndUnreachableStop(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
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

	h := lyricSongs("h", 150)
	for from := 0; from < len(h); from += lyricsAhead { // all 150 kept, a window a Play
		e.s.start(h, from)
		eventually(t, "the window's lines", func() bool { return lyricLines(dir) == lyricsAhead })
	}
	e.s.start(lyricSongs("x", 3), 0)
	eventually(t, "the next list's lines", func() bool { return lyricLines(dir) == 3 })
	if kept, _ := filepath.Glob(filepath.Join(dir, "id-*.lrc")); len(kept) != lyricsKeep+3 {
		t.Errorf("%d songs' lyrics kept, want %d and the list's 3", len(kept), lyricsKeep)
	}

	down := newEnv(t, fw.Caps{URLs: true})
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
		e := newEnv(t, fw.Caps{URLs: true, Now: now})
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

// A Play asks the server for the lyrics of 20 lines from the song started on, not of the whole
// queue (up to 500 songs, one request after another).
func TestURLQueueLyricsWindow(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
	src := &gatedLyrics{urlSrc: urlSrc{e.src}}
	e.s.src = src
	dir := lyricsDirOf(e)
	e.s.start(lyricSongs("s", 60), 30)
	eventually(t, "the window's lyrics", func() bool { return lyricLines(dir) == 20 })
	neverWithin(t, "lyrics past the window", 300*time.Millisecond, func() bool { return lyricLines(dir) > 20 })
	if got := src.askedNow(); len(got) != 20 || got[0] != "s30" || got[19] != "s49" {
		t.Errorf("asked %q, want s30 to s49", got)
	}
}

// Pressed near the end of a list, the window wraps round to the queue's start, the songs that
// play next under repeat-all.
func TestURLQueueLyricsWindowWraps(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
	src := &gatedLyrics{urlSrc: urlSrc{e.src}}
	e.s.src = src
	dir := lyricsDirOf(e)
	e.s.start(lyricSongs("s", 30), 29)
	eventually(t, "the window's lyrics", func() bool { return lyricLines(dir) == 20 })
	if got := src.askedNow(); len(got) != 20 || got[0] != "s29" || got[1] != "s0" || got[19] != "s18" {
		t.Errorf("asked %q, want s29 then s0 to s18", got)
	}
}

// Past lyricsKeep songs' lyrics kept, the next Play removes the oldest of them down to lyricsKeep,
// not all of them: the rest aren't asked of the server again. The last queue's lines go.
func TestKeptLyricsTrimOldestFirst(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for i := 0; i < lyricsKeep+5; i++ {
		p := keptLyrics(dir, fmt.Sprint("s", i))
		os.WriteFile(p, []byte("x"), 0o644)
		at := old.Add(time.Duration(i) * time.Second) // s0 the oldest
		os.Chtimes(p, at, at)
	}
	os.WriteFile(filepath.Join(dir, "3.lrc"), []byte("x"), 0o644)
	clearLyricLines(dir)
	if kept, _ := filepath.Glob(filepath.Join(dir, "id-*.lrc")); len(kept) != lyricsKeep {
		t.Fatalf("%d kept, want %d", len(kept), lyricsKeep)
	}
	for i := 0; i < 6; i++ {
		if got := exists(keptLyrics(dir, fmt.Sprint("s", i))); got != (i == 5) {
			t.Errorf("s%d kept %v", i, got)
		}
	}
	if exists(filepath.Join(dir, "3.lrc")) {
		t.Error("the last queue's line left")
	}
}

// The first song's lyrics come before .play names the queue, as Q2 Pod reads a song's .lrc once,
// as it loads; a server slow to give them holds Play back lyricsWait at most.
func TestURLQueueFirstLyricsBounded(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
	e.s.src = &gatedLyrics{urlSrc: urlSrc{e.src}, gates: map[string]chan struct{}{"s0": make(chan struct{})}}
	t0 := time.Now()
	e.s.start(lyricSongs("s", 3), 0)
	if d := time.Since(t0); d > lyricsWait+time.Second {
		t.Errorf("Play took %v", d)
	}
	if !exists(e.play) {
		t.Error("no .play")
	}
}

// syncingLyrics is urlSrc whose lyrics request lets a song's sync finish meanwhile.
type syncingLyrics struct {
	urlSrc
	done func()
}

func (s syncingLyrics) Lyrics(context.Context, string) (string, error) { s.done(); return "", nil }

// A song whose sync finishes while its queue is written (here, as its lyrics are fetched) is
// reported as the line it got: streamed, by the app; on the card, by Q2 Pod's log; never neither
// or both.
func TestURLQueueSyncedMeanwhile(t *testing.T) {
	for _, now := range []bool{false, true} {
		e := newEnv(t, fw.Caps{URLs: true, Now: now})
		path := e.root + "/Jellyfin/s1.flac"
		e.man.Songs["s1"] = &library.Entry{ID: "s1", Path: path} // being synced: no file yet
		e.s.src = syncingLyrics{urlSrc{e.src}, func() {
			os.MkdirAll(e.root+"/Jellyfin", 0o755)
			os.WriteFile(path, []byte("x"), 0o644)
		}}
		var ids, started []string
		e.s.queued = func(_ string, i []string) { ids = i }
		e.s.started = func(sg jellyfin.Song) { started = append(started, sg.ID) }
		ss := songs(2, "flac")
		ss[1].HasLyrics = true
		e.s.start(ss, 1)
		if now && !slices.Equal(ids, []string{"s0", "s1"}) || !now && !slices.Equal(started, []string{"s1"}) {
			t.Errorf("now %v: ids %q, started %q", now, ids, started)
		}
	}
}
