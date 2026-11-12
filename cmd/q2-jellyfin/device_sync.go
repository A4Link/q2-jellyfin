package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

const planMargin = 16 << 20 // plan.Compute's headroom on top of what a sync needs

// syncPage is the Sync screen: the running sync, the one just finished, or the preview, computed
// in the background while the page refreshes. Its options (syncSources) come first in every state:
// the firmware keeps the highlight at the same position when the page redraws (M33), and the rows
// below them come and go.
func (d *daemon) syncPage(cl *jellyfin.Client) q2spot.Reply {
	snap := d.runner.Snapshot()
	d.mu.Lock()
	showDone := d.showDone
	d.mu.Unlock()
	switch {
	case snap.State == "running" || snap.State == "paused":
		return d.syncRows(snap)
	case snap.State == "done" && showDone:
		return d.doneRows(snap)
	case snap.State == "error" && showDone:
		msg := "Sync stopped"
		if snap.Err != nil {
			msg += " · " + d.stopMessage(snap.Err)
		}
		op, h, code := "sync", 0, 0
		var ae *jellyfin.APIError
		if errors.As(snap.Err, &ae) {
			op, h, code = ae.Op, ae.HTTP, ae.Code
		}
		det := detailsCtx(op, h, code, snap.ErrAt, "") // when it stopped (review G3, Minor 3)
		if snap.Err != nil {
			det = withErr(det, snap.Err)
		}
		return q2spot.Reply{Rows: append(d.syncSources(), info(msg), act("retry", "Retry", ""), act("check", "Check Again", ""),
			list("details", "Details", "", det))}
	}
	if _, err := d.selection(); err != nil {
		return damagedSelection()
	}
	if _, err := d.manifestErr(); err != nil {
		d.log.Printf("sync: manifest: %v", err)
		return q2spot.Reply{Rows: append(d.syncSources(), info("Couldn't read the sync records on the card"))}
	}
	checking := q2spot.Reply{Message: "refresh", Rows: append(d.syncSources(), info("Checking server…"))}
	p, started := d.startPreview(cl)
	if started {
		return checking
	}
	d.mu.Lock()
	done, pl, err := p.done, p.plan, p.err
	if done && err != nil && d.preview == p {
		d.preview = nil // shown once; the next Sync tries again
	}
	d.mu.Unlock()
	switch {
	case !done:
		return checking
	case err != nil: // offline too: the options stay, On This Q2 needs no server
		r := d.errRows(err, "")
		r.Rows = append(d.syncSources(), r.Rows...)
		return r
	}
	r := previewRows(pl, keepFreeBytes(d.settings().KeepFree))
	r.Rows = append(d.syncSources(), r.Rows...)
	return r
}

// startPreview starts computing the preview unless a fresh one exists, and reports whether it did,
// with the preview, which a background start may replace as soon as d.mu is let go.
func (d *daemon) startPreview(cl *jellyfin.Client) (*preview, bool) {
	d.mu.Lock()
	p := d.preview
	if p != nil && p.gen == d.gen && !(p.done && time.Since(p.at) > previewTTL) {
		d.mu.Unlock()
		return p, false
	}
	d.dropPreview()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	p = &preview{gen: d.gen, at: time.Now(), cancel: cancel}
	d.preview = p
	d.mu.Unlock()
	d.spawn(func() { defer cancel(); d.computePreview(ctx, cl, p) })
	return p, true
}

// dropPreview forgets the preview, stopping its fetches if it is still at work (M11). d.mu is held.
func (d *daemon) dropPreview() {
	if d.preview != nil && d.preview.cancel != nil {
		d.preview.cancel()
	}
	d.preview = nil
}

// stopMessage is why a sync stopped, in a sentence: a card error by what it means, never Go's
// text for it (M34).
func (d *daemon) stopMessage(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	var pe *fs.PathError
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "The card is full"
	case errors.Is(err, syscall.EROFS):
		return "The card is read-only"
	case errors.Is(err, q2.ErrNoCard):
		return "Card not available"
	case errors.As(err, &se) || errors.As(err, &te) || errors.Is(err, io.ErrUnexpectedEOF):
		return "Couldn't read the sync records on the card"
	case errors.As(err, &pe):
		return "Couldn't write to the card"
	}
	return jellyfin.Message(err, d.hostName())
}

