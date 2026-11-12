package library

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2"
)

// Entry is one synced song. Meta is MetaKey at sync time; Items are the selection keys holding it.
// Size is the bytes on the card; SrcSize, for an original, the server's size of the file it came
// from (0 when unknown: synced before it was kept), so a re-rip is noticed.
type Entry struct {
	ID, Path, Quality string
	Size              int64
	SrcSize           int64 `json:",omitempty"`
	Items             []string
	Meta              string
	// Moving is where a rename under way is taking the file, saved before it moves (LoadManifest
	// settles it): a crash or the card going part-way never leaves a moved file unrecorded.
	Moving string `json:",omitempty"`
}

// Manifest is what the app has put on the card. Playlists maps a playlist name to the absolute
// path of the .m3u written for it, the only playlist files the app may overwrite or delete.
type Manifest struct {
	Songs     map[string]*Entry
	Playlists map[string]string
	// PlaylistsStale: the recorded .m3u files hold old paths (after a folder migration), so the next
	// sync rewrites every one even when no song changes. The syncer clears it.
	PlaylistsStale bool `json:",omitempty"`
	// Imported holds the playlist files the firmware confirmed importing (R35): the Q2 playlist of
	// that name is the app's, so later requests replace it ("import"); any other file is sent as
	// "add", which never touches a playlist the user made. A file stays here after it is removed,
	// as its playlist stays on the Q2.
	Imported map[string]bool
	// PlaylistSums holds, per playlist name, the PlaylistSum of the song paths last written for it:
	// a playlist whose songs or order changed is written again though no song moves (I10).
	PlaylistSums map[string]string `json:",omitempty"`
	// SyncedAt is when a sync last ran to its end: the file is saved by previews and imports too,
	// so its time says nothing of a sync (audit 2026-11-10 #10). Zero before the first, or in an
	// older build's file.
	SyncedAt time.Time
}

// LoadManifest reads path; a missing file is an empty manifest.
func LoadManifest(path string) (*Manifest, error) {
	m := &Manifest{}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, m)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if m.Songs == nil {
		m.Songs = map[string]*Entry{}
	}
	if m.Playlists == nil {
		m.Playlists = map[string]string{}
	}
	if m.PlaylistSums == nil {
		m.PlaylistSums = map[string]string{}
	}
	if m.Imported == nil { // an older manifest: its playlists were imported by name-replacing requests
		m.Imported = map[string]bool{}
		for _, f := range m.Playlists {
			m.Imported[f] = true
		}
	}
	if err == nil {
		settleMoves(m)
	}
	return m, err
}

// settleMoves records each cut rename's song where its file is: at Moving when it moved (Path
// gone, or taken by the song moved into it), else at Path. With neither there (the card away, or
// deleted by hand) the entry stays as it is.
func settleMoves(m *Manifest) {
	targets := map[string]bool{}
	for _, e := range m.Songs {
		if e.Moving != "" {
			targets[e.Moving] = true
		}
	}
	for _, e := range m.Songs {
		if e.Moving == "" {
			continue
		}
		_, perr := os.Lstat(e.Path)
		if _, err := os.Lstat(e.Moving); err == nil && (perr != nil || targets[e.Path]) {
			e.Path = e.Moving
		} else if perr != nil {
			continue
		}
		e.Moving = ""
	}
}

// MovePlaylist renames the .m3u recorded for playlist name to to, a free file directly in
// <root>/_explaylist_data/, and records it there.
func (m *Manifest) MovePlaylist(root, name, to string) error {
	from, ok := m.Playlists[name]
	if !ok {
		return errors.New("no playlist file recorded for " + name)
	}
	if err := checkOwned(root, from, m); err != nil {
		return err
	}
	if !ValidPlaylistFile(root, to) {
		return errors.New("refusing playlist path: " + to)
	}
	if _, err := os.Lstat(to); err == nil {
		return errors.New("already exists: " + to)
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	m.Playlists[name] = to
	return nil
}

// Save writes the manifest atomically.
func (m *Manifest) Save(root, path string) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return q2.WriteWith(path, func(w io.Writer) error { return json.NewEncoder(w).Encode(m) })
}
