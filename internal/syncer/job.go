package syncer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
)

func (r *Runner) work(ctx context.Context, p plan.Plan) error {
	r.fileMu.Lock() // an edit that began before the launch saves first
	defer r.fileMu.Unlock()
	m, err := library.LoadManifest(r.manifestPath())
	if err != nil {
		return r.cardErr(err)
	}
	j := &job{Runner: r, ctx: ctx, p: p, m: m, saved: r.Now(), recorded: map[string]int{}}
	for _, e := range m.Songs {
		j.recorded[strings.ToLower(e.Path)]++
	}
	j.dropParts()
	n := j.dropStale() // what a stop left of the last run's
	j.mu.Lock()
	j.stat.Deleted = n
	j.mu.Unlock()
	defer func() { // a stop keeps what is done; a crash re-fetches at most the songs since the last save
		if j.unsaved > 0 {
			j.save()
		}
	}()
	for _, phase := range []func() error{j.renames, j.downloads, j.keep, j.playlists, j.prunePlaylists, j.removals} {
		if err := phase(); err != nil {
			return err
		}
	}
	if err := j.check(j.ctx); err != nil { // a stop dropStale cut short, as in removals' last batch
		return err
	}
	j.m.SyncedAt = j.Now() // "Last synced" (a stopped run is no sync to the end)
	return j.save()
}

// job is one run of a plan over the manifest it loaded, phase by phase. The manifest never names a
// file that isn't there: each phase records a change before the file it frees goes.
type job struct {
	*Runner
	ctx     context.Context
	p       plan.Plan
	m       *library.Manifest
	unsaved int       // changes not on the card yet
	saved   time.Time // when the manifest was last saved
	failed  int       // songs whose download failed this run ...
	arrived int       // ... and songs downloaded or taken from the cache
	hold    bool      // a failure the whole sync shares: nothing is removed (I2)
	removed bool      // the removals ran (space ran them early)
	// recorded counts the songs recorded at each file (lower case): two at one, from older builds,
	// keep it when one of them is replaced or removed.
	recorded map[string]int
}

// held reports whether this run removes nothing (I2): a failure looked server-wide (hold), or,
// outside Entire Library, several songs failed and fewer arrived, as when an album taken out of
// Sync is swapped for one that didn't come. A single song failing on its own holds nothing back,
// nor do any under Entire Library: nothing is swapped there, and its songs can't be taken out of
// Sync, so they would stop every removal for good.
func (j *job) held() bool { return j.hold || !j.p.Whole && j.failed > 1 && j.arrived < j.failed }

// space stops the run before w when downloading it would leave less than the reserve free. First,
// unless the failures so far hold the removals (held), the songs taken out of Sync are removed to
// make room (they go at the end anyway); a sync that removes and adds would otherwise stop at the
// same song every time. Decided 2026-11-10 (audit #2): a download that fails after that leaves the card with the
// removed songs gone and the new ones in part, as the user asked for them to go. When it still
// doesn't fit, what is on the card stays recorded, and its playlists are written for the songs
// that arrived (nothing more is removed, I2), so a part-synced playlist plays at once. Held back
// by failed songs, the error says so (Held), and whether they failed for a shared cause (Retry).
func (j *job) space(w plan.Want) error {
	if j.Free == nil || j.Reserve == nil {
		return nil
	}
	left, reserve := j.Free(), j.Reserve()
	if left-w.Bytes >= reserve {
		return nil
	}
	if len(j.m.Stale) > 0 { // replaced and removed songs' files, which go with a save: they would anyway
		if err := j.save(); err != nil {
			return err
		}
		if left = j.Free(); left-w.Bytes >= reserve {
			return nil
		}
	}
	// a replace that shrinks its song frees more than it takes once the next save drops the old
	// file: the card goes under the reserve by at most this one song
	if e := j.m.Songs[w.Song.ID]; e != nil && e.Size > w.Bytes && left >= w.Bytes {
		return nil
	}
	if !j.held() && !j.removed && len(j.p.Remove) > 0 {
		j.logf("sync: short of space: removing the songs taken out of Sync first")
		if err := j.removals(); err != nil {
			return err
		}
		if left = j.Free(); left-w.Bytes >= reserve {
			return nil
		}
	}
	for _, phase := range []func() error{j.keep, j.playlists} { // no prunePlaylists: nothing more is removed (I2)
		if err := phase(); err != nil {
			return err
		}
	}
	err := &SpaceLowError{Left: left, Next: w.Bytes, Reserve: reserve, Held: j.held() && !j.removed && j.toRemove() > 0, Retry: j.hold}
	j.logf("sync: stopped: %v", err)
	return err
}

