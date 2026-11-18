package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/library"
	"github.com/A4Link/q2-jellyfin/internal/plan"
	"github.com/A4Link/q2-jellyfin/internal/q2"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// LyricsSource is a MediaSource that also gives a song's lyrics as LRC text ("" for none): a
// synced song then gets a .lrc beside it (library.LyricsPath), which Q2 Pod's player shows on Now
// Playing.
type LyricsSource interface {
	Lyrics(ctx context.Context, id string) (string, error)
}

// WriteLyrics writes song's lyrics beside path when it has any and src gives them. Failures are
// logged, and returned for a caller that asks again later: a song without its lyrics still plays.
func WriteLyrics(ctx context.Context, src any, song jellyfin.Song, path string, logf func(string, ...any)) error {
	ls, ok := src.(LyricsSource)
	if !ok || !song.HasLyrics {
		return nil
	}
	text, err := ls.Lyrics(ctx, song.ID)
	if err == nil && text != "" {
		err = q2.WriteFile(library.LyricsPath(path), []byte(text))
	}
	if err != nil && logf != nil {
		logf("lyrics %s: %v", song.ID, err)
	}
	return err
}

// StopReason tells a failure the whole sync shares from one of this song's own (an HTTP error, or
// jellyfin.CodeGone, not found): ErrOffline (no answer, or a proxy's answer that the
// server is down, wrapping err), ErrCardFull, ErrRefused (wrapping err) for an answer about the
// account: the sign-in (40-44) or downloads turned off for it (50, R39), or err itself for any other
// card error (read-only, EIO). nil for the song's own.
func StopReason(err error) error {
	var ae *jellyfin.APIError
	var ne net.Error
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return ErrCardFull
	case errors.As(err, &pe) || errors.As(err, &le): // before net.Error: an errno is one
		return err
	case errors.Is(err, jellyfin.ErrAuth) || errors.As(err, &ae) && ae.Code >= 40 && ae.Code <= 50:
		return fmt.Errorf("%w: %w", ErrRefused, err)
	case errors.As(err, &ae):
		if ae.HTTP == 0 || serverDown(ae.HTTP) {
			return fmt.Errorf("%w: %w", ErrOffline, err)
		}
		return nil
	case errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF):
		return ErrOffline
	}
	return nil
}

// serverDown reports an HTTP status a proxy or CDN in front of the server answers for every
// request while the server is down or overloaded (Caddy's 502, Cloudflare's 520-527, 503, 504), or
// the server's own 429 (too many transcodes).
func serverDown(status int) bool {
	return status == 429 || status >= 502 && status <= 504 || status >= 520 && status <= 527
}

// fetchTries fetches w, one try per Backoff entry, waiting that long after each failed try but
// the last. A song the server says isn't there (jellyfin.CodeGone) gets one try.
func (r *Runner) fetchTries(ctx context.Context, w plan.Want) (int64, error) {
	var err error
	for i, d := range r.Backoff {
		var n int64
		r.op("try", w.Path)
		if n, err = r.fetch(ctx, w); err == nil {
			return n, nil
		}
		var ae *jellyfin.APIError
		if ctx.Err() != nil || !r.CardMounted() || errors.Is(StopReason(err), ErrRefused) || errors.As(err, &ae) && ae.Code == jellyfin.CodeGone {
			return 0, err // a refusal for the account, or a song not there, is the same on every try
		}
		r.logf("sync: %s try %d: %s", w.Song.ID, i+1, err)
		if i == len(r.Backoff)-1 {
			break
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, err
}

// fetch downloads w to <path>.part, resuming an original with Range when the server answers
// 206 at the right offset, then fsyncs and renames it into place. It returns the file's size.
func (r *Runner) fetch(ctx context.Context, w plan.Want) (int64, error) {
	if err := owned(r.root, w.Path); err != nil {
		return 0, err
	}
	part := w.Path + ".part"
	// The .part's key names the version it holds (quality, server size): another version's bytes
	// are never continued into a spliced song (M14). A .part with no key is an older build's.
	key, stamp := part+".key", fmt.Sprintf("%s %d", w.Quality, w.Bytes)
	r.mu.Lock()
	r.part = part // Cancel deletes it (and its key)
	r.mu.Unlock()
	var from int64
	// A .part already at the planned size (cut between fsync and rename, or an older, longer
	// version) is fetched again whole.
	if st, err := os.Stat(part); err == nil && w.Quality == "original" && (w.Bytes <= 0 || st.Size() < w.Bytes) {
		if b, err := os.ReadFile(key); errors.Is(err, os.ErrNotExist) || err == nil && string(b) == stamp {
			from = st.Size()
		}
	}
	u := r.src.MediaURL(w.Song.ID, w.Quality)
	resp, err := r.src.Get(ctx, u, from)
	var ae *jellyfin.APIError
	if from > 0 && errors.As(err, &ae) && ae.HTTP == http.StatusRequestedRangeNotSatisfiable {
		from = 0
		resp, err = r.src.Get(ctx, u, 0)
	}
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if resp.StatusCode == http.StatusPartialContent {
		if from == 0 || !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", from)) {
			library.Remove(r.root, part)
			return 0, fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
		}
		flag = os.O_WRONLY | os.O_APPEND
	} else if from > 0 {
		from = 0 // 200: the server ignored Range, start over
	}
	// The folder and key are made only once the server answered, so a song it refuses leaves
	// nothing. The old .part goes first: a new key never names another version's bytes (M14).
	if from == 0 {
		library.Remove(r.root, part)
		if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
			return 0, err
		}
		if err := q2.WriteFile(key, []byte(stamp)); err != nil {
			return 0, err
		}
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.cur, r.curMax = from, w.Bytes
	r.mu.Unlock()
	limit := Limit(resp.ContentLength, w.Bytes)
	n, err := r.copy(f, io.LimitReader(resp.Body, limit+1), from)
	if err == nil {
		err = SizeErr(n-from, resp.ContentLength, limit) // checked before the rename (I8)
	}
	switch {
	case err != nil || resp.ContentLength >= 0:
	case !quality.IsOriginal(w.Quality):
		err = OggCut(part) // a transcode has no length to fall short of
	case w.Bytes > 0 && n != w.Bytes: // an original's length dropped on the way (a proxy's gzip)
		err = fmt.Errorf("the server sent %d of %d bytes", n, w.Bytes)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, w.Path)
	}
	if err != nil {
		if w.Quality != "original" { // a transcode is never resumed: its bytes are junk
			library.Remove(r.root, part)
			library.Remove(r.root, key)
		}
		return 0, err
	}
	library.Remove(r.root, key)
	r.mu.Lock()
	r.part = ""
	r.mu.Unlock()
	return n, nil
}

