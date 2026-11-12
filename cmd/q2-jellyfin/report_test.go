package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
)

// reportSongs are synced songs as the sync lays them out: a plain one, one of a second disc whose
// name carries its id, and two of the same title on albums of the same name by different artists.
func reportSongs(card string) (*library.Manifest, []jellyfin.Song) {
	ss := []jellyfin.Song{
		{ID: "blue1", Title: "Blue", Album: "Kind", AlbumArtist: "Miles", Year: 1959, Disc: 1, Track: 2, Suffix: "flac"},
		{ID: "what2", Title: "So What", Album: "Kind", AlbumArtist: "Miles", Year: 1959, Disc: 2, Track: 3, Suffix: "flac"},
		{ID: "intro3", Title: "Intro", Album: "Live", AlbumArtist: "Band A", Track: 1, Suffix: "flac"},
		{ID: "intro4", Title: "Intro", Album: "Live", AlbumArtist: "Band B", Track: 1, Suffix: "flac"},
	}
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for i, s := range ss {
		p := library.SongPath(card, s, "opus-160", s.Disc > 1)
		if i == 1 {
			p = library.WithID(p, s.ID)
		}
		m.Songs[s.ID] = &library.Entry{ID: s.ID, Path: p, Quality: "opus-160"}
	}
	return m, ss
}

// listen is a line of Q2 Pod's log for s, started at t.
func listen(artist, album, title string, track int, t int64) string {
	return fmt.Sprintf("%s\t%s\t%s\t%d\t180\tL\t%d\t\n", artist, album, title, track, t)
}

const logHeader = "#AUDIOSCROBBLER/1.1\n#TZ/UTC\n#CLIENT/Q2 Pod\n"

func (d *tdaemon) sendNow(t *testing.T) {
	t.Helper()
	d.wantReports(true)
	d.tick()
	d.bg.Wait()
}

func (d *tdaemon) outbox() []report { return d.rep.box.pending() }

