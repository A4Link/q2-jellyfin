package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// play streams a list from an index ("kind:id index", after a song row's own "song:<id>"). A song
// that plays at once gets no message: Q2 Pod toasts a message full-screen before Now Playing opens.
func (d *daemon) play(ctx context.Context, cl *jellyfin.Client, arg string) q2spot.Reply {
	r, title, ok := d.startPlay(ctx, cl, arg)
	if !ok {
		return r
	}
	return d.playing(title)
}

// playing is a Play's reply once its song title is started: "Buffering …" while it waits.
func (d *daemon) playing(title string) q2spot.Reply {
	if st, waiting := d.waitingNow(); waiting && st.title == title {
		return q2spot.Reply{Message: "Buffering " + title + "…"}
	}
	return q2spot.Reply{}
}

// startPlay starts the list and song arg names ("[song:id] <list kind:id> <index>"), reporting the
// song's title, or a failed reply. Without Wi-Fi only a synced song of the page's list plays
// (fromCard).
func (d *daemon) startPlay(ctx context.Context, cl *jellyfin.Client, arg string) (q2spot.Reply, string, bool) {
	f := strings.Fields(arg)
	if len(f) == 3 {
		f = f[1:]
	}
	if len(f) != 2 {
		return q2spot.Fail(q2spot.Bad, "Bad request"), "", false
	}
	i, _ := strconv.Atoi(f[1])
	kind, id := parseKey(f[0])
	l, _ := d.shownList(cl, selection.Key(kind, id)) // the list the page showed, if fresh (audit B3)
	songs := l.songs
	if songs == nil {
		if r, off := d.noWiFi(true); off {
			return r, "", false
		}
		var err error
		if songs, _, err = d.songsOf(ctx, cl, kind, id); err != nil {
			return d.errToast(err), "", false
		}
	}
	if i < 0 || i >= len(songs) {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play"), "", false
	}
	if !d.fromCard(songs[i].ID) {
		if r, off := d.noWiFi(true); off {
			return r, "", false
		}
	}
	d.startSongs(songs, i, f[0])
	return q2spot.Reply{}, songs[i].Title, true
}

// fromCard is whether song id plays from the card, without Wi-Fi: synced, with the firmware's
// playlist extension (stream.go). Without it every song is fetched.
func (d *daemon) fromCard(id string) bool {
	return d.caps.Playlist && d.stream.synced(d.manifest().Songs[id])
}

// startSongs plays songs from i, of the list key ("kind:id"; status.go shows its song waiting).
func (d *daemon) startSongs(songs []jellyfin.Song, i int, key string) {
	// R27: starting stops the last stream, which may be mid-fetch; reply now, start behind it.
	d.mu.Lock()
	d.playSeq++
	seq := d.playSeq
	d.playKey = key
	d.mu.Unlock()
	// status.go: the list shows this song waiting until it plays; one on the card plays at once.
	if d.caps.URLs || d.fromCard(songs[i].ID) {
		// URL queues (stream_urls.go): Now Playing shows the firmware's own buffering
		d.stream.setStatus(streamStatus{})
	} else {
		d.stream.setStatus(streamStatus{seq: -1, index: i, id: songs[i].ID, title: songs[i].Title, at: time.Now()})
	}
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