// computePreview resolves every selected item to its songs, 4 at a time, and plans the sync. Any
// failure fails the preview: a missing item's songs would otherwise read as removed. ctx ends with
// the preview (dropPreview).
func (d *daemon) computePreview(ctx context.Context, cl *jellyfin.Client, p *preview) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sel, err := d.selection()
	resolved, names := map[string][]jellyfin.Song{}, map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var werr error // the first worker's failure, under mu; it cancels ctx, so the rest return at once
	items := sel.Items
	for _, it := range items {
		if err != nil {
			break // the selection didn't load; no worker has started
		}
		wg.Add(1)
		go func(it selection.Item) {
			defer wg.Done()
			if it.Kind == selection.KindLibrary {
				// Holds no slot itself: its requests take them, so 4 stay the most in flight.
				lib, e := d.resolveLibrary(ctx, cl, sem)
				mu.Lock()
				defer mu.Unlock()
				if e != nil && werr == nil {
					werr = e
					cancel()
				}
				key := selection.Key(it.Kind, it.ID)
				resolved[key] = lib.songs
				for _, p := range lib.playlists {
					pk := selection.Key(selection.KindPlaylist, p.id)
					if sel.Has(selection.KindPlaylist, p.id) {
						continue // chosen on its own: that item's songs (and hold) stand
					}
					sel.Items = append(sel.Items, selection.Item{Kind: selection.KindPlaylist, ID: p.id, Name: p.name,
						Held: it.Held}) // held with it
					resolved[pk], names[pk] = p.songs, p.name
				}
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			songs, name, e := d.songsOf(ctx, cl, it.Kind, it.ID)
			key := selection.Key(it.Kind, it.ID)
			mu.Lock()
			defer mu.Unlock()
			if e != nil && werr == nil {
				werr = e
				cancel()
			}
			resolved[key], names[key] = songs, name
		}(it)
	}
	wg.Wait()
	if err == nil {
		err = werr
	}
	if _, merr := d.manifestErr(); err == nil { // Sync Favourites and a hold start one with no Sync page to stop it
		err = merr
	}
	var pl plan.Plan
	if err == nil {
		// Songs deleted from the card are planned again, not taken as there (I9).
		if derr := d.runner.DropMissing(); derr != nil && !errors.Is(derr, syncer.ErrBusy) {
			d.log.Printf("sync: %v", derr)
		}
		// The playback cache gives way to a sync: its bytes count as free, and Start Sync evicts
		// what the sync needs (cache.go).
		pl = plan.Compute(sel, d.settings().def(), resolved, names, d.manifest(), d.card, d.free(d.card)+d.stream.cacheSize())
		// The items holding songs already on the card, recorded now: an up-to-date preview starts
		// no sync, and status reads them after a restart (I12). A running sync records its own.
		if rerr := d.runner.Refresh(pl.Keep); rerr != nil && !errors.Is(rerr, syncer.ErrBusy) {
			d.log.Printf("sync: refresh: %v", rerr)
		}
	}
	d.mu.Lock()
	p.done, p.plan, p.err, p.at = true, &pl, err, time.Now()
	auto := d.autoStart && p == d.preview && d.cl == cl // never for an account signed out meanwhile
	again := auto && p.gen != d.gen                     // changed again meanwhile: start from a new preview
	if auto && !again {
		d.autoStart = false
	}
	d.mu.Unlock()
	if again {
		d.startPreview(cl)
		return
	}
	if auto && err == nil && changes(pl) { // Sync Favourites: start without a second press
		if r := d.syncAction("start"); r.Message != "" {
			d.log.Printf("sync favourites: %s", r.Message)
		}
	}
	if err == nil { // after the preview shows: a request per song
		d.backfillLyrics(ctx, resolved, pl)
	}
}

// changes reports whether p has anything to do: a song to move, or a playlist file to write or delete.
func changes(p plan.Plan) bool {
	return len(p.Add)+len(p.Replace)+len(p.Rename)+len(p.Remove)+p.PlaylistsChanged > 0 || p.PlaylistsStale
}