// U31: synced songs played from the card are found in Q2 Pod's play log (and in .sent, where its
// scrobble upload moves what it sent), matched through the manifest and reported as played at
// their time, once each. The first read starts then; skipped plays ("S"), other apps' songs, an
// unfinished line and a song the tags can't single out send nothing. The log is never written.
func TestReportsFromPlayLog(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	m, _ := reportSongs(d.card)
	if err := m.Save(d.card, filepath.Join(d.dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	logf, sent := filepath.Join(d.card, ".scrobbler.log"), filepath.Join(d.card, ".scrobbler.log.sent")
	old := time.Now().Unix() - 3600
	os.WriteFile(logf, []byte(logHeader+listen("Miles", "Kind", "Blue", 2, old)), 0o644) // before reporting was on
	d.sendNow(t)
	if got := n.reported(); len(got) != 0 {
		t.Fatalf("the first read sent %q", got)
	}
	now := time.Now().Unix()
	body := logHeader + listen("Miles", "Kind", "Blue", 2, old) +
		listen("Miles Davis", "Kind", "Blue", 2, now+10) + // the track artist: the album and title decide
		"Miles\tKind\tBlue\t2\t180\tS\t" + fmt.Sprint(now+11) + "\t\n" + // skipped
		listen("Other", "Album", "Song", 1, now+12) + // not synced by this app
		listen("Band B", "Live", "Intro", 1, now+13) + // the artist singles it out
		listen("Someone", "Live", "Intro", 1, now+14) + // two songs fit
		"Miles\tKind\tBlue\t2\t180\tL\t" + fmt.Sprint(now+99) // still being written
	os.WriteFile(logf, []byte(body), 0o644)
	os.WriteFile(sent, []byte(listen("Miles", "Kind", "So What", 3, now+20)), 0o644)
	d.sendNow(t)
	want := []string{"played blue1 " + day(now+10), "played intro4 " + day(now+13), "played what2 " + day(now+20)}
	if got := n.reported(); !slices.Equal(got, want) {
		t.Fatalf("reported\n%q\nwant\n%q", got, want)
	}
	if b, _ := os.ReadFile(logf); string(b) != body {
		t.Fatal("the log was changed")
	}
	if len(d.outbox()) != 0 {
		t.Fatalf("outbox %+v", d.outbox())
	}
	// Q2 Pod's upload moves the lines it sent to .sent and rewrites the log; a new listen follows.
	os.WriteFile(sent, []byte(listen("Miles", "Kind", "So What", 3, now+20)+listen("Miles Davis", "Kind", "Blue", 2, now+10)), 0o644)
	os.WriteFile(logf, []byte(logHeader+listen("Miles", "Kind", "Blue", 2, now+20)), 0o644) // the same second as So What
	d.sendNow(t)
	want = append(want, "played blue1 "+day(now+20))
	if got := n.reported(); !slices.Equal(got, want) {
		t.Fatalf("after the upload\n%q\nwant\n%q", got, want)
	}
	// Both deleted (an uploader on a computer): nothing breaks, nothing is sent again.
	os.Remove(logf)
	os.Remove(sent)
	d.sendNow(t)
	if got := n.reported(); len(got) != len(want) {
		t.Fatalf("%q", got)
	}
}

// A song reported live while it streamed is in Q2 Pod's log too (synced songs stream from the
// stream folder on firmware without playlists, or while a sync moves them): it isn't sent twice.
func TestReportsLiveNotTwice(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	m, ss := reportSongs(d.card)
	m.Save(d.card, filepath.Join(d.dir, "manifest.json"))
	d.sendNow(t) // the first read
	d.streamStarted(ss[0])
	d.bg.Wait()
	now := time.Now().Unix()
	os.WriteFile(filepath.Join(d.card, ".scrobbler.log"), []byte(logHeader+listen("Miles", "Kind", "Blue", 2, now)), 0o644)
	d.sendNow(t)
	if got := n.reported(); !slices.Equal(got, []string{"start blue1"}) {
		t.Fatalf("%q", got)
	}
}

// Streamed songs are reported live: a start when one starts, and the song before stops then at
// the time it played. A start the server didn't take is kept as a play at its time, and sent once
// the server is back; a song it doesn't know is dropped.
func TestReportsLive(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	a := jellyfin.Song{ID: "al0.0", Duration: 200}
	b := jellyfin.Song{ID: "al0.1", Duration: 200}
	d.streamStarted(a)
	d.bg.Wait()
	d.rep.mu.Lock()
	d.rep.live.at = d.rep.live.at.Add(-3 * time.Second) // a played 3 s
	d.rep.mu.Unlock()
	d.streamStarted(b)
	d.bg.Wait()
	got := n.reported()
	if len(got) != 3 || got[0] != "start al0.0" || !strings.HasPrefix(got[1], "stop al0.0 3") || got[2] != "start al0.1" {
		t.Fatalf("%q", got)
	}
	n.reportStatus.Store(503)
	c := jellyfin.Song{ID: "al0.2", Duration: 200}
	d.streamStarted(c)
	d.bg.Wait()
	ob := d.outbox()
	if len(ob) != 1 || ob[0].Kind != "played" || ob[0].Item != "al0.2" || time.Now().Unix()-ob[0].At > 5 {
		t.Fatalf("outbox %+v", ob)
	}
	n.reportStatus.Store(0)
	d.sendNow(t)
	if got := n.reported(); got[len(got)-1] != "played al0.2 "+day(ob[0].At) || len(d.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, d.outbox())
	}
	n.reportStatus.Store(404) // gone from the server: dropped, not tried for ever
	d.rep.box.add(report{Kind: "played", Item: "gone", At: 1})
	d.sendNow(t)
	if len(d.outbox()) != 0 {
		t.Fatalf("outbox %+v", d.outbox())
	}
}

// The outbox waits for the server: an entry stays until it is taken, a failure waits before the
// next try (a tick finds it not due), and Wi-Fi coming back tries at once.
func TestReportsRetry(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1000}, report{Kind: "played", Item: "al0.1", At: 2000})
	n.reportStatus.Store(503)
	d.sendNow(t)
	if len(d.outbox()) != 2 {
		t.Fatalf("outbox %+v", d.outbox())
	}
	n.reportStatus.Store(0)
	d.tick() // backing off
	d.bg.Wait()
	if len(n.reported()) != 0 {
		t.Fatalf("sent while backing off: %q", n.reported())
	}
	d.localIP = func() string { return "" }
	d.tick()
	d.localIP = func() string { return "192.168.1.20" }
	d.tick() // Wi-Fi back
	d.bg.Wait()
	if got := n.reported(); len(got) != 2 || len(d.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, d.outbox())
	}
	// Without Wi-Fi nothing is tried.
	d.localIP = func() string { return "" }
	d.rep.box.add(report{Kind: "played", Item: "al0.2", At: 3000})
	d.sendNow(t)
	if len(n.reported()) != 2 {
		t.Fatalf("%q", n.reported())
	}
}

