package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/creds"
	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

const (
	answerIn    = 8 * time.Second  // every Q2SPOT/1 reply within this (the firmware gives up at 15 s)
	idleAfter   = 30 * time.Minute // exit 0 after this long with no request, sync or stream
	previewTTL  = time.Minute      // a preview older than this is computed again when Sync is opened
	clearArmFor = 10 * time.Second // Clear Playback Cache: a second press within this clears
	logoutIn    = 5 * time.Second  // Sign Out's word to the server, in the background
)

var errSignedOut = errors.New("signed out")

// settings is .jellyfin/settings.json. DefaultQuality is Sync Quality (its name up to 0.6, kept
// in the file). StreamQuality "" (Same as Default up to 0.6) is Sync Quality's: it takes that
// value when Sync Quality next changes (setdefq).
type settings struct {
	DefaultQuality string `json:"defaultQuality,omitempty"`
	StreamQuality  string `json:"streamQuality,omitempty"`
	PlaybackCache  string `json:"playbackCache,omitempty"` // cacheSizes; "" is defaultCache
	KeepFree       string `json:"keepFree,omitempty"`      // keepFreeSizes; "" is defaultKeepFree
	// Swipe is Swipe Between Sections (swipe.go): "" until the firmware first offers sections, then
	// "on" (the default) or "off"; "on" turns "off" when the firmware no longer offers them.
	Swipe string `json:"swipe,omitempty"`
	// Sort is each list's sort ("albums", "artists", "playlists"; sort.go); none is Name, A to Z.
	Sort map[string]listSort `json:"sort,omitempty"`
	// ReportPlays is Report Plays to Server (report.go): "off", or "" for on.
	ReportPlays string `json:"reportPlays,omitempty"`
}

// def is Sync Quality, every synced song's: Original (the server's file, no transcoding) unless
// an Opus transcode is chosen.
func (s settings) def() string { return quality.Normal(s.DefaultQuality) }

// stream is Streaming Quality: Sync Quality's until one is chosen (see settings).
func (s settings) stream() string {
	if s.StreamQuality == "" {
		return s.def()
	}
	return quality.Normal(s.StreamQuality)
}

// stateKeys is the state file: version, and hold=1 while a sync, a stream or Quick Connect is under way.
type stateKeys struct {
	st   *q2.State
	mu   sync.Mutex
	hold bool
}

func (s *stateKeys) setHold(h bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = h
	return s.flush()
}

func (s *stateKeys) remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Remove()
}

func (s *stateKeys) flush() error {
	m := map[string]string{"version": version}
	if s.hold {
		m["hold"] = "1"
	}
	return s.st.Write(m)
}

type preview struct {
	gen    int
	at     time.Time
	done   bool
	plan   *plan.Plan
	err    error
	cancel context.CancelFunc // stops its fetches (dropPreview)
}

type libJob struct {
	seq    int64
	done   bool
	ok     bool
	msg    string
	doneAt time.Time
}

