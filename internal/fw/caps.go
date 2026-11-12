package fw

import (
	"os"
	"strings"
)

// Caps represents capabilities from the firmware.
type Caps struct {
	Playlist bool
	Progress bool
	Library  bool
	// Sections: the first page may be a set of sections, shown one at a time and swiped between
	// (q2-pod patch/addon.c sec_take).
	Sections bool
	// Art: a row's cover, a JPEG at /tmp/q2-<id>.art/<key>.jpg, draws the list as Tidal's
	// (Q2 Pod 1.7.3, patch/addon.c art_rows).
	Art bool
	// URLs: .play may name a queue of stream URLs (Q2URLS/1), which the firmware plays through
	// stock's Tidal player: it streams, waits for slow data and seeks itself (Q2 Pod 1.7.3).
	URLs bool
	// Window: a row of kind "page" shows its list from the cursor in its context, in the list's
	// place (Q2 Pod 1.7.3, patch/addon.c page_to), so a long list is shown a window at a time.
	Window bool
	// Sort: a list's row of kind "sort" right after its search row is a sort icon in the title bar,
	// left of the search icon; elsewhere it is a list row (Q2 Pod, patch/addon.c take_find).
	Sort bool
	// Field: a row of kind "field" is a value typed on the page: its text goes to the row's op and
	// the reply is the page's, in its place, nothing titled with the text (Q2 Pod, patch/addon.c
	// field_sent).
	Field bool
	// Fav: a URL queue's line may carry the song's item and heart, and Now Playing's heart presses
	// come back in .jellyfin/fav.log (Q2 Pod, patch/addon.c addon_fav, docs/librespot.md Hearts).
	Fav bool
	// Now: a URL queue whose header holds now=1 is told what plays, in /tmp/q2-<id>.now (Q2 Pod,
	// patch/addon.c now_poll, docs/librespot.md), so every streamed song can be reported.
	Now bool
}

// ReadCaps reads the firmware capabilities from path. A missing file gives all false.
// The file format is space-separated words; unknown words are ignored.
// Trailing newline/whitespace is tolerated.
func ReadCaps(path string) Caps {
	data, err := os.ReadFile(path)
	if err != nil {
		return Caps{}
	}

	words := strings.Fields(string(data))
	caps := Caps{}
	for _, word := range words {
		switch word {
		case "playlist":
			caps.Playlist = true
		case "progress":
			caps.Progress = true
		case "library":
			caps.Library = true
		case "sections":
			caps.Sections = true
		case "art":
			caps.Art = true
		case "urls":
			caps.URLs = true
		case "window":
			caps.Window = true
		case "sort":
			caps.Sort = true
		case "field":
			caps.Field = true
		case "fav":
			caps.Fav = true
		case "now":
			caps.Now = true
		}
	}
	return caps
}