// The outbox holds outboxMax entries, the oldest going first (counted in the log); a damaged
// one is left for a new one with a log line; an entry of an unknown kind stays.
func TestOutbox(t *testing.T) {
	dir := t.TempDir()
	lg := applog.Open(filepath.Join(dir, "log.txt"), 1<<20)
	o := &outbox{path: filepath.Join(dir, "outbox.json"), logf: lg.Printf}
	var rs []report
	for i := range outboxMax + 5 {
		rs = append(rs, report{Kind: "played", Item: "x", At: int64(i)})
	}
	o.add(rs...)
	o.add(rs[len(rs)-1]) // already there
	got := o.pending()
	if len(got) != outboxMax || got[0].At != 5 {
		t.Fatalf("%d entries, first %+v", len(got), got[0])
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "log.txt")); !strings.Contains(string(b), "dropped the 5 oldest") {
		t.Fatalf("log %q", b)
	}
	os.WriteFile(o.path, []byte(`{"reports":[{"kind":"pla`), 0o644)
	if len(o.pending()) != 0 {
		t.Fatal("damaged outbox read")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "log.txt")); !strings.Contains(string(b), "outbox.json is damaged") {
		t.Fatalf("log %q", b)
	}
	o.add(report{Kind: "favourite", Item: "y", At: 0}, report{Kind: "played", Item: "z", At: 1})
	o.remove([]report{{Kind: "played", Item: "z", At: 1}})
	if got := o.pending(); len(got) != 1 || got[0].Kind != "favourite" {
		t.Fatalf("%+v", got)
	}
	if b, _ := os.ReadFile(o.path); strings.Contains(string(b), "tok") || !json.Valid(b) {
		t.Fatalf("%s", b)
	}
}

// Past outboxMax the oldest plays go first: a favourite waiting stays (U30), and goes only when the
// outbox would still be over with no play left.
func TestOutboxKeepsFavourites(t *testing.T) {
	dir := t.TempDir()
	o := &outbox{path: filepath.Join(dir, "outbox.json"), logf: func(string, ...any) {}}
	o.add(report{Kind: favKind, Item: "fav0", On: true})
	for i := range outboxMax {
		o.add(report{Kind: "played", Item: "x", At: int64(i)})
	}
	got := o.pending()
	if len(got) != outboxMax || !slices.ContainsFunc(got, func(r report) bool { return r.Item == "fav0" }) ||
		slices.ContainsFunc(got, func(r report) bool { return r.Kind == "played" && r.At == 0 }) {
		t.Fatalf("%d entries, first %+v", len(got), got[0])
	}
	var fs []report
	for i := range outboxMax + 1 {
		fs = append(fs, report{Kind: favKind, Item: fmt.Sprint("f", i), On: true})
	}
	o.add(fs...)
	got = o.pending()
	if len(got) != outboxMax || got[0].Item != "f1" || slices.ContainsFunc(got, func(r report) bool { return r.Kind == "played" }) {
		t.Fatalf("%d entries, first %+v", len(got), got[0])
	}
}

// Settings → Report Plays to Server: On by default; Off sends nothing, forgets what waits, and
// the log's place; On again reads on from then.
func TestReportsSetting(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	if _, ok := find(d.do("settings"), "Report Plays to Server: On"); !ok {
		t.Fatalf("%q", titles(d.do("settings")))
	}
	d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1})
	d.notePlays(func(*playsMark) {})
	if m := d.do("reportplays").Message; m != "Report Plays to Server: Off" {
		t.Fatalf("%q", m)
	}
	if _, ok := find(d.do("settings"), "Report Plays to Server: Off"); !ok || len(d.outbox()) != 0 || exists(d.playsPath()) {
		t.Fatalf("%q outbox %+v plays %v", titles(d.do("settings")), d.outbox(), exists(d.playsPath()))
	}
	d.streamStarted(jellyfin.Song{ID: "al0.1"})
	d.bg.Wait()
	d.sendNow(t)
	if got := n.reported(); len(got) != 0 {
		t.Fatalf("sent while off: %q", got)
	}
	if m := d.do("reportplays").Message; m != "Report Plays to Server: On" {
		t.Fatalf("%q", m)
	}
}