func previewRows(p *plan.Plan, reserve int64) q2spot.Reply {
	if !changes(*p) {
		return q2spot.Reply{Rows: []q2spot.Row{info("Everything is up to date")}}
	}
	var rows []q2spot.Row
	if len(p.Add) > 0 {
		var b int64
		for _, w := range p.Add {
			b += w.Bytes
		}
		rows = append(rows, list("added", "+"+plural(len(p.Add), "song")+" · "+size(b), "", ""))
	}
	if len(p.Remove) > 0 {
		var b int64
		for _, e := range p.Remove {
			b += e.Size
		}
		rows = append(rows, list("removed", "−"+plural(len(p.Remove), "song")+" · "+size(b), "", ""))
	}
	if len(p.Replace) > 0 {
		delta := "+" + size(p.ReplaceBytes)
		if p.ReplaceBytes < 0 {
			delta = "−" + size(-p.ReplaceBytes)
		}
		rows = append(rows, info("Replace "+plural(len(p.Replace), "song")+" · "+delta))
	}
	if len(p.Rename) > 0 {
		rows = append(rows, info("Rename "+plural(len(p.Rename), "song")))
	}
	switch {
	case p.PlaylistsChanged > 0:
		rows = append(rows, info("Update "+plural(p.PlaylistsChanged, "playlist")))
	case p.PlaylistsStale:
		rows = append(rows, info("Update playlists"))
	}
	rows = append(rows, info("Needs "+size(max(p.NeedBytes, 0))+" · "+size(p.FreeBytes)+" free"))
	// Whether it all fits keeps Space to Keep Free in hand too: a sync that would end under it stops
	// there. It starts anyway, and the run stops before the card falls under it. The removals count
	// as freed: a run short of space removes them first (syncer space).
	if p.NeedBytes > 0 && p.NeedBytes-p.RemoveBytes+planMargin+reserve > p.FreeBytes {
		rows = append(rows, info("Not all of it fits on the card"), info("It stops with "+size(reserve)+" left"))
	}
	rows = append(rows, act("start", "Start Sync", ""))
	return q2spot.Reply{Rows: rows}
}

// syncRows is the running (or paused) sync, live, below the options: the ETA, Now and pause rows
// come and go as it refreshes, so the options above them keep their places (M33).
func (d *daemon) syncRows(s syncer.Snapshot) q2spot.Reply {
	pct := fmt.Sprintf("%d %%", s.Permille/10)
	rows := d.syncSources()
	if d.caps.Progress {
		sub := ""
		if s.State == "paused" {
			sub = "Paused"
		}
		rows = append(rows, q2spot.Row{Kind: "progress", Title: pct, Subtitle: sub, Position: strconv.Itoa(s.Permille)})
	} else {
		n := min(max((s.Permille+50)/100, 0), 10)
		rows = append(rows, info("["+strings.Repeat("#", n)+strings.Repeat("-", 10-n)+"] "+pct))
	}
	rows = append(rows, info(fmt.Sprintf("%d of %s", s.Done, plural(s.Total, "song"))))
	if s.ETA > 0 && s.State == "running" {
		rows = append(rows, info(eta(s.ETA)))
	}
	if s.Current != "" {
		rows = append(rows, info("Now: "+s.Current))
	}
	if s.State == "paused" {
		switch {
		case errors.Is(s.Err, syncer.ErrSpaceLow):
			rows = append(rows, spaceRows(s.Err)...)
		case errors.Is(s.Err, q2.ErrNoCard):
			rows = append(rows, info("Card not available"))
		case errors.Is(s.Err, syncer.ErrCardFull):
			rows = append(rows, info("The card is full"))
		case errors.Is(s.Err, syncer.ErrOffline) || errors.Is(s.Err, syncer.ErrRefused):
			rows = append(rows, info(jellyfin.Message(s.Err, d.hostName()))) // with code 50 (R39) too
		}
		rows = append(rows, act("resume", "Resume", ""))
	} else {
		rows = append(rows, act("pause", "Pause", ""))
	}
	rows = append(rows, act("cancel", "Cancel", ""))
	return q2spot.Reply{Message: "refresh", Rows: rows}
}

// jobsRow opens Sync Jobs.
var jobsRow = list("jobs", "Sync Jobs", "Hold or resume what syncs", "")

