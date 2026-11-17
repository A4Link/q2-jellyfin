package main

import (
	"errors"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
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

// spaceRows say why a run stopped at Space to Keep Free, and what to do; nil for any other stop.
func spaceRows(err error) []q2spot.Row {
	var sl *syncer.SpaceLowError
	if !errors.As(err, &sl) {
		return nil
	}
	free := q2spot.Row{Kind: "info", Title: size(sl.Left) + " free · keeping " + size(sl.Reserve) + " free"}
	if sl.Next > 0 { // 0 when the server gave no size
		free.Subtitle = "The next song needs " + size(sl.Next)
	}
	rows := []q2spot.Row{info("Paused: the card is nearly full"), free, info("Songs synced so far are on the card")}
	if sl.Held && sl.Retry { // a shared failure (a lost connection): the next run tries again
		return append(rows, info("Songs failed, so nothing was removed"), info("Resume to try them again"))
	}
	if sl.Held { // songs' own failures, outside Entire Library (re-audit 2026-11-10 #3)
		return append(rows, info("Songs failed, so nothing was removed"), info("Resume, or take them out of Sync"))
	}
	return append(rows, info("Free some space or sync less, then Resume"))
}
