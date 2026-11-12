package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Play reporting (Settings → Report Plays to Server, on unless turned off; TODO #8, U31): the
// server learns what the Q2 played, as from Jellyfin's own apps, for its play counts, Played marks
// and last-played dates.
//
//   - A streamed song is reported live: when it starts (POST /Sessions/Playing, which counts the
//     play), and when the next one starts or it should have ended (/Sessions/Playing/Stopped). The
//     app knows a song starts from the firmware's .playing (folder and .m3u queues); of a URL queue,
//     which the firmware's Tidal player plays, from Q2 Pod's .now (now.go: with progress), or
//     without it only the song pressed. A start the server didn't take goes in the outbox as a play
//     at that time.
//   - A synced song, played from the Q2's library whether the app runs or not, is found later in Q2
//     Pod's play log (.scrobbler.log on the card, and .scrobbler.log.sent where Q2 Pod's scrobble
//     upload moves the lines it sent), matched to its song through the manifest (songIndex), and
//     reported as played at its time (POST /UserPlayedItems/{id}?datePlayed=).
//
// The outbox, .jellyfin/outbox.json, is what is still to send, written whole (aside, then renamed).
// An entry leaves it only once the server took it (2xx), or refused it for good (an item it doesn't
// know). It is sent in the background when the app finds Wi-Fi (at its start, or back), when a sync
// starts, and after a failure with a growing wait; never on a page's or a song's time. It keeps at
// most outboxMax entries, the oldest going first (counted in the log). It holds item ids, times and
// hearts (fav.go), no secret. A damaged one is left for a new one, with a log line. The server
// counts a play once a call; a call it took whose answer was lost is sent again and counts twice.
//
// Q2 Pod's log is read on from the newest listen taken, kept in .jellyfin/plays.json with the
// lines of that second already taken: Q2 Pod's upload rewrites the log without what it sent, so a
// byte offset would not hold, and the log is never written here. The first read starts at that
// moment: listens from before reporting was on, or older than the newest taken (a clock set back),
// are not sent. Turning reporting off drops the plays waiting and forgets the place; signing out
// empties the outbox (hearts too) and forgets the place.

const (
	outboxMax  = 1000
	reportWait = 15 * time.Second // one report request
	liveGrace  = 30 * time.Second // a live song counts as stopped this long after its length
	liveSpan   = 10 * 60          // seconds: a log line of a song reported live this near is that play
	liveKeep   = 2 * 24 * 60 * 60 // seconds a live report is remembered for that
	reportMax  = 30 * time.Minute // the longest wait after failures
	reportMin  = time.Minute      // the first
	logLineMax = 4 << 10          // a longer line in Q2 Pod's log is no listen
	playedKind = "played"         // report.Item was played at report.At
	scrobbles  = ".scrobbler.log" // Q2 Pod's play log, at the card's root
	scrobbled  = ".scrobbler.log.sent"
)

// report is one outbox entry: Kind says what to tell the server about Item: played (at At), or
// favourite (On: the heart, fav.go); an entry of a kind this build doesn't know stays.
type report struct {
	Kind string `json:"kind"`
	Item string `json:"item"`
	At   int64  `json:"at,omitempty"` // Unix seconds
	On   bool   `json:"on,omitempty"` // a favourite's state
	// Tried: sent once with no answer, so the server may have counted it. Before it goes again the
	// item's last-played date is asked: equal to At, it counted and is not sent twice.
	Tried bool `json:"tried,omitempty"`
}

// same reports whether a and b are the same report, tried or not.
func (a report) same(b report) bool {
	return a.Kind == b.Kind && a.Item == b.Item && a.At == b.At && a.On == b.On
}

// clockSet reports whether the Q2's clock is set: until Wi-Fi sets it, it reads 1970 or 2000, and a
// play's time, or the place in Q2 Pod's log, would be decades off. Q2 Pod's own test (navigation.c
// scrobble): it logs no listen before it either.
func clockSet(t time.Time) bool { return t.Unix() >= 1600000000 }

// clock is the Q2's clock for reports; tests set it.
var clock = time.Now

type outbox struct {
	path string
	logf func(string, ...any)
	mu   sync.Mutex
}

type outboxFile struct {
	Reports []report `json:"reports"`
}

// load is the outbox; a damaged file is none (it is replaced at the next save). o.mu is held.
func (o *outbox) load() ([]report, error) {
	b, err := os.ReadFile(o.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f outboxFile
	if err := json.Unmarshal(b, &f); err != nil {
		o.logf("reports: %s is damaged, starting a new one: %v", filepath.Base(o.path), err)
		return nil, nil
	}
	return f.Reports, nil
}

// save writes rs whole, or removes the file when there are none. o.mu is held.
func (o *outbox) save(rs []report) error {
	if len(rs) == 0 {
		if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, _ := json.Marshal(outboxFile{rs})
	return q2.WriteFile(o.path, b)
}

// add appends the reports not in the outbox yet; past outboxMax the oldest plays go, then the
// oldest of any kind. A favourite replaces the one waiting for its item: only the last state goes.
func (o *outbox) add(rs ...report) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, err := o.load()
	if err != nil {
		return err
	}
	changed := false
	for _, r := range rs {
		if slices.ContainsFunc(cur, r.same) {
			continue
		}
		if r.Kind == favKind {
			cur = slices.DeleteFunc(cur, func(x report) bool { return x.Kind == favKind && x.Item == r.Item })
		}
		cur, changed = append(cur, r), true
	}
	if !changed {
		return nil
	}
	if over := len(cur) - outboxMax; over > 0 {
		o.logf("reports: the outbox is full: dropped the %d oldest", over)
		// The oldest plays first: a heart waiting is one press, plays come by the hundred.
		cur = slices.DeleteFunc(cur, func(r report) bool {
			if over > 0 && r.Kind == playedKind {
				over--
				return true
			}
			return false
		})
		cur = cur[over:] // still over: the oldest of any kind
	}
	return o.save(cur)
}

func (o *outbox) pending() []report {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, _ := o.load()
	return cur
}

// remove takes rs out of the outbox; what was added meanwhile stays.
func (o *outbox) remove(rs []report) error {
	if len(rs) == 0 {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, err := o.load()
	if err != nil {
		return err
	}
	for _, r := range rs {
		if i := slices.IndexFunc(cur, r.same); i >= 0 {
			cur = slices.Delete(cur, i, i+1)
		}
	}
	return o.save(cur)
}

// tried marks r as sent without an answer (report.Tried).
func (o *outbox) tried(r report) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, err := o.load()
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(cur, r.same); i >= 0 && !cur[i].Tried {
		cur[i].Tried = true
		return o.save(cur)
	}
	return nil
}

// drop removes every report of kind, every report when kind is "".
func (o *outbox) drop(kind string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, err := o.load()
	if err != nil {
		return err
	}
	return o.save(slices.DeleteFunc(cur, func(r report) bool { return kind == "" || r.Kind == kind }))
}

// reporter is the daemon's reporting state.
type reporter struct {
	box     outbox
	playsMu sync.Mutex // plays.json

	mu      sync.Mutex
	want    bool      // a send is asked for: the app's start, Wi-Fi back, a sync, a report waiting
	next    time.Time // no send before, after a failure
	fails   int
	sending bool
	wifi    bool // Wi-Fi at the last tick
	live    *liveSong
	now     nowState // Q2 Pod's .now (now.go)

	favAt   time.Time       // when fav.log was taken aside (fav.go); zero when it wasn't
	favNow  map[string]bool // the hearts pressed in this run, by item
	refused bool            // the server refused a favourite for good, in this run (favRows)
}

// liveSong is the streamed song last reported started.
type liveSong struct {
	id   string
	at   time.Time
	dur  time.Duration
	stop *time.Timer
}

func (d *daemon) reportsOn() bool { return d.settings().ReportPlays != "off" }

// wantReports asks for a send at the next tick with Wi-Fi; now drops the wait after a failure.
func (d *daemon) wantReports(now bool) {
	d.rep.mu.Lock()
	d.rep.want = true
	if now {
		d.rep.next, d.rep.fails = time.Time{}, 0
	}
	d.rep.mu.Unlock()
}

// reportTick, from tick, starts a send when one is asked for and due and the Q2 has Wi-Fi; Wi-Fi
// found (at the app's start too) asks for one.
func (d *daemon) reportTick(now time.Time) {
	up := d.localIP() != ""
	r := &d.rep
	r.mu.Lock()
	if up && !r.wifi {
		r.want, r.next, r.fails = true, time.Time{}, 0
	}
	r.wifi = up
	due := up && r.want && !r.sending && !now.Before(r.next)
	if due {
		r.want, r.sending = false, true
	}
	r.mu.Unlock()
	if due {
		d.spawn(d.sendReports)
	}
}

// sendReports reads Q2 Pod's log into the outbox (reporting on), then sends the outbox in order
// until the server can't be reached; the next try waits reportMin, doubling to reportMax. Plays go
// only with reporting on; favourites whatever it is.
func (d *daemon) sendReports() {
	var failed error
	sent := 0
	defer func() {
		r := &d.rep
		r.mu.Lock()
		r.sending = false
		if failed != nil {
			r.fails++
			r.want, r.next = true, time.Now().Add(min(reportMin<<(r.fails-1), reportMax))
		} else {
			r.fails = 0
		}
		r.mu.Unlock()
		if failed != nil {
			d.log.Printf("reports: %d sent, the rest wait: %v", sent, failed)
		} else if sent > 0 {
			d.log.Printf("reports: %d sent", sent)
		}
	}()
	cl := d.client()
	if cl == nil {
		return
	}
	if d.reportsOn() {
		d.readPlays()
	}
	var done []report
	favs := false
	for _, r := range d.rep.box.pending() {
		if d.client() != cl {
			break // signed out meanwhile
		}
		var counted bool
		var err error
		switch {
		case r.Kind == favKind:
			err, favs = d.sendFav(cl, r), true
		case r.Kind == playedKind && d.reportsOn():
			counted, err = d.sendPlayed(cl, r)
		default:
			continue // a later build's, or a play with reporting turned off meanwhile
		}
		if err != nil && !jellyfin.Refused(err) && !(r.Kind == favKind && httpStatus(err) == 403) {
			failed = err
			break
		}
		switch {
		case err != nil:
			d.log.Printf("reports: %s %s refused: %v", r.Kind, r.Item, err)
			if r.Kind == favKind && httpStatus(err) != 404 { // 404: the song is gone, so is its heart
				d.rep.mu.Lock()
				d.rep.refused = true
				d.rep.mu.Unlock()
			}
		case counted:
			d.log.Printf("reports: %s %s at %s had counted: not sent again", r.Kind, r.Item, day(r.At))
		default:
			sent++
		}
		done = append(done, r)
	}
	if err := d.rep.box.remove(done); err != nil {
		d.log.Printf("reports: outbox: %v", err)
	}
	if favs {
		d.favsChanged()
	}
}

// sendPlayed sends a played report; counted when, tried before, the server had counted it already
// (its last-played date is the report's own). A request with no answer at all marks it tried.
func (d *daemon) sendPlayed(cl *jellyfin.Client, r report) (counted bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), reportWait)
	defer cancel()
	if r.Tried {
		last, err := cl.LastPlayed(ctx, r.Item)
		if err != nil {
			return false, err
		}
		if !last.IsZero() && last.Unix() == r.At {
			return true, nil
		}
	}
	err = cl.MarkPlayed(ctx, r.Item, time.Unix(r.At, 0))
	// No answer, or an answer cut short (a body lost after its status): the server may have counted it.
	var ae *jellyfin.APIError
	if err != nil && (!errors.As(err, &ae) || ae.HTTP == 0 || errors.Is(err, jellyfin.ErrUnreachable)) {
		d.log.Printf("reports: %s %s at %s: no answer, may count twice", r.Kind, r.Item, day(r.At))
		if terr := d.rep.box.tried(r); terr != nil {
			d.log.Printf("reports: outbox: %v", terr)
		}
	}
	return false, err
}