// count adds one to a counter the snapshot reads.
func (j *job) count(n *int) {
	j.mu.Lock()
	*n++
	j.mu.Unlock()
}

func (j *job) save() error {
	if !j.CardMounted() { // never MkdirAll .jellyfin onto the Q2's own storage under the mount point
		return q2.ErrNoCard
	}
	j.op("save", "")
	if err := j.m.Save(j.root, j.manifestPath()); err != nil {
		return j.cardErr(err)
	}
	j.unsaved, j.saved = 0, j.Now()
	j.dropStale()
	return nil
}

// dropStale deletes the replaced and removed songs' files in m.Stale, recorded there by a save (I4),
// until a stop, and returns how many it deleted. A file deleted or gone, or now a song's, leaves the
// list; the rest stay for the next save. The shorter list goes with the next save; after a stop, or
// an error that ends the run, none: DropMissing drops the gone ones before the next plan.
func (j *job) dropStale() int {
	var left []string
	n := 0
	for i, f := range j.m.Stale {
		if j.check(j.ctx) != nil { // a stop gets 3 s, not a batch of deletes
			left = append(left, j.m.Stale[i:]...)
			break
		}
		if j.recorded[strings.ToLower(f)] > 0 {
			continue
		}
		_, lerr := os.Lstat(f) // one already gone isn't counted again
		if err := j.remove(f, false); err != nil {
			j.logf("sync: remove %s: %v", f, err)
			left = append(left, f)
			continue
		}
		j.op("remove", f)
		if lerr == nil {
			n++
		}
	}
	if len(left) < len(j.m.Stale) && j.ctx.Err() == nil {
		j.unsaved++
	}
	j.m.Stale = left
	return n
}

// dropParts deletes the unfinished downloads (.part and .part.key under Jellyfin/) that this run
// won't continue: a song paused, cut or left by a restart, then taken out of Sync, whose .part no
// removal would ever be for (audit 2026-11-10 #7). Once a run, in its background.
func (j *job) dropParts() {
	want := map[string]bool{} // the planned songs' paths
	for _, ws := range [][]plan.Want{j.p.Add, j.p.Replace} {
		for _, w := range ws {
			want[w.Path] = true
		}
	}
	filepath.WalkDir(filepath.Join(j.root, "Jellyfin"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		part := strings.TrimSuffix(p, ".key") // a .part's key goes with it
		if !strings.HasSuffix(part, ".part") || want[strings.TrimSuffix(part, ".part")] {
			return nil
		}
		if library.Remove(j.root, p) == nil {
			j.logf("sync: dropped %s", filepath.Base(p))
		}
		return nil
	})
}

