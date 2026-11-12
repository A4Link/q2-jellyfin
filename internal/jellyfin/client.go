// Package jellyfin is a small Jellyfin client over HTTPS only. It signs in once (password or Quick
// Connect) and keeps only the access token; every request carries it in the Authorization header,
// never in a URL, so URLs and logs hold no secret.
//
// Endpoints follow the Jellyfin 10.9+ REST API. TODO(verify): check each path and query name against
// your server's OpenAPI document (https://<server>/api-docs/openapi.json) before the first release.
package jellyfin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/applog"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// ClientName and Version identify the app to the server (its Devices page and its logs).
var (
	ClientName = "Q2 Jellyfin"
	DeviceName = "Shanling Q2"
	Version    = "0.1.0"
)

// Creds is a sign-in. Before SignIn it may hold a password; what SignIn returns, and what the card
// keeps, holds the access token instead and never the password.
type Creds struct {
	Server, User string
	Password     string `json:",omitempty"` // only until SignIn
	Token        string `json:",omitempty"` // Jellyfin access token, or an API key when Kind is KindAPIKey
	UserID       string `json:",omitempty"`
	DeviceID     string `json:",omitempty"` // random per sign-in, kept with it; the server lists sessions by it
	Kind         string `json:",omitempty"` // "" a user's access token; KindAPIKey a server API key
}

// KindAPIKey marks Creds whose Token is a server API key. Jellyfin's API keys are made by an admin
// in the Dashboard and belong to no user: they reach every library on the server, so the app says
// so wherever one is used or stored.
const KindAPIKey = "apikey"

type Client struct {
	c    Creds
	base *url.URL
	hc   *http.Client
	// Log, when set, receives one redacted line per request.
	Log   *applog.Log
	idle  time.Duration // a body Read that waits longer fails; 0 is idleAfter
	scope scopeCache    // the user's music libraries (music.go)
	last  lastResult    // the latest request's outcome (Last)
}

// lastResult is the latest request's outcome: when, and the error if the server couldn't be
// reached (nil once it answered at all).
type lastResult struct {
	mu  sync.Mutex
	at  time.Time
	err error
}

// Last is when the latest request to the server ended, and its error when the server couldn't be
// reached (any answer, even an HTTP error, counts as reached); a zero time before the first. The
// menu shows Connected from it instead of pinging the server on a timer (audit B9).
func (c *Client) Last() (time.Time, error) {
	c.last.mu.Lock()
	defer c.last.mu.Unlock()
	return c.last.at, c.last.err
}

func (c *Client) setLast(err error) {
	c.last.mu.Lock()
	c.last.at, c.last.err = time.Now(), err
	c.last.mu.Unlock()
}

// New refuses anything but an https server address.
func New(c Creds, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.Server), "/"))
	if err != nil || u.Host == "" {
		return nil, &APIError{Op: "new", Msg: "bad server address", kind: ErrUnreachable}
	}
	if u.Scheme != "https" {
		return nil, ErrInsecure
	}
	if c.DeviceID == "" {
		b := make([]byte, 16)
		rand.Read(b)
		c.DeviceID = hex.EncodeToString(b)
	}
	h := *hc
	h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Referer")
		if req.URL.Scheme != "https" {
			return ErrInsecure
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization") // never hand the token to another host
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &Client{c: c, base: u, hc: &h}, nil
}

// authHeader is Jellyfin's client identification, with the token once signed in.
func (c *Client) authHeader() string {
	q := func(s string) string { return strings.ReplaceAll(s, `"`, "") }
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		q(ClientName), q(DeviceName), q(c.c.DeviceID), q(Version))
	if c.c.Token != "" {
		h += fmt.Sprintf(`, Token="%s"`, q(c.c.Token))
	}
	return h
}

// endpoint is the server URL for p (beginning with /) and q.
func (c *Client) endpoint(p string, q url.Values) string {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + p
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		c.Log.Printf(format, a...) // Printf redacts every line
	}
}

const (
	// maxBody caps an API answer: decoding takes about twice its size, and the Q2 has about 60 MB.
	maxBody = 8 << 20
	// idleAfter fails a body Read that gets no data for this long (a hung transcode or NAS).
	idleAfter = 30 * time.Second
)