// daemon is the running app: the Q2SPOT/1 handler (device) and what it drives.
type daemon struct {
	dir, card, tmp string // the app folder (.jellyfin), the card's root, /tmp
	caps           fw.Caps
	log            *applog.Log
	state          *stateKeys
	login          login // sign-in on the Q2 (login.go)
	runner         *syncer.Runner
	stream         *streamer
	newClient      func(jellyfin.Creds) (*jellyfin.Client, error)
	free           func(path string) int64
	localIP        func() string // the Q2's Wi-Fi address, "" without Wi-Fi (q2.LocalIP)
	wifiOff        func() bool   // the user's Wi-Fi switch is off (q2.WiFiSwitchedOff)
	answerIn       time.Duration
	selMu          sync.Mutex // held from loading selection.json to saving it, so no edit is lost

	mu                 sync.Mutex
	cl                 *jellyfin.Client
	server, user, host string
	lastReq            time.Time
	clearArmed         time.Time         // Clear Playback Cache's first press
	sectionsShown      bool              // the last menu reply was sections: Settings shows a stream's failure
	shuffles           map[string]uint64 // each list's Random order (sort.go)
	albumsAll          *albumsFetch      // every album, being fetched for Random (everyAlbum)
	autoStart          bool              // Sync Favourites: start the sync once its preview is ready
	searchIn           string            // what a typed search searches: the page with a search box asked for last (searchSections)
	windowAt           map[string]int    // a paged section's window, from its last page asked for (sectionCursor)
	trimTimer          *time.Timer
	gen                int // bumped by every selection change
	preview            *preview
	showDone           bool // the finished sync's page, until Check Again, a new Start, or a choice made after it ended
	lib                *libJob
	libTaken           int64                       // the library request whose answer was taken last (libPoll)
	libAgain           bool                        // a request was wanted while one awaited its answer: sent once that comes
	libRest            [2][]string                 // the import and add paths the request on file had no room for (I20)
	resolved           map[string][]jellyfin.Song  // selection key -> its songs, as last fetched
	artistAlbums       map[string][]jellyfin.Album // artist id -> its albums, as last fetched
	lists              listCache                   // whole Artists and Playlists lists, between a list's pages
	libCount           [2]int                      // albums, playlists of the Entire Library, as last resolved
	playSeq            int                         // the latest play request; an older one still waiting is dropped
	playMu             sync.Mutex                  // one streamer.start at a time
	playKey            string                      // the list ("kind:id") the latest Play started in (status.go)
	polls              pollCache                   // song lists kept while a song buffers (status.go)
	queue              queueCache                  // the play queue as last read (playqueue.go)
	art                artState                    // list covers (art.go)
	man                *library.Manifest
	manErr             error // the manifest is there but can't be read: man is empty, not the card's
	manAt              time.Time
	manSize            int64
	wasBusy            bool            // the runner, at the last tick
	noWiFiAt           time.Time       // when a request first found no Wi-Fi; zero while there is (wifi)
	lyricsTried        map[string]bool // songs the lyrics backfill asked the server for, in this run
	rep                reporter        // plays reported to the server (report.go)

	bg sync.WaitGroup // the work replies leave running (ping, preview, play, sign-out's stops); tests wait for it
}

// spawn runs f in the background, counted in d.bg.
func (d *daemon) spawn(f func()) {
	d.bg.Add(1)
	go func() { defer d.bg.Done(); f() }()
}

func newDaemon(dir, card, tmp string) *daemon {
	os.MkdirAll(dir, 0o755)
	lg := applog.Open(filepath.Join(dir, "log.txt"), 1<<20)
	creds.Logf = lg.Printf // why a saved sign-in can't be read, on the card
	d := &daemon{dir: dir, card: card, tmp: tmp, log: lg,
		caps:  fw.ReadCaps(filepath.Join(tmp, "q2-addon.caps")),
		state: &stateKeys{st: &q2.State{Path: filepath.Join(tmp, "q2-jellyfin.state")}},
		newClient: func(c jellyfin.Creds) (*jellyfin.Client, error) {
			return jellyfin.New(c, tlsroots.Client(15*time.Second))
		},
		free: freeBytes, localIP: q2.LocalIP, wifiOff: q2.WiFiSwitchedOff, answerIn: answerIn, lyricsTried: map[string]bool{},
		lastReq: time.Now(), resolved: map[string][]jellyfin.Song{}, artistAlbums: map[string][]jellyfin.Album{}}
	d.rep.box = outbox{path: filepath.Join(dir, "outbox.json"), logf: lg.Printf}
	d.loadServer()
	d.runner = syncer.New(card, src{d}, lg)
	d.runner.OnDone = d.syncDone
	d.runner.Free = func() int64 { return d.free(d.card) }
	d.runner.Reserve = func() int64 { return keepFreeBytes(d.settings().KeepFree) }
	d.stream = newStreamer(card, filepath.Join(tmp, "q2-jellyfin.play"), filepath.Join(tmp, "q2-jellyfin.playing"),
		d.caps, src{d}, d.manifest, syncer.Net, time.Second)
	d.stream.log = lg
	d.stream.quality = func() string { return d.settings().stream() }
	d.stream.cacheLimit = func() int64 { return cacheBytes(d.settings().PlaybackCache) }
	d.stream.moving = d.runner.Moving
	d.stream.started = d.streamStarted
	d.stream.queued = d.nowQueued
	d.stream.hearts = d.hearts
	d.runner.FromCache = d.stream.forSync // a song played before needs no download (cache.go)
	d.stream.syncing = func() bool { st := d.runner.Snapshot().State; return st == "running" || st == "paused" }
	d.stream.clean()               // a stream an earlier run left (M26)
	os.Remove(d.stream.urlsFile()) // and its URL queue, with the token, after a crash
	d.state.setHold(false)         // the state file, with the version
	d.logCrash()
	return d
}

