package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
	"github.com/A4Link/q2-jellyfin/internal/selection"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

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
		return d.stoppedRows(snap)
	}
	if _, err := d.selection(); err != nil {
		return damagedSelection()
	}
	man, err := d.manifestErr()
	if err != nil {
		d.log.Printf("sync: manifest: %v", err)
		return q2spot.Reply{Rows: append(d.syncSources(), info("Couldn't read the sync records on the card"))}
	}
	checking := q2spot.Reply{Message: "refresh", Rows: append(d.syncSources(), info("Checking server…"))}
	p, started := d.startPreview(cl)
	if started {
		return checking
	}
	d.mu.Lock()
	done, at, pl, err, gone := p.done, p.at, p.plan, p.err, p.gone
	if done && err != nil && d.preview == p {
		d.preview = nil // shown once; the next Sync tries again
	}
	d.mu.Unlock()
	switch {
	case !done:
		if time.Since(at) > 20*time.Second {
			checking.Rows[len(checking.Rows)-1].Subtitle = "A big library takes minutes"
		}
		return checking
	case err != nil: // offline too: the options stay, On This Q2 needs no server
		r := d.errRows(err)
		r.Rows = append(d.syncSources(), r.Rows[0], act("check", "Check Again", ""), r.Rows[1])
		return r
	}
	rows := d.syncSources()
	for i, it := range gone { // a few by name: the page is one reply, and Start Sync is its last row
		if i == 5 {
			rows = append(rows, info(fmt.Sprintf("and %d more no longer on the server", len(gone)-5)))
			break
		}
		rows = append(rows, info(it.Name+" · no longer on the server"))
	}
	r := previewRows(pl, keepFreeBytes(d.settings().KeepFree), len(man.Songs))
	r.Rows = append(rows, r.Rows...)
	return r
}

// stopMessage is why a sync stopped, in a sentence: a card error by what it means, never Go's
// text for it (M34).
func (d *daemon) stopMessage(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "The card is full"
	case errors.Is(err, syscall.EROFS):
		return "The card is read-only. Check it in a computer"
	case errors.Is(err, q2.ErrNoCard):
		return "Card not available"
	case errors.As(err, &se) || errors.As(err, &te) || errors.Is(err, io.ErrUnexpectedEOF):
		return "Couldn't read the sync records on the card"
	case errors.As(err, &pe) || errors.As(err, &le):
		return "Couldn't write to the card. Check it in a computer"
	}
	return jellyfin.Message(err, d.hostName())
}

// downloadsHint is the way round a refused download, a row of its own under the message: under an
// Opus Sync Quality only a song kept as it is (an MP3, AAC or Opus one) is downloaded, so Always
// Re-encode syncs without the permission; under Original, an Opus one with it does.
func (d *daemon) downloadsHint(err error) []q2spot.Row {
	s := d.settings()
	switch {
	case !errors.Is(err, jellyfin.ErrDownloadsOff):
		return nil
	case quality.IsOriginal(s.def()):
		return []q2spot.Row{info("Or choose an Opus Sync Quality and Always Re-encode")}
	case !s.reencode():
		return []q2spot.Row{info("Turn on Always Re-encode in Quality and Storage")}
	}
	return nil
}

func previewRows(p *plan.Plan, reserve int64, onCard int) q2spot.Reply {
	if !p.Changes() {
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
		rows = append(rows, list("removed", "−"+plural(len(p.Remove), "song")+" · "+size(p.RemoveBytes), "", ""))
		if len(p.Remove)*2 > onCard { // a library the server lost, or a Remove from Sync too many
			rows = append(rows, info("Removes most of the synced songs"))
		}
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
	if p.Leftover > 0 {
		rows = append(rows, info("Delete "+plural(p.Leftover, "file")+" an earlier sync left"))
	}
	switch {
	case p.PlaylistsChanged > 0:
		rows = append(rows, info("Update "+plural(p.PlaylistsChanged, "playlist")))
	case p.PlaylistsStale:
		rows = append(rows, info("Update playlists"))
	}
	rows = append(rows, info("Needs "+size(max(p.NeedBytes, 0))+" · "+size(p.FreeBytes)+" free"))
	// Whether it all fits keeps Space to Keep Free in hand too: a sync that would end under it pauses
	// there. It starts anyway, and the run pauses before the card falls under it. The removals count
	// as freed: a run short of space removes them first (syncer space).
	if p.NeedBytes > 0 && p.NeedBytes-p.RemoveBytes+plan.Margin+reserve > p.FreeBytes {
		rows = append(rows, info("Not all of it fits on the card"), info("It pauses with "+size(reserve)+" left"))
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
	count := fmt.Sprintf("%d of %s", s.Done, plural(s.Total, "song"))
	if s.Failed > 0 {
		count += fmt.Sprintf(" · %d failed", s.Failed)
	}
	rows = append(rows, info(count))
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
			rows = append(append(rows, info(jellyfin.Message(s.Err, d.hostName()))), d.downloadsHint(s.Err)...) // with code 50 (R39) too
		}
		if s.Failed > 0 {
			rows = append(rows, list("failed", "Failed Songs", "", ""))
		}
		rows = append(rows, act("resume", "Resume", ""))
	} else {
		rows = append(rows, act("pause", "Pause", ""))
	}
	rows = append(rows, act("cancel", "Cancel", ""))
	return q2spot.Reply{Message: "refresh", Rows: rows}
}