// Signing out forgets the reports waiting: they are the account's.
func TestReportsSignOut(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1})
	d.do("signout")
	d.bg.Wait()
	if len(d.outbox()) != 0 {
		t.Fatalf("%+v", d.outbox())
	}
}

// The stream says when a song it fetched starts (from .playing); a synced song in its queue is
// left to Q2 Pod's log. Of a URL queue it says only the song pressed, unless that one is synced.
func TestStreamStartedHook(t *testing.T) {
	var mu sync.Mutex
	var started []string
	hook := func(sg jellyfin.Song) { mu.Lock(); started = append(started, sg.ID); mu.Unlock() }
	took := func() []string { mu.Lock(); defer mu.Unlock(); s := started; started = nil; return s }
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.started = hook
	ss := songs(3, "flac")
	e.man.Songs["s1"] = &library.Entry{ID: "s1", Path: e.root + "/Jellyfin/s1.flac"}
	os.MkdirAll(e.root+"/Jellyfin", 0o755)
	os.WriteFile(e.root+"/Jellyfin/s1.flac", []byte("x"), 0o644)
	e.s.start(ss, 0)
	for _, p := range []string{e.stream(0, "flac"), e.root + "/Jellyfin/s1.flac", e.stream(2, "flac")} {
		until(t, p, func() bool { return exists(p) })
		os.WriteFile(e.playing, []byte(p), 0o644)
		time.Sleep(50 * time.Millisecond)
	}
	e.s.stop()
	if got := took(); !slices.Equal(got, []string{"s0", "s2"}) {
		t.Fatalf("%q", got)
	}

	u := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	u.s.src, u.s.started = urlSrc{u.src}, hook
	u.s.manifest = func() *library.Manifest { return e.man }
	u.s.start(ss, 1) // synced: Q2 Pod's log has it, if anything
	u.s.start(ss, 2)
	if got := took(); !slices.Equal(got, []string{"s2"}) {
		t.Fatalf("URL queue: %q", got)
	}
}

// unsetClock makes the Q2's clock read 2000, as before Wi-Fi sets it, for the test.
func unsetClock(t *testing.T) {
	clock = func() time.Time { return time.Date(2000, 1, 1, 0, 5, 0, 0, time.UTC) }
	t.Cleanup(func() { clock = time.Now })
}

// With the clock unset nothing is dated by it: Q2 Pod's log isn't read and no place in it is kept
// (a mark in 2000 would send the whole log again), and a streamed song the server didn't take
// isn't kept as a play at a wrong time. The live start goes, dated by the server.
func TestReportsClockUnset(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	m, ss := reportSongs(d.card)
	m.Save(d.card, filepath.Join(d.dir, "manifest.json"))
	now := time.Now().Unix()
	os.WriteFile(filepath.Join(d.card, ".scrobbler.log"), []byte(logHeader+listen("Miles", "Kind", "Blue", 2, now-60)), 0o644)
	unsetClock(t)
	d.sendNow(t)
	if exists(d.playsPath()) || len(n.reported()) != 0 {
		t.Fatalf("plays.json %v, sent %q", exists(d.playsPath()), n.reported())
	}
	d.streamStarted(ss[0])
	d.bg.Wait()
	n.reportStatus.Store(503)
	d.streamStarted(ss[1])
	d.bg.Wait()
	if got := n.reported(); !slices.Equal(got, []string{"start blue1"}) || len(d.outbox()) != 0 || exists(d.playsPath()) {
		t.Fatalf("%q outbox %+v plays %v", got, d.outbox(), exists(d.playsPath()))
	}
	n.reportStatus.Store(0)
	clock = time.Now // set: reading starts now, the listen before stays
	d.sendNow(t)
	if !exists(d.playsPath()) || len(n.reported()) != 1 {
		t.Fatalf("plays %v, %q", exists(d.playsPath()), n.reported())
	}
}

