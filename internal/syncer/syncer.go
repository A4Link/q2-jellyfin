// Package syncer runs a sync plan: renames, downloads (to .part, fsynced, renamed), covers,
// playlists, then removals (held back while failures look server-wide, I2). The manifest never names a file
// that isn't there; downloads are recorded in batches, so a crash re-fetches at most a batch. It
// pauses when the card goes, the server can't be reached or the card is full, and a plan run again
// continues where it stopped.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
)

var ErrBusy = errors.New("a sync is already running")

// ErrOffline, ErrCardFull and ErrRefused pause a sync: every song would fail the same way.
var (
	ErrOffline  = fmt.Errorf("lost the server: %w", jellyfin.ErrUnreachable)
	ErrCardFull = errors.New("the card is full")
	// ErrSpaceLow is a run stopped at the card's reserve (Runner.Reserve), before it was full.
	ErrSpaceLow = errors.New("the card is nearly full")
	ErrRefused  = errors.New("the server refused this account") // wraps the server's error: its code says why
)

// SpaceLowError is a run stopped at the card's reserve: Left bytes were free, and the next song
// (Next bytes) would have left fewer than Reserve.
type SpaceLowError struct {
	Left, Next, Reserve int64
	Held                bool // songs taken out of Sync would have made room, but songs failed first (I2)
	Retry               bool // held by a failure the whole sync shares: Resume alone can make the removals
}

func (e *SpaceLowError) Error() string {
	return fmt.Sprintf("the card is nearly full: %d bytes free, keeping %d free", e.Left, e.Reserve)
}
func (e *SpaceLowError) Is(target error) bool { return target == ErrSpaceLow }

// MediaSource is the part of *jellyfin.Client the runner uses.
type MediaSource interface {
	MediaURL(id, quality string) string
	CoverURL(id string) string
	Get(ctx context.Context, u string, from int64) (*http.Response, error)
}

type Failure struct {
	SongID, Title, Op string // Op: download or rename
	HTTP, Code        int
	At                time.Time
	Msg               string // redacted
}

type Snapshot struct {
	State                 string // idle, running, paused, done or error
	Done, Total, Failed   int
	BytesDone, BytesTotal int64
	Permille              int
	ETA                   time.Duration
	Current               string // "Artist – Title" of the song in flight
	Err                   error
	ErrAt                 time.Time // when the run stopped with Err
	Renamed, Removed      int       // songs this run renamed and removed
	Held                  int       // songs taken out of Sync that this run left, as songs failed (I2)
	Deleted               int       // files an earlier run left listed (Stale), deleted at this run's start
	HeldRetry             bool      // Held came from a shared failure (a connection, the card): Retry Failed is the way out
	Failures              []Failure
}

type Runner struct {
	// Backoff is the wait after each failed try of a song; one try per entry.
	Backoff     []time.Duration
	CardMounted func() bool
	Now         func() time.Time
	Net         *NetSlot
	// OnDone is called, with no lock held, after each run that ends in state "done". It must not
	// block. The runner is Busy until it returns.
	OnDone func(plan.Plan)
	// Free and Reserve, when both are set, stop a run before a download would leave the card with
	// less than Reserve bytes free: it pauses with a *SpaceLowError (ErrSpaceLow), after writing the
	// playlists for what it has, so the card never fills up (the Q2 needs room of its own).
	Free    func() int64
	Reserve func() int64
	// FromCache, when set, moves a copy of w's song already on the card at w's quality and whole
	// (the playback cache) to w.Path, and reports its size; false when there is none. Such a song
	// needs no download, and no room: it is a rename on the same card.
	FromCache func(w plan.Want) (int64, bool)

	root string
	src  MediaSource
	log  *applog.Log
	onOp func(op, path string) // tests: "download" and "remove", in order

	// fileMu is held by a run, or an edit, while it has the manifest loaded: a whole load and save
	// takes seconds on a big library, and Snapshot, Busy and Moving must not wait for it (r.mu).
	// editMu, taken before fileMu, lets edits wait for each other: fileMu is then only a run's.
	fileMu, editMu sync.Mutex

	mu     sync.Mutex
	plan   *plan.Plan
	moving map[string]bool // the songs plan renames, replaces or removes, for Moving
	stopAs string
	ending bool // a run is done, and its OnDone hasn't returned yet
	cancel context.CancelFunc
	done   chan struct{}
	// stat is the run as Snapshot shows it, kept as it goes: BytesDone counts the files finished,
	// and Snapshot adds the one in flight and works out Permille and ETA.
	stat        Snapshot
	cur, curMax int64 // the file in flight: bytes on the card, its planned size
	part        string
	meter       meter
}