// jobs is Sync Jobs: every item in Sync not yet all on the Q2, and every held one. A press holds
// or resumes it (hold).
func (d *daemon) jobs(cursor string) q2spot.Reply {
	sel, err := d.selection()
	if err != nil {
		return damagedSelection()
	}
	v := d.view()
	var rows []q2spot.Row
	for _, it := range sel.Items {
		st := v.jobStatus(it.Kind, it.ID)
		if st == "On Q2" && !it.Held {
			continue
		}
		name := it.Name
		if it.Kind == selection.KindFavorites {
			name = favName
		}
		switch {
		case it.Held && st == "":
			st = "Held"
		case it.Held:
			st = "Held · " + st
		}
		rows = append(rows, q2spot.Row{Kind: "action", URI: "hold", Title: name, Subtitle: st,
			Context: selection.Key(it.Kind, it.ID)})
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("Nothing waiting to sync")}
	}
	return page(rows, cursor)
}

// hold holds the item with key, or resumes a held one. A sync under way starts again without it
// (or with it), so what is left goes first.
func (d *daemon) hold(cl *jellyfin.Client, key string) q2spot.Reply {
	d.selMu.Lock()
	defer d.selMu.Unlock()
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, selDamaged)
	}
	kind, id := parseKey(key)
	i := slices.IndexFunc(sel.Items, func(it selection.Item) bool { return it.Kind == kind && it.ID == id })
	if i < 0 {
		return q2spot.Reply{Message: "No longer in Sync"}
	}
	held := !sel.Items[i].Held
	sel.SetHeld(kind, id, held)
	if err := d.saveSelection(sel); err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't save")
	}
	msg := "Resumed"
	if held {
		msg = "Held"
	}
	return q2spot.Reply{Message: msg + d.restartSync(cl)}
}

// restartSync stops a sync that runs or is paused and plans it again, so it follows a change to
// what it syncs or at what quality. A running one starts again by itself; a paused one waits for
// Start Sync on the Sync page. It returns what to add to the reply: "" with no sync under way.
func (d *daemon) restartSync(cl *jellyfin.Client) string {
	st := d.runner.Snapshot().State
	if cl == nil || st != "running" && st != "paused" {
		return ""
	}
	d.spawn(func() {
		d.runner.Cancel()
		d.mu.Lock()
		if d.cl != cl { // signed out (or in again) while the run stopped: nothing restarts
			d.mu.Unlock()
			return
		}
		d.autoStart = st == "running"
		d.dropPreview()
		d.mu.Unlock()
		d.startPreview(cl)
	})
	if st == "paused" {
		return " · press Start Sync to continue"
	}
	return " · sync restarting"
}

func eta(t time.Duration) string {
	m := int(t.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "Less than a minute left"
	case m < 60:
		return fmt.Sprintf("About %d min left", m)
	}
	return fmt.Sprintf("About %d h %d min left", m/60, m%60)
}

func (d *daemon) doneRows(s syncer.Snapshot) q2spot.Reply {
	// What the run did: songs fetched, renamed, removed; a run that moved none rewrote playlists
	// (review G3, Minor 7).
	var did []string
	if s.Total > 0 {
		did = append(did, plural(s.Done, "song"))
	}
	if s.Renamed > 0 {
		did = append(did, plural(s.Renamed, "song")+" renamed")
	}
	if s.Removed > 0 {
		did = append(did, plural(s.Removed, "song")+" removed")
	}
	if len(did) == 0 {
		did = []string{"Playlists updated"}
	}
	rows := append(d.syncSources(), info(fmt.Sprintf("Sync complete · %s · %d failed", strings.Join(did, " · "), s.Failed)))
	if s.Failed > 0 {
		rows = append(rows, list("failed", "Failed Songs", "", ""))
	}
	d.mu.Lock()
	var done, ok bool
	var msg string
	var at time.Time
	auto := d.caps.Library && d.lib != nil
	if auto {
		done, ok, msg, at = d.libState()
	}
	d.mu.Unlock()
	reply := q2spot.Reply{}
	switch {
	case !auto:
		rows = append(rows, list("library", "Update Your Library", "", ""))
	case !done:
		rows = append(rows, list("library", "Updating Library…", "", ""))
		reply.Message = "refresh"
	case ok:
		rows = append(rows, info("Library updated"))
	default:
		rows = append(rows, d.libFailedRows(msg, at)...)
	}
	reply.Rows = append(rows, act("check", "Check Again", ""))
	return reply
}