// renames moves songs whose path changed, in batches of renameBatch: a batch's targets are saved
// (Entry.Moving) before its files move, and its new paths go with the next save, so a crash or the
// card going part-way leaves every moved file recorded (LoadManifest settles it) (M13).
func (j *job) renames() error {
	for rest := j.p.Rename; len(rest) > 0; {
		if err := j.check(j.ctx); err != nil {
			return err
		}
		var batch []plan.Move
		for ; len(rest) > 0 && len(batch) < renameBatch; rest = rest[1:] {
			if e := j.m.Songs[rest[0].ID]; e != nil && e.Path != rest[0].To {
				e.Moving = rest[0].To
				batch = append(batch, rest[0])
			}
		}
		if len(batch) == 0 {
			continue
		}
		j.unsaved++
		if err := j.save(); err != nil {
			return err
		}
		for _, mv := range batch {
			if err := j.check(j.ctx); err != nil {
				return err // the rest of the batch keeps Moving: the next load finds them unmoved
			}
			e := j.m.Songs[mv.ID]
			e.Moving = ""
			// a rename never replaces a song's file; j.recorded kept in step tells dropStale too (I4)
			if err := j.rename(e.Path, mv.To, j.recorded[strings.ToLower(mv.To)] == 0); err != nil {
				if !j.CardMounted() {
					return q2.ErrNoCard
				}
				j.fail(mv.ID, filepath.Base(mv.To), "rename", err)
				continue
			}
			j.recorded[strings.ToLower(e.Path)]--
			j.recorded[strings.ToLower(mv.To)]++
			e.Path = mv.To // its MetaKey with the rest of p.Keep, after the downloads
			j.count(&j.stat.Renamed)
			j.unsaved++
		}
	}
	return nil
}

// downloads fetches the adds and replaces, recording them in batches (M18). It ends the run on a
// failure the whole sync shares (I3, R39). A song after one that failed for its own cause gets one
// try with no waits: a server whose music drive is gone, or whose transcoder fails, fails every
// song, and fetchTries' waits on each kept the Q2 from standby for hours.
func (j *job) downloads() error {
	covers := map[string]bool{} // album folders whose cover was tried this run
	streak := 0                 // songs failed in a row for the same reason as the whole sync would (I3)
	once := false               // the song before failed for its own cause: one try for this one
	// replaces first: one that shrinks a song (FLAC to Opus) frees room the adds may need
	for _, ws := range [][]plan.Want{j.p.Replace, j.p.Add} {
		for _, w := range ws {
			id := w.Song.ID
			if e := j.m.Songs[id]; e != nil && e.Path == w.Path && e.Quality == w.Quality && e.Meta == library.MetaKey(w.Song, w.Quality) &&
				(w.Quality != "original" || e.SrcSize == w.Song.Size) { // a re-rip has the same path, quality and tags
				j.finished(w)
				continue
			}
			if err := j.check(j.ctx); err != nil {
				return err
			}
			n, cached := j.fromCache(w)
			var err error
			if !cached {
				if err := j.space(w); err != nil {
					return err
				}
				j.mu.Lock()
				j.stat.Current = w.Song.Title
				if w.Song.Artist != "" {
					j.stat.Current = w.Song.Artist + " – " + w.Song.Title
				}
				j.mu.Unlock()
				if once {
					n, err = j.fetch(j.ctx, w)
				}
				// A stop reason (Wi-Fi gone, the card) gets the retries that ride it out, as any song.
				if !once || err != nil && j.ctx.Err() == nil && StopReason(err) != nil {
					n, err = j.fetchTries(j.ctx, w)
				}
			}
			if err != nil {
				if j.ctx.Err() != nil {
					return j.ctx.Err()
				}
				if !j.CardMounted() {
					return q2.ErrNoCard
				}
				j.fail(id, w.Song.Title, "download", err)
				j.failed++
				switch stop := StopReason(err); {
				case errors.Is(stop, ErrRefused): // the server's answer for the account: at once
					return stop
				case stop == nil:
					streak, once = 0, true
				default: // a connection or the card can come back: two songs in a row
					once = false
					j.hold = true
					if streak++; streak >= 2 {
						return stop
					}
				}
				continue
			}
			streak, once = 0, false
			old := j.m.Songs[id]
			e := &library.Entry{ID: id, Path: w.Path, Quality: w.Quality, Size: n, Items: w.Items,
				Meta: library.MetaKey(w.Song, w.Quality)}
			if w.Quality == "original" {
				e.SrcSize = w.Song.Size
			}
			j.m.Songs[id] = e
			j.unsaved++
			if old != nil {
				j.recorded[strings.ToLower(old.Path)]--
			}
			j.recorded[strings.ToLower(e.Path)]++
			// A replaced song's old file goes after the next save, which records the new one, not after a
			// save of its own: a Sync Quality change replaces every song, and each save writes the whole
			// manifest (audit 2026-11-16 #6). Until then the manifest names the old file, still there (I4).
			// A crash first leaves the new file unrecorded: the next sync replaces the song again, its
			// download or cached copy renamed over that file (wanted at its old quality again, the song
			// stays, and the file is left unrecorded). Another song recorded in the old file keeps it
			// (removals' rule).
			if old != nil && !oneFile(old.Path, w.Path) && j.recorded[strings.ToLower(old.Path)] == 0 {
				library.Remove(j.root, library.LyricsPath(old.Path)) // its .lrc now: Song.flac's is Song.opus's too
				j.m.Stale = append(j.m.Stale, old.Path)
			}
			if j.unsaved >= max(saveEvery, len(j.m.Songs)/saveShare) || j.Now().Sub(j.saved) >= saveAfter {
				if err := j.save(); err != nil {
					return err
				}
			}
			if _, err := os.Stat(library.LyricsPath(w.Path)); !cached || err != nil { // a cached song brought its own
				WriteLyrics(j.ctx, j.src, w.Song, w.Path, j.logf) // after the old file's .lrc went
			}
			if dir := filepath.Dir(w.Path); !covers[dir] {
				covers[dir] = true
				j.cover(j.ctx, w)
			}
			j.arrived++
			j.finished(w)
			j.op("download", w.Path)
		}
	}
	return nil
}

