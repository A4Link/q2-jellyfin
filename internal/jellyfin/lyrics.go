package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
		line := strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(l.Text, "\r", ""), "\n", " "), " ")
		if l.Start != nil {
			cs := *l.Start / 100_000 // centiseconds
			line = fmt.Sprintf("[%02d:%02d.%02d]%s", cs/6000, cs/100%60, cs%100, line)
		} else if line == "" {
			continue
		}
		if b.Len()+len(line)+1 > lyricsMax {
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}

// lyricsMax is the most of a song's lyrics kept: Q2 Pod reads no more of a .lrc (lyrics.c
// LYRIC_TAG_MAX), and they are kept in /tmp, which is RAM, and on the card.
const lyricsMax = 64 << 10