func New(root string, src MediaSource, log *applog.Log) *Runner {
	return &Runner{Backoff: []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second},
		CardMounted: q2.CardMounted, Now: time.Now, Net: Net, root: root, src: src, log: log, stat: Snapshot{State: "idle"}}
}

func (r *Runner) manifestPath() string { return filepath.Join(r.root, ".jellyfin", "manifest.json") }

func (r *Runner) logf(format string, a ...any) {
	if r.log != nil {
		r.log.Printf(format, a...) // Printf redacts every line
	}
}

// Start runs p in the background. Files already finished are skipped through the manifest, so a
// plan run again continues where it stopped.
func (r *Runner) Start(p plan.Plan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stat.State == "running" {
		return ErrBusy
	}
	// Older builds kept the whole plan here, megabytes for an Entire Library that ran the app out of
	// memory (I16); nothing reads it.
	library.Remove(r.root, filepath.Join(r.root, ".jellyfin", "pending.json"))
	r.plan = &p
	r.launch()
	return nil
}

// launch starts a run of r.plan; r.mu is held.
func (r *Runner) launch() {
	p := *r.plan
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	// a new run's numbers; ErrAt stays, as it always has, until a stop sets it again
	r.stat = Snapshot{State: "running", Total: len(p.Add) + len(p.Replace), ErrAt: r.stat.ErrAt}
	r.meter, r.cur, r.curMax = meter{}, 0, 0
	for _, ws := range [][]plan.Want{p.Add, p.Replace} {
		for _, w := range ws {
			r.stat.BytesTotal += w.Bytes
		}
	}
	r.moving = p.Moving()
	go r.run(ctx, p, r.done)
}