// fromCache takes w from the playback cache (FromCache) when it is there, dropping a .part an
// earlier try left for it.
func (r *Runner) fromCache(w plan.Want) (int64, bool) {
	if r.FromCache == nil || owned(r.root, w.Path) != nil {
		return 0, false
	}
	n, ok := r.FromCache(w)
	if ok {
		library.Remove(r.root, w.Path+".part")
		library.Remove(r.root, w.Path+".part.key")
		r.logf("sync: %s from the playback cache", w.Song.ID)
	}
	return n, ok
}

// keep records the items and MetaKeys of songs that needed no transfer (I12).
func (j *job) keep() error {
	if applyKeep(j.m, j.p.Keep) {
		j.unsaved++
	}
	return nil
}

// playlists writes every selected playlist's .m3u, naming where each song is.
func (j *job) playlists() error {
	m, p := j.m, j.p
	for name, f := range m.Playlists {
		switch {
		case !library.ValidPlaylistFile(j.root, f):
			j.logf("sync: ignoring bad recorded playlist path %q for %s", f, name)
			delete(m.Playlists, name)
		case library.StalePlaylistFile(f): // an older build's name, which the Q2 refuses or cuts
			if err := m.Remove(j.root, f); err != nil {
				j.logf("sync: remove stale playlist %s: %v", f, err) // kept, and tried again next sync
			} else {
				delete(m.Playlists, name) // written again below under a name that fits, and sent as "add" (R35)
			}
		}
	}
	planned := map[string]string{} // a path the plan gives a song -> its id
	for _, ws := range [][]plan.Want{p.Add, p.Replace} {
		for _, w := range ws {
			planned[w.Path] = w.Song.ID
		}
	}
	for _, mv := range p.Rename {
		planned[mv.To] = mv.ID
	}
	names := make([]string, 0, len(p.Playlists))
	for name := range p.Playlists {
		names = append(names, name)
	}
	sort.Strings(names)
	// Every file is recorded, in one save, before any is written: the guard writes only recorded
	// files, and a stop part-way leaves at worst a recorded file not written yet, never a written
	// one a later sync takes for the user's own (and so writes a "(Jellyfin)" copy beside).
	named := false
	for _, name := range names {
		if m.Playlists[name] == "" {
			m.Playlists[name] = j.freePlaylistFile(m, name, false)
			named = true
		}
	}
	if named {
		if err := j.save(); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := j.check(j.ctx); err != nil { // USB storage gives the app 3 s to let the card go
			return err
		}
		f := m.Playlists[name]
		have := []string{} // songs that failed are left out until they arrive
		for _, s := range p.Playlists[name] {
			if id, ok := planned[s]; ok && m.Songs[id] != nil {
				s = m.Songs[id].Path // where the song is: a failed rename or replace left it at its old path
			}
			if _, err := os.Stat(s); err == nil {
				have = append(have, s)
			}
		}
		if err := m.WriteM3U(j.root, f, have); err != nil {
			return j.cardErr(err)
		}
		m.PlaylistSums[name] = library.PlaylistSum(p.Playlists[name]) // what the next plan compares (I10)
		j.unsaved++                                                   // saved with a stop between playlists too
		j.op("playlist", f)
	}
	m.PlaylistsStale = false
	j.unsaved++ // saved by work's last save, or on a stop by its deferred one
	return nil
}

