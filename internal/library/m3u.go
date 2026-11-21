package library

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/q2"
)

// WriteM3U writes #EXTM3U and one path per line, duplicates kept, atomically, to path (one of the
// playlist files recorded in m, or under Jellyfin/). The paths are written as given (absolute,
// /mnt/mmc/...). A playlist whose text is unchanged isn't written again.
func (m *Manifest) WriteM3U(root, path string, paths []string) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == b.String() {
		return nil
	}
	return q2.WriteFile(path, []byte(b.String()))
}
