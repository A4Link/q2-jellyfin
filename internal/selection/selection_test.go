package selection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToggleAddRemove(t *testing.T) {
	var s Set
	it := Item{Kind: KindAlbum, ID: "a1", Name: "X"}
	if !s.Toggle(it) || !s.Has(KindAlbum, "a1") || s.Items[0].Added.IsZero() {
		t.Fatal("add failed")
	}
	if s.Has(KindSong, "a1") {
		t.Fatal("kind must matter")
	}
	if s.Toggle(it) || s.Has(KindAlbum, "a1") || len(s.Items) != 0 {
		t.Fatal("remove failed")
	}
	if Key(KindLibrary, "all") != "library:all" || Key(KindAlbum, "a1") != "album:a1" {
		t.Fatal("key")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "selection.json")
	s, err := Load(p)
	if err != nil || s.Version != 1 || len(s.Items) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	s.Toggle(Item{Kind: KindPlaylist, ID: "p", Name: "P", Quality: "mp3", Added: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, w := range []string{`"version":1`, `"added":"2026-01-02T03:04:05Z"`, `"kind":"playlist"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("missing %s in %s", w, b)
		}
	}
	got, err := Load(p)
	if err != nil || len(got.Items) != 1 || got.Items[0] != s.Items[0] {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
}

// A file an older version wrote, with "exclude", or one with a key added by hand, still loads its
// items; a save writes only what Set has (review A5-P1-11).
func TestUnknownFieldsIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "selection.json")
	os.WriteFile(p, []byte(`{"version":2,"items":[{"kind":"album","id":"al0","name":"A"}],"exclude":["x"],"web":{"theme":"dark"}}`), 0o644)
	s, err := Load(p)
	if err != nil || s.Version != 2 || len(s.Items) != 1 || s.Items[0].ID != "al0" {
		t.Fatalf("%+v %v", s, err)
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "exclude") || strings.Contains(string(b), "web") {
		t.Fatalf("got %s", b)
	}
}

// Some Windows editors save "UTF-8 with BOM": the file still loads, rather than reading as damaged
// with Reset Selection the only way out (review A5-P3-1).
func TestLoadBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "selection.json")
	b := append([]byte{0xEF, 0xBB, 0xBF}, `{"version":1,"items":[{"kind":"album","id":"al0","name":"A"}],"exclude":[]}`...)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil || len(s.Items) != 1 || s.Items[0].ID != "al0" {
		t.Fatalf("%+v %v", s, err)
	}
}
