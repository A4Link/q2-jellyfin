// Package selection models .jellyfin/selection.json: what is in Sync.
package selection

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2"
)

type Kind string

const (
	KindPlaylist Kind = "playlist"
	KindAlbum    Kind = "album"
	KindArtist   Kind = "artist"
	KindSong     Kind = "song"
	KindLibrary  Kind = "library" // the whole library, id "all"
	// KindFavorites is the user's favourite songs, id "all": synced like a playlist, with its own
	// "Favourites" .m3u.
	KindFavorites Kind = "favorites"
)

type Item struct {
	Kind    Kind      `json:"kind"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Quality string    `json:"quality"` // written as the app's Sync Quality, never read: every item syncs at it
	Added   time.Time `json:"added"`
	// Held: the item stays in Sync with the songs already on the card, but a sync adds none of its
	// missing ones until it is resumed (Sync Jobs).
	Held bool `json:"held,omitempty"`
}

// Set is the selection. A key the file has but Set doesn't (an older "exclude") is ignored.
type Set struct {
	Version int    `json:"version"`
	Items   []Item `json:"items"`
}

func Key(kind Kind, id string) string { return string(kind) + ":" + id }

// Load returns an empty Set (version 1) when path does not exist.
func Load(path string) (Set, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Set{Version: 1}, nil
	}
	if err != nil {
		return Set{}, err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // "UTF-8 with BOM" from a Windows editor
	s := Set{Version: 1}
	if err := json.Unmarshal(b, &s); err != nil {
		return Set{}, err
	}
	return s, nil
}

func (s Set) Save(path string) error {
	if s.Items == nil {
		s.Items = []Item{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return q2.WriteFile(path, b)
}

func (s Set) index(kind Kind, id string) int {
	for i, it := range s.Items {
		if it.Kind == kind && it.ID == id {
			return i
		}
	}
	return -1
}

func (s Set) Has(kind Kind, id string) bool { return s.index(kind, id) >= 0 }

// Toggle removes a present item (false) or appends it (true), stamping Added if zero.
func (s *Set) Toggle(it Item) bool {
	if i := s.index(it.Kind, it.ID); i >= 0 {
		s.Items = append(s.Items[:i], s.Items[i+1:]...)
		return false
	}
	if it.Added.IsZero() {
		it.Added = time.Now().UTC()
	}
	s.Items = append(s.Items, it)
	return true
}

// SetHeld holds or resumes an item.
func (s *Set) SetHeld(kind Kind, id string, held bool) {
	if i := s.index(kind, id); i >= 0 {
		s.Items[i].Held = held
	}
}