func (d *daemon) failed(cursor string) q2spot.Reply {
	rows := []q2spot.Row{act("retry", "Retry Failed", "")}
	for _, f := range d.runner.Snapshot().Failures {
		if f.Op != "cover" {
			rows = append(rows, list("details", f.Title, f.Msg, detailsCtx(f.Op, f.HTTP, f.Code, f.At, f.SongID)))
		}
	}
	return page(rows, cursor)
}

// removed lists what the previewed sync deletes.
func (d *daemon) removed(cursor string) q2spot.Reply {
	d.mu.Lock()
	p := d.preview
	ready := p != nil && p.done && p.plan != nil
	d.mu.Unlock()
	var rows []q2spot.Row
	if ready {
		for _, e := range p.plan.Remove {
			rows = append(rows, info(strings.TrimPrefix(e.Path, filepath.Join(d.card, "Jellyfin")+"/")))
		}
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("Nothing to remove")}
	}
	return page(rows, cursor)
}

// added lists the songs the sync will add, as removed lists those it takes away.
func (d *daemon) added(cursor string) q2spot.Reply {
	d.mu.Lock()
	p := d.preview
	ready := p != nil && p.done && p.plan != nil
	d.mu.Unlock()
	var rows []q2spot.Row
	if ready {
		for _, w := range p.plan.Add {
			rows = append(rows, q2spot.Row{Kind: "info", Title: w.Song.Title, Subtitle: join(w.Song.Artist, size(w.Bytes))})
		}
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("Nothing to add")}
	}
	return page(rows, cursor)
}

func (d *daemon) syncAction(op string) q2spot.Reply {
	switch op {
	case "start":
		d.mu.Lock()
		p := d.preview
		ready := p != nil && p.done && p.err == nil && p.gen == d.gen
		d.mu.Unlock()
		if !ready {
			return q2spot.Reply{Message: "Open Sync again to check"}
		}
		// The removals count as freed, as in the plan's fit: a run short of space removes them first.
		// Songs the sync takes from the playback cache stay in it until then, and need no room.
		keep, cached := d.stream.cachedFor(append(append([]plan.Want(nil), p.plan.Add...), p.plan.Replace...))
		if net := p.plan.NeedBytes - p.plan.RemoveBytes - cached; net > 0 {
			need := net + planMargin + keepFreeBytes(d.settings().KeepFree)
			d.stream.evictFor(need, func() int64 { return d.free(d.card) }, keep)
		}
		d.stream.keepForSync(keep)
		d.mu.Lock()
		d.lib = nil // before Start: the sync's own OnDone sets it
		d.mu.Unlock()
		if err := d.runner.Start(*p.plan); err != nil {
			d.stream.keepForSync(nil)
			d.log.Printf("sync: start: %v", err)
			return q2spot.Fail(q2spot.Error, "Couldn't start the sync")
		}
		d.mu.Lock()
		d.dropPreview() // its lyrics backfill too: the sync writes lyrics for what it fetches
		d.showDone = true
		d.mu.Unlock()
		d.wantReports(true) // with the server in reach: the plays waiting go too
	case "pause": // R27: stopping waits for the download to notice; the reply doesn't
		d.spawn(d.runner.Pause)
	case "resume":
		d.runner.Resume()
	case "cancel":
		d.spawn(d.runner.Cancel)
	case "retry":
		d.runner.RetryFailed()
		d.mu.Lock()
		d.showDone = true
		d.mu.Unlock()
	case "check":
		d.mu.Lock()
		d.showDone = false
		d.dropPreview()
		d.mu.Unlock()
	}
	return q2spot.Reply{}
}

// library asks the firmware (F3) to update the library and import the playlists, or says how.
func (d *daemon) library() q2spot.Reply {
	if !d.caps.Library {
		return q2spot.Reply{Rows: []q2spot.Row{info("1. Close Jellyfin: Streaming → Configuration → Stop Streaming Services"),
			info("2. Library → Update Local Music"), info("Run after the sync has finished and Jellyfin is closed"),
			info("3. Library → Playlists → Import playlist"), info("Import playlist only applies to synced playlists")}}
	}
	d.mu.Lock()
	j := d.lib
	if j == nil || j.done && !j.ok && time.Since(j.doneAt) > 30*time.Second {
		var err error
		if j, err = d.sendLibrary(); err != nil {
			d.mu.Unlock()
			d.log.Printf("library: %v", err)
			return q2spot.Reply{Rows: []q2spot.Row{info("Couldn't ask for a library update"),
				list("details", "Details", "", detailsCtx("library", 0, 0, time.Now(), ""))}}
		}
	}
	done, ok, msg, at := d.libState()
	d.mu.Unlock()
	switch {
	case !done:
		rows := []q2spot.Row{info("Updating Library…")}
		if d.stream.active() { // ponytail: only our own stream is known; the firmware also waits for local playback
			rows = append(rows, info("Waiting for playback to pause…"))
		}
		return q2spot.Reply{Message: "refresh", Rows: rows}
	case ok:
		return q2spot.Reply{Rows: []q2spot.Row{info("Library updated")}}
	}
	return q2spot.Reply{Rows: d.libFailedRows(msg, at)}
}

