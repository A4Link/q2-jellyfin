package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

// URL queues, for Q2 Pod's "urls" (Q2 Pod 1.7.3): instead of fetching songs to the card, the stream
// hands the firmware a queue of stream URLs, which it plays through stock's Tidal player. That
// player streams the song itself: it waits for slow data (Now Playing's spinner) rather than
// ending the song, and seeks with HTTP ranges. Songs on the card stay card paths in the same queue.
//
// The queue file, /tmp/q2-jellyfin.urls (Q2URLS/1), written whole and named in .play:
//
//	#Q2URLS/1<TAB>start=<index>[<TAB>now=1]
//	<url or card path><TAB><title><TAB><artist><TAB><album><TAB><cover url><TAB><seconds>[<TAB><item><TAB><fav>]
//
// a line a song, fields with no tab or line break in them. With the firmware's "fav", every line
// carries the song's item (its id) and heart (1 a favourite), so Now Playing's heart is the
// server's favourite (fav.go); firmware without it gets six fields. With the firmware's "now", the
// header asks it to say which line plays (now=1), for the server's play reports (now.go).

// playerSource is a stream source that can also give URLs for the firmware's own player.
type playerSource interface {
	PlayerURL(id, quality string) string
	PlayerCoverURL(id string) string
}

const (
	urlsBefore = 100       // songs kept in the queue before the one started
	urlsAfter  = 400       // and from it on
	urlsMax    = 256 << 10 // the most of the file Q2 Pod reads (tidal.c AU_MAX); a line cut there is dropped
)

func urlField(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\t' || r == '\n' || r == '\r' }), " ")
}

// urlsQueue is the Q2URLS/1 file for songs from from on, and the songs[lo:hi] it holds. It fits in
// urlsMax: the song started and those after it first, then the ones before it as room allows.
func (s *streamer) urlsQueue(songs []jellyfin.Song, from int, ps playerSource) (b []byte, lo, hi int) {
	lo, hi = max(0, from-urlsBefore), min(len(songs), from+urlsAfter)
	q := quality.Original
	if s.quality != nil {
		q = quality.Normal(s.quality()) // once: it reads settings.json
	}
	m := s.manifest()
	var hearts map[string]bool
	if s.caps.Fav && s.hearts != nil {
		hearts = s.hearts()
	}
	lines := make([]string, hi-lo)
	for i, sg := range songs[lo:hi] {
		src := ""
		if e := m.Songs[sg.ID]; s.synced(e) {
			src = e.Path // on the card: played as a local file within the queue
		} else {
			src = ps.PlayerURL(sg.ID, library.Quality(q, sg.Suffix))
		}
		cover := ""
		if sg.CoverArt != "" {
			cover = ps.PlayerCoverURL(sg.CoverArt)
		}
		lines[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%d", urlField(src), urlField(sg.Title), urlField(sg.Artist),
			urlField(sg.Album), urlField(cover), sg.Duration)
		if s.caps.Fav { // every line, a card path's too: a line without an item has no heart
			fav, ok := hearts[sg.ID]
			if !ok {
				fav = sg.Favorite
			}
			lines[i] += "\t" + urlField(sg.ID) + "\t" + map[bool]string{true: "1", false: "0"}[fav]
		}
		lines[i] += "\n"
	}
	header := func(start int) string {
		if s.caps.Now {
			return fmt.Sprintf("#Q2URLS/1\tstart=%d\tnow=1\n", start)
		}
		return fmt.Sprintf("#Q2URLS/1\tstart=%d\n", start)
	}
	room := urlsMax - len(header(from-lo)) // start= only gets shorter below
	end := from
	for ; end < hi && len(lines[end-lo]) <= room; end++ {
		room -= len(lines[end-lo])
	}
	first := from
	for ; first > lo && len(lines[first-1-lo]) <= room; first-- {
		room -= len(lines[first-1-lo])
	}
	b = []byte(header(from - first))
	for _, l := range lines[first-lo : end-lo] {
		b = append(b, l...)
	}
	return b, first, end
}

// urlsFile is the queue file, beside .play in /tmp.
func (s *streamer) urlsFile() string {
	return filepath.Join(filepath.Dir(s.playFile), "q2-jellyfin.urls")
}

