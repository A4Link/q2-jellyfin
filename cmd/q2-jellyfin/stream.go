package main

import (
	"os"
	"sync"
	"sync/atomic"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// streamer plays a list through Q2 Pod's own player: it hands the firmware a queue of the relay's
// URLs, synced songs as card paths, through .play (stream_urls.go). Nothing is fetched to the card.
type streamer struct {
	playFile string
	caps     fw.Caps
	src      any // its Lyrics, when it is a syncer.LyricsSource
	manifest func() *library.Manifest
	log      *applog.Log          // optional
	quality  func() string        // Streaming Quality: a transcode (Opus) applies to every streamed song
	reencode func() bool          // Always Re-encode: a lossy song is transcoded too (library.Quality)
	moving   func(id string) bool // a sync running or paused will move or remove this song
	started  func(jellyfin.Song)  // a song streamed from the server starts playing (report.go)
	// queued is a URL queue named in .play with now=1 (Q2 Pod's "now", now.go): its seq, and its
	// songs' ids by line, "" for one played from the card.
	queued func(seq string, ids []string)
	// hearts are the hearts pressed that the songs queued may not have yet (fav.go).
	hearts func() map[string]bool

	lyricsGen atomic.Int64 // the URL queue whose lyrics are being fetched (stream_urls.go)
	queueGen  atomic.Int64 // play queues saved (playqueue.go)

	mu sync.Mutex // one start or stop at a time
}

// newStreamer's hooks do nothing until the daemon sets them: Original quality, no re-encode, no
// sync moving songs, no reports or hearts.
func newStreamer(play string, caps fw.Caps, src any, man func() *library.Manifest) *streamer {
	return &streamer{playFile: play, caps: caps, src: src, manifest: man,
		quality: func() string { return quality.Original }, reencode: func() bool { return false },
		moving: func(string) bool { return false }, started: func(jellyfin.Song) {}, queued: func(string, []string) {},
		hearts: func() map[string]bool { return nil }}
}

func (s *streamer) logf(format string, a ...any) {
	if s.log != nil {
		s.log.Printf(format, a...) // Printf redacts every line
	}
}

// stop forgets the queue, on Sign Out and as the app exits: the URL queue file goes (the firmware
// read it when it started the queue), and so do its lyrics still to fetch.
func (s *streamer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lyricsGen.Add(1)
	os.Remove(s.urlsFile())
}

// synced reports whether e's file can play from the card: it is there, and no sync is about to
// move or remove it (M24). (The firmware passes over only a real ".." segment, which a Clean path
// has none of, so an ellipsis in a name plays: I19.)
func (s *streamer) synced(e *library.Entry) bool {
	return e != nil && exists(e.Path) && !s.moving(e.ID)
}

// start replaces whatever plays with songs, playing from index from.
func (s *streamer) start(songs []jellyfin.Song, from int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from < 0 || from >= len(songs) {
		return
	}
	s.startURLs(songs, from)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
