package main

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
)

// fakeSrc is a server with nothing to give: the streamer asks its source only for lyrics.
type fakeSrc struct{}

type env struct {
	root, play string
	src        *fakeSrc
	man        *library.Manifest
	s          *streamer
}

func newEnv(t *testing.T, caps fw.Caps) *env {
	t.Helper()
	e := &env{root: t.TempDir(), src: &fakeSrc{}, man: &library.Manifest{Songs: map[string]*library.Entry{}}}
	e.play = t.TempDir() + "/play"
	e.s = newStreamer(e.play, caps, e.src, func() *library.Manifest { return e.man })
	t.Cleanup(e.s.stop)
	return e
}

func songs(n int, suffix string) []jellyfin.Song {
	var out []jellyfin.Song
	for i := 0; i < n; i++ {
		out = append(out, jellyfin.Song{ID: fmt.Sprint("s", i), Title: fmt.Sprint("T", i), Suffix: suffix})
	}
	return out
}

// Play reads Streaming Quality (settings.json) once, not once a song.
func TestStreamReadsQualityOnce(t *testing.T) {
	e := newEnv(t, fw.Caps{URLs: true})
	e.s.src = urlSrc{e.src}
	var n atomic.Int32
	e.s.quality = func() string { n.Add(1); return "mp3-320" }
	e.s.start(songs(5, "flac"), 0)
	if n.Load() != 1 {
		t.Errorf("read %d times", n.Load())
	}
}
