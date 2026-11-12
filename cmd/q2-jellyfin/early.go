package main

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// Early start: a streamed song plays before it has all arrived. Q2 Pod plays a card app's song once
// its file has its final name (it passes over .part files), and the player reads the file as it
// plays. So the stream downloads into .part and renames it to its final name while the rest still
// downloads. The player can't tell the end of what has arrived from the end of the song: if it
// catches up with the download it ends the song there and plays the next. So a song starts early
// only when that is unlikely:
//
//   - only a FLAC or WAV original: their headers give the song's whole length, so Now Playing
//     shows it and the player doesn't take the part that has arrived for all of it. Ogg and Opus
//     (every transcode), MP3 and M4A play once they have arrived whole; a transcode is small;
//   - at bufferSeconds of audio when the link is clearly faster than the song (fastRatio times
//     its bitrate, measured over the whole download and over the last recentWindow, the slower of
//     the two); otherwise at slowBufferSeconds, and only when the rest arrives at rateSafety of
//     that rate before the player gets there. A slower link waits for more, as long as it must.
//
// The download goes on writing the same file after the rename. A song whose bitrate can't be known
// plays once the whole file has arrived.
const (
	bufferSeconds     = 3
	slowBufferSeconds = 15
	fastRatio         = 2.0
	minMeasure        = 2 * time.Second
	recentWindow      = 3 * time.Second
	rateSafety        = 0.6
)

type earlyStart struct {
	bps     float64 // the song's bytes per second of audio
	dur     float64 // its length, seconds
	total   int64   // the bytes expected
	start   time.Time
	now     func() time.Time
	samples []sample // (time, bytes) as they arrived, the last recentWindow's and one before
}

type sample struct {
	at  time.Time
	got int64
}

// earlyExt reports whether a file of this extension may start before it has all arrived.
func earlyExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "flac", "wav":
		return true
	}
	return false
}

// newEarly is the early start for a song at quality q in a file of extension ext, total bytes
// expected; nil when it plays only once whole.
func newEarly(sg jellyfin.Song, q, ext string, total int64, now func() time.Time) *earlyStart {
	if !quality.IsOriginal(q) || !earlyExt(ext) {
		return nil
	}
	var bps float64
	switch {
	case sg.BitRate > 0:
		bps = float64(sg.BitRate) * 1000 / 8
	case sg.Size > 0 && sg.Duration > 0:
		bps = float64(sg.Size) / float64(sg.Duration)
	}
	if bps <= 0 || sg.Duration <= 0 || total <= 0 {
		return nil
	}
	return &earlyStart{bps: bps, dur: float64(sg.Duration), total: total, start: now(), now: now}
}

// rate is the download's bytes a second: the slower of the whole download's and the last
// recentWindow's; 0 before minMeasure.
func (e *earlyStart) rate(got int64) float64 {
	t := e.now()
	el := t.Sub(e.start)
	if el < minMeasure {
		return 0
	}
	r := float64(got) / el.Seconds()
	// the newest sample at least recentWindow old is where the recent rate is measured from
	for i := len(e.samples) - 1; i >= 0; i-- {
		if s := e.samples[i]; t.Sub(s.at) >= recentWindow {
			if d := t.Sub(s.at).Seconds(); d > 0 {
				r = min(r, float64(got-s.got)/d)
			}
			break
		}
	}
	return r
}

// note records got bytes arrived now, keeping the samples rate needs.
func (e *earlyStart) note(got int64) {
	t := e.now()
	e.samples = append(e.samples, sample{t, got})
	keep := 0 // drop all but the newest sample older than recentWindow
	for i := len(e.samples) - 1; i >= 0; i-- {
		if t.Sub(e.samples[i].at) >= recentWindow {
			keep = i
			break
		}
	}
	e.samples = e.samples[keep:]
}

// target is the bytes to have before the song may start, at the rate measured now; total when the
// link is too slow for it to start before it is whole.
func (e *earlyStart) target(got int64) int64 {
	r := e.rate(got)
	if r <= 0 {
		return int64(slowBufferSeconds * e.bps)
	}
	if r >= fastRatio*e.bps {
		return int64(bufferSeconds * e.bps)
	}
	// The song plays from its start: t seconds in, the player wants bps·t bytes and got + rate·t
	// have arrived. Slower than the song, the tightest point is its end: got + safety·rate·dur ≥ total.
	want := float64(e.total) - rateSafety*r*e.dur
	return min(e.total, max(int64(want), int64(slowBufferSeconds*e.bps)))
}

// ready reports whether got bytes are enough to start playing.
func (e *earlyStart) ready(got int64) bool {
	if e == nil || e.rate(got) <= 0 {
		return false
	}
	return got >= e.target(got) && got < e.total
}

// copyEarly copies body into f, calling release once early says playback may start (at most once),
// and progress, when set, with the bytes so far and the bytes playback waits for. It reports the
// bytes copied and whether release ran.
func copyEarly(f *os.File, body io.Reader, early *earlyStart, release func() error, progress func(got, want int64)) (int64, bool, error) {
	buf := make([]byte, 64<<10)
	var got int64
	released := false
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return got, released, err
			}
			got += int64(n)
			if early != nil {
				early.note(got)
			}
			if !released && early.ready(got) {
				if err := f.Sync(); err != nil { // what the player reads first is on the card
					return got, released, err
				}
				if err := release(); err != nil {
					return got, released, err
				}
				released = true
			}
			if progress != nil {
				want := int64(-1) // the whole song
				if released {
					want = 0
				} else if early != nil {
					want = early.target(got)
				}
				progress(got, want)
			}
		}
		if rerr == io.EOF {
			return got, released, nil
		}
		if rerr != nil {
			return got, released, rerr
		}
	}
}
