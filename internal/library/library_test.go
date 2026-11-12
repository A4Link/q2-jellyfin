package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

func TestSafeNameReserved(t *testing.T) {
	if got := SafeName("AC/DC: Live?"); got != "AC_DC_ Live_" {
		t.Fatal(got)
	}
	if got := SafeName("a\x01b<>\"\\|*"); got != "a_b______" {
		t.Fatal(got)
	}
}

func TestSafeNameTrailingDotsSpaces(t *testing.T) {
	if got := SafeName("Hello. . "); got != "Hello" {
		t.Fatal(got)
	}
	if got := SafeName(".."); got != "…" { // never the ".." component
		t.Fatal(got)
	}
}

// The firmware refuses ".." anywhere in a playlist or queue line (I18), and the Q2's scan skips a
// folder or file whose name starts with "." (M40).
func TestSafeNameDots(t *testing.T) {
	for in, want := range map[string]string{
		"Wait... What": "Wait… What", "...Baby One More Time": "…Baby One More Time", "Oops!... I Did It Again": "Oops!… I Did It Again",
		".38 Special": "_38 Special", "a.b": "a.b", "Wait..": "Wait…", ".": "_",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// StaleName finds song paths made before SafeName cleaned dots, so the plan moves them.
func TestStaleName(t *testing.T) {
	for p, want := range map[string]bool{
		"/r/Jellyfin/.38 Special/Alb/01 - a.flac":     true,
		"/r/Jellyfin/A/...Like Clockwork/01 - a.flac": true,
		"/r/Jellyfin/A/B/01 - Wait... What.flac":      true,
		"/r/Jellyfin/A/B/01 - a.b.flac":               false,
		"/r/Jellyfin/…And You/B/01 - a.flac":          false,
		"/r/.jellyfin/x/y":                            false,
	} {
		if StaleName("/r", p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}

func TestSafeNameUTF8Cut(t *testing.T) {
	for _, s := range []string{strings.Repeat("æøå", 34), strings.Repeat("日本語", 22)} {
		got := SafeName(s)
		if len(got) > 120 || !utf8.ValidString(got) || len(got) < 110 {
			t.Fatalf("%d bytes valid=%v", len(got), utf8.ValidString(got))
		}
	}
}

var song = jellyfin.Song{ID: "1", Title: "Intro", Album: "Alb", AlbumArtist: "Art", Artist: "Other", Year: 1999, Disc: 1, Track: 3, Suffix: "flac"}

func TestSongPathSingleDisc(t *testing.T) {
	got := SongPath("/mnt/mmc", song, "original", false)
	if want := "/mnt/mmc/Jellyfin/Art/1999 - Alb/03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
	if got := SongPath("/mnt/mmc", song, "mp3-320", false); !strings.HasSuffix(got, "03 - Intro.mp3") {
		t.Fatal(got)
	}
}

func TestSongPathMultiDisc(t *testing.T) {
	s := song
	s.Disc = 2
	if got, want := SongPath("/r", s, "original", true), "/r/Jellyfin/Art/1999 - Alb/2-03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
}

func TestSongPathNoYear(t *testing.T) {
	s := song
	s.Year = 0
	if got, want := SongPath("/r", s, "original", false), "/r/Jellyfin/Art/Alb/03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
}

func TestWithID(t *testing.T) {
	if got := WithID("/a/b/03 - x.flac", "42"); got != "/a/b/03 - x [42].flac" {
		t.Fatal(got)
	}
}

func TestMetaKey(t *testing.T) {
	a := MetaKey(song, "original")
	s := song
	s.Title = "Outro"
	if a == MetaKey(s, "original") || a == MetaKey(song, "mp3-128") || a != MetaKey(song, "original") {
		t.Fatal("MetaKey")
	}
	s = song
	s.Duration = 99 // not a path field
	if a != MetaKey(s, "original") {
		t.Fatal("MetaKey sensitive to non-path field")
	}
}

func mk(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesOutside(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		root + "/Music/x.flac",
		root + "/Jellyfin/../Music/x.flac",
		root + "/_explaylist_data/Other.m3u",
		root + "/Subsonic",
	} {
		mk(t, filepath.Clean(p))
		if err := Remove(root, p); err == nil {
			t.Fatalf("no error for %s", p)
		}
		if _, err := os.Stat(filepath.Clean(p)); err != nil {
			t.Fatalf("%s gone", p)
		}
	}
	// symlink escape
	root = t.TempDir()
	out := t.TempDir()
	mk(t, out+"/f.flac")
	os.MkdirAll(root+"/Jellyfin", 0o755)
	if err := os.Symlink(out, root+"/Jellyfin/link"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, root+"/Jellyfin/link/f.flac"); err == nil {
		t.Fatal("symlink escape allowed")
	}
	if _, err := os.Stat(out + "/f.flac"); err != nil {
		t.Fatal("escaped file removed")
	}
}

func TestOwnedOutsideRoot(t *testing.T) {
	if Owned(t.TempDir(), "/etc/passwd", nil) {
		t.Fatal("owned")
	}
}

func TestOwnedAllows(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{root + "/Jellyfin/A/B/1.mp3", root + "/.jellyfin/log.txt"} {
		if !Owned(root, p, nil) {
			t.Fatal(p)
		}
	}
	// Other apps' files are never this app's, whatever their name.
	for _, p := range []string{root + "/_explaylist_data/Navidrome - X.m3u", root + "/Subsonic/A/1.mp3", root + "/Navidrome/A/1.mp3"} {
		if Owned(root, p, nil) {
			t.Fatal("owned:", p)
		}
	}
}

func TestRemovePrunesEmptyAlbum(t *testing.T) {
	root := t.TempDir()
	a, b := root+"/Jellyfin/Art/1999 - Alb/01 - a.flac", root+"/Jellyfin/Art/1999 - Alb/02 - b.flac"
	mk(t, a)
	mk(t, b)
	if err := Remove(root, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(b)); err != nil {
		t.Fatal("album removed while non-empty")
	}
	if err := Remove(root, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root + "/Jellyfin/Art"); !os.IsNotExist(err) {
		t.Fatal("empty folders left")
	}
	if _, err := os.Stat(root + "/Jellyfin"); err != nil {
		t.Fatal("Jellyfin itself removed")
	}
}

func TestM3UKeepsDuplicates(t *testing.T) {
	root := t.TempDir()
	p := PlaylistFile(root, "Mix: 1")
	if p != root+"/_explaylist_data/Mix_ 1.m3u" {
		t.Fatal(p)
	}
	m := &Manifest{Playlists: map[string]string{"Mix: 1": p}}
	if err := m.WriteM3U(root, p, []string{"/mnt/mmc/a.flac", "/mnt/mmc/b.flac", "/mnt/mmc/a.flac"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "#EXTM3U\n/mnt/mmc/a.flac\n/mnt/mmc/b.flac\n/mnt/mmc/a.flac\n" {
		t.Fatalf("%q", b)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/.jellyfin/manifest.json"
	m, err := LoadManifest(p)
	if err != nil || len(m.Songs) != 0 {
		t.Fatal(err, m)
	}
	m.Songs["1"] = &Entry{ID: "1", Path: "/x", Quality: "original", Size: 5, Items: []string{"album:a"}, Meta: "h"}
	if err := m.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadManifest(p)
	if err != nil || m2.Songs["1"].Meta != "h" || m2.Songs["1"].Items[0] != "album:a" {
		t.Fatal(err, m2)
	}
}

func TestRemoveSymlinkDotDotRefused(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	mk(t, filepath.Join(filepath.Dir(out), "f"))
	defer os.Remove(filepath.Join(filepath.Dir(out), "f"))
	os.MkdirAll(root+"/Subsonic", 0o755)
	os.Symlink(out, root+"/Jellyfin/link")
	mk(t, root+"/Jellyfin/f")
	if err := Remove(root, root+"/Jellyfin/link/../f"); err == nil {
		t.Fatal("allowed")
	}
	for _, p := range []string{filepath.Join(filepath.Dir(out), "f"), root + "/Jellyfin/f"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(p, "gone")
		}
	}
}

func TestMetaKeyArtistFallback(t *testing.T) {
	a, b := song, song
	a.AlbumArtist, b.AlbumArtist = "", ""
	b.Artist = "Someone Else"
	if MetaKey(a, "original") == MetaKey(b, "original") {
		t.Fatal("Artist change not detected")
	}
}

func TestWritesRefuseOutsideOwned(t *testing.T) {
	root := t.TempDir()
	if err := (&Manifest{}).WriteM3U(root, root+"/Music/x.m3u", []string{"a"}); err == nil {
		t.Fatal("m3u")
	}
	if err := (&Manifest{}).WriteM3U(root, root+"/_explaylist_data/Other.m3u", nil); err == nil {
		t.Fatal("m3u other")
	}
	m := &Manifest{Songs: map[string]*Entry{}}
	if err := m.Save(root, root+"/elsewhere/manifest.json"); err == nil {
		t.Fatal("save")
	}
	for _, d := range []string{"Music", "_explaylist_data", "elsewhere"} {
		if _, err := os.Stat(root + "/" + d); err == nil {
			t.Fatal(d, "created")
		}
	}
}

func TestWithIDKeeps120(t *testing.T) {
	stem := strings.Repeat("é", 59) + "a" // 119 bytes
	got := WithID("/r/"+stem+".flac", "12345")
	name := filepath.Base(got)
	if len(name) > 120 || !utf8.ValidString(name) || !strings.HasSuffix(name, " [12345].flac") {
		t.Fatal(len(name), name)
	}
}

func TestPlaylistFileNoPrefix(t *testing.T) {
	if got := PlaylistFile("/r", "Andreas"); got != "/r/_explaylist_data/Andreas.m3u" {
		t.Fatal(got)
	}
}

// The Q2 keeps 63 bytes of a playlist's name and never finds a longer one by name again, so every
// import adds a copy (issue #1): the file's name before .m3u is cut to 63 bytes on a character
// boundary, and never to a dot (".." before .m3u is refused).
func TestPlaylistFileFitsQ2Name(t *testing.T) {
	for name, n := range map[string]int{
		strings.Repeat("曲", 30):                63, // CJK: 3 bytes each
		strings.Repeat("Песня", 10):            62, // Cyrillic: 2 bytes each, 63 would cut one in half
		strings.Repeat("x", 62) + ". and more": 62,
		"Short":                                5,
	} {
		base := filepath.Base(PlaylistFile("/r", name))
		stem := strings.TrimSuffix(base, ".m3u")
		if len(stem) != n || !utf8.ValidString(stem) || !strings.HasPrefix(name, stem) || strings.Contains(base, "..") {
			t.Errorf("%q -> %q (%d bytes)", name, stem, len(stem))
		}
	}
}

func TestOwnedPlaylistsOnlyRecorded(t *testing.T) {
	root := t.TempDir()
	rec, other := PlaylistFile(root, "Mine"), PlaylistFile(root, "Other")
	m := &Manifest{Playlists: map[string]string{"Mine": rec}}
	if !Owned(root, rec, m) || Owned(root, other, m) || Owned(root, rec, nil) {
		t.Fatal("recorded only")
	}
	if err := Remove(root, other); err == nil {
		t.Fatal("removed unrecorded")
	}
	if err := m.WriteM3U(root, other, nil); err == nil {
		t.Fatal("wrote unrecorded")
	}
}

// A hostile server's suffix or id must never steer a song path out of its folder.
func TestHostileSuffixAndID(t *testing.T) {
	s := song
	s.Suffix = "x/../../../../.jellyfin/q2-jellyfin"
	got := SongPath("/r", s, "original", false)
	if want := "/r/Jellyfin/Art/1999 - Alb/03 - Intro.opus"; got != want { // a hostile suffix: the Opus fallback
		t.Fatal(got)
	}
	for _, sfx := range []string{"FLAC", "m4a", "opus"} {
		s.Suffix = sfx
		if p := SongPath("/r", s, "original", false); filepath.Ext(p) != "."+strings.ToLower(sfx) {
			t.Fatal(p)
		}
	}
	if got := WithID("/r/Jellyfin/A/B/03 - x.mp3", "../../../.jellyfin/run"); filepath.Dir(got) != "/r/Jellyfin/A/B" {
		t.Fatal(got)
	}
}

func TestInSongs(t *testing.T) {
	root := t.TempDir()
	for p, want := range map[string]bool{
		root + "/Jellyfin/A/B/1.mp3":                 true,
		root + "/.jellyfin/q2-jellyfin":              false,
		root + "/.jellyfin/manifest.json":            false,
		root + "/_explaylist_data/Navidrome - X.m3u": false,
		root + "/Subsonic":                           false,
		root + "/Jellyfin/../.jellyfin/run":          false,
	} {
		if InSongs(root, p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}

// The playlists an older manifest recorded were imported by name-replacing requests (or stock's
// Import playlist, which also replaces), so they count as the app's own (R35); a manifest that
// records Imported keeps exactly that.
func TestImportedLegacy(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/manifest.json"
	os.WriteFile(p, []byte(`{"Songs":{},"Playlists":{"Mix":"/r/_explaylist_data/Mix.m3u"}}`), 0o644)
	if m, _ := LoadManifest(p); !m.Imported["/r/_explaylist_data/Mix.m3u"] {
		t.Fatal(m.Imported)
	}
	os.WriteFile(p, []byte(`{"Songs":{},"Playlists":{"Mix":"/r/_explaylist_data/Mix.m3u"},"Imported":{}}`), 0o644)
	if m, _ := LoadManifest(p); m.Imported == nil || len(m.Imported) != 0 {
		t.Fatal(m.Imported)
	}
}

// A cut rename's song is recorded where its file is: moved, not moved yet, or moved into the
// place of a song moved away in the same batch. A manifest from before Moving loads as it was.
func TestSettleMoves(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/.jellyfin/manifest.json"
	os.MkdirAll(dir+"/.jellyfin", 0o755)
	os.WriteFile(p, []byte(`{"Songs":{"a":{"ID":"a","Path":"/r/a.flac","Quality":"original","Size":1,"Items":null,"Meta":""}}}`), 0o644)
	if m, err := LoadManifest(p); err != nil || m.Songs["a"].Path != "/r/a.flac" || m.Songs["a"].Moving != "" {
		t.Fatal(err, m.Songs["a"])
	}
	f := func(n string) string { return filepath.Join(dir, n) }
	for _, n := range []string{"moved.new", "stays.old", "a.old", "b.old"} {
		os.WriteFile(f(n), []byte(n), 0o644)
	}
	m := &Manifest{Songs: map[string]*Entry{
		"moved": {ID: "moved", Path: f("moved.old"), Moving: f("moved.new")},
		"stays": {ID: "stays", Path: f("stays.old"), Moving: f("stays.new")},
		"gone":  {ID: "gone", Path: f("gone.old"), Moving: f("gone.new")}, // the card away: left as it is
		"a":     {ID: "a", Path: f("a.old"), Moving: f("a.new")},          // b moved into a's place...
		"b":     {ID: "b", Path: f("b.old"), Moving: f("a.old")},          // ...after a moved away
	}}
	os.Rename(f("a.old"), f("a.new"))
	os.Rename(f("b.old"), f("a.old"))
	if err := m.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][2]string{"moved": {f("moved.new"), ""}, "stays": {f("stays.old"), ""},
		"gone": {f("gone.old"), f("gone.new")}, "a": {f("a.new"), ""}, "b": {f("a.old"), ""}} {
		if e := m.Songs[id]; e.Path != want[0] || e.Moving != want[1] {
			t.Errorf("%s: %+v", id, e)
		}
	}
}

// MovePlaylist renames a recorded playlist file to a free one in _explaylist_data/ and records it.
func TestMovePlaylist(t *testing.T) {
	root := t.TempDir()
	from, to := PlaylistFile(root, "Mix"), root+"/_explaylist_data/Mix (Jellyfin).m3u"
	mk(t, from)
	m := &Manifest{Playlists: map[string]string{"Mix": from}}
	for _, bad := range []string{root + "/Music/x.m3u", root + "/_explaylist_data/sub/x.m3u", from} {
		if err := m.MovePlaylist(root, "Mix", bad); err == nil {
			t.Error("moved to", bad)
		}
	}
	if err := m.MovePlaylist(root, "Mix", to); err != nil || m.Playlists["Mix"] != to {
		t.Fatal(err, m.Playlists)
	}
	if _, err := os.Stat(to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatal("old file left")
	}
	if err := (&Manifest{Playlists: map[string]string{}}).MovePlaylist(root, "None", to+"x.m3u"); err == nil {
		t.Fatal("moved an unrecorded playlist")
	}
}

// The formats the Q2 plays: demo's own scan list (R40), so Opus, TAK, DTS, MP2 and AC3 originals
// sync and stream as they are.
func TestPlayable(t *testing.T) {
	for _, sfx := range []string{"mp3", "wav", "wma", "ape", "flac", "aiff", "aif", "m4a", "aac", "dff", "dsf", "ogg",
		"dts", "mp2", "ac3", "opus", "tak", "alac", "OPUS"} {
		if !Playable(sfx) {
			t.Errorf("%s not playable", sfx)
		}
	}
	for _, sfx := range []string{"wv", "mpc", ""} {
		if Playable(sfx) {
			t.Errorf("%s playable", sfx)
		}
	}
}

// One rule for every caller: an original the Q2 can't play, or of no known format, is fetched as
// quality.Fallback; a playable original and every transcode stay as asked.
func TestQuality(t *testing.T) {
	for _, c := range []struct{ q, sfx, want string }{
		{quality.Original, "flac", quality.Original}, {quality.Original, "FLAC", quality.Original},
		{quality.Original, "wv", quality.Fallback}, {quality.Original, "", quality.Fallback},
		{"opus-128", "wv", "opus-128"}, {"mp3-320", "flac", "mp3-320"},
	} {
		if got := Quality(c.q, c.sfx); got != c.want {
			t.Errorf("Quality(%q, %q) = %q, want %q", c.q, c.sfx, got, c.want)
		}
	}
}