// prunePlaylists deletes the recorded playlists no longer selected, unless failed songs hold the
// removals (I2). A stop short of space (space) never runs it.
func (j *job) prunePlaylists() error {
	if j.held() {
		return nil
	}
	for name, f := range j.m.Playlists {
		if _, ok := j.p.Playlists[name]; !ok && j.m.Remove(j.root, f) == nil {
			delete(j.m.Playlists, name)
			delete(j.m.PlaylistSums, name)
			delete(j.m.Imported, f) // the user may make their own of that name: added back, it goes as "add" (R35)
		}
	}
	return nil
}

// toRemove counts the songs of the plan's Remove still on the card: a Retry Failed or Resume runs
// the plan again after an earlier run removed them.
func (j *job) toRemove() int {
	n := 0
	for _, e := range j.p.Remove {
		if j.m.Songs[e.ID] != nil {
			n++
		}
	}
	return n
}

// removals deletes the songs no longer selected, once: last, or earlier when the card is short of
// space (space), and only while failed songs don't hold them (I2). Each batch leaves the manifest
// with its files listed in m.Stale, in one save, before they go: the save's dropStale deletes them,
// and a stop or a pulled card part-way leaves the rest listed on the card for the next sync (I4).
func (j *job) removals() error {
	if j.removed || len(j.p.Remove) == 0 {
		return nil
	}
	if j.held() { // I2: what was asked for didn't come, so what it replaces stays; Retry Failed or the next Sync removes
		j.mu.Lock()
		j.stat.Held, j.stat.HeldRetry = j.toRemove(), j.hold
		j.mu.Unlock()
		return nil
	}
	j.removed = true
	for rest := j.p.Remove; len(rest) > 0; {
		if err := j.check(j.ctx); err != nil {
			return err
		}
		batch := rest[:min(len(rest), removeBatch)]
		rest = rest[len(batch):]
		n := 0
		for _, pe := range batch {
			e := j.m.Songs[pe.ID]
			if e == nil {
				continue // an earlier run removed it
			}
			n++
			delete(j.m.Songs, pe.ID)
			f := strings.ToLower(e.Path)
			if j.recorded[f]--; j.recorded[f] > 0 {
				continue // another song recorded in the file keeps it (one file under two names, from older builds)
			}
			library.Remove(j.root, library.LyricsPath(e.Path)) // now: dropStale keeps lyrics, and the folder must empty
			j.m.Stale = append(j.m.Stale, e.Path)
		}
		if n == 0 {
			continue
		}
		j.unsaved++
		if err := j.save(); err != nil {
			return err
		}
		j.mu.Lock()
		j.stat.Removed += n
		j.mu.Unlock()
	}
	return nil
}

const (
	saveEvery   = 20              // downloads recorded per manifest save (M18), at least ...
	saveShare   = 200             // ... 1 in this many of the songs recorded: each save writes them all ...
	saveAfter   = 3 * time.Minute // ... or this long since the last (audit B7: fewer big card writes)
	removeBatch = 1000            // removals per manifest save, their files listed in Stale (I4)
	renameBatch = 1000            // renames per manifest save: Entry.Moving makes any batch crash-safe
)