// A listen dated past the hour to come (logged under a clock since set back) isn't taken and
// doesn't move the mark; the same listen in the log and in .sent in one read counts once.
func TestListensLateAndTwice(t *testing.T) {
	card := t.TempDir()
	m, _ := reportSongs(card)
	now := time.Now()
	line := listen("Miles", "Kind", "Blue", 2, now.Unix()-10)
	os.WriteFile(filepath.Join(card, ".scrobbler.log"), []byte(logHeader+line+listen("Miles", "Kind", "So What", 3, now.Unix()+7200)), 0o644)
	os.WriteFile(filepath.Join(card, ".scrobbler.log.sent"), []byte(line), 0o644)
	rs, next := listens(card, m, playsMark{At: now.Unix() - 100}, now)
	if len(rs) != 1 || rs[0].Item != "blue1" || next.At != now.Unix()-10 {
		t.Fatalf("%+v, mark %d", rs, next.At-now.Unix())
	}
}

// Turned off, or signed out, while a send runs: it stops there. A song streamed with reporting
// off, or after Sign Out, leaves nothing to send. On again reads on from then, even after a read
// that finished once it was off.
func TestReportsStopInFlight(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1000}, report{Kind: "played", Item: "al0.1", At: 2000})
	n.onReport = func() {
		if len(n.reported()) == 0 {
			d.do("reportplays") // off, as the first is being sent
		}
	}
	d.sendNow(t)
	if got := n.reported(); len(got) != 1 || len(d.outbox()) != 0 {
		t.Fatalf("%q outbox %+v", got, d.outbox())
	}
	n.onReport = nil
	n.reportStatus.Store(503)
	d.streamStarted(jellyfin.Song{ID: "al0.2"})
	d.bg.Wait()
	d.notePlays(func(*playsMark) {}) // a read that ended after Off
	if m := d.do("reportplays").Message; m != "Report Plays to Server: On" || exists(d.playsPath()) || len(d.outbox()) != 0 {
		t.Fatalf("%q plays %v outbox %+v", m, exists(d.playsPath()), d.outbox())
	}
	d.do("signout")
	d.bg.Wait()
	d.streamStarted(jellyfin.Song{ID: "al0.3"})
	d.bg.Wait()
	if len(d.outbox()) != 0 {
		t.Fatalf("outbox %+v", d.outbox())
	}
}

// A play sent with no answer may have counted: it is logged, marked tried, and before it goes
// again the server's last-played date is asked; equal to its own, it is not sent twice.
func TestReportsNoAnswer(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	r := report{Kind: "played", Item: "al0.0", At: 1700000000}
	d.rep.box.add(r)
	n.dropReply.Store(true)
	d.sendNow(t)
	if ob := d.outbox(); len(ob) != 1 || !ob[0].Tried {
		t.Fatalf("outbox %+v", ob)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt")); !strings.Contains(string(b), "reports: played al0.0 at 2023-11-14T22:13:20Z: no answer, may count twice") {
		t.Fatalf("log %s", b)
	}
	n.dropReply.Store(false)
	d.sendNow(t)
	if got := n.reported(); len(got) != 1 || len(d.outbox()) != 0 {
		t.Fatalf("sent again: %q, outbox %+v", got, d.outbox())
	}
	// Tried, but the server has another date: it hadn't counted, and goes.
	d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1700000500, Tried: true})
	d.sendNow(t)
	if got := n.reported(); len(got) != 2 || len(d.outbox()) != 0 {
		t.Fatalf("%q, outbox %+v", got, d.outbox())
	}
}

// A tried play is compared with the server's last-played date to the second, whatever its
// fraction or zone: the same, it had counted and is dropped unsent; another, or one that can't be
// read, and it goes.
func TestReportsTriedCompare(t *testing.T) {
	for _, c := range []struct {
		server string
		sent   bool
	}{
		{"2023-11-14T22:13:20.123Z", false},
		{"2023-11-14T22:13:20.1234567", false}, // no zone: UTC
		{"2023-11-14T22:13:21.000Z", true},
		{"2023-11-14T22:13:20", false},
		{"not a date", true},
	} {
		n := &nav{}
		d := newTestDaemon(t, n, "", true)
		n.lastPlayed.Store("al0.0", c.server)
		d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1700000000, Tried: true})
		d.sendNow(t)
		if sent := len(n.reported()) == 1; sent != c.sent || len(d.outbox()) != 0 {
			t.Errorf("%s: sent %v, outbox %+v", c.server, sent, d.outbox())
		}
	}
}