// day is Unix seconds as the log shows them (UTC).
func day(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) }

// stopReports is reporting turned off, or a sign-out: the outbox's plays and the log's place go.
func (d *daemon) stopReports(kind string) {
	if kind == "" {
		d.nowForget() // the account's queue: none of it goes to the next sign-in
	}
	if err := d.rep.box.drop(kind); err != nil {
		d.log.Printf("reports: outbox: %v", err)
	}
	d.rep.playsMu.Lock()
	os.Remove(d.playsPath())
	d.rep.playsMu.Unlock()
}

// ---- live: streamed songs ----

// streamStarted is the streamer's started: song sg, streamed, starts now. The song before it
// stops; the server hears of both in the background. Not a song of the manifest's played from the
// card: Q2 Pod's log has those.
func (d *daemon) streamStarted(sg jellyfin.Song) {
	if !d.reportsOn() {
		return
	}
	cur := &liveSong{id: sg.ID, at: clock(), dur: time.Duration(sg.Duration) * time.Second}
	d.rep.mu.Lock()
	prev := d.rep.live
	d.rep.live = cur
	if prev != nil {
		prev.stop.Stop()
	}
	cur.stop = time.AfterFunc(cur.dur+liveGrace, func() { d.liveEnded(cur) })
	d.rep.mu.Unlock()
	d.spawn(func() {
		if prev != nil {
			d.liveStopped(prev, cur.at)
		}
		d.liveStart(cur.id, cur.at, true)
	})
}