// syncSources are the Sync page's options, in every state of it: Sync Jobs, On This Q2, Sync
// Favourites, Choose Playlists and Sync Entire Library. A damaged selection still lists them, so
// On This Q2 can reset it; the others say so when pressed.
func (d *daemon) syncSources() []q2spot.Row {
	sel, err := d.selection()
	fav := q2spot.Row{Kind: "action", URI: "syncfav", Title: "Sync Favourites", Subtitle: "Add your favourites to Sync"}
	if sel.Has(selection.KindFavorites, "all") {
		fav.Subtitle = "In Sync · press to check for new hearts"
	}
	n := 0
	for _, it := range sel.Items {
		if it.Kind == selection.KindPlaylist {
			n++
		}
	}
	pl := plural(n, "playlist") + " in Sync"
	if sel.Has(selection.KindLibrary, "all") { // as the rows of Choose Playlists say
		pl = "Included via Entire Library"
	}
	return []q2spot.Row{jobsRow, list("onq2", "On This Q2", d.onQ2Subtitle(sel, err), ""), fav,
		list("syncpl", "Choose Playlists", pl, ""), d.libraryRow(sel)}
}

// jobsRow opens Sync Jobs.
var jobsRow = list("jobs", "Sync Jobs", "Hold or resume what syncs", "")

// jobs is Sync Jobs: every item in Sync not yet all on the Q2, and every held one. A press holds
// or resumes it (hold). One the preview found gone is an info row: holding it would keep nothing.
func (d *daemon) jobs(cursor string) q2spot.Reply {
	sel, err := d.selection()
	if err != nil {
		return damagedSelection()
	}
	gone := d.previewGone()
	v := d.view()
	var rows []q2spot.Row
	for _, it := range sel.Items {
		if isGone(gone, it) {
			rows = append(rows, q2spot.Row{Kind: "info", Title: it.Name, Subtitle: "No longer on the server"})
			continue
		}
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
// what it syncs or at what quality. A running one starts again by itself unless it removes songs;
// a paused one waits for Start Sync on the Sync page. It returns what to add to the reply: "" with
// no sync under way.
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
	return " · checking the sync again"
}

// onQ2Subtitle is On This Q2's subtitle: how many items are chosen, and the bytes on the card. sel
// and err are the selection syncSources already read.
func (d *daemon) onQ2Subtitle(sel selection.Set, err error) string {
	if err != nil {
		return "Selection file is damaged"
	}
	// While a sync runs, the copy already read: reading each save again would hold two copies
	// (the bytes catch up when it ends).
	d.manMu.Lock()
	m := d.man
	d.manMu.Unlock()
	if m == nil || !d.runner.Busy() {
		m = d.manifest()
	}
	var bytes int64
	for _, e := range m.Songs {
		bytes += e.Size
	}
	return plural(len(sel.Items), "item") + " · " + size(bytes)
}

// onQ2 is the selection, each with its status and size on the card; a hold takes one out. Entire
// Library is switched from the Sync page; here it shows only while on, as one of the items.
func (d *daemon) onQ2(cursor string) q2spot.Reply {
	if _, err := d.selection(); err != nil {
		return damagedSelection()
	}
	gone := d.previewGone()
	v := d.view()
	var rows []q2spot.Row
	if len(v.sel.Items) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{{Kind: "info", Title: "Nothing chosen yet",
			Subtitle: "Open a playlist, album or artist and press Add to Sync"}}}
	}
	for _, it := range v.sel.Items {
		key := selection.Key(it.Kind, it.ID)
		if it.Kind == selection.KindLibrary {
			lib := d.libraryRow(v.sel) // inert here, as songs are; a hold still takes it out
			lib.Kind, lib.URI, lib.Title = "info", "", "Entire Library"
			rows = append([]q2spot.Row{lib}, rows...)
			continue
		}
		var b int64
		for _, sid := range v.keyed[key] {
			b += v.m.Songs[sid].Size
		}
		total := 0
		if it.Kind == selection.KindSong {
			total = 1
		}
		row := v.item(it.Kind, it.ID, it.Name, "", total)
		row.Subtitle = join(row.Subtitle, size(b))
		gn := isGone(gone, it)
		if gn { // as Sync Jobs says; its page would fail, and a hold still takes it out
			row.Subtitle = join("No longer on the server", size(b))
		}
		if it.Kind == selection.KindSong || gn {
			row.Kind, row.URI = "info", ""
		}
		rows = append(rows, row)
	}
	return page(rows, cursor)
}