// startURLs plays songs from from as a URL queue; false when the source can't make player URLs.
func (s *streamer) startURLs(songs []jellyfin.Song, from int) bool {
	ps, ok := s.src.(playerSource)
	if !ok {
		return false
	}
	path := s.urlsFile()
	b, lo, hi := s.urlsQueue(songs, from, ps)
	if err := q2.WriteFile(path, b); err != nil {
		s.logf("stream: urls: %v", err)
		return true
	}
	s.urlsLyrics(songs[lo:hi], from-lo, filepath.Join(filepath.Dir(s.playFile), "q2-jellyfin.lyrics"))
	seq := strconv.FormatInt(time.Now().UnixNano(), 10)
	// the queue the firmware gets, for Now Playing's ⋯ (playqueue.go)
	s.saveQueue(seq, songs[lo:hi], from-lo)
	if s.caps.Now && s.queued != nil { // before .play: the firmware tells of the queue once it plays
		m := s.manifest()
		ids := make([]string, hi-lo)
		for i, sg := range songs[lo:hi] {
			if !s.synced(m.Songs[sg.ID]) { // a synced song is left to Q2 Pod's log, as below
				ids[i] = sg.ID
			}
		}
		s.queued(seq, ids)
	}
	if err := q2.WriteFile(s.playFile, []byte(seq+" "+path)); err != nil {
		s.logf("stream: urls: %v", err)
	}
	s.setStatus(streamStatus{}) // the firmware's Now Playing shows its own buffering
	// Without the firmware's "now", of a URL queue the app knows only that the song pressed starts:
	// the firmware's Tidal player says nothing of the ones after it.
	if !s.caps.Now && s.started != nil && !s.synced(s.manifest().Songs[songs[from].ID]) {
		s.started(songs[from])
	}
	s.logf("stream: %d songs as URLs from %q", len(songs), songs[from].Title)
	return true
}

// urlsLyrics gives a URL queue's songs their server lyrics for Q2 Pod's Now Playing: dir/<line>.lrc,
// which Q2 Pod (its lyrics change, not yet merged upstream) reads for the track at that line (tidal.c app_lyrics). The first song's come
// before it plays; the rest follow in queue order from it, until another queue starts.
func (s *streamer) urlsLyrics(songs []jellyfin.Song, first int, dir string) {
	gen := s.lyricsGen.Add(1)
	clearLyricLines(dir) // the last queue's lines; songs' lyrics stay, by id, for the next Play (audit B1)
	ls, ok := s.src.(syncer.LyricsSource)
	if !ok {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logf("stream: lyrics: %v", err)
		return
	}
	m := s.manifest()
	// write gives line i its lyrics: from the card beside a synced song, from an earlier Play, else
	// from the server; a line is written only while its queue is the current one. It reports whether
	// the server was out of reach.
	write := func(i int) (unreachable bool) {
		line := filepath.Join(dir, strconv.Itoa(i)+".lrc")
		if e := m.Songs[songs[i].ID]; s.synced(e) {
			if b, err := os.ReadFile(library.LyricsPath(e.Path)); err == nil {
				if s.lyricsGen.Load() == gen {
					q2.WriteFile(line, b)
				}
				return false // on the card already: no network
			}
		}
		if !songs[i].HasLyrics {
			return false
		}
		kept := keptLyrics(dir, songs[i].ID)
		if !exists(kept) {
			ctx, cancel := context.WithTimeout(context.Background(), lyricsWait)
			defer cancel()
			text, err := ls.Lyrics(ctx, songs[i].ID)
			if err == nil && text != "" {
				err = q2.WriteFile(kept, []byte(text))
			}
			if err != nil {
				s.logf("stream: lyrics %s: %v", songs[i].ID, err)
				var ae *jellyfin.APIError // an HTTP answer is a server in reach
				return errors.Is(err, jellyfin.ErrUnreachable) && !(errors.As(err, &ae) && ae.HTTP != 0)
			}
			if text == "" {
				return false
			}
		}
		if s.lyricsGen.Load() == gen { // not a line of a newer queue
			linkArt(kept, line)
		}
		return false
	}
	// Two answers of a server out of reach end this queue's lyrics: the rest would each wait up to
	// lyricsWait for nothing. The next Play tries again.
	misses := 0
	if write(first) {
		misses++
	}
	go func() {
		for k := 1; k < len(songs) && misses < 2 && s.lyricsGen.Load() == gen; k++ {
			if write((first + k) % len(songs)) {
				misses++
			}
		}
	}()
}

// keptLyrics is where song id's lyrics are kept between Plays (Jellyfin's ids are hex; Base keeps
// any other inside dir).
func keptLyrics(dir, id string) string { return filepath.Join(dir, "id-"+filepath.Base(id)+".lrc") }

// clearLyricLines removes the last queue's line files (<n>.lrc), keeping the songs' kept lyrics,
// and all of those too once there are more than lyricsKeep.
func clearLyricLines(dir string) {
	ents, _ := os.ReadDir(dir)
	kept := 0
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "id-") {
			kept++
		} else if _, err := strconv.Atoi(strings.TrimSuffix(n, ".lrc")); err == nil {
			os.Remove(filepath.Join(dir, n))
		}
	}
	if kept > lyricsKeep {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "id-") {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// lyricsKeep is how many songs' lyrics may stay from earlier Plays: /tmp is RAM and each file takes
// at least a 4 KB page, so past it they all go when the next Play starts (independent review
// 2026-11-10 #2). The queue playing keeps its own songs' lyrics, as before.
const lyricsKeep = 100

// lyricsWait is the most one song's lyrics may take.
const lyricsWait = 3 * time.Second
