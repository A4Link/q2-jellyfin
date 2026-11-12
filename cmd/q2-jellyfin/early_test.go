package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// A 100 s FLAC of 1,000,000 bytes (10,000 bytes a second).
func TestEarlyReady(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	sg := jellyfin.Song{Duration: 100, Size: 1_000_000}
	newE := func() *earlyStart {
		clk.t = time.Unix(1000, 0)
		return newEarly(sg, "original", "flac", 1_000_000, clk.now)
	}
	// feed reports ready after the download's bytes arrive evenly at bps for sec seconds
	feed := func(e *earlyStart, bps float64, sec float64) (bool, int64) {
		var got int64
		for ms := 100; ms <= int(sec*1000); ms += 100 {
			clk.t = time.Unix(1000, 0).Add(time.Duration(ms) * time.Millisecond)
			got = int64(bps * float64(ms) / 1000)
			e.note(got)
		}
		return e.ready(got), got
	}

	// a link 3× the song's rate starts at the 3 s buffer, once 2 s are measured
	if ok, _ := feed(newE(), 30_000, 1); ok {
		t.Error("before 2 s of measure")
	}
	if ok, got := feed(newE(), 30_000, 2); !ok || got < 30_000 {
		t.Errorf("fast link: should start at the buffer (%d)", got)
	}
	// a link just faster than the song: counted at 60 % (7,200 B/s) it would fall behind, so it
	// waits for 1,000,000 − 0.6·12,000·100 = 280,000 bytes
	if ok, _ := feed(newE(), 12_000, 20); ok {
		t.Error("1.2×: 240,000 is short of 280,000")
	}
	if ok, _ := feed(newE(), 12_000, 24); !ok {
		t.Error("1.2×: 288,000 should start")
	}
	// a link slower than the song (4,000 B/s): got + 0.6·4000·100 ≥ 1,000,000 needs 760,000
	if ok, _ := feed(newE(), 4_000, 30); ok {
		t.Error("slow: the player would overtake the download")
	}
	if ok, _ := feed(newE(), 4_000, 191); !ok {
		t.Error("slow, enough in: should start")
	}
	// a link that was fast and slowed down: the recent rate counts
	e := newE()
	feed(e, 30_000, 1) // 30,000 in the first second
	for ms := 1100; ms <= 6000; ms += 100 {
		clk.t = time.Unix(1000, 0).Add(time.Duration(ms) * time.Millisecond)
		e.note(30_000 + int64(ms-1000)) // then 1,000 B/s
	}
	if e.ready(35_000) {
		t.Error("slowed to 1,000 B/s: must not start on the early average")
	}

	if newEarly(jellyfin.Song{}, "original", "flac", 1000, clk.now) != nil {
		t.Error("no duration or size: no early start")
	}
	if newEarly(jellyfin.Song{Duration: 100, Size: 1_200_000}, "opus-96", "opus", 1_200_000, clk.now) != nil {
		t.Error("a transcode plays once whole")
	}
	for _, ext := range []string{"mp3", "m4a", "ogg", "opus"} {
		if newEarly(sg, "original", ext, 1_000_000, clk.now) != nil {
			t.Errorf("%s plays once whole", ext)
		}
	}
}

// heldBody trickles head bytes, 40 KB every 100 ms as a real download arrives, then waits for
// release before the rest.
type heldBody struct {
	head, rest []byte
	release    chan struct{}
}

func (b *heldBody) Read(p []byte) (int, error) {
	if len(b.head) > 0 {
		time.Sleep(100 * time.Millisecond)
		n := copy(p[:min(len(p), 40_000)], b.head)
		b.head = b.head[n:]
		return n, nil
	}
	if b.release != nil {
		<-b.release
		b.release = nil
	}
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	return n, nil
}

// A streamed song plays (its file appears under its final name, and the folder is announced)
// while the rest of it is still on its way; the file then grows to the whole song.
func TestStreamPlaysEarly(t *testing.T) {
	for _, caps := range []fw.Caps{{}, {Playlist: true}} {
		e := newEnv(t, caps)
		release := make(chan struct{})
		var once sync.Once
		free := func() { once.Do(func() { close(release) }) }
		t.Cleanup(free) // a failure never leaves the download (and so the test) waiting
		e.src.body = func(u string) (io.Reader, int64) {
			return &heldBody{head: make([]byte, 900_000), rest: make([]byte, 100_000), release: release}, 1_000_000
		}
		ss := songs(1, "flac")
		ss[0].Duration, ss[0].Size = 100, 1_000_000
		e.s.start(ss, 0)
		deadline := time.Now().Add(5 * time.Second)
		for !exists(e.stream(0, "flac")) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		fi, err := os.Stat(e.stream(0, "flac"))
		if err != nil || fi.Size() >= 1_000_000 {
			t.Fatalf("%+v: not playing early: %v", caps, err)
		}
		if !caps.Playlist { // announced just after the rename, while the rest still downloads
			until(t, "the folder announced at the early start", func() bool {
				b, _ := os.ReadFile(e.play)
				return strings.Contains(string(b), ".jellyfin/stream")
			})
		}
		free()
		until(t, "the whole song", func() bool { fi, err := os.Stat(e.stream(0, "flac")); return err == nil && fi.Size() == 1_000_000 })
		if exists(e.stream(0, "flac") + ".part") {
			t.Fatal(".part left")
		}
	}
}

// lyrSrc is a stream source whose songs have lyrics.
type lyrSrc struct{ *fakeSrc }

func (l lyrSrc) Lyrics(_ context.Context, id string) (string, error) {
	return "[00:01.00]Words of " + id + "\n", nil
}

// A streamed song with lyrics gets its .lrc beside it before it plays, and keeps it in the cache.
func TestStreamLyrics(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.src = lyrSrc{e.src}
	e.s.cacheLimit = func() int64 { return 1 << 30 }
	ss := songs(4, "flac")
	for i := range ss {
		ss[i].HasLyrics = i == 0
	}
	e.s.start(ss, 0)
	until(t, "s0", func() bool { return exists(e.stream(0, "flac")) })
	lrc := strings.TrimSuffix(e.stream(0, "flac"), ".flac") + ".lrc"
	if b, _ := os.ReadFile(lrc); string(b) != "[00:01.00]Words of s0\n" {
		t.Fatalf("lyrics %q", b)
	}
	if exists(strings.TrimSuffix(e.stream(1, "flac"), ".flac") + ".lrc") {
		t.Fatal("lyrics for a song without")
	}
	e.s.start(songs(1, "mp3"), 0) // a new list: s0 goes to the cache, its lyrics with it
	until(t, "cached", func() bool { return exists(e.cached("s0", "original", "flac")) })
	if !exists(strings.TrimSuffix(e.cached("s0", "original", "flac"), ".flac") + ".lrc") {
		t.Fatal("lyrics not cached with the song")
	}
}