// liveStart tells the server song id started at at (Playing, which counts it); a start it didn't
// take is kept in the outbox as a play at that time. mark notes it in plays.json, so its line in Q2
// Pod's log, if the song has one, isn't sent too.
func (d *daemon) liveStart(id string, at time.Time, mark bool) {
	// The server's clock dates the live report; the Q2's dates the rest, unless it is unset.
	dated := clockSet(at)
	if dated && mark {
		d.notePlays(func(m *playsMark) { m.Live = append(m.Live, liveMark{id, at.Unix()}) })
	}
	cl := d.client()
	if cl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportWait)
	err := cl.PlaybackStarted(ctx, id)
	cancel()
	// kept for later, as a play at its time: not for an account signed out, or reporting off, since
	if err != nil && !jellyfin.Refused(err) && dated && cl == d.client() && d.reportsOn() {
		if aerr := d.rep.box.add(report{Kind: playedKind, Item: id, At: at.Unix()}); aerr != nil {
			d.log.Printf("reports: outbox: %v", aerr)
		}
		d.wantReports(false)
	}
}

// liveEnded is a live song's length (and liveGrace) gone with no other song started: it is taken
// as played to its end.
func (d *daemon) liveEnded(s *liveSong) {
	d.rep.mu.Lock()
	last := d.rep.live == s
	if last {
		d.rep.live = nil
	}
	d.rep.mu.Unlock()
	if last {
		d.liveStopped(s, s.at.Add(s.dur))
	}
}