func (d *daemon) libFailedRows(msg string, at time.Time) []q2spot.Row {
	if msg == "" {
		msg = "Library update failed"
	}
	return []q2spot.Row{info(msg), list("details", "Details", "", withErr(detailsCtx("library", 0, 0, at, ""), errors.New(msg)))}
}

// sendLibrary asks the firmware (F3) to scan and import the playlist files the app has recorded;
// the user's own playlists are never named (R35). A file whose playlist the app made before
// (manifest Imported) is sent as "import", which replaces the Q2 playlist of that name; any other as
// "add", which the firmware skips when the Q2 has a playlist of that name (one the user made). One
// request at a time: while one awaits its answer, the next is sent once that comes (libPoll), so no
// answer is lost. seq = UnixNano, never reused, even across restarts.
// ponytail: names every recorded playlist file, not only the ones this sync rewrote; replacing an
// unchanged playlist is harmless. d.mu is held.
func (d *daemon) sendLibrary() (*libJob, error) {
	d.libPoll()
	if seq, _ := fw.ReadRequest(d.libReq()); seq != 0 && seq != d.libTaken { // still awaiting its answer
		d.libAgain = true
		if d.lib == nil || d.lib.seq != seq {
			d.lib = &libJob{seq: seq}
		}
		return d.lib, nil
	}
	var imports, adds []string
	if m, _ := library.LoadManifest(filepath.Join(d.card, ".jellyfin", "manifest.json")); m != nil {
		for _, f := range m.Playlists {
			if _, err := os.Stat(f); err != nil || !library.ValidPlaylistFile(d.card, f) || library.LongPlaylistFile(f) {
				continue // an older version's name, longer than the Q2 keeps, waits for the sync that moves it
			}
			if m.Imported[f] {
				imports = append(imports, f)
			} else {
				adds = append(adds, f)
			}
		}
		sort.Strings(imports)
		sort.Strings(adds)
	}
	return d.writeLibrary(true, imports, adds)
}

// libMax is the most a library request may hold: the firmware (F3) reads 32 KiB (LIB_MAX) and
// answers "Request too long" to more.
const libMax = 30 << 10

// writeLibrary writes a request, the one the Sync page follows from now: scan when asked, and the
// import and add lines that fit in libMax. The rest wait in d.libRest for a follow-up, sent once
// this one is answered (libPoll), so any number of playlists is imported (I20). d.mu is held.
func (d *daemon) writeLibrary(scan bool, imports, adds []string) (*libJob, error) {
	seq := time.Now().UnixNano()
	room := libMax - len(strconv.FormatInt(seq, 10)) - 1
	if scan {
		room -= len("scan\n")
	}
	fit := func(paths []string, word string) (now, rest []string) {
		for i, p := range paths {
			if room -= len(word) + len(p) + 2; room < 0 {
				return paths[:i], paths[i:]
			}
		}
		return paths, nil
	}
	imports, restImports := fit(imports, "import")
	adds, restAdds := fit(adds, "add")
	if err := fw.RequestLibrary(d.libReq(), seq, scan, imports, adds); err != nil {
		return nil, err
	}
	d.lib, d.libRest = &libJob{seq: seq}, [2][]string{restImports, restAdds}
	return d.lib, nil
}

func (d *daemon) libReq() string { return filepath.Join(d.tmp, "q2-jellyfin.library") }

// libState is the followed request's state, once any answer is taken. d.mu is held, d.lib is not nil.
func (d *daemon) libState() (done, ok bool, msg string, at time.Time) {
	d.libPoll()
	j := d.lib
	return j.done, j.ok, j.msg, j.doneAt
}