// do sends one request and logs it. Its body fails with ErrStalled once one Read waits longer than
// the idle time, so a stalled server can't hang a download.
func (c *Client) do(ctx context.Context, method, op, u string, body []byte, from int64) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		cancel()
		return nil, &APIError{Op: op, Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	req.Header.Set("Authorization", c.authHeader())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// Only the app cancelling (a newer request, the user leaving) is giving up; a deadline that
		// ran out is a server that didn't answer in time. Read before cancel(), which ends ctx.
		gaveUp := errors.Is(ctx.Err(), context.Canceled)
		cancel()
		c.logf("%s %s -> %v", method, u, err)
		ae := &APIError{Op: op, Msg: applog.Redact(err.Error()), kind: transportKind(err)}
		// Not the app giving up: the server really couldn't be reached. A cover's or lyrics' failure
		// doesn't decide it: they run on a short budget of their own, which a slow link misses while
		// the lists themselves load fine.
		if !gaveUp && op != "Primary" && op != "Lyrics" {
			c.setLast(ae)
		}
		return nil, ae
	}
	if code := resp.StatusCode; code == http.StatusUnauthorized || code >= 502 && code <= 504 {
		c.setLast(statusError(op, code)) // the sign-in refused, or a proxy whose Jellyfin is down
	} else {
		c.setLast(nil)
	}
	if resp.StatusCode/100 != 2 || (op != "Primary" && op != "Lyrics") { // a cover or lyrics that came: no line (audit B6)
		c.logf("%s %s -> %d", method, u, resp.StatusCode)
	}
	idle := c.idle
	if idle == 0 {
		idle = idleAfter
	}
	b := &idleBody{ReadCloser: resp.Body, op: op, idle: idle, cancel: cancel}
	b.t = time.AfterFunc(idle, func() { b.fired.Store(true); cancel() })
	b.t.Stop()
	resp.Body = b
	return resp, nil
}

// idleBody is a response body whose Reads each get idle to see data; then the request is cancelled.
type idleBody struct {
	io.ReadCloser
	op     string
	idle   time.Duration
	t      *time.Timer
	fired  atomic.Bool
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.t.Reset(b.idle)
	n, err := b.ReadCloser.Read(p)
	b.t.Stop()
	if err != nil && b.fired.Load() {
		err = &APIError{Op: b.op, Msg: fmt.Sprintf("no data for %v", b.idle), kind: ErrStalled}
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.t.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

// call performs one JSON request: in (when not nil) is the body, out (when not nil) receives the
// answer. Any status but 2xx is an *APIError.
func (c *Client) call(ctx context.Context, method, p string, q url.Values, in, out any) error {
	op := opName(p)
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	resp, err := c.do(ctx, method, op, c.endpoint(p, q), body, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			return err
		}
		return &APIError{Op: op, HTTP: resp.StatusCode, Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(op, resp.StatusCode)
	}
	if len(data) > maxBody {
		return &APIError{Op: op, HTTP: resp.StatusCode, Msg: "Answer too large for the Q2"}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &APIError{Op: op, HTTP: resp.StatusCode, Msg: "not a Jellyfin response", kind: ErrNotJellyfin}
		}
	}
	return nil
}

// opName names a request for logs and the Details row: the first path segment that isn't an id
// ("Items", "Playlists", "Users"), or the last for media ("Download", "universal", "stream").
func opName(p string) string {
	switch b := path.Base(p); b {
	case "Download", "universal", "stream", "Primary", "Lyrics", "AuthenticateByName", "AuthenticateWithQuickConnect", "Initiate", "Connect", "Enabled", "Me", "Public":
		return b
	}
	seg := strings.Split(strings.Trim(p, "/"), "/")
	return seg[0]
}

// ---- sign-in ----

type authResult struct {
	AccessToken string `json:"AccessToken"`
	User        struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	} `json:"User"`
}

func (c *Client) keep(r authResult) Creds {
	return Creds{Server: c.c.Server, User: r.User.Name, Token: r.AccessToken, UserID: r.User.ID, DeviceID: c.c.DeviceID}
}