func (r *Runner) run(ctx context.Context, p plan.Plan, done chan struct{}) {
	defer close(done)
	err := r.work(ctx, p)
	finished := false
	defer func() { // deferred before the unlock below, so it runs after it
		if finished && r.OnDone != nil {
			q := p
			p = plan.Plan{} // so the plan can be freed once OnDone has used it
			r.OnDone(q)
		}
		r.mu.Lock()
		r.ending = false
		r.mu.Unlock()
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stat.Current = ""
	switch {
	case ctx.Err() != nil:
		r.stat.State = r.stopAs
	case errors.Is(err, q2.ErrNoCard):
		r.stat.State, r.stat.Err, r.stat.ErrAt = "paused", q2.ErrNoCard, r.Now()
	case errors.Is(err, ErrOffline) || errors.Is(err, ErrCardFull) || errors.Is(err, ErrRefused) || errors.Is(err, ErrSpaceLow):
		r.stat.State, r.stat.Err, r.stat.ErrAt = "paused", err, r.Now() // Resume, or the next Sync, continues once it is back
		r.logf("sync: paused: %v", err)
	case err != nil:
		r.stat.State, r.stat.Err, r.stat.ErrAt = "error", err, r.Now()
		r.logf("sync: %v", err)
	default:
		r.stat.State, finished, r.ending = "done", true, true
		r.moving = nil // Moving answers only while running or paused
		if r.stat.Failed == 0 {
			r.plan = nil // only Retry Failed reads it again; a big plan is megabytes
		}
	}
}

func (r *Runner) stop(as string) {
	r.mu.Lock()
	if r.stat.State != "running" {
		r.mu.Unlock()
		return
	}
	r.stopAs = as
	r.cancel()
	done := r.done
	r.mu.Unlock()
	<-done
}

// Refresh records keep (plan.Keep: the items holding songs that need no transfer) while no sync
// runs, so an up-to-date preview still updates what each song is on the card for (I12). ErrBusy
// while one runs; that run records them itself.
func (r *Runner) Refresh(keep []plan.Want) error {
	if len(keep) == 0 {
		return nil
	}
	return r.edit(true, func(m *library.Manifest) (bool, error) { return applyKeep(m, keep), nil })
}

// Imported takes the firmware's answer to a library request that imported sent[0], added sent[1],
// and skipped exists (R35). What it imported or added is now a Q2 playlist the app made, so later
// requests replace it by name. A skipped add still under its playlist's own name met a playlist
// the user made: it is renamed "<name> (Jellyfin).m3u" and returned in again, to add once more.
// One skipped under that name too is a clash; its sum is forgotten, so the next preview offers a
// sync, whose request adds it again. Only an exists naming exactly an add of the request
// counts; any other is logged and ignored, and then no add is recorded (one may have been
// skipped: the guard fails closed). ErrBusy while a sync runs or another edit saves: it doesn't
// wait, as its caller holds the daemon's lock.
func (r *Runner) Imported(sent [2][]string, exists []string) (again, clash []string, err error) {
	imports, adds := sent[0], sent[1]
	added, skipped, trusted := map[string]bool{}, map[string]bool{}, true
	for _, f := range adds {
		added[f] = true
	}
	for _, f := range exists {
		if added[f] {
			skipped[f] = true
			continue
		}
		r.logf("library: ignoring \"exists %s\": not an add of this request", f)
		trusted = false
	}
	err = r.edit(false, func(m *library.Manifest) (bool, error) {
		changed, kept := false, map[string]bool{}
		for _, f := range m.Playlists {
			kept[f] = true
		}
		for _, f := range append(append([]string(nil), imports...), adds...) {
			if !kept[f] {
				continue // removed by a sync since the request: no longer the app's (R35)
			}
			if (!added[f] || trusted && !skipped[f]) && !m.Imported[f] {
				m.Imported[f], changed = true, true
			}
		}
		names := make([]string, 0, len(m.Playlists))
		for name := range m.Playlists {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			f := m.Playlists[name]
			switch {
			case !skipped[f]:
			case f != library.PlaylistFile(r.root, name):
				clash = append(clash, f)
				delete(m.PlaylistSums, name) // the next sync writes it again and adds it once the Q2's copy is gone
				changed = true
			default:
				to := r.freePlaylistFile(m, name, true)
				if err := m.MovePlaylist(r.root, name, to); err != nil {
					return changed, err
				}
				again, changed = append(again, to), true
			}
		}
		return changed, nil
	})
	return again, clash, err
}

// edit changes the manifest while no sync runs: a run holds it in memory and saves it whole.
// It waits for another edit, or, unless wait, is ErrBusy (Imported: its caller holds the daemon's
// lock and tries again).
func (r *Runner) edit(wait bool, f func(*library.Manifest) (bool, error)) error {
	if wait {
		r.editMu.Lock()
	} else if !r.editMu.TryLock() {
		return ErrBusy
	}
	defer r.editMu.Unlock()
	if !r.fileMu.TryLock() { // a run's
		return ErrBusy
	}
	defer r.fileMu.Unlock()
	r.mu.Lock()
	running := r.stat.State == "running" // launched, its work not yet holding fileMu
	r.mu.Unlock()
	if running {
		return ErrBusy
	}
	m, err := library.LoadManifest(r.manifestPath())
	if err != nil {
		return err
	}
	changed, err := f(m)
	if changed {
		if serr := m.Save(r.root, r.manifestPath()); err == nil {
			err = serr
		}
	}
	return err
}

// applyKeep records each kept song's items and MetaKey, once it is where the plan puts it (a
// rename that failed is left for the next sync).
func applyKeep(m *library.Manifest, keep []plan.Want) bool {
	changed := false
	for _, w := range keep {
		e := m.Songs[w.Song.ID]
		if e == nil || e.Path != w.Path || e.Quality != w.Quality {
			continue
		}
		if meta := library.MetaKey(w.Song, w.Quality); e.Meta != meta || !slices.Equal(e.Items, w.Items) {
			e.Meta, e.Items, changed = meta, w.Items, true
		}
	}
	return changed
}

// Pause stops after cutting the file in flight; its .part stays for Resume.
func (r *Runner) Pause() { r.stop("paused") }

// Resume continues a paused sync (paused by Pause or by the card going away).
func (r *Runner) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stat.State == "paused" && r.plan != nil {
		r.launch()
	}
}

// Cancel stops and forgets the plan. Finished files and their manifest entries stay.
func (r *Runner) Cancel() {
	r.stop("idle")
	r.mu.Lock()
	r.stat.State, r.stat.Err, r.plan, r.moving, r.stat.Current = "idle", nil, nil, nil, ""
	part := r.part
	r.part = ""
	r.mu.Unlock()
	if part != "" {
		library.Remove(r.root, part)
		library.Remove(r.root, part+".key")
	}
}

// RetryFailed runs the last plan again once it has ended with failures, or is paused (as Resume);
// finished songs are skipped. It reports whether it started a run.
func (r *Runner) RetryFailed() bool {
	r.mu.Lock()
	p := r.plan
	ok := p != nil && (r.stat.State == "error" || r.stat.State == "paused" || r.stat.State == "done" && r.stat.Failed > 0)
	r.mu.Unlock()
	return ok && r.Start(*p) == nil
}