// freePlaylistFile picks <name>.m3u (unless skipBase), else <name> (Jellyfin).m3u, else
// <name> (Jellyfin 2).m3u... (library.PlaylistFileN): the first neither on the card (a user's own
// file) nor recorded for another playlist (case-folded: the card is FAT/exFAT).
func (r *Runner) freePlaylistFile(m *library.Manifest, name string, skipBase bool) string {
	taken := func(f string) bool {
		if _, err := os.Lstat(f); err == nil {
			return true
		}
		for _, o := range m.Playlists {
			if strings.EqualFold(o, f) {
				return true
			}
		}
		return false
	}
	n := 0
	if skipBase {
		n = 1
	}
	f := library.PlaylistFileN(r.root, name, n)
	for taken(f) {
		n++
		f = library.PlaylistFileN(r.root, name, n)
	}
	return f
}

// remove deletes path through the guard, with its .lrc when lyrics, and, when its album folder is
// left holding only cover.jpg, a replaced plan's leftover .part and .part.key files and a hard
// kill's .tmp files, those too, so the guard prunes the folder.
// ponytail: a leftover of a song later in this run, paused earlier, is fetched again from the start.
func (r *Runner) remove(path string, lyrics bool) error {
	rm := library.Remove
	if !lyrics {
		rm = library.RemoveKeepLyrics
	}
	if err := rm(r.root, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range es {
		if n := e.Name(); e.IsDir() || n != "cover.jpg" && !strings.HasSuffix(n, ".part") && !strings.HasSuffix(n, ".part.key") && !strings.HasSuffix(n, ".tmp") {
			return nil // a song, or something else, still lives here
		}
	}
	for _, e := range es {
		library.Remove(r.root, filepath.Join(dir, e.Name()))
	}
	return nil
}

// oneFile reports whether a and b name one file: on the case-insensitive card, case variants do.
func oneFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// owned gates song writes (downloads and renames): only under Jellyfin/, never .jellyfin/.
func owned(root, path string) error {
	if !library.InSongs(root, path) {
		return errors.New("refusing song path outside Jellyfin/: " + path)
	}
	return nil
}

// rename moves a song's file from from to to. A file already at to is replaced only when orphan
// (no song records it: one the app forgot in a crash window), else the move fails.
func (r *Runner) rename(from, to string, orphan bool) error {
	for _, p := range []string{from, to} {
		if err := owned(r.root, p); err != nil {
			return err
		}
	}
	fi, ferr := os.Lstat(from)
	ti, terr := os.Lstat(to)
	switch {
	case ferr == nil && terr == nil && os.SameFile(fi, ti):
		// A case-only change on the case-insensitive card: one file. The rename may keep the old
		// case (FAT and casefold ignore it), and to reaches the file either way.
		os.Rename(from, to)
		return nil
	case terr == nil && errors.Is(ferr, os.ErrNotExist):
		return nil // moved already, and cut before the manifest was saved (M13)
	case terr == nil && !orphan:
		return errors.New("rename target exists: " + to)
	case terr == nil:
		r.logf("sync: replacing %s, which no song records", to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if lf, lt := library.LyricsPath(from), library.LyricsPath(to); lf != from {
		os.Rename(lf, lt) // its lyrics, when it has any, move with it
	}
	r.carryCover(filepath.Dir(from), filepath.Dir(to))
	return r.remove(from, true) // the file is gone already; this prunes the folders it left empty
}

// carryCover copies the album's cover.jpg into a song's new folder when that has none: the old
// one goes with the old folder's last song, and a renamed song is never downloaded (M17).
func (r *Runner) carryCover(from, to string) {
	src, dst := filepath.Join(from, "cover.jpg"), filepath.Join(to, "cover.jpg")
	if _, err := os.Stat(dst); err == nil || owned(r.root, dst) != nil {
		return
	}
	f, err := os.Open(src)
	if err != nil {
		return
	}
	defer f.Close()
	writeCover(dst, f) // one planted over Wi-Fi Transfer past maxCover is refused
}
