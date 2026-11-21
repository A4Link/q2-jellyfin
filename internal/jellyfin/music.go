package jellyfin

import (
	"context"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Music only. A Jellyfin server also holds films, shows, audiobooks, photos and "mixed content"
// libraries, and filtering by item type is not enough: an MP3 in a mixed library is an Audio item,
// an audiobook's author is an album artist, and a playlist can hold anything. Checked against a
// live 12.4 server (contrib/testserver): such items reached Artists, Favourites, Search and
// playlists until every query was held to the user's music libraries (UserViews whose
// CollectionType is "music").
//
//   - Lists (albums, artists, favourites, search, an artist's albums) are asked of each music
//     library with parentId, which the server honours.
//   - A playlist lives outside every library, and the server ignores parentId when ids is given,
//     so its entries are kept only when their file lies under a music library's folder (the
//     folders its top-level items sit in, asked only when a playlist or song is checked). An entry
//     whose path the server doesn't show counts as music only when it belongs to an album, which
//     only a music library makes.

type musicScope struct {
	libs []string // the music libraries' ids, in the server's order
}

// musicRoots is the folders the music libraries hold, "/"-separated, without a trailing "/".
type musicRoots []string

type scopeCache struct {
	mu sync.Mutex
	s  *musicScope

	rootsMu sync.Mutex // its own lock: a list never waits for the folders
	roots   musicRoots
	scanned bool // roots is set; a scan that failed leaves it false, so the next call retries
}

// music is the user's music libraries, asked once per client: one request, before any list. None
// is ErrNoMusic, not kept, so the next call asks again (a library shared with the account later).
func (c *Client) music(ctx context.Context) (*musicScope, error) {
	c.scope.mu.Lock()
	defer c.scope.mu.Unlock()
	if c.scope.s != nil {
		return c.scope.s, nil
	}
	var views itemPage
	if err := c.call(ctx, "GET", "/UserViews", url.Values{"userId": {c.c.UserID}}, nil, &views); err != nil {
		return nil, err
	}
	s := &musicScope{}
	for _, v := range views.Items {
		if !strings.EqualFold(v.CollectionType, "music") {
			continue
		}
		s.libs = append(s.libs, v.ID)
	}
	if len(s.libs) == 0 {
		return nil, ErrNoMusic
	}
	c.scope.s = s
	return s, nil
}

// musicFolders is the music libraries' folders, asked once per client and only for what inMusic
// checks (a playlist, a song): a big library has a top-level item per artist, too many to make
// every list wait for (audit 2026-11-16 #14).
func (c *Client) musicFolders(ctx context.Context) (musicRoots, error) {
	s, err := c.music(ctx)
	if err != nil {
		return nil, err
	}
	c.scope.rootsMu.Lock()
	defer c.scope.rootsMu.Unlock()
	if c.scope.scanned {
		return c.scope.roots, nil
	}
	var roots musicRoots
	seen := map[string]bool{}
	for _, lib := range s.libs {
		// Every top-level item, paged, not just the first 200: a library can have more than one
		// content folder, and a song in a later one must not be filtered out as non-music (audit S15).
		q := url.Values{"userId": {c.c.UserID}, "parentId": {lib}, "fields": {"Path"}, "enableUserData": {"false"}}
		_, err := c.eachPage(ctx, "/Items", q, 0, func(k wireItem) {
			// the folder, cut at the last "/": path.Dir would clean a share's "//nas" to "/nas"
			p := slashPath(k.Path)
			if i := strings.LastIndexByte(p, '/'); i > 0 && !seen[p[:i]] {
				seen[p[:i]] = true
				roots = append(roots, p[:i])
			}
		})
		if err != nil {
			return nil, listErr(err)
		}
	}
	c.scope.roots, c.scope.scanned = roots, true
	return roots, nil
}

// CheckMusic asks the server again for the user's music libraries, not the ones kept: ErrNoMusic
// when it shows none. Jellyfin answers 404 for an item of a library the account no longer sees,
// as for one deleted.
func (c *Client) CheckMusic(ctx context.Context) error {
	c.scope.mu.Lock()
	c.scope.s = nil
	c.scope.mu.Unlock()
	_, err := c.music(ctx)
	return err
}

// slashPath is a server path with "/" separators (a Windows server's too), without a trailing one.
func slashPath(p string) string { return strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/") }

// inMusic reports whether w is a song of a music library (see the package comment above).
func (roots musicRoots) inMusic(w wireItem) bool {
	if w.Type != "" && w.Type != "Audio" {
		return false
	}
	p := w.Path
	if p == "" && len(w.MediaSources) > 0 {
		p = w.MediaSources[0].Path
	}
	if p = slashPath(p); p == "" {
		return w.AlbumID != ""
	}
	for _, r := range roots {
		if strings.HasPrefix(strings.ToLower(p), strings.ToLower(r)+"/") {
			return true
		}
	}
	return false
}

// eachMusic runs GET p with q once per music library (parentId), and hands each item to f once,
// in library order. /Items gets the user, fields and image types as items() gives them.
func (c *Client) eachMusic(ctx context.Context, p string, q url.Values, f func(wireItem)) error {
	s, err := c.music(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, lib := range s.libs {
		lq := maps.Clone(q)
		lq.Set("parentId", lib)
		lq.Set("userId", c.c.UserID)
		if p == "/Items" {
			lq.Set("fields", itemFields)
			lq.Set("enableImageTypes", "Primary")
		}
		var r itemPage
		if err := c.call(ctx, "GET", p, lq, nil, &r); err != nil {
			return err
		}
		for _, w := range r.Items {
			if !seen[w.ID] {
				seen[w.ID] = true
				f(w)
			}
		}
	}
	return nil
}

// songOrder is every song list's order; DateCreated last, so songs tied on the rest (two copies of
// an album) keep one order from page to page. The API has no sort by id.
const songOrder = "AlbumArtist,Album,ParentIndexNumber,IndexNumber,SortName,DateCreated"

// allMusicItems calls f once for each item of GET /Items with q in every music library, every page
// (eachPage), in library order: an item in two libraries comes once. Items tied on every sort key
// can swap between pages, one coming twice and another never, so a library that sends fewer items
// than its total is ErrChanged: a sync would otherwise delete the one missed (audit 2026-11-10 #3).
func (c *Client) allMusicItems(ctx context.Context, q url.Values, f func(wireItem)) error {
	s, err := c.music(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, lib := range s.libs {
		lq := maps.Clone(q)
		lq.Set("parentId", lib)
		lq.Set("userId", c.c.UserID)
		lq.Set("fields", itemFields)
		lq.Set("enableImageTypes", "Primary")
		got := map[string]bool{} // this library's items: fewer than its total means one was skipped
		total, err := c.eachPage(ctx, "/Items", lq, len(seen), func(w wireItem) {
			got[w.ID] = true
			if !seen[w.ID] {
				seen[w.ID] = true
				f(w)
			}
		})
		if err == nil && len(got) < total {
			err = ErrChanged
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// allMusic is every song of q in every music library, each converted as it comes so a big
// library's raw items never pile up.
func (c *Client) allMusic(ctx context.Context, q url.Values) ([]Song, error) {
	out := []Song{} // never nil: an empty list is still an answer
	err := c.allMusicItems(ctx, q, func(w wireItem) {
		if w.Type == "" || w.Type == "Audio" {
			out = append(out, w.song())
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pagedMusic is one page (offset, size) of q across the music libraries, taken in library order:
// each library's total first (limit 0), then the slice that falls in the window.
func (c *Client) pagedMusic(ctx context.Context, q url.Values, offset, size int) ([]wireItem, error) {
	s, err := c.music(ctx)
	if err != nil {
		return nil, err
	}
	var out []wireItem
	skip := offset
	for _, lib := range s.libs {
		if len(out) >= size {
			break
		}
		lq := maps.Clone(q)
		lq.Set("parentId", lib)
		if len(s.libs) > 1 { // where does this library's share of the window start?
			lq.Set("limit", "0")
			n, err := c.items(ctx, lq)
			if err != nil {
				return nil, err
			}
			if skip >= n.TotalRecordCount {
				skip -= n.TotalRecordCount
				continue
			}
		}
		lq.Set("startIndex", strconv.Itoa(skip))
		lq.Set("limit", strconv.Itoa(size-len(out)))
		r, err := c.items(ctx, lq)
		if err != nil {
			return nil, err
		}
		if !r.startsAt(skip) {
			return nil, ErrChanged
		}
		out = append(out, r.Items[:min(len(r.Items), size-len(out))]...) // no more than asked
		skip = 0
	}
	return out, nil
}