// An answer cut short after its status (the body lost) may have counted: marked tried too.
func TestReportsCutAnswer(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	d.rep.box.add(report{Kind: "played", Item: "al0.0", At: 1700000000})
	n.cutBody.Store(true)
	d.sendNow(t)
	if ob := d.outbox(); len(ob) != 1 || !ob[0].Tried {
		t.Fatalf("outbox %+v", ob)
	}
	n.cutBody.Store(false)
	d.sendNow(t)
	if got := n.reported(); len(got) != 1 || len(d.outbox()) != 0 {
		t.Fatalf("sent again: %q, outbox %+v", got, d.outbox())
	}
}

// nowLine writes Q2 Pod's .now line and has the app read it at at; the reports sent since.
func (d *tdaemon) nowLine(t *testing.T, n *nav, at time.Time, line string) []string {
	t.Helper()
	before := len(n.reported())
	if err := os.WriteFile(filepath.Join(d.tmp, "q2-jellyfin.now"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.nowTick(at)
	d.bg.Wait()
	return n.reported()[before:]
}

// nowSteps checks each line's reports in turn.
func (d *tdaemon) nowSteps(t *testing.T, n *nav, t0 time.Time, steps []nowStep) {
	t.Helper()
	for _, s := range steps {
		if got := d.nowLine(t, n, t0.Add(s.at), s.line); !slices.Equal(got, s.want) {
			t.Fatalf("%q at %v: %q, want %q", s.line, s.at, got, s.want)
		}
	}
}

type nowStep struct {
	at   time.Duration
	line string
	want []string
}

// U31, Q2 Pod's "now": every song of a URL queue is reported as the firmware's .now tells it: a
// start (Playing) as it plays, Progress for a pause or play at once and for its position at most
// every nowEvery, Stopped at the position last read when the next song starts or it stops. A
// synced song in the queue sends nothing (Q2 Pod's log has it); a line read again changes nothing;
// a new queue's songs are known by its seq. A start the server didn't take waits in the outbox as
// a play at the time the song began.
func TestReportsNow(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0", "", "al0.1"})
	t0 := time.Now()
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t0\tplaying\t0\t200", []string{"start al0.0"}},
		{5 * time.Second, "100\t0\tplaying\t5\t200", nil}, // too soon for Progress
		{25 * time.Second, "100\t0\tplaying\t25\t200", []string{"progress al0.0 25 false"}},
		{27 * time.Second, "100\t0\tpaused\t27\t200", []string{"progress al0.0 27 true"}},
		{29 * time.Second, "100\t0\tplaying\t27\t200", []string{"progress al0.0 27 false"}},
		{32 * time.Second, "100\t0\tplaying\t30\t200", nil},
		{60 * time.Second, "100\t1\tplaying\t0\t180", []string{"stop al0.0 300000000"}}, // synced: no start
		{70 * time.Second, "100\t1\tplaying\t10\t180", nil},
		{240 * time.Second, "100\t2\tplaying\t0\t200", []string{"start al0.1"}}, // nothing stops for the synced one
		{250 * time.Second, "100\t2\tstopped\t9\t200", []string{"stop al0.1 90000000"}},
		{260 * time.Second, "100\t2\tstopped\t9\t200", nil},                                // read again
		{270 * time.Second, "100\t2\tplaying\t9\t200", []string{"progress al0.1 9 false"}}, // the same song again: no second count
		{275 * time.Second, "999\t0\tplaying\t0\t200", []string{"stop al0.1 90000000"}},    // a queue not known: nothing starts
	})
	d.nowQueued("200", []string{"al1.0"})
	n.reportStatus.Store(503)
	d.nowSteps(t, n, t0, []nowStep{{300 * time.Second, "200\t0\tplaying\t3\t100", nil}})
	ob := d.outbox()
	if len(ob) != 1 || ob[0].Kind != "played" || ob[0].Item != "al1.0" || time.Now().Unix()-3-ob[0].At > 5 {
		t.Fatalf("outbox %+v", ob)
	}
}

