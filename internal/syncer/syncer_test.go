package syncer

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/selection"
)

// fake is a Navidrome stand-in serving deterministic bytes per song id.
type fake struct {
	mu      sync.Mutex
	status  map[string]int  // id -> forced HTTP status
	once    map[string]int  // id -> HTTP status of the next answer only
	drop    map[string]bool // id -> cut the next response at 50 %
	noRange bool            // answer 200 with the whole file even when asked for a Range
	block   map[string]chan struct{}
	slow    map[string]bool     // id -> send in 32 KiB writes, 3 ms apart
	ranges  map[string][]string // id -> Range headers seen
	hits    map[string]int
	sizes   []string // the size asked of getCoverArt
}

// seen returns the hits and Range headers for id.
func (f *fake) seen(id string) (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[id], append([]string(nil), f.ranges[id]...)
}

func data(id string) []byte {
	if strings.HasPrefix(id, "big") {
		return bytes.Repeat([]byte("song-"+id+"|"), 300000)
	}
	return bytes.Repeat([]byte("song-"+id+"|"), 7000)
}

type slowWriter struct{ http.ResponseWriter }

func (w slowWriter) Write(b []byte) (int, error) {
	time.Sleep(3 * time.Millisecond)
	n, err := w.ResponseWriter.Write(b)
	w.ResponseWriter.(http.Flusher).Flush()
	return n, err
}

