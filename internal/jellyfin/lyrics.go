package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Lyrics is a song's lyrics as the Jellyfin server holds them (GET /Audio/{id}/Lyrics: a .lrc or
// .txt beside the file, or lyrics in its tags), as LRC text for a .lrc beside the song on the card,
// which Q2 Pod's player shows on Now Playing. Synced lines keep their times ("[mm:ss.xx]Text");
// unsynced ones are plain lines. "" when the song has none. Only the server is asked: never its
// RemoteSearch endpoints, which fetch lyrics from other sites.
func (c *Client) Lyrics(ctx context.Context, id string) (string, error) {
	var r struct {
		Lyrics []struct {
			Text  string `json:"Text"`
			Start *int64 `json:"Start"` // 100 ns ticks; absent for unsynced lyrics
		} `json:"Lyrics"`
	}
	if err := c.call(ctx, "GET", "/Audio/"+url.PathEscape(id)+"/Lyrics", nil, nil, &r); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.HTTP == 404 { // no lyrics for this song
			return "", nil
		}
		return "", err
	}
	var b strings.Builder
	for _, l := range r.Lyrics {
		text := strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(l.Text, "\r", ""), "\n", " "), " ")
		if l.Start != nil {
			cs := *l.Start / 100_000 // centiseconds
			fmt.Fprintf(&b, "[%02d:%02d.%02d]%s\n", cs/6000, cs/100%60, cs%100, text)
		} else if text != "" {
			b.WriteString(text + "\n")
		}
	}
	return b.String(), nil
}

// latestPage is how many albums Newly Added lists.
const latestPage = 50

// Latest is the music libraries' most recently added albums, newest first.
func (c *Client) Latest(ctx context.Context) ([]Album, error) {
	var items []wireItem
	err := c.eachMusic(ctx, "/Items", url.Values{"includeItemTypes": {"MusicAlbum"}, "recursive": {"true"},
		"sortBy": {"DateCreated,SortName"}, "sortOrder": {"Descending"}, "limit": {fmt.Sprint(latestPage)}},
		func(w wireItem) { items = append(items, w) })
	if err != nil {
		return nil, err
	}
	// Several libraries: one list, newest first (DateCreated is ISO 8601, so it sorts as text).
	sort.SliceStable(items, func(i, j int) bool { return items[i].DateCreated > items[j].DateCreated })
	if len(items) > latestPage {
		items = items[:latestPage]
	}
	return albums(items), nil
}