// SignIn checks the sign-in and returns what to keep on the card: with a token already, the token
// is checked (GET /Users/Me); else the password is exchanged for a token (POST
// /Users/AuthenticateByName) and dropped. The server must be Jellyfin (GET /System/Info/Public).
func (c *Client) SignIn(ctx context.Context) (Creds, error) {
	if err := c.checkServer(ctx); err != nil {
		return Creds{}, err
	}
	if c.c.Token != "" {
		if err := c.Ping(ctx); err != nil {
			return Creds{}, err
		}
		return c.c, nil
	}
	var r authResult
	in := map[string]string{"Username": c.c.User, "Pw": c.c.Password}
	if err := c.call(ctx, "POST", "/Users/AuthenticateByName", nil, in, &r); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.HTTP == 400 || ae.HTTP == 401) {
			ae.kind = ErrAuth
		}
		return Creds{}, err
	}
	if r.AccessToken == "" {
		return Creds{}, &APIError{Op: "AuthenticateByName", Msg: "no access token in the answer", kind: ErrNotJellyfin}
	}
	return c.keep(r), nil
}

// SignInWithKey signs in with a server API key (in Creds.Token) for the user named user, whose
// library the app then shows. The key is checked by listing the server's users (GET /Users, which
// only an admin-level token may do) and picking that user by name. What it returns keeps the key:
// a Jellyfin API key can't be swapped for a narrower token.
// TODO(verify): confirm GET /Users answers an API key on the Jellyfin versions you support.
func (c *Client) SignInWithKey(ctx context.Context, user string) (Creds, error) {
	if err := c.checkServer(ctx); err != nil {
		return Creds{}, err
	}
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := c.call(ctx, "GET", "/Users", nil, nil, &users); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.HTTP == 401 || ae.HTTP == 403) {
			ae.kind = ErrBadKey
		}
		return Creds{}, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Name, strings.TrimSpace(user)) {
			return Creds{Server: c.c.Server, User: u.Name, Token: c.c.Token, UserID: u.ID, DeviceID: c.c.DeviceID, Kind: KindAPIKey}, nil
		}
	}
	return Creds{}, &APIError{Op: "Users", Msg: "No user named " + strings.TrimSpace(user) + " on the server", kind: ErrNoSuchUser}
}

// CheckServer reports ErrNotJellyfin (or why it can't tell) for a server whose public info doesn't
// name Jellyfin. It sends no credentials.
func (c *Client) CheckServer(ctx context.Context) error { return c.checkServer(ctx) }

// checkServer reports ErrNotJellyfin for a server whose public info doesn't name Jellyfin.
func (c *Client) checkServer(ctx context.Context) error {
	var info struct {
		ProductName string `json:"ProductName"`
		Version     string `json:"Version"`
	}
	if err := c.call(ctx, "GET", "/System/Info/Public", nil, nil, &info); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.HTTP == 404 {
			ae.kind = ErrNotJellyfin
		}
		return err
	}
	if !strings.Contains(strings.ToLower(info.ProductName), "jellyfin") {
		return &APIError{Op: "Public", Msg: "not a Jellyfin server", kind: ErrNotJellyfin}
	}
	return nil
}

// KeyAuth reports whether the client signs in with a server API key (admin-level) rather than a
// user's own access token.
func (c *Client) KeyAuth() bool { return c.c.Kind == KindAPIKey }

// Ping checks the stored token: a user's token with GET /Users/Me; an API key, which belongs to no
// user, with GET /Users/{id}.
func (c *Client) Ping(ctx context.Context) error {
	if c.KeyAuth() {
		return c.call(ctx, "GET", "/Users/"+url.PathEscape(c.c.UserID), nil, nil, nil)
	}
	return c.call(ctx, "GET", "/Users/Me", nil, nil, nil)
}

// ---- library ----

// items runs GET /Items for the signed-in user with q added.
func (c *Client) items(ctx context.Context, q url.Values) (itemPage, error) {
	q.Set("userId", c.c.UserID)
	q.Set("fields", itemFields)
	q.Set("enableImageTypes", "Primary")
	var r itemPage
	err := c.call(ctx, "GET", "/Items", q, nil, &r)
	return r, err
}