// syncFavourites is Sync Favourites: Favourites joins Sync (at Sync Quality) if it isn't there,
// held in Sync Jobs it is resumed, and the sync starts by itself once its preview is ready, unless
// it removes songs. As every sync, it downloads what is missing for the whole selection and stops
// before the card falls under Space to Keep Free.
func (d *daemon) syncFavourites(ctx context.Context, cl *jellyfin.Client) q2spot.Reply {
	if st := d.runner.Snapshot().State; st == "running" || st == "paused" {
		return q2spot.Reply{Message: "A sync is already under way"}
	}
	d.selMu.Lock()
	sel, err := d.selection()
	if err != nil {
		d.selMu.Unlock()
		return q2spot.Fail(q2spot.Error, selDamaged)
	}
	i := slices.IndexFunc(sel.Items, func(it selection.Item) bool { return it.Kind == selection.KindFavorites && it.ID == "all" })
	if i >= 0 && sel.Items[i].Held { // held, it would add no new hearts
		sel.SetHeld(selection.KindFavorites, "all", false)
		err = d.saveSelection(sel)
	}
	d.selMu.Unlock()
	if err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't save")
	}
	// No start is promised: a sync that removes songs waits for Start Sync.
	msg := "Checking Favourites"
	if i < 0 { // Entire Library has its songs, not its playlist
		r := d.toggle(ctx, cl, favKey)
		if r.Status != "" && r.Status != q2spot.OK {
			return r
		}
		msg = r.Message // "Added to Sync · N songs"
	}
	d.mu.Lock()
	d.autoStart, d.showDone = true, false
	d.dropPreview()
	d.mu.Unlock()
	d.startPreview(cl)
	return q2spot.Reply{Message: msg}
}

// syncPlaylists is Choose Playlists: every music playlist, each pressed to join or leave Sync.
func (d *daemon) syncPlaylists(ctx context.Context, cl *jellyfin.Client, cursor string) q2spot.Reply {
	// The Playlists list, fetched at most once a listFor: the page asks again every 2 s, and a press
	// changes only its subtitle, which the selection gives.
	pls, err := cachedList(d, cl, "playlists", false, func() ([]jellyfin.Playlist, error) { return cl.Playlists(ctx, "name", false) })
	if err != nil {
		r := d.listErr(err, cursor)
		if cursor == "" { // live still, so it shows the list once the server answers
			r.Message = "refresh"
		}
		return r
	}
	if len(pls) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("No playlists on the server")}}
	}
	sel, err := d.selection()
	if err != nil {
		return damagedSelection()
	}
	all := sel.Has(selection.KindLibrary, "all")
	var rows []q2spot.Row
	for _, p := range pls {
		sub := plural(p.SongCount, "song")
		switch {
		case all:
			sub = "Included via Entire Library"
		case sel.Has(selection.KindPlaylist, p.ID):
			sub = "In Sync · " + sub
		}
		rows = append(rows, q2spot.Row{Kind: "action", URI: "toggle", Title: p.Name, Subtitle: sub,
			Context: selection.Key(selection.KindPlaylist, p.ID)})
	}
	r := page(rows, cursor)
	r.Message = "refresh" // every page: a press changes its row
	return r
}