// idOf is the item id in a Jellyfin media or image path: /Items/{id}/Download,
// /Audio/{id}/universal, /Audio/{id}/stream, /Items/{id}/Images/Primary.
func idOf(p string) string {
	seg := strings.Split(strings.Trim(p, "/"), "/")
	if len(seg) >= 2 {
		return seg[1]
	}
	return ""
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := idOf(r.URL.Path)
	f.mu.Lock()
	f.hits[id]++
	f.ranges[id] = append(f.ranges[id], r.Header.Get("Range"))
	st, drop, block, slow := f.status[id], f.drop[id], f.block[id], f.slow[id]
	if o := f.once[id]; o != 0 {
		st = o
	}
	delete(f.drop, id)
	delete(f.once, id)
	noRange := f.noRange
	f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/Images/Primary") {
		f.mu.Lock()
		f.sizes = append(f.sizes, r.URL.Query().Get("maxWidth"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		if strings.HasPrefix(id, "big") {
			w.Write(bytes.Repeat([]byte("j"), 16<<20+1))
			return
		}
		w.Write([]byte("jpeg-" + id))
		return
	}
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	if st != 0 {
		w.WriteHeader(st)
		return
	}
	b := data(id)
	w.Header().Set("Content-Type", "audio/flac")
	if drop {
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Write(b[:len(b)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	if noRange {
		r.Header.Del("Range")
	}
	if slow {
		w = slowWriter{w}
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func setup(t *testing.T) (*fake, *Runner, string) {
	t.Helper()
	f := &fake{status: map[string]int{}, once: map[string]int{}, drop: map[string]bool{}, block: map[string]chan struct{}{}, slow: map[string]bool{},
		ranges: map[string][]string{}, hits: map[string]int{}}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c, err := jellyfin.New(jellyfin.Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := New(root, c, nil)
	r.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	r.CardMounted = func() bool { return true }
	r.Net = NewNetSlot()
	return f, r, root
}

func want(root, id, quality string) plan.Want {
	s := jellyfin.Song{ID: id, Title: "T" + id, Album: "Alb", AlbumArtist: "Art", Year: 2000, Disc: 1, Track: len(id),
		Suffix: "flac", CoverArt: "al-1"}
	return plan.Want{Song: s, Quality: quality, Path: library.SongPath(root, s, quality, false), Items: []string{"album:al"},
		Bytes: int64(len(data(id)))}
}

func wait(t *testing.T, r *Runner) Snapshot {
	t.Helper()
	for i := 0; i < 12000; i++ {
		if s := r.Snapshot(); s.State != "running" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("still running")
	return Snapshot{}
}

func manifest(t *testing.T, root string) *library.Manifest {
	t.Helper()
	b, err := os.ReadFile(root + "/.jellyfin/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m := &library.Manifest{}
	if err := json.Unmarshal(b, m); err != nil {
		t.Fatal("manifest not valid JSON:", err)
	}
	return m
}

func sameFile(t *testing.T, path, id string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(b, data(id)) {
		t.Fatalf("%s: %v, %d bytes", path, err, len(b))
	}
}

func noParts(t *testing.T, root string) {
	t.Helper()
	filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
		if strings.HasSuffix(p, ".part") || strings.HasSuffix(p, ".tmp") || strings.HasSuffix(p, ".key") {
			t.Errorf("left over: %s", p)
		}
		return nil
	})
}

func TestRunDownloadsAndRenames(t *testing.T) {
	_, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "mp3-320")
	a.Song.Size = 4242 // the server's size, kept to notice a re-rip (I7)
	old, moved := root+"/Jellyfin/Old/x/01 - r.flac", root+"/Jellyfin/Art/2000 - Alb/09 - r.flac"
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("r"), 0o644)
	m := &library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: old, Quality: "original", Size: 1}}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	p := plan.Plan{Add: []plan.Want{a, b}, Rename: []plan.Move{{From: old, To: moved, ID: "r"}},
		Playlists: map[string][]string{"Mix": {a.Path, moved, a.Path}}}
	if err := r.Start(p); err != nil {
		t.Fatal(err)
	}
	s := wait(t, r)
	if s.State != "done" || s.Done != 2 || s.Total != 2 || s.Failed != 0 || s.Permille != 1000 {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
	sameFile(t, b.Path, "bb")
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("not renamed")
	}
	if _, err := os.Stat(root + "/Jellyfin/Old"); !os.IsNotExist(err) {
		t.Fatal("old album folder left")
	}
	if c, err := os.ReadFile(filepath.Dir(a.Path) + "/cover.jpg"); err != nil || string(c) != "jpeg-al-1" {
		t.Fatal("cover", err)
	}
	noParts(t, root)
	mf := manifest(t, root)
	for _, id := range []string{"a", "bb", "r"} {
		if mf.Songs[id] == nil {
			t.Fatal("manifest misses", id)
		}
	}
	if mf.Songs["r"].Path != moved || mf.Songs["a"].Meta != library.MetaKey(a.Song, "original") ||
		mf.Songs["bb"].Quality != "mp3-320" || mf.Songs["a"].Size != int64(len(data("a"))) || mf.Songs["a"].SrcSize != 4242 {
		t.Fatalf("%+v %+v", mf.Songs["r"], mf.Songs["a"])
	}
	m3u, _ := os.ReadFile(library.PlaylistFile(root, "Mix"))
	if string(m3u) != "#EXTM3U\n"+a.Path+"\n"+moved+"\n"+a.Path+"\n" {
		t.Fatalf("%q", m3u)
	}
	if _, err := os.Stat(root + "/.jellyfin/pending.json"); !os.IsNotExist(err) {
		t.Fatal("pending.json left after done")
	}
}

func TestRunResumeAfterDrop(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "mp3-320")
	f.drop["a"], f.drop["bb"] = true, true
	r.Start(plan.Plan{Add: []plan.Want{a, b}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	half := len(data("a")) / 2
	_, ra := f.seen("a")
	_, rb := f.seen("bb")
	if got := ra; len(got) != 2 || got[0] != "" || got[1] != fmt.Sprintf("bytes=%d-", half) {
		t.Fatalf("ranges %q", got)
	}
	if got := rb; len(got) != 2 || got[1] != "" {
		t.Fatalf("transcoded resumed with Range: %q", got)
	}
	sameFile(t, a.Path, "a")
	sameFile(t, b.Path, "bb")
	noParts(t, root)
}

func TestRunRangeIgnoredRestarts(t *testing.T) {
	f, r, root := setup(t)
	a := want(root, "a", "original")
	f.drop["a"], f.noRange = true, true
	r.Start(plan.Plan{Add: []plan.Want{a}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
}

func TestRunFailureDoesNotStop(t *testing.T) {
	f, r, root := setup(t)
	a, x, c := want(root, "a", "original"), want(root, "x", "original"), want(root, "ccc", "original")
	f.status["x"] = 404
	r.Start(plan.Plan{Add: []plan.Want{a, x, c}})
	s := wait(t, r)
	if hx, _ := f.seen("x"); s.State != "done" || s.Failed != 1 || s.Done != 2 || hx != 1 { // not found: no retry
		t.Fatalf("%+v hits=%d", s, hx)
	}
	fl := s.Failures[0]
	if len(s.Failures) != 1 || fl.HTTP != 404 || fl.SongID != "x" || fl.Op != "download" || fl.Title != "Tx" ||
		fl.At.IsZero() || !strings.Contains(fl.Msg, "404") {
		t.Fatalf("%+v", s.Failures)
	}
	sameFile(t, c.Path, "ccc")
	if _, err := os.Stat(x.Path); !os.IsNotExist(err) {
		t.Fatal("failed song left a file")
	}

	f.mu.Lock()
	delete(f.status, "x")
	f.mu.Unlock()
	r.RetryFailed()
	if s := wait(t, r); s.State != "done" || s.Failed != 0 || s.Done != 3 {
		t.Fatalf("retry %+v", s)
	}
	sameFile(t, x.Path, "x")
	if ha, _ := f.seen("a"); ha != 1 {
		t.Fatal("finished song fetched again")
	}
}

// albumWants is songs songs of album name, which the server answers with status (0: the song).
func albumWants(f *fake, root, name string, songs, status int) []plan.Want {
	var ws []plan.Want
	for i := 0; i < songs; i++ {
		w := want(root, fmt.Sprintf("%s%02d", name, i), "original")
		w.Song.Album = name
		w.Path = library.SongPath(root, w.Song, "original", false)
		ws = append(ws, w)
		f.status[w.Song.ID] = status
	}
	return ws
}

// A server that fails every song (its music drive unmounted, its transcoder broken) doesn't stop
// the sync: after the first failure each song gets one try with no waits, and the playlists are
// written at the end.
func TestFailingSongsDoNotStop(t *testing.T) {
	f, r, root := setup(t)
	var ws []plan.Want
	for i := 0; i < 6; i++ {
		ws = append(ws, albumWants(f, root, fmt.Sprintf("b%d", i), 2, 500)...)
	}
	ok := albumWants(f, root, "ok", 1, 0)
	r.Start(plan.Plan{Add: append(ws, ok...), Playlists: map[string][]string{"Mix": {ok[0].Path, ws[0].Path}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 12 || s.Done != 1 {
		t.Fatalf("%s %v, %d failed, %d done", s.State, s.Err, s.Failed, s.Done)
	}
	hits := 0
	for _, w := range ws {
		h, _ := f.seen(w.Song.ID)
		hits += h
	}
	if hits != len(r.Backoff)+11 {
		t.Errorf("%d requests, want %d for the first song and 1 for each after", hits, len(r.Backoff)+11)
	}
	if _, err := os.Stat(library.PlaylistFile(root, "Mix")); err != nil {
		t.Error("playlist not written:", err)
	}
}

// A song after one that failed gets one request; a song that arrives gives the next its retries.
func TestOneTryAfterFailure(t *testing.T) {
	f, r, root := setup(t)
	a, b, c, d := want(root, "a", "original"), want(root, "bb", "original"), want(root, "ccc", "original"), want(root, "dddd", "original")
	f.status["a"], f.status["bb"], f.status["dddd"] = 500, 500, 500
	r.Start(plan.Plan{Add: []plan.Want{a, b, c, d}})
	if s := wait(t, r); s.State != "done" || s.Failed != 3 || s.Done != 1 {
		t.Fatalf("%+v", s)
	}
	for id, n := range map[string]int{"a": 3, "bb": 1, "ccc": 1, "dddd": 3} {
		if h, _ := f.seen(id); h != n {
			t.Errorf("%s: %d requests, want %d", id, h, n)
		}
	}
}

// One long album the server can't send (its files unreadable) is that album's own: the sync goes
// on to the albums after it.
func TestFailingAlbumDoesNotStop(t *testing.T) {
	f, r, root := setup(t)
	ws := albumWants(f, root, "a", 1, 0)
	ws = append(ws, albumWants(f, root, "bad", 25, 500)...)
	ws = append(ws, albumWants(f, root, "c", 2, 0)...)
	r.Start(plan.Plan{Add: ws})
	if s := wait(t, r); s.State != "done" || s.Failed != 25 || s.Done != 3 {
		t.Fatalf("%s %v, %d failed, %d done", s.State, s.Err, s.Failed, s.Done)
	}
}

func TestRunCardGone(t *testing.T) {
	_, r, root := setup(t)
	a, b, c := want(root, "a", "original"), want(root, "bb", "original"), want(root, "ccc", "original")
	var mounted atomic.Bool
	mounted.Store(true)
	var checks atomic.Int32
	r.CardMounted = func() bool {
		if checks.Add(1) > 2 { // the card goes after the first song
			mounted.Store(false)
		}
		return mounted.Load()
	}
	p := plan.Plan{Add: []plan.Want{a, b, c}}
	r.Start(p)
	s := wait(t, r)
	if s.State != "paused" || !errors.Is(s.Err, q2.ErrNoCard) {
		t.Fatalf("%+v", s)
	}
	// A yanked card loses the records not saved yet (M18); whatever the manifest names is whole.
	m, _ := library.LoadManifest(root + "/.jellyfin/manifest.json")
	for id, e := range m.Songs {
		sameFile(t, e.Path, id)
	}
	if m.Songs["ccc"] != nil {
		t.Fatal("manifest names a song never fetched")
	}

	r.CardMounted = func() bool { return true }
	r.Resume()
	if s := wait(t, r); s.State != "done" || s.Done != 3 || s.Err != nil {
		t.Fatalf("%+v", s)
	}
	for _, w := range p.Add {
		sameFile(t, w.Path, w.Song.ID)
	}
}

// Retry Failed on a paused sync works as Resume: the paused page lists the failed songs with it.
func TestRetryFailedWhenPaused(t *testing.T) {
	f, r, root := setup(t)
	a, x := want(root, "a", "original"), want(root, "x", "original")
	f.status["x"] = 500
	r.CardMounted = func() bool { return false }
	r.Start(plan.Plan{Add: []plan.Want{a, x}})
	if s := wait(t, r); s.State != "paused" {
		t.Fatalf("%+v", s)
	}
	r.CardMounted = func() bool { return true }
	r.RetryFailed()
	if s := wait(t, r); s.State != "done" || s.Done != 1 || s.Failed != 1 {
		t.Fatalf("%+v", s)
	}
}

// A run lists at most maxFailures song failures but counts them all.
func TestFailuresCapped(t *testing.T) {
	_, r, _ := setup(t)
	for i := 0; i < maxFailures+2; i++ {
		r.fail("s", "T", "download", errors.New("x"))
	}
	if s := r.Snapshot(); s.Failed != maxFailures+2 || len(s.Failures) != maxFailures {
		t.Fatalf("failed %d, listed %d", s.Failed, len(s.Failures))
	}
}

func TestDeletesAfterDownloads(t *testing.T) {
	_, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	z := &library.Entry{ID: "z", Path: root + "/Jellyfin/Z/z/01 - z.flac", Quality: "original"}
	os.MkdirAll(filepath.Dir(z.Path), 0o755)
	os.WriteFile(z.Path, []byte("z"), 0o644)
	os.WriteFile(filepath.Dir(z.Path)+"/cover.jpg", []byte("c"), 0o644)
	stale := library.PlaylistFile(root, "Gone")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("#EXTM3U\n"), 0o644)
	m := &library.Manifest{Songs: map[string]*library.Entry{"z": z}, Playlists: map[string]string{"Gone": stale}}
	m.Save(root, root+"/.jellyfin/manifest.json")
	var mu sync.Mutex
	var ops []string
	r.onOp = func(op, path string) {
		mu.Lock()
		if op == "download" || op == "remove" {
			ops = append(ops, op)
		}
		mu.Unlock()
	}
	r.Start(plan.Plan{Add: []plan.Want{a, b}, Remove: []*library.Entry{z}, Playlists: map[string][]string{}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if strings.Join(ops, ",") != "download,download,remove" {
		t.Fatal(ops)
	}
	if _, err := os.Stat(root + "/Jellyfin/Z"); !os.IsNotExist(err) {
		t.Fatal("removed song's folder left")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale playlist left")
	}
	if manifest(t, root).Songs["z"] != nil {
		t.Fatal("removed song still in manifest")
	}
}

func TestProgressPermilleAndETA(t *testing.T) {
	if p := permille(500, 1000); p != 500 {
		t.Fatal(p)
	}
	if p := permille(2000, 1000); p != 1000 {
		t.Fatal(p)
	}
	t0 := time.Unix(1000, 0)
	var m meter
	m.add(t0, 0)
	for i := 1; i <= 9; i++ { // 100 kB/s for 9 s
		m.add(t0.Add(time.Duration(i)*time.Second), 100_000)
	}
	if e := m.eta(t0.Add(9*time.Second), 1_000_000); e != 0 {
		t.Fatal("ETA before 10 s of data:", e)
	}
	for i := 10; i <= 60; i++ {
		m.add(t0.Add(time.Duration(i)*time.Second), 100_000)
	}
	if e := m.eta(t0.Add(60*time.Second), 1_000_000); e < 9*time.Second || e > 11*time.Second {
		t.Fatal("ETA", e)
	}

	_, r, _ := setup(t)
	clock := t0
	r.Now = func() time.Time { return clock }
	r.mu.Lock()
	r.state, r.bytesTotal, r.bytesDone = "running", 1000, 500
	r.mu.Unlock()
	if s := r.Snapshot(); s.Permille != 500 || s.ETA != 0 || s.BytesDone != 500 {
		t.Fatalf("%+v", s)
	}
}

func TestCancelKeepsFinished(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	f.block["bb"] = make(chan struct{})
	p := plan.Plan{Add: []plan.Want{a, b}}
	r.Start(p)
	for i := 0; ; i++ {
		if h, _ := f.seen("bb"); h > 0 {
			break
		}
		if i > 1000 {
			t.Fatal("bb never requested")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !r.Busy() || r.Start(p) != ErrBusy {
		t.Fatal("not busy")
	}
	r.Cancel()
	if s := r.Snapshot(); s.State != "idle" || r.Busy() {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
	m := manifest(t, root)
	if m.Songs["a"] == nil || m.Songs["bb"] != nil {
		t.Fatalf("%+v", m.Songs)
	}
	noParts(t, root)
	if _, err := os.Stat(root + "/.jellyfin/pending.json"); !os.IsNotExist(err) {
		t.Fatal("pending.json kept after cancel")
	}
}

func TestNetSlotPriority(t *testing.T) {
	s := NewNetSlot()
	ctx := context.Background()
	s.Acquire(ctx, false)
	got := make(chan string, 2)
	go func() { s.Acquire(ctx, false); got <- "normal"; s.Release() }()
	time.Sleep(20 * time.Millisecond)
	go func() { s.Acquire(ctx, true); got <- "prio"; s.Release() }()
	time.Sleep(20 * time.Millisecond)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if s.Acquire(cctx, false) == nil {
		t.Fatal("acquired with a cancelled context while held")
	}
	s.Release()
	if a, b := <-got, <-got; a != "prio" || b != "normal" {
		t.Fatal(a, b)
	}
	if err := s.Acquire(ctx, false); err != nil {
		t.Fatal(err)
	}
}

func TestRunFullSizePartRecovers(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	b.Bytes = 0 // size unknown: the full .part asks for a Range past the end and gets 416
	for _, w := range []plan.Want{a, b} {
		os.MkdirAll(filepath.Dir(w.Path), 0o755)
		os.WriteFile(w.Path+".part", bytes.Repeat([]byte("x"), len(data(w.Song.ID))), 0o644)
	}
	r.Start(plan.Plan{Add: []plan.Want{a, b}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
	sameFile(t, b.Path, "bb")
	if _, ra := f.seen("a"); len(ra) != 1 || ra[0] != "" {
		t.Fatalf("full .part at planned size sent Range: %q", ra)
	}
	if _, rb := f.seen("bb"); len(rb) != 2 || rb[0] != fmt.Sprintf("bytes=%d-", len(data("bb"))) || rb[1] != "" {
		t.Fatalf("416 not recovered in one try: %q", rb)
	}
	noParts(t, root)
}

func TestStreamerPreemptsMidFile(t *testing.T) {
	f, r, root := setup(t)
	big := want(root, "big", "original")
	f.slow["big"] = true
	r.Start(plan.Plan{Add: []plan.Want{big}})
	for i := 0; r.Snapshot().BytesDone == 0; i++ {
		if i > 1000 {
			t.Fatal("download never started")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Net.Acquire(ctx, true); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.State != "running" || s.BytesDone >= s.BytesTotal {
		t.Fatalf("streamer served only after the file: %+v", s)
	}
	time.Sleep(50 * time.Millisecond)
	if b := r.Snapshot().BytesDone; b != s.BytesDone {
		t.Fatal("download kept going while the streamer held the slot", b, s.BytesDone)
	}
	r.Net.Release()
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	sameFile(t, big.Path, "big")
	if _, rg := f.seen("big"); len(rg) != 1 {
		t.Fatalf("download restarted: %q", rg)
	}
}

func TestRestartResumes(t *testing.T) {
	f, ra, root := setup(t)
	a, big := want(root, "a", "original"), want(root, "big", "original")
	f.slow["big"] = true
	p := plan.Plan{Add: []plan.Want{a, big}}
	ra.Start(p)
	for i := 0; ; i++ {
		if s := ra.Snapshot(); s.Done == 1 && s.BytesDone > a.Bytes {
			break
		}
		if i > 2000 {
			t.Fatal("big never started")
		}
		time.Sleep(time.Millisecond)
	}
	ra.Pause() // the process dies mid-file: the .part and pending.json stay
	st, err := os.Stat(big.Path + ".part")
	if err != nil || st.Size() == 0 {
		t.Fatal("no .part", err)
	}

	rb := New(root, ra.src, nil)
	rb.Backoff, rb.CardMounted, rb.Net = ra.Backoff, ra.CardMounted, NewNetSlot()
	if err := rb.Start(p); err != nil {
		t.Fatal(err)
	}
	if s := wait(t, rb); s.State != "done" || s.Done != 2 {
		t.Fatalf("%+v", s)
	}
	if h, _ := f.seen("a"); h != 1 {
		t.Fatal("finished song fetched again", h)
	}
	if _, rg := f.seen("big"); len(rg) != 2 || rg[1] != fmt.Sprintf("bytes=%d-", st.Size()) {
		t.Fatalf("ranges %q, part %d", rg, st.Size())
	}
	sameFile(t, big.Path, "big")
}

// A paused song's .part goes when the next plan no longer fetches it (taken out of Sync), as does
// one an earlier run left (a restart, a cut song) (audit 2026-11-10 #7).
func TestStartDropsUnwantedPart(t *testing.T) {
	f, r, root := setup(t)
	a, big := want(root, "a", "original"), want(root, "big", "original")
	f.slow["big"] = true
	r.Start(plan.Plan{Add: []plan.Want{a, big}})
	for i := 0; ; i++ {
		if s := r.Snapshot(); s.Done == 1 && s.BytesDone > a.Bytes {
			break
		}
		if i > 2000 {
			t.Fatal("big never started")
		}
		time.Sleep(time.Millisecond)
	}
	r.Pause()
	if _, err := os.Stat(big.Path + ".part"); err != nil {
		t.Fatal("no .part", err)
	}
	stray := root + "/Jellyfin/Old/Album/Song.flac.part" // left by an earlier run, before a restart
	put(t, stray, "x")
	put(t, stray+".key", "original 9")
	if err := r.Start(plan.Plan{}); err != nil { // big taken out of Sync
		t.Fatal(err)
	}
	wait(t, r)
	for _, p := range []string{stray, stray + ".key"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept: %v", p, err)
		}
	}
	if _, err := os.Stat(big.Path + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part kept: %v", err)
	}
	if _, err := os.Stat(big.Path + ".part.key"); !os.IsNotExist(err) {
		t.Fatalf(".part.key kept: %v", err)
	}
}

func TestLogRedacted(t *testing.T) {
	_, r, root := setup(t)
	r.log = applog.Open(root+"/log.txt", 1<<20)
	r.logf("sync: %v", errors.New("GET https://h/rest/download?u=u&t=tok123&s=salt99&p=pw77 -> EOF"))
	b, _ := os.ReadFile(root + "/log.txt")
	if len(b) == 0 || strings.Contains(string(b), "tok123") || strings.Contains(string(b), "salt99") ||
		strings.Contains(string(b), "pw77") {
		t.Fatalf("%q", b)
	}
}

// syncPlaylists runs a sync of the given playlists (name -> no songs) over a card prepared by prep.
func syncPlaylists(t *testing.T, prep func(root string), names ...string) (string, *library.Manifest) {
	t.Helper()
	_, r, root := setup(t)
	if prep != nil {
		prep(root)
	}
	pl := map[string][]string{}
	for _, n := range names {
		pl[n] = nil
	}
	r.Start(plan.Plan{Playlists: pl})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	return root, manifest(t, root)
}

func put(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUserPlaylistNotOverwritten(t *testing.T) {
	var user string
	root, m := syncPlaylists(t, func(root string) {
		user = library.PlaylistFile(root, "Andreas")
		put(t, user, "mine")
	}, "Andreas")
	if b, _ := os.ReadFile(user); string(b) != "mine" {
		t.Fatalf("user file changed: %q", b)
	}
	want := root + "/_explaylist_data/Andreas (Jellyfin).m3u"
	if _, err := os.Stat(want); err != nil || m.Playlists["Andreas"] != want {
		t.Fatal(err, m.Playlists)
	}
}

func TestRecordedPlaylistRewrittenAndStaleOnlyRecorded(t *testing.T) {
	var other, gone string
	root, m := syncPlaylists(t, func(root string) {
		gone = library.PlaylistFile(root, "Gone")
		other = library.PlaylistFile(root, "Other")
		put(t, gone, "x")
		put(t, other, "user")
		put(t, library.PlaylistFile(root, "Keep"), "old")
		mm := &library.Manifest{Playlists: map[string]string{"Gone": gone, "Keep": library.PlaylistFile(root, "Keep")}}
		mm.Save(root, root+"/.jellyfin/manifest.json")
	}, "Keep")
	if b, _ := os.ReadFile(other); string(b) != "user" {
		t.Fatal("unrecorded Other.m3u touched")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("recorded stale file left")
	}
	if b, _ := os.ReadFile(library.PlaylistFile(root, "Keep")); string(b) != "#EXTM3U\n" {
		t.Fatalf("Keep not rewritten in place: %q", b)
	}
	if len(m.Playlists) != 1 || m.Playlists["Keep"] != library.PlaylistFile(root, "Keep") {
		t.Fatal(m.Playlists)
	}
}

// q2-subsonic's older playlist files ("Navidrome - <name>.m3u") belong to that app, never to this
// one: a sync, stale flag or not, leaves them alone (q2-subsonic removed them during its own
// migration; this app has none).
func TestOtherAppsPlaylistsUntouched(t *testing.T) {
	var theirs string
	root, m := syncPlaylists(t, func(root string) {
		theirs = filepath.Join(root, "_explaylist_data", "Navidrome - Mix.m3u")
		put(t, theirs, "#EXTM3U\n")
		(&library.Manifest{PlaylistsStale: true}).Save(root, root+"/.jellyfin/manifest.json")
	}, "Mix")
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("another app's playlist was removed:", err)
	}
	if library.Owned(root, theirs, m) {
		t.Fatal("another app's playlist counted as this app's")
	}
	if _, err := os.Stat(library.PlaylistFile(root, "Mix")); err != nil || m.Playlists["Mix"] != library.PlaylistFile(root, "Mix") {
		t.Fatal(err, m.Playlists)
	}
}

func TestPlaylistWriteFailureKeepsRecords(t *testing.T) {
	_, r, root := setup(t)
	bad := library.PlaylistFile(root, "B")
	os.MkdirAll(bad, 0o755) // a directory where B.m3u must go: the write fails
	(&library.Manifest{Playlists: map[string]string{"B": bad}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Playlists: map[string][]string{"A": nil, "B": nil, "C": nil}})
	if s := wait(t, r); s.State != "error" {
		t.Fatalf("%+v", s)
	}
	if got := manifest(t, root).Playlists["A"]; got != library.PlaylistFile(root, "A") {
		t.Fatalf("A not recorded: %v", manifest(t, root).Playlists)
	}
	os.Remove(bad)
	r.Start(plan.Plan{Playlists: map[string][]string{"A": nil, "B": nil, "C": nil}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if m, _ := filepath.Glob(root + "/_explaylist_data/*(Jellyfin*"); len(m) != 0 {
		t.Fatal("duplicates:", m)
	}
}

func TestBadRecordedPlaylistIgnored(t *testing.T) {
	root, m := syncPlaylists(t, func(root string) {
		(&library.Manifest{Playlists: map[string]string{"X": "/etc/passwd", "Y": root + "/Jellyfin/y.m3u"}}).Save(root, root+"/.jellyfin/manifest.json")
	}, "X")
	if m.Playlists["X"] != library.PlaylistFile(root, "X") || len(m.Playlists) != 1 {
		t.Fatal(m.Playlists)
	}
}

// Song writes go under Jellyfin/ only: .jellyfin/ holds the app binary and its run script.
func TestSongWritesOnlyUnderSubsonic(t *testing.T) {
	_, r, root := setup(t)
	bin := root + "/.jellyfin/q2-jellyfin"
	put(t, bin, "app")
	w := want(root, "a", "original")
	w.Path = bin
	src := root + "/Jellyfin/A/B/1.mp3"
	put(t, src, "x")
	r.Start(plan.Plan{Add: []plan.Want{w}, Rename: []plan.Move{{From: src, To: root + "/.jellyfin/run", ID: "r"}}})
	wait(t, r)
	if b, _ := os.ReadFile(bin); string(b) != "app" {
		t.Fatalf("binary overwritten: %d bytes", len(b))
	}
	for _, p := range []string{bin + ".part", root + "/.jellyfin/run"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("written:", p)
		}
	}
}

// A "Navidrome - X.m3u" is never this app's: it is left alone.
func TestUserNavidromePrefixKept(t *testing.T) {
	var mine, sel string
	syncPlaylists(t, func(root string) {
		mine = filepath.Join(root, "_explaylist_data", "Navidrome - X.m3u")
		sel = filepath.Join(root, "_explaylist_data", "Navidrome - Mix.m3u")
		put(t, mine, "#EXTM3U\n")
		put(t, sel, "#EXTM3U\n")
	}, "Mix")
	for _, f := range []string{mine, sel} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal("user playlist deleted:", f)
		}
	}
}

// A playlist file recorded under an older SafeName with ".." in it, which the firmware refuses (and
// with it the whole library update), is replaced by one with a clean name (I18).
func TestDottedPlaylistFileReplaced(t *testing.T) {
	var old string
	root, m := syncPlaylists(t, func(root string) {
		old = root + "/_explaylist_data/Wait... What.m3u"
		put(t, old, "#EXTM3U\n")
		(&library.Manifest{Playlists: map[string]string{"Wait... What": old}}).Save(root, root+"/.jellyfin/manifest.json")
	}, "Wait... What")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file left")
	}
	want := root + "/_explaylist_data/Wait… What.m3u"
	if _, err := os.Stat(want); err != nil || m.Playlists["Wait... What"] != want {
		t.Fatal(err, m.Playlists)
	}
}

// stem is a playlist file's name without .m3u, which the Q2 keeps 63 bytes of (issue #1).
func stem(t *testing.T, f string) string {
	t.Helper()
	s := strings.TrimSuffix(filepath.Base(f), ".m3u")
	if len(s) > 63 || !utf8.ValidString(s) {
		t.Errorf("%q: %d bytes", s, len(s))
	}
	return s
}

// The " (Jellyfin)" a clash adds fits in the 63 bytes too.
func TestLongPlaylistSuffixFits(t *testing.T) {
	long := strings.Repeat("曲", 30)
	var user string
	root, m := syncPlaylists(t, func(root string) {
		user = library.PlaylistFile(root, long)
		put(t, user, "mine")
	}, long)
	f := m.Playlists[long]
	if s := stem(t, f); !strings.HasSuffix(s, " (Jellyfin)") || f == user || filepath.Dir(f) != root+"/_explaylist_data" {
		t.Fatal(f)
	}
	if b, _ := os.ReadFile(user); string(b) != "mine" {
		t.Fatalf("user file changed: %q", b)
	}
}

// Two long names that cut to the same 63 bytes get two files.
func TestLongPlaylistNamesCutAlike(t *testing.T) {
	a, b := strings.Repeat("Песня", 10)+" A", strings.Repeat("Песня", 10)+" B"
	_, m := syncPlaylists(t, nil, a, b)
	fa, fb := m.Playlists[a], m.Playlists[b]
	stem(t, fa)
	stem(t, fb)
	if strings.EqualFold(fa, fb) {
		t.Fatal(fa, fb)
	}
	for _, f := range []string{fa, fb} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
}

// A playlist an older build recorded under a name longer than the Q2 keeps is moved to one that
// fits, and goes out as "add" again: the old build's Imported never proved the Q2 playlist of the
// cut name is the app's, and "import" would replace it (R35). A user's file is never touched.
func TestLongPlaylistFileMigrated(t *testing.T) {
	cjk, cyr := strings.Repeat("曲", 30), strings.Repeat("Песня", 10)
	var oldCJK, oldCyr, user string
	root, m := syncPlaylists(t, func(root string) {
		dir := root + "/_explaylist_data/"
		oldCJK, oldCyr, user = dir+cjk+".m3u", dir+cyr+".m3u", dir+strings.Repeat("曲", 25)+".m3u"
		for _, f := range []string{oldCJK, oldCyr, user} {
			put(t, f, "#EXTM3U\n")
		}
		(&library.Manifest{Playlists: map[string]string{cjk: oldCJK, cyr: oldCyr},
			Imported: map[string]bool{oldCJK: true, oldCyr: true}}).Save(root, root+"/.jellyfin/manifest.json")
	}, cjk, cyr)
	for _, f := range []string{oldCJK, oldCyr} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Error("old file left:", f)
		}
	}
	if _, err := os.Stat(user); err != nil {
		t.Error("user file removed")
	}
	newCJK, newCyr := library.PlaylistFile(root, cjk), library.PlaylistFile(root, cyr)
	if m.Playlists[cjk] != newCJK || m.Playlists[cyr] != newCyr {
		t.Fatal(m.Playlists)
	}
	stem(t, newCJK)
	stem(t, newCyr)
	if m.Imported[newCJK] || m.Imported[newCyr] {
		t.Fatalf("imported %v", m.Imported)
	}
}

// A user's file already at the name that fits: the moved playlist takes "<name> (Jellyfin).m3u".
func TestLongPlaylistMigratedBesideUserFile(t *testing.T) {
	long := strings.Repeat("曲", 30)
	var old, user string
	root, m := syncPlaylists(t, func(root string) {
		old, user = root+"/_explaylist_data/"+long+".m3u", library.PlaylistFile(root, long)
		put(t, old, "#EXTM3U\n")
		put(t, user, "mine")
		(&library.Manifest{Playlists: map[string]string{long: old}, Imported: map[string]bool{old: true}}).Save(root, root+"/.jellyfin/manifest.json")
	}, long)
	if f := m.Playlists[long]; f != library.PlaylistFileN(root, long, 1) || m.Imported[f] {
		t.Fatal(f, m.Imported)
	}
	stem(t, m.Playlists[long])
	if b, _ := os.ReadFile(user); string(b) != "mine" {
		t.Fatalf("user file changed: %q", b)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file left")
	}
}

// A stale playlist file the sync can't remove is logged; it stays recorded for the next sync.
func TestStalePlaylistRemoveFailureLogged(t *testing.T) {
	_, r, root := setup(t)
	r.log = applog.Open(root+"/log.txt", 1<<20)
	long := root + "/_explaylist_data/" + strings.Repeat("曲", 30) + ".m3u"
	put(t, long+"/x", "") // a folder with something in it: os.Remove fails
	(&library.Manifest{Playlists: map[string]string{"L": long}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Playlists: map[string][]string{}})
	wait(t, r)
	if b, _ := os.ReadFile(root + "/log.txt"); !strings.Contains(string(b), long) {
		t.Fatalf("%q", b)
	}
	if manifest(t, root).Playlists["L"] != long {
		t.Fatal("not kept")
	}
}

// casefold runs the calling test again in a user namespace with a case-insensitive tmpfs folder as
// the card (FAT/exFAT are case-insensitive; CI's filesystems are not), and returns that folder in
// the run inside. The outer run returns "" once the inner one passed. It skips where user
// namespaces or casefold tmpfs (Linux 6.13+) aren't available.
func casefold(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("Q2_CASEFOLD"); d != "" {
		return d
	}
	dir := t.TempDir()
	setup := `mount -t tmpfs -o casefold none "$1" && mkdir "$1/card" && chattr +F "$1/card"`
	if out, err := exec.Command("unshare", "-rm", "sh", "-c", setup, "sh", dir).CombinedOutput(); err != nil {
		t.Skipf("no casefold tmpfs here: %v %s", err, out)
	}
	run := setup + ` && Q2_CASEFOLD="$1/card" exec "$2" -test.run "^$3\$" -test.v`
	out, err := exec.Command("unshare", "-rm", "sh", "-c", run, "sh", dir, os.Args[0], t.Name()).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("--- PASS: "+t.Name())) {
		t.Fatalf("on a case-insensitive card: %v\n%s", err, out)
	}
	return ""
}

// On a case-insensitive card, a case-only path change (a) renames, (b) replaces without deleting
// the new file, and (c) a new song on a removed song's case variant isn't deleted with it (I5).
func TestCaseInsensitiveCard(t *testing.T) {
	root := casefold(t)
	if root == "" {
		return
	}
	_, r0, _ := setup(t)
	r := New(root, r0.src, nil)
	r.Backoff, r.CardMounted, r.Net = r0.Backoff, r0.CardMounted, NewNetSlot()
	mk := func(id, artist, q string) jellyfin.Song {
		return jellyfin.Song{ID: id, Title: "T" + id, Album: "Alb", AlbumArtist: artist, Year: 2000, Track: 1, Suffix: "flac", Size: int64(len(data(id)))}
	}
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	old := map[string]jellyfin.Song{"a": mk("a", "Abba", ""), "b": mk("b", "Bee", ""), "c": mk("c", "Cee", "")}
	old["c"] = jellyfin.Song{ID: "c", Title: "Td", Album: "Alb", AlbumArtist: "Cee", Year: 2000, Track: 1, Suffix: "flac"}
	quality := map[string]string{"a": "original", "b": "mp3-128", "c": "original"}
	for id, s := range old {
		p := library.SongPath(root, s, quality[id], false)
		put(t, p, "old-"+id)
		m.Songs[id] = &library.Entry{ID: id, Path: p, Quality: quality[id], Size: 5, Items: []string{"song:" + id}, Meta: library.MetaKey(s, quality[id])}
	}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	a, b, d := mk("a", "ABBA", ""), mk("b", "BEE", ""), mk("d", "CEE", "")
	sel := selection.Set{Items: []selection.Item{{Kind: "song", ID: "a"}, {Kind: "song", ID: "b"}, {Kind: "song", ID: "d"}}}
	p := plan.Compute(sel, "original", false, map[string][]jellyfin.Song{"song:a": {a}, "song:b": {b}, "song:d": {d}}, nil, m, root, 1<<40)
	if err := r.Start(p); err != nil {
		t.Fatal(err)
	}
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	mf := manifest(t, root)
	for id, want := range map[string]string{"a": "old-a", "b": string(data("b")), "d": string(data("d"))} {
		e := mf.Songs[id]
		if e == nil {
			t.Fatalf("%s not in the manifest", id)
		}
		if got, err := os.ReadFile(e.Path); err != nil || string(got) != want {
			t.Errorf("%s at %s: %v, %d bytes", id, e.Path, err, len(got))
		}
	}
	if mf.Songs["c"] != nil {
		t.Error("c kept")
	}
}

// A rename cut after the file moved but before the manifest was saved is finished, not failed
// on every later sync (M13).
func TestRenameRecoversAfterCrash(t *testing.T) {
	_, r, root := setup(t)
	from, to := root+"/Jellyfin/A/x/01 - r.flac", root+"/Jellyfin/A/y/01 - r.flac"
	put(t, to, "r") // moved already; the manifest still says from
	(&library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: from, Quality: "original", Size: 1}}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Rename: []plan.Move{{From: from, To: to, ID: "r"}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if e := manifest(t, root).Songs["r"]; e == nil || e.Path != to {
		t.Fatalf("%+v", e)
	}
}

// A crash after a batch's targets were saved and some of its files moved: the next run records
// each song where its file is, the moved ones at their new paths, and plans none of them again.
func TestRenameCutAfterTargetsSaved(t *testing.T) {
	f, r, root := setup(t)
	from := func(id string) string { return root + "/Jellyfin/A/x/" + id + ".flac" }
	to := func(id string) string { return root + "/Jellyfin/A/y/" + id + ".flac" }
	put(t, to("m"), "m") // moved
	put(t, from("n"), "n")
	(&library.Manifest{Songs: map[string]*library.Entry{
		"m": {ID: "m", Path: from("m"), Moving: to("m"), Quality: "original", Size: 1},
		"n": {ID: "n", Path: from("n"), Moving: to("n"), Quality: "original", Size: 1},
	}}).Save(root, root+"/.jellyfin/manifest.json")
	if err := r.DropMissing(manifest(t, root)); err != nil {
		t.Fatal(err)
	}
	if m := manifest(t, root); m.Songs["m"] == nil || m.Songs["n"] == nil {
		t.Fatalf("dropped, to be downloaded again: %+v", m.Songs)
	}
	r.Start(plan.Plan{Rename: []plan.Move{{From: from("m"), To: to("m"), ID: "m"}, {From: from("n"), To: to("n"), ID: "n"}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	for id, e := range manifest(t, root).Songs {
		if b, err := os.ReadFile(to(id)); e.Path != to(id) || e.Moving != "" || err != nil || string(b) != id {
			t.Errorf("%s: %+v %q %v", id, e, b, err)
		}
	}
	for _, id := range []string{"m", "n"} {
		if n, _ := f.seen(id); n != 0 {
			t.Errorf("%s downloaded", id)
		}
	}
}

// DropMissing checks the manifest it is given, the one the preview reads, not a second copy read
// again from the card (audit 18): with every song still there, the file on the card isn't parsed.
func TestDropMissingTakesManifest(t *testing.T) {
	_, r, root := setup(t)
	p := root + "/Jellyfin/A/x/k.flac"
	put(t, p, "k")
	m := &library.Manifest{Songs: map[string]*library.Entry{"k": {ID: "k", Path: p, Quality: "original", Size: 1}}}
	os.MkdirAll(root+"/.jellyfin", 0o755)
	os.WriteFile(root+"/.jellyfin/manifest.json", []byte("{"), 0o644) // a parse would fail
	if err := r.DropMissing(m); err != nil {
		t.Fatal(err)
	}
}

// A run that ends on an error deletes the old files its last save listed, and no save follows: the
// next preview's DropMissing forgets them, so it offers no delete of files already gone.
func TestDropMissingForgetsGoneStale(t *testing.T) {
	f, r, root := setup(t)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	var ws []plan.Want
	for _, id := range []string{"a", "bb", "ccc"} {
		old, w := want(root, id, "original"), want(root, id, "opus-128")
		put(t, old.Path, "old rip")
		m.Songs[id] = &library.Entry{ID: id, Path: old.Path, Quality: "original", Size: 7, Items: old.Items,
			Meta: library.MetaKey(old.Song, "original")}
		ws = append(ws, w)
	}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	f.status["bb"], f.status["ccc"] = 502, 502
	r.Start(plan.Plan{Replace: ws})
	if s := wait(t, r); s.State != "paused" || !errors.Is(s.Err, ErrOffline) {
		t.Fatalf("%+v", s)
	}
	if err := r.DropMissing(manifest(t, root)); err != nil {
		t.Fatal(err)
	}
	mf := manifest(t, root)
	if len(mf.Stale) != 0 {
		t.Errorf("deleted files still listed: %q", mf.Stale)
	}
	if p := plan.Compute(selection.Set{}, "opus-128", false, nil, nil, mf, root, 1<<40); p.Leftover != 0 {
		t.Errorf("Leftover %d", p.Leftover)
	}
}

// A song deselected after a cut rename moved its file leaves the card with its file, wherever the
// file is: no orphan stays behind.
func TestRemoveAfterCutRename(t *testing.T) {
	_, r, root := setup(t)
	from, to := root+"/Jellyfin/A/x/01 - r.flac", root+"/Jellyfin/A/y/01 - r.flac"
	put(t, to, "r")
	(&library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: from, Moving: to, Quality: "original", Size: 1}}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Remove: []*library.Entry{{ID: "r", Path: from}}})
	if s := wait(t, r); s.State != "done" || s.Removed != 1 {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the moved file stayed: %v", err)
	}
	if m := manifest(t, root); len(m.Songs) != 0 {
		t.Errorf("%+v", m.Songs)
	}
}

// A playlist names where its song is: after a rename that failed, the song's old path, never the
// planned one (which may hold nothing, or another song) (I6).
func TestPlaylistNamesWhereTheSongIs(t *testing.T) {
	_, r, root := setup(t)
	from, to := root+"/Jellyfin/A/x/01 - r.flac", root+"/Jellyfin/A/y/01 - r.flac"
	put(t, from, "r")
	put(t, root+"/Jellyfin/A/y", "a file where the folder must go: the rename fails")
	(&library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: from, Quality: "original", Size: 1}}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Rename: []plan.Move{{From: from, To: to, ID: "r"}}, Playlists: map[string][]string{"Mix": {to}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 1 {
		t.Fatalf("%+v", s)
	}
	if b, _ := os.ReadFile(library.PlaylistFile(root, "Mix")); string(b) != "#EXTM3U\n"+from+"\n" {
		t.Fatalf("%q", b)
	}
}

// Nothing is removed when the songs asked for didn't come (I2): songs taken out of Sync for ones
// that failed stay, and so does a playlist no longer selected.
func TestRemovesWaitForSongsThatDidntCome(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	f.status["a"], f.status["bb"] = 404, 404
	z := &library.Entry{ID: "z", Path: root + "/Jellyfin/Z/z/01 - z.flac", Quality: "original"}
	put(t, z.Path, "z")
	stale := library.PlaylistFile(root, "Gone")
	put(t, stale, "#EXTM3U\n")
	(&library.Manifest{Songs: map[string]*library.Entry{"z": z}, Playlists: map[string]string{"Gone": stale}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Add: []plan.Want{a, b}, Remove: []*library.Entry{z}, Playlists: map[string][]string{}})
	if s := wait(t, r); s.State != "done" || s.Failed != 2 || s.Held != 1 {
		t.Fatalf("%+v", s)
	}
	for _, p := range []string{z.Path, stale} {
		if _, err := os.Stat(p); err != nil {
			t.Error("removed after a failed download:", p)
		}
	}
	if m := manifest(t, root); m.Songs["z"] == nil || m.Playlists["Gone"] != stale {
		t.Errorf("%+v %+v", m.Songs, m.Playlists)
	}
}

// A single song that fails on its own doesn't hold back the removals: under Entire Library it can't
// be taken out of Sync, and nothing would ever be removed again. A failure the whole sync shares (a
// proxy's 502), or several songs failing and more than arrive, still does (I2). After a shared
// failure the snapshot says so (HeldRetry), as taking songs out of Sync doesn't help.
func TestRemovalsHeldOnlyBySharedFailures(t *testing.T) {
	all := []string{"a", "x", "bb", "ccc"}
	for _, c := range []struct {
		name   string
		ids    []string
		status map[string]int
		whole  bool // Entire Library: nothing is swapped, so own failures hold nothing
		held   bool
	}{
		{"one of four fails", all, map[string]int{"x": 404}, false, false},
		{"one bad song, nothing else to fetch", []string{"x"}, map[string]int{"x": 404}, false, false},
		{"most fail", all, map[string]int{"x": 404, "bb": 404, "ccc": 500}, false, true},
		{"server down once", all, map[string]int{"x": 502}, false, true},
		{"two bad songs, a swap", []string{"x", "bb"}, map[string]int{"x": 404, "bb": 404}, false, true},
		{"two bad songs, Entire Library", []string{"x", "bb"}, map[string]int{"x": 404, "bb": 404}, true, false},
		{"server down once, Entire Library", all, map[string]int{"x": 502}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, r, root := setup(t)
			f.status = c.status
			z := &library.Entry{ID: "z", Path: root + "/Jellyfin/Z/z/01 - z.flac", Quality: "original"}
			put(t, z.Path, "z")
			stale := library.PlaylistFile(root, "Gone")
			put(t, stale, "#EXTM3U\n")
			(&library.Manifest{Songs: map[string]*library.Entry{"z": z}, Playlists: map[string]string{"Gone": stale}}).Save(root, root+"/.jellyfin/manifest.json")
			var ws []plan.Want
			for _, id := range c.ids {
				ws = append(ws, want(root, id, "original"))
			}
			r.Start(plan.Plan{Add: ws, Remove: []*library.Entry{z}, Playlists: map[string][]string{}, Whole: c.whole})
			s := wait(t, r)
			if s.State != "done" || s.Failed != len(c.status) || (s.Held == 1) != c.held || (s.Removed == 1) == c.held {
				t.Fatalf("%s: %d failed, %d held, %d removed", s.State, s.Failed, s.Held, s.Removed)
			}
			if retry := c.status["x"] == 502; s.HeldRetry != retry { // the done page's advice: Retry only
				t.Errorf("HeldRetry %v, want %v", s.HeldRetry, retry)
			}
			for _, p := range []string{z.Path, stale} {
				if _, err := os.Stat(p); (err == nil) != c.held {
					t.Errorf("%s: %v", p, err)
				}
			}
		})
	}
}

// A plan run again (Retry Failed, Resume) after an earlier run removed its songs holds none back:
// the done page doesn't say songs were not removed that are gone already.
func TestHeldCountsOnlySongsStillOnTheCard(t *testing.T) {
	f, r, root := setup(t)
	f.status["x"], f.status["bb"] = 404, 404
	z := &library.Entry{ID: "z", Path: root + "/Jellyfin/Z/z/01 - z.flac", Quality: "original"}
	r.Start(plan.Plan{Add: []plan.Want{want(root, "x", "original"), want(root, "bb", "original")}, Remove: []*library.Entry{z}})
	if s := wait(t, r); s.State != "done" || s.Failed != 2 || s.Held != 0 {
		t.Fatalf("%s: %d failed, %d held", s.State, s.Failed, s.Held)
	}
}

// Losing the server (no Wi-Fi, server down) pauses the sync after two songs in a row, with the
// reason, instead of failing every remaining song for hours (I3).
func TestOfflinePauses(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	c, err := jellyfin.New(jellyfin.Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	root := t.TempDir()
	r := New(root, c, nil)
	r.Backoff, r.CardMounted, r.Net = []time.Duration{time.Millisecond, time.Millisecond}, func() bool { return true }, NewNetSlot()
	var tries atomic.Int32
	r.onOp = func(op, _ string) {
		if op == "try" {
			tries.Add(1)
		}
	}
	z := &library.Entry{ID: "z", Path: root + "/Jellyfin/Z/z/01 - z.flac", Quality: "original"}
	put(t, z.Path, "z")
	r.Start(plan.Plan{Add: []plan.Want{want(root, "a", "original"), want(root, "bb", "original"), want(root, "ccc", "original"),
		want(root, "dddd", "original")}, Remove: []*library.Entry{z}})
	s := wait(t, r)
	if s.State != "paused" || !errors.Is(s.Err, ErrOffline) || !errors.Is(s.Err, jellyfin.ErrUnreachable) {
		t.Fatalf("%+v", s)
	}
	if n := tries.Load(); n != 4 {
		t.Errorf("%d tries, want 2 songs × 2", n)
	}
	if _, err := os.Stat(z.Path); err != nil {
		t.Error("removed while offline")
	}
}

// The server going away right after a song failed still gets the retries that ride out a blip
// before the sync pauses: the one try after the failure doesn't count toward the pause on its own.
func TestOfflineAfterFailure(t *testing.T) {
	f, r, root := setup(t)
	ws := []plan.Want{want(root, "a", "original"), want(root, "bb", "original"), want(root, "ccc", "original"), want(root, "dddd", "original")}
	f.status["a"] = 500
	for _, id := range []string{"bb", "ccc", "dddd"} {
		f.status[id] = 502 // a proxy in front of a server that is down
	}
	r.Start(plan.Plan{Add: ws})
	if s := wait(t, r); s.State != "paused" || !errors.Is(s.Err, ErrOffline) {
		t.Fatalf("%+v", s)
	}
	n := len(r.Backoff)
	for id, want := range map[string]int{"a": n, "bb": 1 + n, "ccc": n, "dddd": 0} {
		if h, _ := f.seen(id); h != want {
			t.Errorf("%s: %d requests, want %d", id, h, want)
		}
	}
}

func TestStopReason(t *testing.T) {
	full := &os.PathError{Op: "write", Path: "/mnt/mmc/x.part", Err: syscall.ENOSPC}
	if err := StopReason(full); !errors.Is(err, ErrCardFull) {
		t.Error("ENOSPC:", err)
	}
	if err := StopReason(&net.OpError{Op: "read", Err: syscall.ECONNRESET}); !errors.Is(err, ErrOffline) {
		t.Error("reset:", err)
	}
	if err := StopReason(&jellyfin.APIError{Op: "get", HTTP: 404}); err != nil {
		t.Error("404 is the song's own:", err)
	}
	for _, code := range []int{40, 41, 44, 50} { // the account, not the song (R39)
		if err := StopReason(&jellyfin.APIError{Op: "get", HTTP: 200, Code: code}); !errors.Is(err, ErrRefused) || errors.Is(err, ErrOffline) {
			t.Error(code, err)
		}
	}
	if err := StopReason(&jellyfin.APIError{Op: "get", HTTP: 200, Code: 70}); err != nil {
		t.Error("70 (not found) is the song's own:", err)
	}
	for _, card := range []error{ // the card's, not the server's: errnos are net.Errors too
		&os.PathError{Op: "open", Path: "/mnt/mmc/x.part", Err: syscall.EROFS},
		&os.LinkError{Op: "rename", Old: "/mnt/mmc/x.part", New: "/mnt/mmc/x.flac", Err: syscall.EIO},
	} {
		if err := StopReason(card); err == nil || errors.Is(err, ErrOffline) {
			t.Error(card, err)
		}
	}
}

// Removals are dropped from the manifest before their files go, so a stop (Pause, Cancel, the card
// going, power) never leaves a song recorded as on the card that isn't (I4): each file goes only once
// the manifest saved on the card no longer records its song, whatever the batch size.
func TestInterruptedRemovalsRecorded(t *testing.T) {
	_, r, root := setup(t)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	var rm []*library.Entry
	for _, id := range []string{"x", "y", "z"} {
		e := &library.Entry{ID: id, Path: root + "/Jellyfin/Z/z/" + id + ".flac", Quality: "original"}
		put(t, e.Path, id)
		m.Songs[id], rm = e, append(rm, e)
	}
	m.Save(root, root+"/.jellyfin/manifest.json")
	r.onOp = func(op, path string) {
		if op == "remove" {
			saved, _ := library.LoadManifest(root + "/.jellyfin/manifest.json")
			for _, e := range saved.Songs {
				if e.Path == path {
					t.Errorf("%s removed while the saved manifest records it (power lost now)", path)
				}
			}
			r.mu.Lock()
			r.stopAs = "paused"
			r.cancel() // Pause, from inside the run
			r.mu.Unlock()
		}
	}
	r.Start(plan.Plan{Remove: rm, Playlists: map[string][]string{}})
	wait(t, r)
	for id, e := range manifest(t, root).Songs {
		if _, err := os.Stat(e.Path); err != nil {
			t.Errorf("%s recorded, file gone: %v", id, err)
		}
	}
}

// removalSongs records three songs, with lyrics, in one album folder for removal.
func removalSongs(t *testing.T, root string) []*library.Entry {
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	var rm []*library.Entry
	for _, id := range []string{"x", "y", "z"} {
		e := &library.Entry{ID: id, Path: root + "/Jellyfin/Z/z/" + id + ".flac", Quality: "original"}
		put(t, e.Path, id)
		put(t, library.LyricsPath(e.Path), id)
		m.Songs[id], rm = e, append(rm, e)
	}
	m.Save(root, root+"/.jellyfin/manifest.json")
	return rm
}

// A Pause part-way through a batch of removals stops the deletes at once (USB storage gives the app
// 3 s, not a batch of 1,000). The files not deleted stay listed in the manifest already on the card,
// with no save after the stop, and the next run deletes them first.
func TestRemovalsStopMidBatch(t *testing.T) {
	_, r, root := setup(t)
	rm := removalSongs(t, root)
	removes := 0
	r.onOp = func(op, _ string) {
		if op == "remove" {
			removes++
			r.mu.Lock()
			r.stopAs = "paused"
			r.cancel() // Pause, from inside the run
			r.mu.Unlock()
		}
	}
	p := plan.Plan{Remove: rm, Playlists: map[string][]string{}}
	r.Start(p)
	wait(t, r)
	if removes != 1 {
		t.Fatalf("%d files deleted after the Pause, want 1", removes)
	}
	m := manifest(t, root)
	if !m.SyncedAt.IsZero() {
		t.Error("a stopped sync recorded as synced")
	}
	for _, e := range rm {
		_, err := os.Stat(e.Path)
		if m.Songs[e.ID] != nil || err == nil && !slices.Contains(m.Stale, e.Path) {
			t.Errorf("%s: on the card %v, not listed", e.ID, err == nil)
		}
	}
	r.onOp = nil
	r.Start(p)
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(root + "/Jellyfin/Z"); !os.IsNotExist(err) {
		t.Error("removed songs' files left")
	}
}

// The card going part-way through a batch of removals (or a stop whose save the launcher's KILL
// cuts) loses no file: the batch's save listed them before they went, so the next run deletes them.
func TestRemovalsCardGoneMidBatch(t *testing.T) {
	_, r, root := setup(t)
	rm := removalSongs(t, root)
	var gone atomic.Bool
	r.CardMounted = func() bool { return !gone.Load() }
	r.onOp = func(op, _ string) {
		if op == "remove" {
			gone.Store(true) // no save writes after this one
		}
	}
	p := plan.Plan{Remove: rm, Playlists: map[string][]string{}}
	r.Start(p)
	if s := wait(t, r); s.State == "done" {
		t.Fatalf("%+v", s)
	}
	gone.Store(false)
	r.onOp = nil
	r.Start(p)
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(root + "/Jellyfin/Z"); !os.IsNotExist(err) {
		t.Error("removed songs' files left on the card for good")
	}
	if m := manifest(t, root); len(m.Songs)+len(m.Stale) != 0 {
		t.Errorf("%+v", m)
	}
}

// The playlist phase stops at once when the card goes (USB storage gives the app 3 s) and never
// writes the manifest onto the Q2's own storage once the card is gone (M15).
func TestCardGoneDuringPlaylists(t *testing.T) {
	_, r, root := setup(t)
	var gone atomic.Bool
	r.CardMounted = func() bool { return !gone.Load() }
	r.onOp = func(op, _ string) {
		if op == "playlist" && !gone.Load() {
			gone.Store(true)
			os.RemoveAll(root + "/.jellyfin") // unmounted: the card's folders are no longer there
		}
	}
	r.Start(plan.Plan{Playlists: map[string][]string{"A": nil, "B": nil, "C": nil}})
	if s := wait(t, r); s.State != "paused" {
		t.Fatalf("%+v", s)
	}
	if got, _ := filepath.Glob(root + "/_explaylist_data/*.m3u"); len(got) != 1 {
		t.Errorf("written after the card went: %q", got)
	}
	if _, err := os.Stat(root + "/.jellyfin"); !os.IsNotExist(err) {
		t.Error(".jellyfin written after the card went")
	}
}

// The manifest is saved in batches, not after every song (M18): a crash re-fetches at most a batch.
// The run's end saves it once, not again in each phase with nothing new (audit 0.8.1).
func TestManifestSavedInBatches(t *testing.T) {
	_, r, root := setup(t)
	var ws []plan.Want
	for i := 0; i < 30; i++ {
		ws = append(ws, want(root, fmt.Sprint("s", i), "mp3-320"))
	}
	saves := 0
	r.onOp = func(op, _ string) {
		if op == "save" {
			saves++
		}
	}
	r.Start(plan.Plan{Add: ws})
	if s := wait(t, r); s.State != "done" || s.Done != 30 {
		t.Fatalf("%+v", s)
	}
	if saves != 2 { // after 20 songs, and at the end
		t.Errorf("%d manifest saves for 30 songs", saves)
	}
	if m := manifest(t, root); len(m.Songs) != 30 {
		t.Errorf("%d songs recorded", len(m.Songs))
	}
}

// The batch grows with the manifest (1 in 200 of its songs, at least 20), so a first sync of a big
// library doesn't write its whole manifest again every 20 songs; 3 minutes still saves it.
func TestManifestSaveScalesWithLibrary(t *testing.T) {
	firstSave := func(recorded int, minutes bool) int { // downloads before the first save
		_, r, root := setup(t)
		m := &library.Manifest{Songs: map[string]*library.Entry{}}
		for i := 0; i < recorded; i++ {
			id := fmt.Sprint("old", i)
			m.Songs[id] = &library.Entry{ID: id, Path: root + "/Jellyfin/O/o/" + id + ".flac", Quality: "original"}
		}
		m.Save(root, root+"/.jellyfin/manifest.json")
		if minutes { // a minute passes at each look at the clock
			var n atomic.Int64
			r.Now = func() time.Time { return time.Unix(n.Add(60), 0) }
		}
		var ws []plan.Want
		for i := 0; i < 60; i++ {
			ws = append(ws, want(root, fmt.Sprint("s", i), "mp3-320"))
		}
		downloads, first := 0, -1
		r.onOp = func(op, _ string) {
			switch {
			case op == "download":
				downloads++
			case op == "save" && first < 0:
				first = downloads + 1 // a song is saved before its download is told
			}
		}
		r.Start(plan.Plan{Add: ws})
		if s := wait(t, r); s.State != "done" || s.Done != 60 {
			t.Fatalf("%+v", s)
		}
		return first
	}
	if n := firstSave(0, false); n != 20 {
		t.Errorf("small manifest: first save after %d songs, want 20", n)
	}
	if n := firstSave(10_000, false); n != 50 {
		t.Errorf("10,000 songs: first save after %d songs, want 50", n)
	}
	if n := firstSave(10_000, true); n >= 50 {
		t.Errorf("10,000 songs, minutes passing: first save after %d songs, want before 50", n)
	}
}

// A Sync Quality change replaces songs with files of a new format, saved in batches too, not each
// with a save of its own (audit 2026-11-16 #6). An old file goes right after the save that records
// its song's new one, never before: the manifest on the card names only files that are there (I4).
func TestReplaceNewFormatSavedInBatches(t *testing.T) {
	_, r, root := setup(t)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	olds := map[string]string{} // id -> its old file
	var ws []plan.Want
	for i := 0; i < 30; i++ {
		id := fmt.Sprint("s", i)
		old, w := want(root, id, "original"), want(root, id, "opus-128")
		put(t, old.Path, "old rip")
		put(t, library.LyricsPath(old.Path), "[00:01.00]old") // the new song has none: it goes too
		m.Songs[id] = &library.Entry{ID: id, Path: old.Path, Quality: "original", Size: 7, Items: old.Items,
			Meta: library.MetaKey(old.Song, "original")}
		olds[id] = old.Path
		ws = append(ws, w)
	}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	saves := 0
	r.onOp = func(op, _ string) {
		switch op {
		case "save":
			saves++
		case "download": // after the song's save, if it had one
			b, _ := os.ReadFile(root + "/.jellyfin/manifest.json")
			card := &library.Manifest{}
			json.Unmarshal(b, card)
			for id, e := range card.Songs {
				if _, err := os.Stat(e.Path); err != nil {
					t.Errorf("%s recorded at a file not there: %v", id, err)
				}
				if _, err := os.Stat(olds[id]); e.Path != olds[id] && err == nil {
					t.Errorf("%s saved at its new file, its old one left", id)
				}
			}
		}
	}
	r.Start(plan.Plan{Replace: ws})
	if s := wait(t, r); s.State != "done" || s.Done != 30 {
		t.Fatalf("%+v", s)
	}
	if saves > 5 {
		t.Errorf("%d manifest saves for 30 replaced songs", saves)
	}
	mf := manifest(t, root)
	if len(mf.Stale) != 0 { // else the next preview offers to delete files already gone
		t.Errorf("deleted files still listed: %q", mf.Stale)
	}
	for _, w := range ws {
		if e := mf.Songs[w.Song.ID]; e == nil || e.Path != w.Path {
			t.Errorf("%s: %+v", w.Song.ID, e)
		}
		if _, err := os.Stat(olds[w.Song.ID]); !os.IsNotExist(err) {
			t.Errorf("%s: old file left: %v", w.Song.ID, err)
		}
		if _, err := os.Stat(library.LyricsPath(olds[w.Song.ID])); !os.IsNotExist(err) {
			t.Errorf("%s: old lyrics left: %v", w.Song.ID, err)
		}
	}
}

// Short of space, a save deletes the replaced songs' old files waiting for one before the run stops
// at the reserve: before batching, they went one by one as the new ones arrived.
func TestSpaceLowSavesReplacedFiles(t *testing.T) {
	_, r, root := setup(t)
	old, w := want(root, "a", "original"), want(root, "a", "opus-128")
	put(t, old.Path, "old rip")
	m := &library.Manifest{Songs: map[string]*library.Entry{"a": {ID: "a", Path: old.Path, Quality: "original",
		Size: 7, Items: old.Items, Meta: library.MetaKey(old.Song, "original")}}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	r.Free = func() int64 { // room for a, then for b only once a's old file is gone
		if _, err := os.Stat(old.Path); calls.Add(1) == 1 || os.IsNotExist(err) {
			return 1 << 30
		}
		return 0
	}
	r.Reserve = func() int64 { return 1 }
	r.Start(plan.Plan{Replace: []plan.Want{w, want(root, "b", "opus-128")}})
	if s := wait(t, r); s.State != "done" || s.Done != 2 {
		t.Fatalf("%+v", s)
	}
}

// A Sync Quality change that shrinks songs gets past a card at its reserve: the replaces go first,
// each frees its old file at the next save, and the adds then fit (audit 0.8.1 round 2 #1). Before,
// the adds went first and the run stopped at the first one, every time.
func TestShrinkingReplacesMakeRoom(t *testing.T) {
	_, r, root := setup(t)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	var ws []plan.Want
	for _, id := range []string{"a", "b"} {
		old := want(root, id, "original")
		put(t, old.Path, strings.Repeat("x", 1<<20))
		m.Songs[id] = &library.Entry{ID: id, Path: old.Path, Quality: "original", Size: 1 << 20, Items: old.Items,
			Meta: library.MetaKey(old.Song, "original")}
		ws = append(ws, want(root, id, "opus-128"))
	}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	const reserve = 1 << 20
	r.Free = func() int64 { // a card of 3 MB and 1 KB: 1 KB above the reserve, less than any song
		var used int64
		filepath.Walk(filepath.Join(root, "Jellyfin"), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				used += fi.Size()
			}
			return nil
		})
		return 3<<20 + 1<<10 - used
	}
	r.Reserve = func() int64 { return reserve }
	r.Start(plan.Plan{Add: []plan.Want{want(root, "c", "opus-128")}, Replace: ws})
	if s := wait(t, r); s.State != "done" || s.Done != 3 {
		t.Fatalf("%+v", s)
	}
}

// A replaced song's old file stays until a save records its new one; with the card gone no save is
// made, and it stays, still the song's (I4, audit 2026-11-16 #6). A pause saves it in Stale, its
// delete left for the next run, which deletes it first (audit 0.8.1: it was forgotten).
func TestReplacedFileGoesAfterSave(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprint("card gone ", gone), func(t *testing.T) {
			_, r, root := setup(t)
			old, w := want(root, "a", "original"), want(root, "a", "opus-128")
			put(t, old.Path, "old rip")
			m := &library.Manifest{Songs: map[string]*library.Entry{"a": {ID: "a", Path: old.Path, Quality: "original",
				Size: 7, Items: old.Items, Meta: library.MetaKey(old.Song, "original")}}}
			if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
				t.Fatal(err)
			}
			var away atomic.Bool
			r.CardMounted = func() bool { return !away.Load() }
			r.onOp = func(op, _ string) {
				switch {
				case op != "download":
				case gone:
					away.Store(true)
				default:
					r.mu.Lock()
					r.stopAs = "paused"
					r.cancel() // Pause, from inside the run, after the first song
					r.mu.Unlock()
				}
			}
			r.Start(plan.Plan{Replace: []plan.Want{w, want(root, "b", "opus-128")}}) // b, not on the card, after a
			if s := wait(t, r); s.State != "paused" {
				t.Fatalf("%+v", s)
			}
			_, err := os.Stat(old.Path)
			mf := manifest(t, root)
			e := mf.Songs["a"]
			switch {
			case gone && (err != nil || e.Path != old.Path || len(mf.Stale) != 0):
				t.Fatalf("not saved: old file %v, recorded at %s, stale %q", err, e.Path, mf.Stale)
			case !gone && (err != nil || e.Path != w.Path || len(mf.Stale) != 1 || mf.Stale[0] != old.Path):
				t.Fatalf("saved: old file %v, recorded at %s, stale %q", err, e.Path, mf.Stale)
			}
			if gone {
				return
			}
			r.onOp = nil
			r.Start(plan.Plan{})
			if s := wait(t, r); s.State != "done" {
				t.Fatalf("%+v", s)
			}
			if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
				t.Errorf("old file left: %v", err)
			}
			if mf := manifest(t, root); len(mf.Stale) != 0 || mf.Songs["a"].Path != w.Path {
				t.Errorf("stale %q, a at %s", mf.Stale, mf.Songs["a"].Path)
			}
		})
	}
}

// A replaced file still in Stale (its delete failed, or a stop) that a song is then renamed onto is
// the song's: the save after the rename leaves it and drops it from Stale (I4).
func TestRenameOntoStaleFileKept(t *testing.T) {
	_, r, root := setup(t)
	from, x := root+"/Jellyfin/A/x/01 - c.flac", root+"/Jellyfin/A/y/01 - c.flac"
	put(t, from, "c")
	put(t, x, "old rip")
	m := &library.Manifest{Songs: map[string]*library.Entry{"c": {ID: "c", Path: from, Quality: "original", Size: 1}},
		Stale: []string{x}}
	j := &job{Runner: r, ctx: context.Background(), p: plan.Plan{Rename: []plan.Move{{From: from, To: x, ID: "c"}}},
		m: m, saved: r.Now(), recorded: map[string]int{strings.ToLower(from): 1}}
	if err := j.renames(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(x); err != nil || string(b) != "c" || m.Songs["c"].Path != x {
		t.Fatalf("%q %v, c at %s", b, err, m.Songs["c"].Path)
	}
	if len(m.Stale) != 0 {
		t.Errorf("stale %q", m.Stale)
	}
}

// Renames are saved in batches too, not after every song (audit #14): once before a batch moves
// (its targets) and once after. The card going part-way leaves a manifest the next run finishes
// from, the renames done but not saved included (M13).
func TestRenamesSavedInBatches(t *testing.T) {
	_, r, root := setup(t)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	var mvs []plan.Move
	for i := 0; i < 30; i++ {
		id := fmt.Sprint("r", i)
		from, to := fmt.Sprintf("%s/Jellyfin/A/x/%02d - r.flac", root, i), fmt.Sprintf("%s/Jellyfin/A/y/%02d - r.flac", root, i)
		put(t, from, id)
		m.Songs[id] = &library.Entry{ID: id, Path: from, Quality: "original", Size: int64(len(id))}
		mvs = append(mvs, plan.Move{From: from, To: to, ID: id})
	}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	saves := 0
	r.onOp = func(op, _ string) {
		if op == "save" {
			saves++
		}
	}
	var checks atomic.Int32
	r.CardMounted = func() bool { return checks.Add(1) <= 14 } // the card goes after a dozen renames
	p := plan.Plan{Rename: mvs}
	r.Start(p)
	if s := wait(t, r); s.State != "paused" || s.Renamed != 12 {
		t.Fatalf("%+v", s)
	}
	if saves != 1 {
		t.Errorf("%d manifest saves for 12 renames", saves)
	}
	if m, err := library.LoadManifest(root + "/.jellyfin/manifest.json"); err != nil || m.Songs["r11"].Path != mvs[11].To ||
		m.Songs["r12"].Path != mvs[12].From || m.Songs["r11"].Moving+m.Songs["r12"].Moving != "" {
		t.Errorf("not where the files are: %v %+v %+v", err, m.Songs["r11"], m.Songs["r12"])
	}
	r.CardMounted = func() bool { return true }
	saves = 0
	r.Start(p)
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if saves != 2 { // before the moves, and at the end: the moves need no save of their own
		t.Errorf("%d manifest saves for 30 renames", saves)
	}
	mf := manifest(t, root)
	for _, mv := range mvs {
		if e := mf.Songs[mv.ID]; e == nil || e.Path != mv.To {
			t.Errorf("%s: %+v", mv.ID, e)
		}
		if b, err := os.ReadFile(mv.To); err != nil || string(b) != mv.ID {
			t.Errorf("%s: %q, %v", mv.To, b, err)
		}
	}
}

// A re-ripped original (the server's file changed size) is fetched again though its path, quality
// and tags are the same, and its new size recorded, so the next preview has nothing to do (audit #1).
func TestReRipDownloaded(t *testing.T) {
	f, r, root := setup(t)
	w := want(root, "a", "original")
	w.Song.Size = 5000
	put(t, w.Path, "old rip")
	m := &library.Manifest{Songs: map[string]*library.Entry{"a": {ID: "a", Path: w.Path, Quality: "original", Size: 7,
		Items: w.Items, Meta: library.MetaKey(w.Song, "original"), SrcSize: 4000}}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	r.Start(plan.Plan{Replace: []plan.Want{w}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if h, _ := f.seen("a"); h == 0 {
		t.Fatal("not downloaded")
	}
	sameFile(t, w.Path, "a")
	if e := manifest(t, root).Songs["a"]; e == nil || e.SrcSize != 5000 || e.Size != int64(len(data("a"))) {
		t.Fatalf("%+v", e)
	}
}

// Start keeps no copy of the plan on the card (an Entire Library plan ran the app out of memory);
// an older build's pending.json goes (I16).
func TestStartWritesNoPendingPlan(t *testing.T) {
	f, r, root := setup(t)
	put(t, root+"/.jellyfin/pending.json", "{}")
	a := want(root, "a", "original")
	f.block["a"] = make(chan struct{})
	r.Start(plan.Plan{Add: []plan.Want{a}})
	_, err := os.Stat(root + "/.jellyfin/pending.json")
	close(f.block["a"])
	wait(t, r)
	if !os.IsNotExist(err) {
		t.Fatal("pending.json while running:", err)
	}
}

// A .part left by another version of the file (a transcode, then Original; or a re-rip) is fetched
// again whole, never continued with Range into a spliced song (M14).
func TestResumeRefusesOtherVersion(t *testing.T) {
	f, r, root := setup(t)
	a := want(root, "a", "original")
	put(t, a.Path+".part", "TRANSCODED")
	put(t, a.Path+".part.key", "mp3-320 4000")
	r.Start(plan.Plan{Add: []plan.Want{a}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if _, rg := f.seen("a"); len(rg) != 1 || rg[0] != "" {
		t.Errorf("ranges %q", rg)
	}
	sameFile(t, a.Path, "a")
	noParts(t, root)
}

// A failed try after the server's file changed size doesn't put the new key on the old bytes: the
// next try fetches the song whole, never v1's head plus v2's tail.
func TestFailedTryKeepsOtherVersionOut(t *testing.T) {
	f, r, root := setup(t)
	a := want(root, "a", "original")
	put(t, a.Path+".part", "OLD VERSION")
	put(t, a.Path+".part.key", "original 4000")
	f.once["a"] = 503
	r.Start(plan.Plan{Add: []plan.Want{a}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if _, rg := f.seen("a"); len(rg) != 2 || rg[0] != "" || rg[1] != "" {
		t.Errorf("ranges %q", rg)
	}
	sameFile(t, a.Path, "a")
	noParts(t, root)
}

// A song the server refuses leaves no .part.key and no album folder on the card.
func TestRefusedSongLeavesNothing(t *testing.T) {
	f, r, root := setup(t)
	a := want(root, "a", "original")
	f.status["a"] = 404
	r.Start(plan.Plan{Add: []plan.Want{a}})
	if s := wait(t, r); s.State != "done" || s.Failed != 1 {
		t.Fatalf("%+v", s)
	}
	noParts(t, root)
	if _, err := os.Stat(root + "/Jellyfin"); !os.IsNotExist(err) {
		t.Error("folder left:", err)
	}
}

// A transcode cut part-way leaves no .part: it is never resumed.
func TestCutTranscodeLeavesNoPart(t *testing.T) {
	f, r, root := setup(t)
	r.Backoff = r.Backoff[:1]
	b := want(root, "bb", "mp3-320")
	f.drop["bb"] = true
	r.Start(plan.Plan{Add: []plan.Want{b}})
	if s := wait(t, r); s.State != "done" || s.Failed != 1 {
		t.Fatalf("%+v", s)
	}
	noParts(t, root)
}

// Covers are asked for at 600 px (M19), and one past 16 MiB is refused, never saved cut short.
func TestCoverSizedAndWhole(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	b.Song.Album, b.Song.CoverArt = "Other", "big-cover"
	b.Path = library.SongPath(root, b.Song, "original", false)
	r.Start(plan.Plan{Add: []plan.Want{a, b}})
	s := wait(t, r)
	if s.State != "done" || s.Failed != 0 || len(s.Failures) != 0 { // a cover failure is only logged
		t.Fatalf("%+v", s)
	}
	if c, err := os.ReadFile(filepath.Dir(a.Path) + "/cover.jpg"); err != nil || string(c) != "jpeg-al-1" {
		t.Error("cover", err)
	}
	if _, err := os.Stat(filepath.Dir(b.Path) + "/cover.jpg"); !os.IsNotExist(err) {
		t.Error("a cut cover was saved")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sizes) != 2 || f.sizes[0] != "600" || f.sizes[1] != "600" {
		t.Errorf("sizes %q", f.sizes)
	}
}

// A cover is streamed to the card, so one past 16 MiB costs no more RAM than a small one, and is
// never saved cut short; a Content-Length past 16 MiB is refused before the body is read.
func TestCoverStreamed(t *testing.T) {
	for name, n := range map[string]int64{"no length": -1, "length": maxCover + 1} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			var read int64
			body := func() (io.Reader, int64) { return &counted{io.LimitReader(zeros{}, maxCover+1), &read}, n }
			r := New(root, crafted{body}, nil)
			r.Net = NewNetSlot()
			w := want(root, "a", "original")
			w.Song.CoverArt = "c"
			os.MkdirAll(filepath.Dir(w.Path), 0o755)
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			r.cover(context.Background(), w)
			runtime.ReadMemStats(&m1)
			if a := m1.TotalAlloc - m0.TotalAlloc; a > 4<<20 {
				t.Errorf("allocated %d bytes", a)
			}
			if _, err := os.Stat(filepath.Dir(w.Path) + "/cover.jpg"); !os.IsNotExist(err) {
				t.Error("a cut cover was saved")
			}
			if n > 0 && read != 0 {
				t.Errorf("read %d bytes past a Content-Length over 16 MiB", read)
			}
		})
	}
}

type counted struct {
	r io.Reader
	n *int64
}

func (c *counted) Read(p []byte) (int, error) { k, err := c.r.Read(p); *c.n += int64(k); return k, err }

// Refresh records the items holding songs that need no transfer, while no sync runs (I12); a run
// records them too, and a renamed song's new MetaKey.
func TestRefreshRecords(t *testing.T) {
	f, r, root := setup(t)
	s := jellyfin.Song{ID: "a", Title: "Ta", Album: "Alb", AlbumArtist: "Art", Track: 1, Suffix: "flac"}
	moved := s
	moved.Title = "Tb"
	from, to := library.SongPath(root, s, "original", false), library.SongPath(root, moved, "original", false)
	put(t, from, "a")
	(&library.Manifest{Songs: map[string]*library.Entry{"a": {ID: "a", Path: from, Quality: "original", Size: 1,
		Items: []string{"playlist:p1"}, Meta: library.MetaKey(s, "original")}}}).Save(root, root+"/.jellyfin/manifest.json")
	if err := r.Refresh([]plan.Want{{Song: s, Quality: "original", Path: from, Items: []string{"album:al"}}}); err != nil {
		t.Fatal(err)
	}
	if e := manifest(t, root).Songs["a"]; !reflect.DeepEqual(e.Items, []string{"album:al"}) {
		t.Fatalf("%+v", e)
	}
	b := want(root, "bb", "original")
	f.block["bb"] = make(chan struct{})
	r.Start(plan.Plan{Add: []plan.Want{b}, Rename: []plan.Move{{From: from, To: to, ID: "a"}},
		Keep: []plan.Want{{Song: moved, Quality: "original", Path: to, Items: []string{"album:al"}}}})
	if err := r.Refresh(nil); err != nil {
		t.Fatal("nothing to refresh:", err)
	}
	if err := r.Refresh([]plan.Want{{Song: s, Quality: "original", Path: from}}); !errors.Is(err, ErrBusy) {
		t.Fatal("refresh while running:", err)
	}
	close(f.block["bb"])
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if e := manifest(t, root).Songs["a"]; e.Path != to || e.Meta != library.MetaKey(moved, "original") {
		t.Fatalf("%+v", e)
	}
}

// refusing is a server that refuses every download for the account (Jellyfin error 50: downloads
// turned off for it).
type refusing struct{ gets atomic.Int32 }

func (f *refusing) MediaURL(id, q string) string { return "https://h/rest/download?id=" + id }
func (f *refusing) CoverURL(id string) string    { return "" }
func (f *refusing) Get(context.Context, string, int64) (*http.Response, error) {
	f.gets.Add(1)
	return nil, &jellyfin.APIError{Op: "get", HTTP: 200, Code: 50, Msg: "User not authorized"}
}

// A download the server refuses for the whole account stops the run at its first song, with that
// reason, instead of failing every song one by one (R39).
func TestRefusedStopsAtOnce(t *testing.T) {
	src, root := &refusing{}, t.TempDir()
	r := New(root, src, nil)
	r.Backoff, r.CardMounted, r.Net = []time.Duration{time.Millisecond, time.Millisecond}, func() bool { return true }, NewNetSlot()
	r.Start(plan.Plan{Add: []plan.Want{want(root, "a", "original"), want(root, "bb", "original"), want(root, "ccc", "original")}})
	s := wait(t, r)
	var ae *jellyfin.APIError
	if s.State != "paused" || !errors.Is(s.Err, ErrRefused) || !errors.As(s.Err, &ae) || ae.Code != 50 {
		t.Fatalf("%+v", s)
	}
	if n := src.gets.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// Songs moved to a new album folder (a year or album fix) take the album's cover along (M17).
func TestRenameCarriesCover(t *testing.T) {
	_, r, root := setup(t)
	from, to := root+"/Jellyfin/Art/1999 - Alb/01 - r.flac", root+"/Jellyfin/Art/2000 - Alb/01 - r.flac"
	put(t, from, "r")
	put(t, filepath.Dir(from)+"/cover.jpg", "jpeg")
	(&library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: from, Quality: "original", Size: 1}}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Rename: []plan.Move{{From: from, To: to, ID: "r"}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if c, err := os.ReadFile(filepath.Dir(to) + "/cover.jpg"); err != nil || string(c) != "jpeg" {
		t.Error("cover not carried:", err)
	}
	if _, err := os.Stat(filepath.Dir(from)); !os.IsNotExist(err) {
		t.Error("old album folder left")
	}
}

// A cover.jpg over 16 MiB (planted over Wi-Fi Transfer) is not carried: carryCover streams with the
// same cap as cover().
func TestRenameSkipsOversizeCover(t *testing.T) {
	_, r, root := setup(t)
	from, to := root+"/Jellyfin/Art/1999 - Alb/01 - r.flac", root+"/Jellyfin/Art/2000 - Alb/01 - r.flac"
	put(t, from, "r")
	put(t, filepath.Dir(from)+"/cover.jpg", "")
	if err := os.Truncate(filepath.Dir(from)+"/cover.jpg", maxCover+1); err != nil {
		t.Fatal(err)
	}
	(&library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: from, Quality: "original", Size: 1}}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Rename: []plan.Move{{From: from, To: to, ID: "r"}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if es, _ := os.ReadDir(filepath.Dir(to)); len(es) != 1 {
		t.Errorf("over-size cover carried: %v", es)
	}
}

// A cover.jpg.tmp left by a hard kill doesn't keep the album folder once its last song goes.
func TestRemovePrunesLeftoverTmp(t *testing.T) {
	_, r, root := setup(t)
	z := &library.Entry{ID: "z", Path: root + "/Jellyfin/Z/z/01 - z.flac", Quality: "original"}
	dir := filepath.Dir(z.Path)
	for _, f := range []string{z.Path, dir + "/cover.jpg", dir + "/cover.jpg.tmp"} {
		put(t, f, "x")
	}
	(&library.Manifest{Songs: map[string]*library.Entry{"z": z}}).Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Remove: []*library.Entry{z}, Playlists: map[string][]string{}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(root + "/Jellyfin/Z"); !os.IsNotExist(err) {
		es, _ := os.ReadDir(dir)
		t.Errorf("album folder left: %v", es)
	}
}

// The firmware's answer to a library request (R35): the files it imported become the app's own; an
// added file it skipped because the Q2 has a playlist of that name (the user's) is renamed
// "<name> (Jellyfin).m3u", to add again; one skipped under that name as well is a clash.
func TestImported(t *testing.T) {
	_, r, root := setup(t)
	mix, rock, fav := library.PlaylistFile(root, "Mix"), library.PlaylistFile(root, "Rock"), root+"/_explaylist_data/Fav (Jellyfin).m3u"
	for _, f := range []string{mix, rock, fav} {
		put(t, f, "#EXTM3U\n")
	}
	(&library.Manifest{Playlists: map[string]string{"Mix": mix, "Rock": rock, "Fav": fav}, Imported: map[string]bool{},
		PlaylistSums: map[string]string{"Fav": "x"}}).Save(root, root+"/.jellyfin/manifest.json")
	again, clash, err := r.Imported([2][]string{nil, {mix, rock, fav}}, []string{rock, fav})
	rockS := root + "/_explaylist_data/Rock (Jellyfin).m3u"
	if err != nil || !reflect.DeepEqual(again, []string{rockS}) || !reflect.DeepEqual(clash, []string{fav}) {
		t.Fatal(again, clash, err)
	}
	m := manifest(t, root)
	if !m.Imported[mix] || m.Imported[rock] || m.Imported[fav] || m.Playlists["Rock"] != rockS {
		t.Fatalf("%+v %+v", m.Imported, m.Playlists)
	}
	if _, ok := m.PlaylistSums["Fav"]; ok {
		t.Fatal("clash kept its sum: the next preview finds nothing to do")
	}
	if _, err := os.Stat(rockS); err != nil {
		t.Fatal(err)
	}
}

// Moving names the songs a running or paused sync will rename, replace or remove: the streamer
// fetches them rather than queue a path about to go (M24).
func TestMoving(t *testing.T) {
	f, r, root := setup(t)
	a := want(root, "a", "original")
	f.block["a"] = make(chan struct{})
	r.Start(plan.Plan{Add: []plan.Want{a}, Replace: []plan.Want{want(root, "b", "mp3-320")},
		Rename: []plan.Move{{ID: "c"}}, Remove: []*library.Entry{{ID: "d"}}})
	for _, id := range []string{"b", "c", "d"} {
		if !r.Moving(id) {
			t.Errorf("%s not moving", id)
		}
	}
	if r.Moving("a") {
		t.Error("an added song moving")
	}
	r.Cancel()
	if r.Moving("d") {
		t.Error("moving after Cancel")
	}
	r.Start(plan.Plan{Add: []plan.Want{a}, Remove: []*library.Entry{{ID: "e"}}}) // the next plan's, not the last's
	if !r.Moving("e") || r.Moving("b") || r.Moving("d") {
		t.Errorf("next plan: e %v, b %v, d %v", r.Moving("e"), r.Moving("b"), r.Moving("d"))
	}
	r.Cancel()
}

// A run that ends with no failure drops its plan; one with failures keeps it for Retry Failed until
// Forget (a new preview), and a paused run keeps it for Resume through Forget.
func TestEndedRunDropsPlan(t *testing.T) {
	f, r, root := setup(t)
	held := func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.plan != nil || r.moving != nil }
	r.Start(plan.Plan{Add: []plan.Want{want(root, "a", "original")}, Remove: []*library.Entry{{ID: "d"}}})
	if s := wait(t, r); s.State != "done" || held() {
		t.Fatalf("%+v, plan held %v", s, held())
	}
	f.mu.Lock()
	f.status["x"] = 404
	f.block["b"] = make(chan struct{})
	f.mu.Unlock()
	r.Start(plan.Plan{Add: []plan.Want{want(root, "x", "original")}})
	if s := wait(t, r); s.State != "done" || s.Failed != 1 || !held() {
		t.Fatalf("%+v, plan held %v", s, held())
	}
	r.Forget()
	if held() {
		t.Fatal("Forget kept the plan")
	}
	r.Start(plan.Plan{Add: []plan.Want{want(root, "b", "original")}})
	r.Pause()
	r.Forget()
	if s := r.Snapshot(); s.State != "paused" || !held() {
		t.Fatalf("%+v, plan held %v", s, held())
	}
	r.Cancel()
}

// The runner is Busy until OnDone has returned, so the app never exits between a sync's end and
// its library request (I15).
func TestBusyUntilOnDone(t *testing.T) {
	_, r, root := setup(t)
	in, out := make(chan struct{}), make(chan struct{})
	r.OnDone = func(plan.Plan) { close(in); <-out }
	r.Start(plan.Plan{Add: []plan.Want{want(root, "a", "original")}})
	<-in
	busy := r.Busy()
	close(out)
	if !busy {
		t.Fatal("not busy in OnDone")
	}
}

// An edit loads and saves the manifest without r.mu: Snapshot, Busy and Moving answer while it
// works, Imported's edit is ErrBusy rather than wait, and a run started meanwhile loads the
// manifest once it is saved: both land.
func TestEditHoldsOnlyTheFile(t *testing.T) {
	_, r, root := setup(t)
	var tried atomic.Bool
	r.onOp = func(op, path string) { tried.Store(true) }
	entry := func(id string) func(m *library.Manifest) (bool, error) {
		return func(m *library.Manifest) (bool, error) {
			m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(root, id+".flac")}
			return true, nil
		}
	}
	in, out, edited := make(chan struct{}), make(chan struct{}), make(chan error)
	go func() {
		edited <- r.edit(true, func(m *library.Manifest) (bool, error) {
			close(in)
			<-out
			return entry("z")(m)
		})
	}()
	<-in
	answered := make(chan struct{})
	go func() { r.Snapshot(); r.Busy(); r.Moving("a"); close(answered) }()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		close(out)
		t.Fatal("Snapshot waited for the edit")
	}
	if err := r.edit(false, entry("x")); !errors.Is(err, ErrBusy) {
		t.Fatal("Imported's edit during another:", err)
	}
	r.Start(plan.Plan{Add: []plan.Want{want(root, "a", "original")}})
	time.Sleep(100 * time.Millisecond)
	ran := tried.Load()
	close(out)
	if ran {
		t.Fatal("the run didn't wait for the edit")
	}
	if err := <-edited; err != nil {
		t.Fatal(err)
	}
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if m := manifest(t, root); m.Songs["z"] == nil || m.Songs["a"] == nil {
		t.Fatalf("the edit or the run lost: %+v", m)
	}
}

// Edits wait for each other, and both land.
func TestEditsWaitForEachOther(t *testing.T) {
	_, r, root := setup(t)
	in, out, first, second := make(chan struct{}), make(chan struct{}), make(chan error), make(chan error)
	add := func(id string) func(m *library.Manifest) (bool, error) {
		return func(m *library.Manifest) (bool, error) {
			m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(root, id+".flac")}
			return true, nil
		}
	}
	go func() {
		first <- r.edit(true, func(m *library.Manifest) (bool, error) { close(in); <-out; return add("z")(m) })
	}()
	<-in
	go func() { second <- r.edit(true, add("y")) }()
	time.Sleep(50 * time.Millisecond)
	close(out)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal("the second edit:", err)
	}
	if m := manifest(t, root); m.Songs["z"] == nil || m.Songs["y"] == nil {
		t.Fatalf("an edit lost: %+v", m.Songs)
	}
}

// crafted answers every download with a body and Content-Length of its own (-1: none sent), as a
// transport that doesn't hold a body to its length would pass them on.
type crafted struct{ body func() (io.Reader, int64) }

func (c crafted) MediaURL(id, q string) string { return "https://h/rest/download?id=" + id }
func (c crafted) CoverURL(id string) string    { return "" }
func (c crafted) Get(context.Context, string, int64) (*http.Response, error) {
	b, n := c.body()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(b), ContentLength: n}, nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A body short of its Content-Length, an empty one, or one that runs on past twice its song's
// size (with or without a Content-Length) is never saved as the song (I8, M9).
func TestBodyBounds(t *testing.T) {
	for name, body := range map[string]func() (io.Reader, int64){
		"short":               func() (io.Reader, int64) { return strings.NewReader("half"), 1000 },
		"empty":               func() (io.Reader, int64) { return strings.NewReader(""), -1 },
		"endless":             func() (io.Reader, int64) { return io.LimitReader(zeros{}, 40<<20), -1 },
		"long Content-Length": func() (io.Reader, int64) { return io.LimitReader(zeros{}, 40<<20), 40 << 20 },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			r := New(root, crafted{body}, nil)
			r.Backoff, r.CardMounted, r.Net = []time.Duration{time.Millisecond}, func() bool { return true }, NewNetSlot()
			w := want(root, "a", "original")
			w.Bytes = 1000
			r.Start(plan.Plan{Add: []plan.Want{w}})
			if s := wait(t, r); s.Failed != 1 {
				t.Fatalf("%+v", s)
			}
			if _, err := os.Stat(w.Path); err == nil {
				t.Fatal("saved as the song")
			}
		})
	}
}

// oggPages is an Ogg stream of 1000 pages (over the 64 KiB looked at), its last flagged
// end-of-stream (header_type 4), as a server's whole transcode ends.
func oggPages() []byte {
	var b []byte
	for i := range 1000 {
		h := make([]byte, 28, 28+200)
		copy(h, "OggS")
		if i == 999 {
			h[5] = 4
		}
		h[26], h[27] = 1, 200 // one segment of 200 bytes
		b = append(b, append(h, bytes.Repeat([]byte{byte(i)}, 200)...)...)
	}
	return b
}

// An Ogg transcode sent without Content-Length is a song only when it ends with its whole
// end-of-stream page: one its server stopped part-way also ends cleanly (finding 13c). An
// original is not checked so, as its last page may lack the flag (a recording cut from a radio
// stream); a proxy that gzips it drops its length, so it must come to its planned size.
func TestOggTranscodeEnds(t *testing.T) {
	whole := oggPages()
	cut := whole[:len(whole)/2+100]
	for name, c := range map[string]struct {
		b     []byte
		n     int64
		q     string
		size  int64 // the song's size as planned; 0: all of b
		saved bool
	}{
		"whole":                      {whole, -1, "opus-128", 0, true},
		"cut":                        {cut, -1, "opus-128", 0, false},
		"cut in its last page":       {whole[:len(whole)-50], -1, "opus-128", 0, false},
		"cut, Content-Length":        {cut, int64(len(cut)), "opus-128", 0, true},
		"not Ogg, no length":         {data("a"), -1, "opus-128", 0, true},
		"original, no EOS":           {cut, -1, "original", 0, true},
		"original, no length, short": {cut, -1, "original", int64(len(whole)), false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			r := New(root, crafted{func() (io.Reader, int64) { return bytes.NewReader(c.b), c.n }}, nil)
			r.Backoff, r.CardMounted, r.Net = []time.Duration{time.Millisecond}, func() bool { return true }, NewNetSlot()
			w := want(root, "a", c.q)
			w.Bytes = cmp.Or(c.size, int64(len(c.b)))
			r.Start(plan.Plan{Add: []plan.Want{w}})
			s := wait(t, r)
			if _, err := os.Stat(w.Path); (err == nil) != c.saved || (s.Failed == 0) != c.saved {
				t.Fatalf("saved %v, want %v: %+v", err == nil, c.saved, s)
			}
		})
	}
}

// A body cut at the limit says it was too long, though it is short of its Content-Length.
func TestSizeErrLong(t *testing.T) {
	limit := Limit(40<<20, 1000)
	if err := SizeErr(limit+1, 40<<20, limit); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatal(err)
	}
}

// A playlist's digest is saved with it, so a pause between playlists doesn't offer the written
// ones again (review G3, Minor 9).
func TestPlaylistSumsSurvivePause(t *testing.T) {
	_, r, root := setup(t)
	r.onOp = func(op, _ string) {
		if op == "playlist" {
			r.mu.Lock()
			r.stopAs = "paused"
			r.cancel() // Pause, from inside the run, after the first playlist
			r.mu.Unlock()
		}
	}
	r.Start(plan.Plan{Playlists: map[string][]string{"A": {}, "B": {}}})
	if s := wait(t, r); s.State != "paused" {
		t.Fatalf("%+v", s)
	}
	if m := manifest(t, root); m.PlaylistSums["A"] != library.PlaylistSum([]string{}) {
		t.Fatalf("sums %v", m.PlaylistSums)
	}
}

// A stop records when it happened, for Details (review G3, Minor 3), and the snapshot counts the
// renames and removals a run made, for its summary (Minor 7).
func TestStopTimeAndCounts(t *testing.T) {
	_, r, root := setup(t)
	r.Now = func() time.Time { return time.Unix(1_000_000, 0) }
	os.MkdirAll(root+"/.jellyfin", 0o755)
	os.WriteFile(root+"/.jellyfin/manifest.json", []byte("{"), 0o644)
	r.Start(plan.Plan{})
	if s := wait(t, r); s.State != "error" || !s.ErrAt.Equal(time.Unix(1_000_000, 0)) {
		t.Fatalf("%+v", s)
	}
	os.Remove(root + "/.jellyfin/manifest.json")
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for _, id := range []string{"x", "y"} {
		e := &library.Entry{ID: id, Path: root + "/Jellyfin/Z/z/" + id + ".flac", Quality: "original"}
		put(t, e.Path, id)
		m.Songs[id] = e
	}
	m.Save(root, root+"/.jellyfin/manifest.json")
	r.Start(plan.Plan{Remove: []*library.Entry{m.Songs["x"]},
		Rename: []plan.Move{{ID: "y", From: m.Songs["y"].Path, To: root + "/Jellyfin/Z/z/y2.flac"}}})
	if s := wait(t, r); s.Removed != 1 || s.Renamed != 1 {
		t.Fatalf("%+v", s)
	}
}

// withLyrics is a source that has lyrics for every song.
type withLyrics struct{ MediaSource }

func (withLyrics) Lyrics(context.Context, string) (string, error) { return "[00:01.00]la", nil }

// A replace that changes the format (Song.flac -> Song.opus) keeps the lyrics it wrote: removing
// the old file takes its .lrc, which is the new one's too (audit #4).
func TestReplaceNewFormatKeepsLyrics(t *testing.T) {
	_, r, root := setup(t)
	r.src = withLyrics{r.src}
	old, w := want(root, "a", "original"), want(root, "a", "opus-128")
	w.Song.HasLyrics = true
	put(t, old.Path, "old rip")
	put(t, library.LyricsPath(old.Path), "[00:01.00]old")
	m := &library.Manifest{Songs: map[string]*library.Entry{"a": {ID: "a", Path: old.Path, Quality: "original", Size: 7,
		Items: old.Items, Meta: library.MetaKey(old.Song, "original")}}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	r.Start(plan.Plan{Replace: []plan.Want{w}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
		t.Fatal("old file left")
	}
	if b, err := os.ReadFile(library.LyricsPath(w.Path)); err != nil || string(b) != "[00:01.00]la" {
		t.Fatalf("lyrics: %q, %v", b, err)
	}
}

// A replaced song's old file stays while another song is still recorded in it (one file under two
// names, from older builds), as removals keep it (audit 2026-11-10 #8).
func TestReplaceKeepsSharedFile(t *testing.T) {
	_, r, root := setup(t)
	old, w := want(root, "a", "original"), want(root, "a", "opus-128")
	put(t, old.Path, "old rip")
	m := &library.Manifest{Songs: map[string]*library.Entry{
		"a": {ID: "a", Path: old.Path, Quality: "original", Size: 7, Items: old.Items, Meta: library.MetaKey(old.Song, "original")},
		"b": {ID: "b", Path: old.Path, Quality: "original", Size: 7, Items: old.Items},
	}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	r.Start(plan.Plan{Replace: []plan.Want{w}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(old.Path); err != nil {
		t.Fatalf("shared file deleted: %v", err)
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("new file: %v", err)
	}
}

// Removing one of two songs recorded at one file (older builds) keeps the file and its lyrics for
// the other.
func TestRemovalKeepsSharedFile(t *testing.T) {
	_, r, root := setup(t)
	f := root + "/Jellyfin/Z/z/a.flac"
	put(t, f, "a")
	put(t, library.LyricsPath(f), "a")
	b := &library.Entry{ID: "b", Path: f, Quality: "original"}
	m := &library.Manifest{Songs: map[string]*library.Entry{"a": {ID: "a", Path: f, Quality: "original"}, "b": b}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	r.Start(plan.Plan{Remove: []*library.Entry{b}, Playlists: map[string][]string{}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	for _, p := range []string{f, library.LyricsPath(f)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("shared file deleted: %v", err)
		}
	}
	if m := manifest(t, root); m.Songs["a"] == nil || m.Songs["b"] != nil || len(m.Stale) != 0 {
		t.Errorf("songs %v, stale %q", m.Songs, m.Stale)
	}
}

// Short of space, the removals run first; a file two songs were recorded at (older builds) is then
// still deleted once its last song is replaced, not left with no song recorded at it (re-audit
// 2026-11-10 #5).
func TestEarlyRemovalThenReplaceDropsSharedFile(t *testing.T) {
	_, r, root := setup(t)
	old, w := want(root, "a", "original"), want(root, "a", "opus-128")
	put(t, old.Path, "old rip")
	b := &library.Entry{ID: "b", Path: old.Path, Quality: "original", Size: 7, Items: old.Items}
	m := &library.Manifest{Songs: map[string]*library.Entry{
		"a": {ID: "a", Path: old.Path, Quality: "original", Size: 7, Items: old.Items, Meta: library.MetaKey(old.Song, "original")},
		"b": b,
	}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	r.Free = func() int64 { // short at the first check: the removals run, then there is room
		if calls.Add(1) == 1 {
			return 0
		}
		return 1 << 30
	}
	r.Reserve = func() int64 { return 1 }
	r.Start(plan.Plan{Replace: []plan.Want{w}, Remove: []*library.Entry{b}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
		t.Fatalf("old file left with no song recorded at it: %v", err)
	}
	if mm := manifest(t, root); mm.Songs["b"] != nil || mm.Songs["a"] == nil || mm.Songs["a"].Path != w.Path {
		t.Fatalf("%+v", mm.Songs)
	}
}

// Short of space after songs failed, nothing is removed (I2) and the stop says the removals were
// held back, so the user knows why it stops at the same place (re-audit 2026-11-10 #3). A failure
// the whole sync shares (a lost connection) says Resume alone can make them (Retry), outside
// Entire Library too; songs failing on their own don't.
func TestSpaceLowHeldByFailedSong(t *testing.T) {
	for _, c := range []struct {
		status map[string]int
		retry  bool
	}{{map[string]int{"a": 404, "bb": 404}, false}, {map[string]int{"a": 502}, true}} {
		spaceLowHeld(t, c.status, c.retry)
	}
}

func spaceLowHeld(t *testing.T, status map[string]int, retry bool) {
	f, r, root := setup(t)
	a, b, c, gone := want(root, "a", "original"), want(root, "bb", "original"), want(root, "c", "original"), want(root, "g", "original")
	f.status = status
	put(t, gone.Path, "old")
	ge := &library.Entry{ID: "g", Path: gone.Path, Quality: "original", Size: 3, Items: gone.Items}
	m := &library.Manifest{Songs: map[string]*library.Entry{"g": ge}}
	if err := m.Save(root, root+"/.jellyfin/manifest.json"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	r.Free = func() int64 { // room for a and bb, then none
		if calls.Add(1) <= 2 {
			return 1 << 30
		}
		return 0
	}
	r.Reserve = func() int64 { return 1 }
	r.Start(plan.Plan{Add: []plan.Want{a, b, c}, Remove: []*library.Entry{ge}})
	s := wait(t, r)
	var sl *SpaceLowError
	if !errors.As(s.Err, &sl) || !sl.Held || sl.Retry != retry {
		t.Fatalf("%v: %+v %+v", status, s, sl)
	}
	if _, err := os.Stat(gone.Path); err != nil {
		t.Fatalf("removed though a song failed: %v", err)
	}
}

// FromCache: a song it has needs no download, no room, and leaves no .part an earlier try left; a
// song it hasn't is downloaded as before.
func TestRunTakesFromCache(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	os.MkdirAll(filepath.Dir(a.Path), 0o755)
	os.WriteFile(a.Path+".part", []byte("half"), 0o644)
	os.WriteFile(a.Path+".part.key", []byte("original 1"), 0o644)
	r.FromCache = func(w plan.Want) (int64, bool) {
		if w.Song.ID != "a" {
			return 0, false
		}
		return 6, os.WriteFile(w.Path, []byte("cached"), 0o644) == nil
	}
	r.Free, r.Reserve = func() int64 { return 1 << 20 }, func() int64 { return 1 << 20 } // no room for a download
	r.Start(plan.Plan{Add: []plan.Want{a, b}})
	s := wait(t, r)
	if hits, _ := f.seen("a"); hits != 0 {
		t.Fatalf("a downloaded %d times", hits)
	}
	if !errors.Is(s.Err, ErrSpaceLow) || s.Done != 1 {
		t.Fatalf("%+v", s) // b, downloaded, stops at the reserve; a needed no room
	}
	if got, _ := os.ReadFile(a.Path); string(got) != "cached" {
		t.Fatalf("%q", got)
	}
	if e := manifest(t, root).Songs["a"]; e == nil || e.Size != 6 {
		t.Fatalf("%+v", e)
	}
	if _, err := os.Stat(a.Path + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part left")
	}
	if _, err := os.Stat(a.Path + ".part.key"); !os.IsNotExist(err) {
		t.Fatal(".part.key left")
	}
}