// liveStopped tells the server s stopped at end (best effort: the play counted at its start).
func (d *daemon) liveStopped(s *liveSong, end time.Time) {
	cl := d.client()
	if cl == nil || !d.reportsOn() {
		return
	}
	pos := end.Sub(s.at)
	if s.dur > 0 {
		pos = min(pos, s.dur)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportWait)
	defer cancel()
	cl.PlaybackStopped(ctx, s.id, max(pos, 0)) // the client logs a failure
}

// ---- Q2 Pod's play log ----

// playsMark is .jellyfin/plays.json: how far Q2 Pod's log was read, and the songs reported live
// lately, whose own lines in the log are not sent again.
type playsMark struct {
	At   int64      `json:"at"`             // the newest listen taken, Unix seconds
	Seen []string   `json:"seen,omitempty"` // the lines taken with that time
	Live []liveMark `json:"live,omitempty"`
}

type liveMark struct {
	Item string `json:"item"`
	At   int64  `json:"at"`
}

func (d *daemon) playsPath() string { return filepath.Join(d.dir, "plays.json") }

// loadPlays is plays.json; a missing or damaged one starts now. d.rep.playsMu is held.
func (d *daemon) loadPlays() (playsMark, error) {
	var m playsMark
	b, err := os.ReadFile(d.playsPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return m, err
	default:
		if json.Unmarshal(b, &m) == nil && m.At > 0 {
			return m, nil
		}
		d.log.Printf("reports: plays.json is damaged: reading on from now")
	}
	return playsMark{At: clock().Unix()}, nil
}