// The app may be closed while the queue plays (the firmware keeps .now, and the queue file stays
// in /tmp): opened again on the same song, it carries on without counting it twice; opened on
// another, the song it read last stops and the one playing starts (the ones played whole between
// are not known). A song started paused says so.
func TestReportsNowRestart(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0", "al0.1", "al0.2", "al0.3"})
	t0 := time.Now()
	d.nowSteps(t, n, t0, []nowStep{{0, "100\t0\tplaying\t10\t200", []string{"start al0.0"}}})
	again := func() *tdaemon {
		t.Helper()
		d2 := newTestDaemon(t, n, "urls now", true)
		b, err := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.queue"))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d2.tmp, "q2-jellyfin.queue"), b, 0o644)
		return d2
	}
	d = again()
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t0\tplaying\t15\t200", []string{"progress al0.0 15 false"}},
		{30 * time.Second, "100\t0\tplaying\t40\t200", []string{"progress al0.0 40 false"}},
	})
	d = again()
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t3\tpaused\t30\t200", []string{"stop al0.0 400000000", "start al0.3", "progress al0.3 30 true"}},
	})
	if b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-jellyfin.queue")); strings.Contains(string(b), "ApiKey") || strings.Contains(string(b), "tok") {
		t.Fatalf("%s", b)
	}
}

// Reporting off: nothing goes, and the song playing when it comes on again is reported at the
// next line. Signed out: nothing goes and the queue is forgotten. Firmware without "now": .now
// isn't read.
func TestReportsNowOffSignedOut(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0", "al0.1"})
	t0 := time.Now()
	d.do("reportplays") // off
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t0\tplaying\t0\t200", nil},
		{30 * time.Second, "100\t0\tplaying\t30\t200", nil},
	})
	d.do("reportplays") // on
	d.nowSteps(t, n, t0, []nowStep{
		{40 * time.Second, "100\t0\tplaying\t40\t200", []string{"start al0.0"}},
	})
	d.do("signout")
	d.bg.Wait()
	if exists(filepath.Join(d.tmp, "q2-jellyfin.queue")) {
		t.Fatal("the queue is kept after Sign Out")
	}
	d.nowSteps(t, n, t0, []nowStep{
		{50 * time.Second, "100\t0\tplaying\t50\t200", nil},
		{60 * time.Second, "100\t1\tplaying\t0\t200", nil},
	})

	n2 := &nav{}
	old := newTestDaemon(t, n2, "urls", true)
	old.nowQueued("100", []string{"al0.0"})
	if got := old.nowLine(t, n2, t0, "100\t0\tplaying\t0\t200"); len(got) != 0 {
		t.Fatalf("without now: %q", got)
	}
}

// Q2 Pod writes stopped on Stop Streaming Services, Rockbox and after a firmware restart, the last
// one keeping its seq, n and sec: a song whose start was sent ends there; one never started (the
// app opened after it, or reporting was off as it began) sends no Stopped the server never had a
// Playing for, and a later play of it starts it.
func TestReportsNowStoppedNotStarted(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0", "al0.1"})
	t0 := time.Now()
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t0\tstopped\t50\t200", nil}, // the app's first read
		{10 * time.Second, "100\t0\tplaying\t50\t200", []string{"start al0.0"}},
		{20 * time.Second, "100\t0\tstopped\t60\t200", []string{"stop al0.0 600000000"}}, // a firmware restart
	})
	d.do("reportplays") // off
	d.nowSteps(t, n, t0, []nowStep{{30 * time.Second, "100\t1\tplaying\t0\t200", nil}})
	d.do("reportplays") // on
	d.nowSteps(t, n, t0, []nowStep{{40 * time.Second, "100\t1\tstopped\t5\t200", nil}})
}