// libraryRow is Sync Entire Library: Off, On, or On with the counts once the preview has resolved it.
func (d *daemon) libraryRow(sel selection.Set) q2spot.Row {
	sub := "Off"
	if sel.Has(selection.KindLibrary, "all") {
		sub = "On"
		d.mu.Lock()
		c, known := d.libCount, d.libKnown // kept while a check drops the list
		d.mu.Unlock()
		if known {
			sub = fmt.Sprintf("On · %s · %s", plural(c[0], "album"), plural(c[1], "playlist"))
		}
	}
	return q2spot.Row{Kind: "action", URI: "toggle", Title: "Sync Entire Library", Subtitle: sub,
		Context: "library:all", Position: "toggle"}
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

// stoppedRows is a sync stopped by an error: why, the songs that failed before it, Retry and Details.
func (d *daemon) stoppedRows(s syncer.Snapshot) q2spot.Reply {
	d.mu.Lock()
	check := d.checkRow()
	d.mu.Unlock()
	msg := "Sync stopped"
	if s.Err != nil {
		msg += " · " + d.stopMessage(s.Err)
	}
	det := errDetails(s.Err, "sync", s.ErrAt, "") // when it stopped (review G3, Minor 3)
	if s.Err != nil {
		det = withErr(det, s.Err)
	}
	rows := append(append(d.syncSources(), info(msg)), d.downloadsHint(s.Err)...)
	if s.Failed > 0 {
		rows = append(rows, list("failed", "Failed Songs", "", ""))
	}
	return q2spot.Reply{Rows: append(rows, act("retry", "Retry", ""), check,
		list("details", "Details", "", det))}
}

func (d *daemon) doneRows(s syncer.Snapshot) q2spot.Reply {
	// What the run did: songs fetched, renamed, removed, files left by an earlier sync deleted; a run
	// that did none of these rewrote playlists (review G3, Minor 7).
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
	if s.Deleted > 0 {
		did = append(did, plural(s.Deleted, "file")+" deleted")
	}
	if len(did) == 0 {
		did = []string{"Playlists updated"}
	}
	rows := append(d.syncSources(), info(fmt.Sprintf("Sync complete · %s · %d failed", strings.Join(did, " · "), s.Failed)))
	if s.Held > 0 { // removals wait for the failed songs (I2)
		sub := "Retry in Failed Songs, or take them out of Sync"
		if s.HeldRetry { // a shared failure: taking songs out doesn't help, and can't under Entire Library
			sub = "Retry in Failed Songs"
		}
		rows = append(rows, q2spot.Row{Kind: "info", Title: plural(s.Held, "song") + " not removed: songs failed to sync", Subtitle: sub})
	}
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
	check := d.checkRow()
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
	reply.Rows = append(rows, check)
	return reply
}

// checkRow is Check Again, saying when Sync changed after the sync started: that change waits for
// it. d.mu is held.
func (d *daemon) checkRow() q2spot.Row {
	r := act("check", "Check Again", "")
	if d.gen != d.runGen {
		r.Subtitle = "Sync changed since it started"
	}
	return r
}

func (d *daemon) failed(cursor string) q2spot.Reply {
	s := d.runner.Snapshot()
	var songs []q2spot.Row
	for _, f := range s.Failures {
		songs = append(songs, list("details", f.Title, f.Msg, detailsCtx(f.Op, f.HTTP, f.Code, f.At, f.SongID)))
	}
	rows := []q2spot.Row{act("retry", "Retry Failed", "")}
	// The runner lists only the first of many; the count goes near the top, within the pages
	// the firmware loads before More….
	if s.Failed > len(songs) {
		rows = append(rows, info(fmt.Sprintf("%d failed · the first %d are listed", s.Failed, len(songs))))
	}
	return page(append(rows, songs...), cursor)
}

// removed lists what the previewed sync deletes.
func (d *daemon) removed(cursor string) q2spot.Reply {
	d.mu.Lock()
	p := d.preview
	ready := p != nil && p.done && p.plan != nil
	d.mu.Unlock()
	if !ready || len(p.plan.Remove) == 0 {
		return page([]q2spot.Row{info("Nothing to remove")}, cursor)
	}
	dir := filepath.Join(d.card, "Jellyfin") + "/"
	return pageOf(len(p.plan.Remove), cursor, func(i int) q2spot.Row {
		return info(strings.TrimPrefix(p.plan.Remove[i].Path, dir))
	})
}

// added lists the songs the sync will add, as removed lists those it takes away.
func (d *daemon) added(cursor string) q2spot.Reply {
	d.mu.Lock()
	p := d.preview
	ready := p != nil && p.done && p.plan != nil
	d.mu.Unlock()
	if !ready || len(p.plan.Add) == 0 {
		return page([]q2spot.Row{info("Nothing to add")}, cursor)
	}
	return pageOf(len(p.plan.Add), cursor, func(i int) q2spot.Row {
		w := p.plan.Add[i]
		return q2spot.Row{Kind: "info", Title: w.Song.Title, Subtitle: join(w.Song.Artist, size(w.Bytes))}
	})
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
		keep, cached := d.stream.cachedFor(p.plan.Add, p.plan.Replace)
		if net := p.plan.NeedBytes - p.plan.RemoveBytes - cached; net > 0 {
			need := net + plan.Margin + keepFreeBytes(d.settings().KeepFree)
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
		d.runGen = p.gen
		d.mu.Unlock()
		d.dropGone(p.gone)
		d.wantReports(true) // with the server in reach: the plays waiting go too
	case "pause": // R27: stopping waits for the download to notice; the reply doesn't
		d.spawn(d.runner.Pause)
	case "resume":
		d.runner.Resume()
	case "cancel":
		d.spawn(d.runner.Cancel)
	case "retry":
		started := d.runner.RetryFailed()
		d.mu.Lock()
		d.showDone = true
		d.mu.Unlock()
		if started {
			return q2spot.Reply{Message: "Sync started again"}
		}
	case "check":
		d.mu.Lock()
		d.showDone = false
		d.dropPreview()
		d.mu.Unlock()
	}
	return q2spot.Reply{}
}