// notePlays changes plays.json with f, dropping live reports older than liveKeep. Nothing while the
// clock is unset: a mark made then would read the whole log again once it is.
func (d *daemon) notePlays(f func(*playsMark)) {
	if !clockSet(clock()) {
		return
	}
	d.rep.playsMu.Lock()
	defer d.rep.playsMu.Unlock()
	m, err := d.loadPlays()
	if err == nil {
		f(&m)
		now := clock().Unix()
		m.Live = slices.DeleteFunc(m.Live, func(l liveMark) bool { return now-l.At > liveKeep })
		b, _ := json.Marshal(m)
		err = q2.WriteFile(d.playsPath(), b)
	}
	if err != nil {
		d.log.Printf("reports: plays.json: %v", err)
	}
}

// readPlays puts the listens of synced songs that Q2 Pod logged since the last read in the outbox.
func (d *daemon) readPlays() {
	man, err := d.manifestErr()
	if err != nil {
		return // nothing is known of what is synced: the log waits
	}
	d.notePlays(func(m *playsMark) {
		rs, next := listens(d.card, man, *m, clock())
		if err := d.rep.box.add(rs...); err != nil {
			d.log.Printf("reports: outbox: %v", err)
			return // the same lines next time
		}
		*m = next
	})
}

// listens reads Q2 Pod's log after m: the synced songs' listens as reports, and the new mark.
// The log is read before the lines its upload moved out: Q2 Pod appends those to .sent before it
// rewrites the log, so a line moved meanwhile is in one or the other. A line dated more than an
// hour after now (logged under a clock since set back) waits for its time, not to move the mark
// past the listens to come.
func listens(card string, man *library.Manifest, m playsMark, now time.Time) ([]report, playsMark) {
	idx := newSongIndex(card, man)
	next := m
	seen := map[string]bool{}
	for _, h := range m.Seen {
		seen[h] = true
	}
	var rs []report
	for _, name := range []string{scrobbles, scrobbled} {
		eachLine(filepath.Join(card, name), func(line string) {
			// artist, album, title, track, length, rating, time, MusicBrainz id
			f := strings.Split(line, "\t")
			if len(f) < 7 || f[5] != "L" {
				return
			}
			t, err := strconv.ParseInt(f[6], 10, 64)
			if err != nil || t < m.At || t > now.Unix()+3600 {
				return
			}
			sum := sha1.Sum([]byte(line))
			h := hex.EncodeToString(sum[:8])
			if seen[h] {
				return
			}
			seen[h] = true
			if t > next.At {
				next.At, next.Seen = t, nil
			}
			if t == next.At {
				next.Seen = append(next.Seen, h)
			}
			if id := idx.find(f[0], f[1], f[2], f[3]); id != "" && !liveNear(m.Live, id, t) {
				rs = append(rs, report{Kind: playedKind, Item: id, At: t})
			}
		})
	}
	return rs, next
}

// eachLine calls f with each whole line of path (without its line break); none when it can't be read.
func eachLine(path string, f func(string)) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	br := bufio.NewReaderSize(file, logLineMax)
	for long := false; ; {
		b, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			long = true // the rest of a line too long to be a listen
			continue
		case err != nil: // io.EOF: a line still being written, or none
			return
		case long:
			long = false
		case len(b) > 0 && b[0] != '#':
			f(string(bytes.TrimRight(b, "\r\n")))
		}
	}
}