// item is GET /Items/{id} for the signed-in user, which must be of type kind: anything else (the
// all-zero id is the server's root folder, for one) is an item that isn't there.
func (c *Client) item(ctx context.Context, id, kind string) (wireItem, error) {
	var w wireItem
	// No fields parameter here (the document has none): a single item carries its path and media
	// sources anyway.
	if err := c.call(ctx, "GET", "/Items/"+url.PathEscape(id), url.Values{"userId": {c.c.UserID}}, nil, &w); err != nil {
		return w, err
	}
	if w.Type != kind {
		return w, &APIError{Op: "Items", HTTP: 404, Code: CodeGone, Msg: "Not a " + strings.ToLower(kind)}
	}
	return w, nil
}

// Playlists are the user's music playlists in order (a key of orders["playlists"]), turned round
// by reverse, sorted by the server.
func (c *Client) Playlists(ctx context.Context, order string, reverse bool) ([]Playlist, error) {
	q, err := sortQuery(url.Values{"includeItemTypes": {"Playlist"}, "recursive": {"true"}}, "playlists", order, reverse)
	if err != nil {
		return nil, err
	}
	r, err := c.items(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []Playlist
	for _, w := range r.Items {
		if w.MediaType == "" || w.MediaType == "Audio" { // video playlists stay out
			out = append(out, w.playlist())
		}
	}
	return out, nil
}

// Playlist returns the playlist's songs in order, duplicates included.
func (c *Client) Playlist(ctx context.Context, id string) (Playlist, []Song, error) {
	w, err := c.item(ctx, id, "Playlist")
	if err != nil {
		return Playlist{}, nil, err
	}
	s, err := c.music(ctx)
	if err != nil {
		return Playlist{}, nil, err
	}
	kept := []Song{} // music only, in the playlist's order, duplicates included
	// With the user's data: a song's heart (Favorite) shows in Now Playing.
	q := url.Values{"userId": {c.c.UserID}, "fields": {itemFields}, "enableUserData": {"true"}}
	_, err = c.eachPage(ctx, "/Playlists/"+url.PathEscape(id)+"/Items", q, func(e wireItem) {
		if s.inMusic(e) {
			kept = append(kept, e.song())
		}
	})
	if err != nil {
		return Playlist{}, nil, err
	}
	p := w.playlist()
	p.SongCount = len(kept)
	return p, kept, nil
}

// orders are the orders each list can come in from the server, by key: its sortBy, then the
// directions of each, natural (names A to Z, dates newest first, longest first) and turned round.
// A name breaking a tie stays A to Z either way, unless the name is the order.
var orders = map[string]map[string]struct{ by, natural, turned string }{
	"albums": {
		"name":        {"SortName", "Ascending", "Descending"},
		"released":    {"ProductionYear,PremiereDate,SortName", "Descending,Descending,Ascending", "Ascending,Ascending,Ascending"},
		"added":       {"DateCreated,SortName", "Descending,Ascending", "Ascending,Ascending"},
		"albumartist": {"AlbumArtist,SortName", "Ascending,Ascending", "Descending,Ascending"},
	},
	"artists": {
		"name":  {"SortName", "Ascending", "Descending"},
		"added": {"DateCreated,SortName", "Descending,Ascending", "Ascending,Ascending"},
	},
	"playlists": {
		"name":     {"SortName", "Ascending", "Descending"},
		"added":    {"DateCreated,SortName", "Descending,Ascending", "Ascending,Ascending"},
		"duration": {"Runtime,SortName", "Descending,Ascending", "Ascending,Ascending"},
	},
}

// sortQuery is q with list's order key, turned round by reverse. An order the server can't sort by
// is an error, so a choice that would quietly list by name fails instead.
func sortQuery(q url.Values, list, key string, reverse bool) (url.Values, error) {
	o, ok := orders[list][key]
	if !ok {
		return nil, fmt.Errorf("no %s order %q", list, key)
	}
	q.Set("sortBy", o.by)
	q.Set("sortOrder", map[bool]string{false: o.natural, true: o.turned}[reverse])
	return q, nil
}

// Albums is one page of the music libraries' albums in order (a key of orders["albums"]), turned
// round by reverse, sorted by the server.
// shortcut: with several music libraries each is sorted on its own and they follow one another;
// one list across them needs a query without the library (known limitation, Acceptance Criteria
// L7).
func (c *Client) Albums(ctx context.Context, offset, size int, order string, reverse bool) ([]Album, error) {
	q, err := sortQuery(url.Values{"includeItemTypes": {"MusicAlbum"}, "recursive": {"true"}}, "albums", order, reverse)
	if err != nil {
		return nil, err
	}
	items, err := c.pagedMusic(ctx, q, offset, size)
	if err != nil {
		return nil, err
	}
	return albums(items), nil
}

func (c *Client) Album(ctx context.Context, id string) (Album, []Song, error) {
	w, err := c.item(ctx, id, "MusicAlbum")
	if err != nil {
		return Album{}, nil, err
	}
	r, err := c.items(ctx, url.Values{"parentId": {id}, "includeItemTypes": {"Audio"}, "recursive": {"true"},
		"sortBy": {"ParentIndexNumber,IndexNumber,SortName"}})
	if err != nil {
		return Album{}, nil, err
	}
	a := w.album()
	if a.SongCount == 0 {
		a.SongCount = len(r.Items)
	}
	return a, songs(r.Items), nil
}

// Artists lists album artists, as Jellyfin's music library does.
// /Artists/AlbumArtists is marked deprecated in the 12.4.0 OpenAPI document, which names no
// replacement: /Artists has no album-artist filter, and /Items?includeItemTypes=MusicArtist also
// lists artists who only feature on songs. Keep it until the server offers a successor.
// order is a key of orders["artists"], turned round by reverse, sorted by the server.
func (c *Client) Artists(ctx context.Context, order string, reverse bool) ([]Artist, error) {
	base, err := sortQuery(url.Values{}, "artists", order, reverse)
	if err != nil {
		return nil, err
	}
	s, err := c.music(ctx)
	if err != nil {
		return nil, err
	}
	var out []Artist
	seen := map[string]bool{} // an artist in two music libraries is listed once
	for _, lib := range s.libs {
		q := url.Values{"parentId": {lib}, "userId": {c.c.UserID}, "fields": {artistFields},
			"sortBy": base["sortBy"], "sortOrder": base["sortOrder"]}
		_, err := c.eachPage(ctx, "/Artists/AlbumArtists", q, func(w wireItem) {
			if !seen[w.ID] {
				seen[w.ID] = true
				out = append(out, w.artist())
			}
		})
		if err != nil {
			return nil, err
		}
	}
	if order == "name" && !reverse { // several libraries' artists as one list by name; any other
		sortByName(out, func(a Artist) string { return a.Name }, len(s.libs)) // order: library by library
	}
	return out, nil
}

func (c *Client) Artist(ctx context.Context, id string) (Artist, []Album, error) {
	w, err := c.item(ctx, id, "MusicArtist")
	if err != nil {
		return Artist{}, nil, err
	}
	s, err := c.music(ctx)
	if err != nil {
		return Artist{}, nil, err
	}
	// Paged (listPage a request): "Various Artists" in a big library has thousands of albums, more
	// than one answer may hold (maxBody; audit 2026-11-10 #4). An album in two libraries comes once.
	// As for songs (allMusic), DateCreated breaks ties and an album missed between pages is
	// ErrChanged: an artist in Sync would otherwise have that album's songs deleted.
	var als []wireItem
	seen := map[string]bool{}
	for _, lib := range s.libs {
		q := url.Values{"albumArtistIds": {id}, "includeItemTypes": {"MusicAlbum"}, "recursive": {"true"},
			"sortBy": {"ProductionYear,SortName,DateCreated"}, "parentId": {lib}, "userId": {c.c.UserID},
			"fields": {itemFields}, "enableImageTypes": {"Primary"}}
		got := map[string]bool{}
		total, err := c.eachPage(ctx, "/Items", q, func(x wireItem) {
			got[x.ID] = true
			if !seen[x.ID] {
				seen[x.ID] = true
				als = append(als, x)
			}
		})
		if err == nil && len(got) < total {
			err = ErrChanged
		}
		if err != nil {
			return Artist{}, nil, err
		}
	}
	a := w.artist()
	if a.AlbumCount == 0 {
		a.AlbumCount = len(als)
	}
	return a, albums(als), nil
}

// Search finds up to 50 of each kind starting at offset.
func (c *Client) Search(ctx context.Context, q string, offset int) ([]Artist, []Album, []Song, error) {
	page := func(kind string) ([]wireItem, error) { // offset and limit per music library
		var out []wireItem
		err := c.eachMusic(ctx, "/Items", url.Values{"searchTerm": {q}, "includeItemTypes": {kind}, "recursive": {"true"},
			"startIndex": {strconv.Itoa(offset)}, "limit": {"50"}}, func(w wireItem) { out = append(out, w) })
		return out, err
	}
	// Artists through /Artists/AlbumArtists: artist items belong to the whole server, and /Items
	// ignores parentId for them (a mixed library's "Memo Person" came back); this one honours it.
	var ar []wireItem
	err := c.eachMusic(ctx, "/Artists/AlbumArtists", url.Values{"searchTerm": {q}, "startIndex": {strconv.Itoa(offset)},
		"limit": {"50"}, "fields": {artistFields}}, func(w wireItem) { ar = append(ar, w) })
	if err != nil {
		return nil, nil, nil, err
	}
	al, err := page("MusicAlbum")
	if err != nil {
		return nil, nil, nil, err
	}
	so, err := page("Audio")
	if err != nil {
		return nil, nil, nil, err
	}
	return artists(ar), albums(al), songs(so), nil
}

// listPage is how many items one request of a long list asks for: a whole big playlist or every
// album artist in one answer can pass maxBody.
const listPage = 500

// eachPage calls f for every item of GET p with q, listPage at a time, in order, and returns the
// server's total.
func (c *Client) eachPage(ctx context.Context, p string, q url.Values, f func(wireItem)) (int, error) {
	if q.Get("enableUserData") == "" {
		q.Set("enableUserData", "false") // nothing reads it
	}
	total := -1
	for start := 0; ; start += listPage {
		q.Set("startIndex", strconv.Itoa(start))
		q.Set("limit", strconv.Itoa(listPage))
		var r itemPage
		if err := c.call(ctx, "GET", p, q, nil, &r); err != nil {
			return 0, err
		}
		if total >= 0 && r.TotalRecordCount != total {
			return 0, ErrChanged // items shifted between pages: one may have been skipped
		}
		total = r.TotalRecordCount
		for _, w := range r.Items {
			f(w)
		}
		if len(r.Items) < listPage || start+len(r.Items) >= r.TotalRecordCount {
			return total, nil
		}
	}
}

// Favorites is the user's favourite songs (Jellyfin's heart), every page, by album artist, album
// and track, as a music library lists them.
func (c *Client) Favorites(ctx context.Context) ([]Song, error) {
	return c.allMusic(ctx, url.Values{"isFavorite": {"true"}, "includeItemTypes": {"Audio"}, "recursive": {"true"},
		"sortBy": {songOrder}}, listPage)
}

func (c *Client) Song(ctx context.Context, id string) (Song, error) {
	w, err := c.item(ctx, id, "Audio")
	if err != nil {
		return Song{}, err
	}
	if s, err := c.music(ctx); err != nil {
		return Song{}, err
	} else if !s.inMusic(w) {
		return Song{}, &APIError{Op: "Items", HTTP: 404, Code: CodeGone, Msg: "Not in a music library"}
	}
	return w.song(), nil
}

// ---- media ----

// MediaURL is the sync URL: the server's file as it is for quality.Original (no transcoding; needs
// the account's download permission), else the server's transcode (quality.Parse: Opus, or MP3 as
// the old-firmware fallback) at its bitrate.
func (c *Client) MediaURL(id, q string) string {
	if quality.IsOriginal(q) {
		return c.endpoint("/Items/"+url.PathEscape(id)+"/Download", nil)
	}
	return c.transcodeURL(id, q)
}

// PlayURL is MediaURL for playback: an original is streamed untouched (static=true), which needs
// no download permission.
func (c *Client) PlayURL(id, q string) string {
	if quality.IsOriginal(q) {
		return c.endpoint("/Audio/"+url.PathEscape(id)+"/stream", url.Values{"static": {"true"}})
	}
	return c.transcodeURL(id, q)
}

// transcodeURL asks /Audio/{id}/universal for the song in q's codec at q's bitrate. container is
// what the client takes as it is: a source already in that codec within the bitrate is sent
// untouched; anything else the server transcodes with ffmpeg into transcodingContainer.
// TODO(verify): on a live server, that Opus arrives as Ogg Opus the Q2 plays (container "opus",
// transcodingContainer "ogg"), and that transcoded files keep their tags, which the Q2's library
// reads for Artists and Albums.
func (c *Client) transcodeURL(id, q string) string {
	codec, _ := quality.Parse(q)
	container, muxer := "opus", "ogg"
	if codec == "mp3" {
		container, muxer = "mp3", "mp3"
	}
	rate := strconv.FormatInt(quality.Bitrate(q), 10)
	if rate == "0" {
		rate = strconv.FormatInt(quality.Bitrate(quality.Fallback), 10)
	}
	return c.endpoint("/Audio/"+url.PathEscape(id)+"/universal", url.Values{
		"userId": {c.c.UserID}, "deviceId": {c.c.DeviceID}, "container": {container}, "audioCodec": {cmp.Or(codec, "opus")},
		"transcodingContainer": {muxer}, "transcodingProtocol": {"http"}, "maxStreamingBitrate": {rate},
		"audioBitRate": {rate}})
}

// PlayerURL is PlayURL for Q2 Pod's own player, which sends no headers: the token rides in the
// query (ApiKey). The URL is a secret: it goes only into the firmware's queue file in /tmp.
func (c *Client) PlayerURL(id, q string) string { return c.withKey(c.PlayURL(id, q)) }

// PlayerCoverURL is id's cover for Now Playing, fetched by the firmware without headers.
func (c *Client) PlayerCoverURL(id string) string { return c.withKey(c.ThumbURL(id, 320)) }

func (c *Client) withKey(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := pu.Query()
	q.Set("ApiKey", c.c.Token)
	pu.RawQuery = q.Encode()
	return pu.String()
}

// ThumbURL is id's primary image cropped to a px square JPEG, for a list row's cover.
func (c *Client) ThumbURL(id string, px int) string {
	s := strconv.Itoa(px)
	return c.endpoint("/Items/"+url.PathEscape(id)+"/Images/Primary", url.Values{"fillWidth": {s}, "fillHeight": {s},
		"format": {"Jpg"}, "quality": {"85"}})
}

func (c *Client) CoverURL(id string) string {
	return c.endpoint("/Items/"+url.PathEscape(id)+"/Images/Primary", url.Values{"maxWidth": {"500"}, "quality": {"90"}})
}

// Get fetches u (one of the URLs above), resuming at byte from when from > 0. The caller closes the
// body. A JSON or text answer (an error, a proxy's login page) is never saved as a song or a cover.
func (c *Client) Get(ctx context.Context, u string, from int64) (*http.Response, error) {
	op := "get"
	if pu, err := url.Parse(u); err == nil && pu.Path != "" {
		op = opName(pu.Path)
	}
	resp, err := c.do(ctx, "GET", op, u, nil, from)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, statusError(op, resp.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/json" || mt == "application/problem+json" ||
		strings.HasPrefix(mt, "text/") {
		resp.Body.Close()
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: "The server sent an error instead of the file"}
	}
	return resp, nil
}

func transportKind(err error) error {
	switch {
	case errors.Is(err, ErrInsecure):
		return ErrInsecure
	case isCert(err):
		var ci x509.CertificateInvalidError
		if errors.As(err, &ci) && ci.Reason == x509.Expired && !timeKnown() {
			return ErrUnreachable // the clock is unset and the server's time couldn't be read
		}
		return ErrCert
	}
	return ErrUnreachable
}

// Logout ends the signed-in session on the server (POST /Sessions/Logout), so its token stops working.
func (c *Client) Logout(ctx context.Context) error {
	return c.call(ctx, "POST", "/Sessions/Logout", nil, nil, nil)
}
