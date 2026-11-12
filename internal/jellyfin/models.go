package jellyfin

import (
	"cmp"
	"path"
	"strings"
)

// The app-facing shapes. They keep the field names the rest of q2-jellyfin was written against
// (plan, syncer, device_*), so only this package knows Jellyfin's wire format.

type Song struct {
	ID, Title, Album, AlbumID, Artist, AlbumArtist string
	Year, Disc, Track, Duration                    int
	Size                                           int64
	Suffix, CoverArt                               string
	BitRate                                        int
	HasLyrics                                      bool // the server holds lyrics for it (Lyrics)
	Favorite                                       bool // the user's favourite (UserData.IsFavorite)
}

type Album struct {
	ID, Name, Artist string
	Year, SongCount  int
	CoverArt         string
}

type Artist struct {
	ID, Name   string
	AlbumCount int
}

type Playlist struct {
	ID, Name  string
	SongCount int
}

// itemFields is what every list asks Jellyfin to fill in beyond its defaults.
// ProductionYear is not an ItemFields value (the server returns it anyway), so it is not asked for.
const itemFields = "MediaSources,ChildCount,Path,DateCreated"

// artistFields asks /Artists/AlbumArtists for AlbumCount, which the server fills only for
// ItemCounts (an artist's ChildCount counts its songs and albums alike).
const artistFields = "ItemCounts"

// wireItem is Jellyfin's BaseItemDto, only the fields this app reads.
type wireItem struct {
	ID                   string            `json:"Id"`
	Name                 string            `json:"Name"`
	Type                 string            `json:"Type"`
	CollectionType       string            `json:"CollectionType"` // a library view's ("music", "movies", …)
	HasLyrics            bool              `json:"HasLyrics"`
	DateCreated          string            `json:"DateCreated"`
	MediaType            string            `json:"MediaType"`
	Album                string            `json:"Album"`
	AlbumID              string            `json:"AlbumId"`
	AlbumArtist          string            `json:"AlbumArtist"`
	Artists              []string          `json:"Artists"`
	ProductionYear       int               `json:"ProductionYear"`
	ParentIndexNumber    int               `json:"ParentIndexNumber"` // disc
	IndexNumber          int               `json:"IndexNumber"`       // track
	RunTimeTicks         int64             `json:"RunTimeTicks"`      // 100 ns units
	ChildCount           int               `json:"ChildCount"`
	AlbumCount           int               `json:"AlbumCount"`
	Container            string            `json:"Container"`
	Path                 string            `json:"Path"`
	ImageTags            map[string]string `json:"ImageTags"`
	AlbumPrimaryImageTag string            `json:"AlbumPrimaryImageTag"`
	MediaSources         []struct {
		Size      int64  `json:"Size"`
		Bitrate   int    `json:"Bitrate"`
		Container string `json:"Container"`
		Path      string `json:"Path"`
	} `json:"MediaSources"`
	UserData struct {
		IsFavorite bool `json:"IsFavorite"`
	} `json:"UserData"`
}

// itemPage is the QueryResult every list endpoint returns.
type itemPage struct {
	Items            []wireItem `json:"Items"`
	TotalRecordCount int        `json:"TotalRecordCount"`
}

// suffix is the song's file extension without the dot, lower case: from the file's path when the
// server shows it, else from its container. A list ("mov,mp4,m4a") names m4a when it can, then
// mp4, else its first entry.
func (w wireItem) suffix() string {
	p, c := w.Path, w.Container
	if len(w.MediaSources) > 0 {
		p = cmp.Or(w.MediaSources[0].Path, p)
		c = cmp.Or(w.MediaSources[0].Container, c)
	}
	if ext := strings.TrimPrefix(strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/"))), "."); ext != "" {
		return ext
	}
	parts := strings.Split(strings.ToLower(c), ",")
	for _, want := range []string{"m4a", "mp4"} {
		for _, x := range parts {
			if x == want {
				return x
			}
		}
	}
	return parts[0]
}

func (w wireItem) song() Song {
	artist := strings.Join(w.Artists, ", ")
	aa := cmp.Or(w.AlbumArtist, artist)
	s := Song{ID: w.ID, Title: w.Name, Album: w.Album, AlbumID: w.AlbumID, Artist: artist, AlbumArtist: aa,
		Year: w.ProductionYear, Disc: w.ParentIndexNumber, Track: w.IndexNumber,
		Duration: int(w.RunTimeTicks / 10_000_000), Suffix: w.suffix(), HasLyrics: w.HasLyrics,
		Favorite: w.UserData.IsFavorite}
	if len(w.MediaSources) > 0 {
		s.Size, s.BitRate = w.MediaSources[0].Size, w.MediaSources[0].Bitrate/1000
	}
	// A song's cover is its album's primary image, else its own.
	switch {
	case w.AlbumPrimaryImageTag != "" && w.AlbumID != "":
		s.CoverArt = w.AlbumID
	case w.ImageTags["Primary"] != "":
		s.CoverArt = w.ID
	}
	return s
}

func (w wireItem) album() Album {
	a := Album{ID: w.ID, Name: w.Name, Artist: cmp.Or(w.AlbumArtist, strings.Join(w.Artists, ", ")),
		Year: w.ProductionYear, SongCount: w.ChildCount}
	if w.ImageTags["Primary"] != "" {
		a.CoverArt = w.ID
	}
	return a
}

func (w wireItem) artist() Artist { return Artist{ID: w.ID, Name: w.Name, AlbumCount: w.AlbumCount} }
func (w wireItem) playlist() Playlist {
	return Playlist{ID: w.ID, Name: w.Name, SongCount: w.ChildCount}
}

func songs(ws []wireItem) []Song {
	out := make([]Song, 0, len(ws))
	for _, w := range ws {
		if w.Type == "" || w.Type == "Audio" {
			out = append(out, w.song())
		}
	}
	return out
}

func albums(ws []wireItem) []Album {
	out := make([]Album, len(ws))
	for i, w := range ws {
		out[i] = w.album()
	}
	return out
}

func artists(ws []wireItem) []Artist {
	out := make([]Artist, len(ws))
	for i, w := range ws {
		out[i] = w.artist()
	}
	return out
}