// Forget drops the plan of a run that has ended (done or error), which a new preview replaces:
// Retry Failed then no longer runs it. A paused run keeps it for Resume.
func (r *Runner) Forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stat.State == "done" || r.stat.State == "error" {
		r.plan, r.moving = nil, nil
	}
}

// DropMissing forgets the songs of m, the card's manifest as the caller read it, whose files are
// gone from the card (deleted by hand, or with the folder after the app was removed), so the next
// plan fetches them again (I9). It also forgets the gone files of m.Stale, deleted by a run that
// ended before it saved again. The card is read without the lock, which a reply may wait for;
// nothing is dropped while the card is away, when every file would seem gone. ErrBusy while a sync
// runs.
func (r *Runner) DropMissing(m *library.Manifest) error {
	if !r.CardMounted() {
		return q2.ErrNoCard
	}
	gone := map[string]string{} // id -> its path
	for id, e := range m.Songs {
		if _, err := os.Lstat(e.Path); errors.Is(err, os.ErrNotExist) {
			gone[id] = e.Path
		}
	}
	staleGone := map[string]bool{}
	for _, f := range m.Stale {
		if _, err := os.Lstat(f); errors.Is(err, os.ErrNotExist) {
			staleGone[f] = true
		}
	}
	if len(gone) == 0 && len(staleGone) == 0 || !r.CardMounted() {
		return nil
	}
	return r.edit(true, func(m *library.Manifest) (bool, error) {
		n := 0
		for id, path := range gone {
			if e := m.Songs[id]; e != nil && e.Path == path {
				delete(m.Songs, id)
				n++
			}
		}
		if n > 0 {
			r.logf("sync: %d songs gone from the card, planned again", n)
		}
		was := len(m.Stale)
		m.Stale = slices.DeleteFunc(m.Stale, func(f string) bool { return staleGone[f] })
		return n > 0 || len(m.Stale) < was, nil
	})
}

// Moving reports whether the plan a sync runs, or holds paused, renames, replaces or removes song
// id: the streamer then fetches the song rather than queue a path that is about to go (M24). A
// lookup in a set made once a launch: a synced list starting calls it for each of its songs, and a
// Sync Quality change can replace every song on the card (audit 2026-11-16 #24).
func (r *Runner) Moving(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return (r.stat.State == "running" || r.stat.State == "paused") && r.moving[id]
}

// Busy is whether a sync runs, or has just ended and its OnDone hasn't returned (I15).
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stat.State == "running" || r.ending
}

func (r *Runner) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stat
	s.BytesDone += min(r.cur, r.curMax)
	s.Failures = append([]Failure(nil), r.stat.Failures...)
	switch {
	case s.State == "done":
		s.Permille = 1000
	case s.BytesTotal > 0:
		s.Permille = permille(s.BytesDone, s.BytesTotal)
	default:
		s.Permille = permille(int64(s.Done), int64(s.Total))
	}
	if s.State == "running" {
		s.ETA = r.meter.eta(r.Now(), s.BytesTotal-s.BytesDone)
	}
	return s
}

// maxFailures is how many song failures a run lists, what Q2 Pod pages through. A server that
// fails every song would otherwise keep one per song.
const maxFailures = 500

func (r *Runner) fail(id, title, op string, err error) {
	f := Failure{SongID: id, Title: title, Op: op, At: r.Now(), Msg: applog.Redact(err.Error())}
	var ae *jellyfin.APIError
	if errors.As(err, &ae) {
		f.HTTP, f.Code = ae.HTTP, ae.Code
	}
	r.logf("sync: %s %s failed: %s", op, id, f.Msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stat.Failed++
	if r.stat.Failed > maxFailures { // counted, not listed
		return
	}
	r.stat.Failures = append(r.stat.Failures, f)
}

func (r *Runner) finished(w plan.Want) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stat.Done++
	r.stat.BytesDone += w.Bytes
	r.cur, r.curMax = 0, 0
}

// op tells tests what happened: download, remove, save, playlist, try.
func (r *Runner) op(op, path string) {
	if r.onOp != nil {
		r.onOp(op, path)
	}
}

// check is the gate before every file: stopped, or the card gone (USB storage took it).
func (r *Runner) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.CardMounted() {
		return q2.ErrNoCard
	}
	return nil
}

// cardErr blames a failed card write on the card when it has gone.
func (r *Runner) cardErr(err error) error {
	if err != nil && !r.CardMounted() {
		return q2.ErrNoCard
	}
	return err
}
