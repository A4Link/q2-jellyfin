package main

import (
	"context"
	"errors"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
	"github.com/A4Link/q2-jellyfin/internal/syncer"
)

// Space to Keep Free (Settings): a sync stops before a download would leave the card with less than
// this, rather than filling it (syncer.Runner.Reserve). The Q2 needs room of its own: its library
// database, playlists, the playback cache's moves, a firmware update.

var keepFreeSizes = []string{"256M", "512M", "1G", "2G"}

const defaultKeepFree = "512M"

// keepFreeBytes is the setting in bytes, decimal (512 MB = 512,000,000) as size() shows sizes, so
// the setting and the Sync page's messages read the same.
func keepFreeBytes(v string) int64 {
	switch v {
	case "256M":
		return 256e6
	case "1G":
		return 1e9
	case "2G":
		return 2e9
	}
	return 512e6
}

func keepFreeName(v string) string {
	switch v {
	case "256M":
		return "256 MB"
	case "1G":
		return "1 GB"
	case "2G":
		return "2 GB"
	}
	return "512 MB"
}

func nextKeepFree(v string) string {
	if v == "" {
		v = defaultKeepFree
	}
	for i, s := range keepFreeSizes {
		if s == v {
			return keepFreeSizes[(i+1)%len(keepFreeSizes)]
		}
	}
	return defaultKeepFree
}

// syncSources are the Sync page's options, in every state of it: Sync Jobs, On This Q2, Sync
// Favourites, Choose Playlists and Sync Entire Library. A damaged selection still lists them, so
// On This Q2 can reset it; the others say so when pressed.
func (d *daemon) syncSources() []q2spot.Row {
	sel, err := d.selection()
	fav := q2spot.Row{Kind: "action", URI: "syncfav", Title: "Sync Favourites", Subtitle: "Add your favourites to Sync and start"}
	if sel.Has(selection.KindFavorites, "all") || sel.Has(selection.KindLibrary, "all") {
		fav.Subtitle = "In Sync · press to sync now"
	}
	n := 0
	for _, it := range sel.Items {
		if it.Kind == selection.KindPlaylist {
			n++
		}
	}
	return []q2spot.Row{jobsRow, list("onq2", "On This Q2", d.onQ2Subtitle(sel, err), ""), fav,
		list("syncpl", "Choose Playlists", plural(n, "playlist")+" in Sync", ""), d.libraryRow(sel)}
}

// syncFavourites is Sync Favourites: Favourites joins Sync (at Sync Quality) if it isn't there,
// and the sync starts by itself once its preview is ready. As every sync, it downloads what is
// missing for the whole selection and stops before the card falls under Space to Keep Free.
func (d *daemon) syncFavourites(ctx context.Context, cl *jellyfin.Client) q2spot.Reply {
	if st := d.runner.Snapshot().State; st == "running" || st == "paused" {
		return q2spot.Reply{Message: "A sync is already under way"}
	}
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, selDamaged)
	}
	msg := "Syncing Favourites"
	if !sel.Has(selection.KindFavorites, "all") && !sel.Has(selection.KindLibrary, "all") {
		r := d.toggle(ctx, cl, favKey)
		if r.Status != "" && r.Status != q2spot.OK {
			return r
		}
		msg = r.Message // "Added to Sync · N songs"
	}
	d.mu.Lock()
	d.autoStart = true
	d.dropPreview()
	d.mu.Unlock()
	d.startPreview(cl)
	return q2spot.Reply{Message: msg + " · starting"}
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

// spaceRows say why a run stopped at Space to Keep Free, and what to do; nil for any other stop.
func spaceRows(err error) []q2spot.Row {
	var sl *syncer.SpaceLowError
	if !errors.As(err, &sl) {
		return nil
	}
	rows := []q2spot.Row{info("Stopped: the card is nearly full"),
		info(size(sl.Left) + " free · keeping " + size(sl.Reserve) + " free"),
		info("Songs synced so far are on the card")}
	if sl.Held { // the removals would have made room (re-audit 2026-11-10 #3)
		return append(rows, info("A song failed, so nothing was removed"), info("Take it out of Sync, then Resume"))
	}
	return append(rows, info("Free some space or sync less, then Resume"))
}