// libPoll takes the firmware's answer to the request on file, once (R35). The playlists it imported
// become the app's own. One it skipped because the Q2 has a playlist of that name (the user's) is
// renamed "<name> (Jellyfin).m3u" and added in a follow-up request; skipped again under that name,
// it is an error. The request file says what was sent, so an answer that comes after a restart is
// taken too (taking one twice changes nothing). Called every second (tick) and by the pages that
// show it. d.mu is held.
func (d *daemon) libPoll() {
	seq, sent := fw.ReadRequest(d.libReq())
	if seq == 0 || seq == d.libTaken {
		return
	}
	done, ok, msg, exists := fw.LibraryDone(d.libReq()+".done", seq)
	if !done {
		return
	}
	if ok {
		again, clash, err := d.runner.Imported(sent, exists)
		rest := d.libRest
		switch {
		case errors.Is(err, syncer.ErrBusy):
			return // a sync holds the manifest: taken once it ends
		case err != nil:
			d.log.Printf("library: %v", err)
			ok, msg = false, "Couldn't record the imported playlists"
		case len(clash) > 0:
			ok, msg = false, "Couldn't import "+strings.TrimSuffix(filepath.Base(clash[0]), ".m3u")+
				": a playlist with that name is already on the Q2"
		case !d.libAgain && len(again)+len(rest[0])+len(rest[1]) > 0: // libAgain's request names them all
			d.libTaken = seq
			_, err := d.writeLibrary(false, there(rest[0]), there(append(rest[1], again...)))
			if err == nil {
				return // the follow-up is the request followed now
			}
			d.log.Printf("library: %v", err)
			ok, msg = false, "Couldn't ask for a library update"
		}
	}
	d.libRest = [2][]string{}
	d.libTaken = seq
	if j := d.lib; j != nil && j.seq == seq {
		j.done, j.ok, j.msg, j.doneAt = true, ok, msg, time.Now()
	}
	if d.libAgain {
		d.libAgain = false
		if _, err := d.sendLibrary(); err != nil {
			d.log.Printf("library: %v", err)
		}
	}
}