// logCrash puts in log.txt what the launcher kept of a run that failed (/tmp/q2-jellyfin.crash:
// its stderr, a panic's trace, and its exit code), so a crash or an OOM kill leaves a trace on the
// card (M43). A long trace keeps its start and the exit line.
func (d *daemon) logCrash() {
	f := filepath.Join(d.tmp, "q2-jellyfin.crash")
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	os.Remove(f)
	s := strings.TrimSpace(string(b))
	if len(s) > 4<<10 {
		s = strings.ToValidUTF8(s[:4<<10], "") + "\n…\n" + s[strings.LastIndex(s, "\n")+1:]
	}
	d.log.Printf("the last run failed:\n%s", s)
}

func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// client is the signed-in server client, loaded from the card's credentials; nil when signed out.
func (d *daemon) client() *jellyfin.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cl != nil {
		return d.cl
	}
	c, ok := creds.Load(d.dir)
	if !ok {
		return nil
	}
	cl, err := d.newClient(c)
	if err != nil {
		return nil
	}
	cl.Log = d.log
	d.cl, d.server, d.user, d.host = cl, c.Server, c.User, c.Server
	if u, err := url.Parse(c.Server); err == nil && u.Hostname() != "" {
		d.host = u.Hostname()
	}
	tlsroots.SetDateURL(c.Server)
	return cl
}

func (d *daemon) hostName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.host
}

// signOut forgets the sign-in at once; the sync and stream stop in the background (R27: a reply
// never waits for a download to notice its cancel). The sync is cancelled, not paused: a later
// sign-in, maybe to another server, never resumes this account's plan (M12). Finished songs stay.
// Credentials the card won't let go of change nothing: the next request would load them again.
// The server is told the session ended (best effort); an API key has no session to end.
func (d *daemon) signOut() error {
	if err := creds.Remove(d.dir); err != nil {
		d.log.Printf("sign out: %v", err)
		return err
	}
	d.login.mu.Lock()
	d.login.file = "" // a sign-in file's outcome was about the sign-in that ends here
	d.login.mu.Unlock()
	d.mu.Lock()
	old := d.cl
	d.cl = nil
	d.dropPreview()
	d.dropAlbumsFetch()
	d.autoStart = false // a Sync Favourites or Hold pressed before never starts the next account's sync
	d.playSeq++         // a play still waiting for playMu is dropped
	d.mu.Unlock()
	d.spawn(d.runner.Cancel)
	d.stopReports("") // the account's: another sign-in may be another server's
	d.dropFavs()      // and its heart presses not taken yet
	d.spawn(func() {  // under playMu, so a start already running finishes first and is then stopped
		d.playMu.Lock()
		defer d.playMu.Unlock()
		d.stream.stop()
		os.Remove(d.stream.queueFile()) // the account's: ⋯ shows nothing until the next Play
	})
	if old != nil && !old.KeyAuth() {
		d.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), logoutIn)
			defer cancel()
			old.Logout(ctx) // the client logs its request, redacted
		})
	}
	return nil
}

// signOutFailed is Sign Out's reply when the credentials can't be deleted.
const signOutFailed = "Couldn't sign out: the card can't be written"

// src lends the current client to the runner and the streamer, which outlive a sign-in.
type src struct{ d *daemon }

