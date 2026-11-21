package main

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// play streams a list from an index ("kind:id index", after a song row's own "song:<id>"), or says
// why it can't. A song row plays its own song: from where it is in a list changed on the server
// since the page was drawn, or not at all if it has gone. A song that plays gets no message: Q2 Pod
// toasts a message full-screen before Now Playing opens, which shows the player's own buffering.
// Without Wi-Fi only a synced song of the page's list plays (fromCard).
func (d *daemon) play(ctx context.Context, cl *jellyfin.Client, arg string) q2spot.Reply {
	f := strings.Fields(arg)
	pressed := ""
	if len(f) == 3 {
		_, pressed = parseKey(f[0])
		f = f[1:]
	}
	if len(f) != 2 {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	i, _ := strconv.Atoi(f[1])
	kind, id := parseKey(f[0])
	l, _ := d.shownList(cl, selection.Key(kind, id)) // the list the page showed, if fresh (audit B3)
	songs := l.songs
	if songs == nil {
		if r, off := d.noWiFi(true); off {
			return r
		}
		var err error
		if songs, _, err = d.songsOf(ctx, cl, kind, id); err != nil {
			return d.errToast(err)
		}
	}
	if pressed != "" && (i < 0 || i >= len(songs) || songs[i].ID != pressed) {
		if i = slices.IndexFunc(songs, func(s jellyfin.Song) bool { return s.ID == pressed }); i < 0 {
			return q2spot.Fail(q2spot.Unavailable, "The list has changed")
		}
	}
	if i < 0 || i >= len(songs) {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	if !d.fromCard(songs[i].ID) {
		if r, off := d.noWiFi(true); off {
			return r
		}
	}
	d.startSongs(songs, i)
	return q2spot.Reply{}
}

// fromCard is whether song id plays from the card, without Wi-Fi: synced (stream.go).
func (d *daemon) fromCard(id string) bool {
	return d.stream.synced(d.manifest().Songs[id])
}

// startSongs plays songs from i.
func (d *daemon) startSongs(songs []jellyfin.Song, i int) {
	// R27: reply now, start behind the last start.
	d.mu.Lock()
	d.playSeq++
	seq := d.playSeq
	d.mu.Unlock()
	d.spawn(func() {
		d.playMu.Lock()
		defer d.playMu.Unlock()
		d.mu.Lock()
		latest := seq == d.playSeq
		d.mu.Unlock()
		if latest {
			d.stream.start(songs, i)
		}
	})
}