// Limit is the most an answer's body may hold: twice the size expected (want, an estimate for a
// transcode) and 8 MiB, so a server can't fill the card past what was checked, whatever
// Content-Length it sends (M9); else its Content-Length; no limit when neither is known.
func Limit(contentLength, want int64) int64 {
	switch {
	case want > 0:
		return 2*want + 8<<20
	case contentLength >= 0:
		return contentLength
	}
	return 1<<63 - 2
}

// SizeErr refuses a body of n bytes that is empty, short of its Content-Length (when one came) or
// past limit: it is no song (I8, M9). A server's own Size isn't the measure: a transcode differs.
func SizeErr(n, contentLength, limit int64) error {
	switch {
	case n == 0:
		return errors.New("the server sent an empty file")
	case n > limit:
		return fmt.Errorf("the server sent more than the %d bytes expected", limit)
	case contentLength >= 0 && n < contentLength:
		return fmt.Errorf("the server sent %d of %d bytes", n, contentLength)
	}
	return nil
}

// OggCut refuses an Ogg file (an Opus transcode) at path that doesn't end with a whole
// end-of-stream page: a server whose ffmpeg stops part-way, or that closes the connection, ends
// its answer cleanly, short. A file that isn't Ogg is not checked. An Ogg page is at most 65307
// bytes, so the last one starts within the last 64 KiB.
func OggCut(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != "OggS" {
		return nil
	}
	off := max(fi.Size()-64<<10, 0)
	tail := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil {
		return err
	}
	if p := tail[max(bytes.LastIndex(tail, []byte("OggS\x00")), 0):]; len(p) >= 27 && p[5]&4 != 0 {
		n := 27 + int(p[26]) // the header, its segment table, then the segments it lists
		if len(p) >= n {
			for _, s := range p[27:n] {
				n += int(s)
			}
		}
		if n == len(p) {
			return nil
		}
	}
	return errors.New("the server stopped before the end of the song")
}

// copy writes body to f, counting progress; n starts at from (the bytes already in the .part).
func (r *Runner) copy(f *os.File, body io.Reader, n int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for {
		k, rerr := body.Read(buf)
		if k > 0 {
			if _, err := f.Write(buf[:k]); err != nil {
				return n, err
			}
			n += int64(k)
			r.mu.Lock()
			r.cur = n
			r.meter.add(r.Now(), int64(k))
			r.mu.Unlock()
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// cover writes cover.jpg into w's album folder when it is missing. A failure is logged but does
// not fail the song.
func (r *Runner) cover(ctx context.Context, w plan.Want) {
	c := filepath.Join(filepath.Dir(w.Path), "cover.jpg")
	if w.Song.CoverArt == "" {
		return
	}
	if _, err := os.Stat(c); err == nil {
		return
	}
	err := func() error {
		resp, err := r.src.Get(ctx, sized(r.src.CoverURL(w.Song.CoverArt)), 0)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.ContentLength > maxCover {
			return errBigCover
		}
		return writeCover(c, resp.Body) // streamed: a server that can't resize sends the original, many megabytes
	}()
	if err != nil && ctx.Err() == nil {
		r.logf("sync: cover %s failed: %s", w.Song.ID, applog.Redact(err.Error())) // logged only: no page lists a cover
	}
}

const maxCover = 16 << 20

var errBigCover = errors.New("cover larger than 16 MiB")

// writeCover streams r to the cover.jpg at path, refusing one past maxCover: WriteWith then
// removes the cut file.
func writeCover(path string, r io.Reader) error {
	return q2.WriteWith(path, func(w io.Writer) error {
		n, err := io.Copy(w, io.LimitReader(r, maxCover+1))
		if err == nil && n > maxCover {
			err = errBigCover
		}
		return err
	})
}

// sized asks the image endpoint for at most 600 px: the original can be many megabytes (M19),
// which the plan's space estimate doesn't count.
// ponytail: here, as jellyfin.Client.CoverURL takes no size.
func sized(u string) string {
	p, err := url.Parse(u)
	if err != nil || u == "" {
		return u
	}
	q := p.Query()
	q.Set("maxWidth", "600")
	q.Set("maxHeight", "600")
	p.RawQuery = q.Encode()
	return p.String()
}