func (s src) MediaURL(id, q string) string {
	if c := s.d.client(); c != nil {
		return c.MediaURL(id, q)
	}
	return ""
}

func (s src) PlayURL(id, q string) string {
	if c := s.d.client(); c != nil {
		return c.PlayURL(id, q)
	}
	return ""
}

func (s src) PlayerURL(id, q string) string {
	if c := s.d.client(); c != nil {
		return c.PlayerURL(id, q)
	}
	return ""
}

func (s src) PlayerCoverURL(id string) string {
	if c := s.d.client(); c != nil {
		return c.PlayerCoverURL(id)
	}
	return ""
}

func (s src) CoverURL(id string) string {
	if c := s.d.client(); c != nil {
		return c.CoverURL(id)
	}
	return ""
}

// Lyrics is the song's lyrics from the server, as LRC (syncer.LyricsSource).
func (s src) Lyrics(ctx context.Context, id string) (string, error) {
	if c := s.d.client(); c != nil {
		return c.Lyrics(ctx, id)
	}
	return "", errSignedOut
}

func (s src) Get(ctx context.Context, u string, from int64) (*http.Response, error) {
	if c := s.d.client(); c != nil {
		return c.Get(ctx, u, from)
	}
	return nil, errSignedOut
}

// manifest is the card's manifest, read again only when the file changed; an empty one when it
// can't be read (manifestErr says so).
func (d *daemon) manifest() *library.Manifest {
	m, _ := d.manifestErr()
	return m
}

// manifestErr is manifest with why it couldn't be read: then nothing is known of what is on the
// card, and the Sync page offers no preview, which would plan the whole selection again (M7).
func (d *daemon) manifestErr() (*library.Manifest, error) {
	p := filepath.Join(d.card, ".jellyfin", "manifest.json")
	st, err := os.Stat(p)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.man == nil || err != nil || !st.ModTime().Equal(d.manAt) || st.Size() != d.manSize {
		d.man, d.manErr = library.LoadManifest(p)
		d.manAt, d.manSize = time.Time{}, 0
		if err == nil {
			d.manAt, d.manSize = st.ModTime(), st.Size()
		}
	}
	return d.man, d.manErr
}

func (d *daemon) selPath() string { return filepath.Join(d.dir, "selection.json") }

// selection is selection.json. A quality saved with an item (each had its own up to 0.6) is not
// used: every item syncs at Sync Quality (plan.Compute).
func (d *daemon) selection() (selection.Set, error) { return selection.Load(d.selPath()) }

// saveSelection writes s, every item's quality as Sync Quality (only written, for the file to stay
// what earlier versions read), and the Sync page plans again.
func (d *daemon) saveSelection(s selection.Set) error {
	q := d.settings().def()
	for i := range s.Items {
		s.Items[i].Quality = q
	}
	err := s.Save(d.selPath())
	d.changed()
	return err
}

// changed makes the Sync page plan again after a change to what or how it syncs. The finished
// sync's page gives way to the new preview, but not while a sync runs or is paused: its summary is
// still to come (M38).
func (d *daemon) changed() {
	st := d.runner.Snapshot().State
	d.mu.Lock()
	d.gen++
	if st != "running" && st != "paused" {
		d.showDone = false
	}
	d.mu.Unlock()
}

// resetSelection puts a damaged selection.json aside as selection.json.bad, so the user can start
// again on the Q2 (M21).
func (d *daemon) resetSelection() q2spot.Reply {
	if err := os.Rename(d.selPath(), d.selPath()+".bad"); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.Printf("selection: %v", err)
		return q2spot.Fail(q2spot.Error, "Couldn't reset the selection")
	}
	d.changed()
	return q2spot.Reply{Message: "Selection reset"}
}

// damagedSelection is a page when selection.json can't be read (M21).
func damagedSelection() q2spot.Reply {
	return q2spot.Reply{Rows: []q2spot.Row{info("Selection file is damaged"), act("resetsel", "Reset Selection", "")}}
}

