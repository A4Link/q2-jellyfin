package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/fw"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

type libJob struct {
	seq    int64
	done   bool
	ok     bool
	msg    string
	doneAt time.Time
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
		if !d.libAgain {
			d.libAgain = true
			d.saveLib(true)
		}
		if d.lib == nil || d.lib.seq != seq {
			d.lib = &libJob{seq: seq}
		}
		return d.lib, nil
	}
	var imports, adds []string
	m, _ := d.manifestErr()
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
	d.libAgain = false // this request names them all
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
	d.saveLib(true)
	return d.lib, nil
}

// libMore is .jellyfin/library.more, there while a library request awaits its answer: empty, or
// "more" when it leaves playlists to send (libRest, libAgain). The app may exit before the firmware
// answers (I15), and /tmp is RAM: the next run takes the answer, or sends the request again if
// the Q2 was switched off first (libStart).
func (d *daemon) libMore() string { return filepath.Join(d.dir, "library.more") }

// saveLib keeps library.more: waiting, a request awaits its answer. d.mu is held.
func (d *daemon) saveLib(waiting bool) {
	var err error
	if d.libAgain || len(d.libRest[0])+len(d.libRest[1]) > 0 {
		err = q2.WriteFile(d.libMore(), []byte("more"))
	} else if waiting {
		err = q2.WriteFile(d.libMore(), nil)
	} else if err = os.Remove(d.libMore()); errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		d.log.Printf("library: %v", err)
	}
}

// libStart picks up the library request an earlier run left (library.more): its answer is taken
// as usual (libPoll), and "more" (read by newDaemon: libAgain) sends them all once it comes. With
// no request in /tmp, the Q2 was switched off before the firmware ran it: it is sent again. It runs
// in the background, so the socket listens at once; a request this run already sent (d.lib) names
// them all.
func (d *daemon) libStart() {
	if !exists(d.libMore()) {
		return // no request left by an earlier run
	}
	d.manifestErr() // a resend's playlists: read here, not under d.mu in sendLibrary
	d.mu.Lock()
	defer d.mu.Unlock()
	if !exists(d.libMore()) || d.lib != nil {
		return
	}
	if seq, _ := fw.ReadRequest(d.libReq()); seq == 0 && d.caps.Library {
		if _, err := d.sendLibrary(); err != nil {
			d.log.Printf("library: %v", err)
		}
	}
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
// it is an error, shown once the follow-ups' answers come too. The request file says what was sent, so an answer that comes after a restart is
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
	if d.runner.Busy() {
		return // a sync or its OnDone holds or reads the manifest: taken at the first tick after
	}
	if ok {
		var again, clash []string
		var err error
		// Imported loads the whole manifest: only when it can record something.
		if len(exists) > 0 || d.newImports(sent) {
			again, clash, err = d.runner.Imported(sent, exists)
		}
		rest := d.libRest
		d.libClash = append(d.libClash, clash...)
		for _, f := range clash { // logged now: a follow-up's answer may come only after a restart
			d.log.Printf("library: %s clashes with a Q2 playlist of that name", f)
		}
		switch {
		case errors.Is(err, syncer.ErrBusy):
			return // a sync or a preview holds the manifest: taken once it ends
		case err != nil:
			d.log.Printf("library: %v", err)
			ok, msg = false, "Couldn't record the imported playlists"
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
	if c := d.libClash; ok && len(c) > 0 {
		name := strings.TrimSuffix(filepath.Base(c[0]), ".m3u")
		ok, msg = false, "Couldn't import "+name+": the Q2 has a playlist with that name. Delete it in Library → Playlists, then Check Again"
		if n := len(c) - 1; n > 0 {
			msg = fmt.Sprintf("Couldn't import %s and %d more: the Q2 has playlists with those names. Delete them in Library → Playlists, then Check Again", name, n)
		}
	}
	if !ok {
		d.log.Printf("library: %s", msg)
	}
	d.libRest, d.libClash = [2][]string{}, nil
	d.libTaken = seq
	d.saveLib(false)
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

// newImports is whether sent names a file not yet recorded as imported, read from the copy pages
// share (or a manifest that can't be read: Imported says why).
func (d *daemon) newImports(sent [2][]string) bool {
	m, err := d.manifestErr()
	if err != nil {
		return true
	}
	for _, files := range sent {
		for _, f := range files {
			if !m.Imported[f] {
				return true
			}
		}
	}
	return false
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
// to scan and import, whether or not the Sync page is open. library.more is written first, before
// the manifest read: an app stopped during that read sends the request at the next start (libStart).
func (d *daemon) syncDone(p plan.Plan) {
	d.mu.Lock()
	d.lastReq = time.Now() // the summary and Failed Songs stay as long as after a press (I15)
	send := d.caps.Library && p.Changes()
	if send {
		if err := q2.WriteFile(d.libMore(), []byte("more")); err != nil {
			d.log.Printf("library: %v", err)
		}
	}
	d.mu.Unlock()
	if !send {
		return
	}
	d.manifestErr() // the run's last save is read here, not under d.mu in sendLibrary
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.sendLibrary(); err != nil {
		d.log.Printf("library: %v", err)
	}
}

// libWait bounds how long a library request awaiting its answer has the running app tick every
// second, so its follow-ups go at once (libPoll). The firmware may hold one for ever (playback under
// a lit screen, a page it won't leave).
const libWait = 2 * time.Hour

// libWaiting is whether the followed library request awaits its answer, sent less than libWait
// ago: its seq is the time it was written. d.mu is held.
func (d *daemon) libWaiting(now time.Time) bool {
	return d.lib != nil && !d.lib.done && now.Sub(time.Unix(0, d.lib.seq)) < libWait
}