func liveNear(live []liveMark, id string, t int64) bool {
	for _, l := range live {
		if l.Item == id && l.At-t <= liveSpan && t-l.At <= liveSpan {
			return true
		}
	}
	return false
}

// songIndex finds a synced song from the tags Q2 Pod logs, through the path the sync gave it
// (library.SongPath: Jellyfin/<album artist>/<year - ><album>/<disc->track - title>[ [id]].ext),
// keyed by album and title as SafeName makes them, in lower case. The tags are the file's own,
// which a transcode carries from the server's file, and the path the server's: they agree unless
// the server's metadata was edited away from the file's tags; such a song is not found.
type songIndex map[[2]string][]indexed

type indexed struct {
	id, artist string
	track      int
}

var (
	trackPrefix = regexp.MustCompile(`^(?:\d+-)?(\d+) - `)
	yearPrefix  = regexp.MustCompile(`^\d+ - `)
)

func newSongIndex(card string, man *library.Manifest) songIndex {
	top := filepath.Join(card, "Jellyfin")
	idx := songIndex{}
	for id, e := range man.Songs {
		rel, err := filepath.Rel(top, e.Path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if err != nil || len(parts) != 3 {
			continue
		}
		stem := strings.TrimSuffix(parts[2], filepath.Ext(parts[2]))
		stem = strings.TrimSuffix(stem, " ["+library.SafeName(id)+"]")
		track := 0
		if m := trackPrefix.FindStringSubmatch(stem); m != nil {
			track, _ = strconv.Atoi(m[1])
			stem = stem[len(m[0]):]
		}
		k := [2]string{strings.ToLower(yearPrefix.ReplaceAllString(parts[1], "")), strings.ToLower(stem)}
		idx[k] = append(idx[k], indexed{id, strings.ToLower(parts[0]), track})
	}
	return idx
}

// find is the song of a logged listen, "" when none or more than one fit.
func (idx songIndex) find(artist, album, title, track string) string {
	if album == "" {
		album = "Unknown Album"
	}
	fold := func(s string) string { return strings.ToLower(library.SafeName(s)) }
	c := idx[[2]string{fold(album), fold(title)}]
	if n, err := strconv.Atoi(track); err == nil && len(c) > 1 {
		c = slices.DeleteFunc(slices.Clone(c), func(x indexed) bool { return x.track != n })
	}
	if len(c) > 1 {
		c = slices.DeleteFunc(slices.Clone(c), func(x indexed) bool { return x.artist != fold(artist) })
	}
	if len(c) != 1 {
		return ""
	}
	return c[0].id
}

// ---- the setting ----

// reportRow is Settings' Report Plays to Server.
func reportRow(s settings) q2spot.Row {
	state := "On"
	if s.ReportPlays == "off" {
		state = "Off"
	}
	return q2spot.Row{Kind: "action", URI: "reportplays", Title: "Report Plays to Server: " + state,
		Subtitle: "Your Jellyfin server counts the songs you play"}
}

// toggleReports is the row's press (settingsMu held). Off forgets the plays not sent yet.
func (d *daemon) toggleReports(s settings) q2spot.Reply {
	if s.ReportPlays == "off" {
		s.ReportPlays = ""
	} else {
		s.ReportPlays = "off"
	}
	if d.saveSettings(s) != nil {
		return q2spot.Fail(q2spot.Error, settingNotSaved)
	}
	if s.ReportPlays == "off" {
		d.stopReports(playedKind)
		return q2spot.Reply{Message: "Report Plays to Server: Off"}
	}
	d.rep.playsMu.Lock()
	os.Remove(d.playsPath()) // a read still running when it went off may have left one: on from now
	d.rep.playsMu.Unlock()
	d.wantReports(true)
	return q2spot.Reply{Message: "Report Plays to Server: On"}
}