const selDamaged = "Selection file is damaged: reset it in On This Q2"

func (d *daemon) settings() settings {
	var s settings
	if b, err := os.ReadFile(filepath.Join(d.dir, "settings.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (d *daemon) saveSettings(s settings) error {
	b, _ := json.Marshal(s)
	return q2.WriteFile(filepath.Join(d.dir, "settings.json"), b)
}

// tick keeps the state file's hold=1 while files move (a sync, or a stream fetching) or a Quick
// Connect code waits for approval, and takes the firmware's answer to a library request as soon as it comes.
// A run's end, however it ends (done, paused, stopped), counts as activity: its page stays as long
// as after a press (I15, review G3 Minor 8).
func (d *daemon) tick() {
	busy := d.runner.Busy()
	d.mu.Lock()
	if d.wasBusy && !busy {
		d.lastReq = time.Now()
	}
	d.wasBusy = busy
	d.mu.Unlock()
	d.state.setHold(busy || d.stream.busy() || d.qcWaiting())
	d.takeFavs(time.Now()) // fav.go: before reportTick, which then sends them
	d.nowTick(time.Now())  // now.go: a URL queue's songs, as Q2 Pod tells them
	d.reportTick(time.Now())
	if d.caps.Library {
		d.mu.Lock()
		d.libPoll()
		d.mu.Unlock()
	}
}

// idle is whether the app may exit: no request for idleAfter (the end of a sync counts as one), no
// sync or stream, no library request awaiting its answer (I15) for less than libWait, and its URL
// queue not played for idleAfter (now.go).
func (d *daemon) idle(now time.Time) bool {
	d.mu.Lock()
	last, lib := d.lastReq, d.libWaiting(now)
	d.mu.Unlock()
	return now.Sub(last) >= idleAfter && !lib && !d.runner.Busy() && !d.stream.active() && !d.qcWaiting() &&
		!d.nowKeeps(now) // now.go: its URL queue playing, to report each song
}

// libWait bounds how long a library request awaiting its answer keeps the app up, so its follow-ups
// go at once (libPoll). The firmware may hold one for ever (playback under a lit screen, a page it
// won't leave); it answers with the app gone too, and the next run takes that answer.
const libWait = 2 * time.Hour

// libWaiting is whether the followed library request awaits its answer, sent less than libWait
// ago: its seq is the time it was written. d.mu is held.
func (d *daemon) libWaiting(now time.Time) bool {
	return d.lib != nil && !d.lib.done && now.Sub(time.Unix(0, d.lib.seq)) < libWait
}

// close ends what the app runs before it exits; a sync paused here resumes at the next Sync.
func (d *daemon) close(sock string) {
	d.cancelQC()
	d.mu.Lock()
	if d.trimTimer != nil {
		d.trimTimer.Stop()
	}
	d.mu.Unlock()
	d.stream.stop()
	d.runner.Pause()
	d.state.remove()
	os.Remove(sock)
}

// serve answers Q2SPOT/1 on sock, and exits 0 once idle. It ticks every second while something
// is under way (a sync, a stream, Quick Connect, a library request waiting for its answer), else
// every 5 s, so an idle app wakes the CPU a fifth as often (audit B12).
func (d *daemon) serve(sock string) error {
	go func() {
		for {
			wait := tickIdle
			if d.urgent() {
				wait = time.Second
			}
			time.Sleep(wait)
			d.tick()
			if d.idle(time.Now()) {
				d.log.Printf("idle, exiting")
				d.close(sock)
				os.Exit(0)
			}
		}
	}()
	return q2spot.Serve(sock, d.device)
}

// tickIdle is how often an app with nothing under way ticks.
const tickIdle = 5 * time.Second

// urgent is whether the tick should come every second: a sync, a stream, a Quick Connect code or a
// library request waiting for the firmware's answer.
func (d *daemon) urgent() bool {
	d.mu.Lock()
	lib := d.libWaiting(time.Now())
	d.mu.Unlock()
	return lib || d.runner.Busy() || d.stream.busy() || d.qcWaiting()
}
