package jellyfin

import (
	"context"
	"net/url"
	"path"
	"sort"
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
//     folders its top-level items sit in). An entry whose path the server doesn't show counts as
//     music only when it belongs to an album, which only a music library makes.

type musicScope struct {
	libs  []string // the music libraries' ids, in the server's order
	roots []string // the folders they hold, "/"-separated, without a trailing "/"
}

type scopeCache struct {
	mu sync.Mutex
	s  *musicScope
}

// music is the user's music libraries, asked once per client.
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
	seen := map[string]bool{}
	for _, v := range views.Items {
		if !strings.EqualFold(v.CollectionType, "music") {
			continue
		}
		s.libs = append(s.libs, v.ID)
		// Every top-level item, paged, not just the first 200: a library can have more than one
		// content folder, and a song in a later one must not be filtered out as non-music (audit S15).
		q := url.Values{"userId": {c.c.UserID}, "parentId": {v.ID}, "fields": {"Path"}}
		_, err := c.eachPage(ctx, "/Items", q, func(k wireItem) {
			if p := slashPath(k.Path); p != "" {
				if dir := path.Dir(p); !seen[dir] {
					seen[dir] = true
					s.roots = append(s.roots, dir)
				}
			}
		})
		if err != nil {
			return nil, err
		}
	}
	c.scope.s = s
	return s, nil
}

// slashPath is a server path with "/" separators (a Windows server's too), without a trailing one.
func slashPath(p string) string { return strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/") }

// inMusic reports whether w is a song of a music library (see the package comment above).
func (s *musicScope) inMusic(w wireItem) bool {
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
	for _, r := range s.roots {
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
		lq := url.Values{}
		for k, v := range q {
			lq[k] = v
		}
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

// allMusic is every song of q in every music library, converted page by page so a big library's
// raw items never pile up.
func (c *Client) allMusic(ctx context.Context, q url.Values, page int) ([]Song, error) {
	s, err := c.music(ctx)
	if err != nil {
		return nil, err
	}
	out := []Song{} // never nil: an empty list is still an answer
	seen := map[string]bool{}
	for _, lib := range s.libs {
		total := -1
		got := map[string]bool{} // this library's songs: fewer than its total means one was skipped
		for start := 0; ; start += page {
			lq := url.Values{}
			for k, v := range q {
				lq[k] = v
			}
			lq.Set("parentId", lib)
			lq.Set("startIndex", strconv.Itoa(start))
			lq.Set("limit", strconv.Itoa(page))
			r, err := c.items(ctx, lq)
			if err != nil {
				return nil, err
			}
			if total >= 0 && r.TotalRecordCount != total {
				return nil, ErrChanged // items shifted between pages: one may have been skipped
			}
			total = r.TotalRecordCount
			fresh := r.Items[:0]
			for _, w := range r.Items {
				got[w.ID] = true
				if !seen[w.ID] {
					seen[w.ID] = true
					fresh = append(fresh, w)
				}
			}
			out = append(out, songs(fresh)...)
			if len(r.Items) < page || start+len(r.Items) >= r.TotalRecordCount {
				break
			}
		}
		// Songs tied on every sort key can swap between pages, one coming twice and another never:
		// a sync would then delete the one missed (audit 2026-11-10 #3).
		if len(got) < total {
			return nil, ErrChanged
		}
	}
	return out, nil
}

// AllSongs is every song of the music libraries, 500 a request, by album artist, album and track:
// Entire Library's songs in about one request per 500, not one per album.
func (c *Client) AllSongs(ctx context.Context) ([]Song, error) {
	return c.allMusic(ctx, url.Values{"includeItemTypes": {"Audio"}, "recursive": {"true"},
		"sortBy": {songOrder}}, 500)
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
		lq := url.Values{}
		for k, v := range q {
			lq[k] = v
		}
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
		out = append(out, r.Items...)
		skip = 0
	}
	return out, nil
}

// sortByName orders xs by name when they came from more than one library.
func sortByName[T any](xs []T, name func(T) string, libs int) {
	if libs > 1 {
		sort.SliceStable(xs, func(i, j int) bool { return strings.ToLower(name(xs[i])) < strings.ToLower(name(xs[j])) })
	}
}