// The app stays up while its URL queue plays, so every song is reported: a playing line read keeps
// it for idleAfter; paused or stopped (no line written) it exits idleAfter after the queue last
// played; a line of another queue keeps nothing. Reporting off, or signed out, it exits as before.
func TestReportsNowKeepsAppUp(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0"})
	t0 := time.Now().Add(2 * idleAfter) // long after the last press
	if !d.idle(t0) {
		t.Fatal("not idle with nothing playing")
	}
	d.nowLine(t, n, t0, "100\t0\tplaying\t0\t200")
	d.nowLine(t, n, t0.Add(5*time.Second), "100\t0\tpaused\t5\t200")
	if d.idle(t0.Add(idleAfter - time.Second)) {
		t.Fatal("exits while its queue played less than idleAfter ago")
	}
	if !d.idle(t0.Add(idleAfter + time.Second)) {
		t.Fatal("paused idleAfter: still up")
	}
	d.nowLine(t, n, t0.Add(time.Hour), "999\t0\tplaying\t0\t200") // another queue's
	if !d.idle(t0.Add(time.Hour + time.Second)) {
		t.Fatal("kept up by a queue not its own")
	}
	d.nowLine(t, n, t0.Add(2*time.Hour), "100\t0\tplaying\t10\t200")
	d.do("reportplays") // off
	if !d.idle(t0.Add(2*time.Hour + time.Second)) {
		t.Fatal("reporting off: still up")
	}
	d.do("reportplays") // on
	d.do("signout")
	d.bg.Wait()
	if !d.idle(t0.Add(2*time.Hour + time.Second)) {
		t.Fatal("signed out: still up")
	}
}

// Repeat one: the same line going back from its last seconds to its first is the song ended and
// played again (Stopped, then a new Playing that counts, as Jellyfin's own apps do); a seek back
// mid-song is Progress. A song the length of which isn't known yet (0) never reads as repeated.
func TestReportsNowRepeat(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0"})
	t0 := time.Now()
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t0\tplaying\t0\t200", []string{"start al0.0"}},
		{30 * time.Second, "100\t0\tplaying\t120\t200", []string{"progress al0.0 120 false"}},
		{60 * time.Second, "100\t0\tplaying\t5\t200", []string{"progress al0.0 5 false"}}, // a seek back
		{90 * time.Second, "100\t0\tplaying\t195\t200", []string{"progress al0.0 195 false"}},
		{100 * time.Second, "100\t0\tplaying\t2\t200", []string{"stop al0.0 1950000000", "start al0.0"}}, // played again
		{130 * time.Second, "100\t0\tplaying\t190\t0", []string{"progress al0.0 190 false"}},
		{160 * time.Second, "100\t0\tplaying\t1\t0", []string{"progress al0.0 1 false"}},
	})
}

// Paused, the firmware writes nothing more; Progress (paused, the same position) goes again every
// nowPaused so the server keeps the session on its dashboard.
func TestReportsNowPausedKeepsSession(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0"})
	t0 := time.Now()
	d.nowSteps(t, n, t0, []nowStep{
		{0, "100\t0\tplaying\t0\t200", []string{"start al0.0"}},
		{10 * time.Second, "100\t0\tpaused\t10\t200", []string{"progress al0.0 10 true"}},
		{10*time.Second + nowPaused - time.Second, "100\t0\tpaused\t10\t200", nil},
		{10*time.Second + nowPaused, "100\t0\tpaused\t10\t200", []string{"progress al0.0 10 true"}},
		{10*time.Second + nowPaused + time.Minute, "100\t0\tpaused\t10\t200", nil},
		{10*time.Second + 2*nowPaused, "100\t0\tpaused\t10\t200", []string{"progress al0.0 10 true"}},
		{10*time.Second + 3*nowPaused, "100\t0\tstopped\t10\t200", []string{"stop al0.0 100000000"}},
		{10*time.Second + 5*nowPaused, "100\t0\tstopped\t10\t200", nil},
	})
}

// A start told by .now writes no mark in plays.json on the card: a URL queue's streamed songs are
// never in Q2 Pod's log, so there is nothing to keep from counting twice.
func TestReportsNowNoPlaysMark(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "urls now", true)
	d.nowQueued("100", []string{"al0.0"})
	if got := d.nowLine(t, n, time.Now(), "100\t0\tplaying\t0\t200"); !slices.Equal(got, []string{"start al0.0"}) {
		t.Fatalf("%q", got)
	}
	if exists(d.playsPath()) {
		b, _ := os.ReadFile(d.playsPath())
		t.Fatalf("plays.json written: %s", b)
	}
}