// there is paths but those gone since (a sync deleted the playlist): the firmware fails a whole
// request over one it can't find.
func there(paths []string) []string {
	var out []string
	for _, p := range paths {
		if exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// syncDone is the runner's OnDone: with cap library, a sync that changed anything asks the firmware
// to scan and import, whether or not the Sync page is open.
func (d *daemon) syncDone(p plan.Plan) {
	d.mu.Lock()
	d.lastReq = time.Now() // the summary and Failed Songs stay as long as after a press (I15)
	d.mu.Unlock()
	if !d.caps.Library || !changes(p) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.sendLibrary(); err != nil {
		d.log.Printf("library: %v", err)
	}
}

type libPlaylist struct {
	id, name string
	songs    []jellyfin.Song
}

type libResult struct {
	songs     []jellyfin.Song
	playlists []libPlaylist
}

const maxAlbums = 100_000 // what Entire Library lists at most: past that the server is broken, or the Q2 too small

// resolveLibrary lists every album and every song (500 a page) and every playlist, then fetches
// each playlist's songs. Every request takes a slot of sem, so the library never runs more than the
// preview's 4 at once. Albums keep their listing order; so do playlists.
func (d *daemon) resolveLibrary(ctx context.Context, cl *jellyfin.Client, sem chan struct{}) (libResult, error) {
	ctx, cancel := context.WithCancel(ctx) // the first failure stops the queued fetches
	defer cancel()
	slot := func() func() { sem <- struct{}{}; return func() { <-sem } }
	var albums []jellyfin.Album
	seen := map[string]bool{}
	for off := 0; ; {
		release := slot()
		page, err := cl.Albums(ctx, off, 500, "name", false)
		release()
		if err != nil {
			return libResult{}, err
		}
		n := len(albums)
		for _, a := range page {
			if !seen[a.ID] {
				seen[a.ID] = true
				albums = append(albums, a)
			}
		}
		// A short page is the last; a page with nothing new is a server that ignores the offset (M10).
		if len(page) < 500 || len(albums) == n {
			break
		}
		if len(albums) > maxAlbums {
			return libResult{}, fmt.Errorf("The library has more than %d albums", maxAlbums)
		}
		off += len(page)
	}
	release := slot()
	all, err := cl.AllSongs(ctx) // every album's songs, 500 a request
	release()
	if err != nil {
		return libResult{}, err
	}
	// In the albums' order, as getAlbum gave them, sorted in place (one copy of a big library). A
	// song whose album the album pages missed (they shifted mid-fetch, audit S14) is kept and sorted
	// last, so a library scan on the server doesn't make the sync delete songs already on the card; a
	// song with no album at all stays out, as before.
	at := make(map[string]int, len(albums))
	for i, a := range albums {
		at[a.ID] = i
	}
	pos := func(id string) int {
		if i, ok := at[id]; ok {
			return i
		}
		return len(albums)
	}
	all = slices.DeleteFunc(all, func(s jellyfin.Song) bool { return s.AlbumID == "" })
	slices.SortStableFunc(all, func(x, y jellyfin.Song) int { return cmp.Compare(pos(x.AlbumID), pos(y.AlbumID)) })
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].AlbumID == all[i].AlbumID {
			j++
		}
		if k, ok := at[all[i].AlbumID]; ok { // skip oneArtist for a song whose album wasn't listed
			oneArtist(albums[k], all[i:j])
		}
		i = j
	}
	release = slot()
	pls, err := cl.Playlists(ctx, "name", false)
	release()
	if err != nil {
		return libResult{}, err
	}
	res := libResult{playlists: make([]libPlaylist, len(pls))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	// The slot is taken before the goroutine starts: fetches run in order, a failure stops the
	// rest at once, and a large library never parks thousands of goroutines on the semaphore.
	run := func(f func() error) {
		release := slot()
		if ctx.Err() != nil {
			release()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer release()
			if e := f(); e != nil {
				mu.Lock()
				first = cmp.Or(first, e)
				mu.Unlock()
				cancel()
			}
		}()
	}
	for i, p := range pls {
		run(func() (err error) {
			var pl jellyfin.Playlist
			pl, res.playlists[i].songs, err = cl.Playlist(ctx, p.ID)
			res.playlists[i].id, res.playlists[i].name = p.ID, cmp.Or(pl.Name, p.Name)
			return
		})
	}
	wg.Wait()
	if first != nil {
		return libResult{}, first
	}
	res.songs = all
	d.mu.Lock()
	d.resolved[selection.Key(selection.KindLibrary, "all")] = res.songs
	d.libCount = [2]int{len(albums), len(pls)}
	d.mu.Unlock()
	return res, nil
}

// backfillLyrics gives a song already on the card, whose lyrics the server has, the .lrc it lacks:
// songs synced before the app wrote lyrics (0.1), or whose lyrics were added on the server since.
// Songs the plan moves, replaces or removes are left to the sync. A song is asked for once a run
// (lyricsTried): one the server has no lyrics for after all would be asked at every preview. It is
// marked before the request, so an overlapping backfill doesn't ask too, and unmarked when no
// answer came (the preview dropped, the server out of reach), so a later preview asks again.
func (d *daemon) backfillLyrics(ctx context.Context, resolved map[string][]jellyfin.Song, pl plan.Plan) {
	busy := map[string]bool{}
	for _, w := range pl.Replace {
		busy[w.Song.ID] = true
	}
	for _, mv := range pl.Rename {
		busy[mv.ID] = true
	}
	for _, e := range pl.Remove {
		busy[e.ID] = true
	}
	m := d.manifest()
	for _, songs := range resolved {
		for _, s := range songs {
			e := m.Songs[s.ID]
			if !s.HasLyrics || e == nil || busy[s.ID] || ctx.Err() != nil || exists(library.LyricsPath(e.Path)) {
				continue
			}
			d.mu.Lock()
			tried := d.lyricsTried[s.ID]
			d.lyricsTried[s.ID] = true
			d.mu.Unlock()
			if tried {
				continue
			}
			err := syncer.WriteLyrics(ctx, src{d}, s, e.Path, d.log.Printf)
			var ae *jellyfin.APIError
			if ctx.Err() != nil || errors.Is(err, jellyfin.ErrUnreachable) && !(errors.As(err, &ae) && ae.HTTP != 0) {
				d.mu.Lock()
				delete(d.lyricsTried, s.ID)
				d.mu.Unlock()
			}
		}
	}
}
