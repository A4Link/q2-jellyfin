package main

import (
	"github.com/A4Link/q2-jellyfin/internal/q2spot"
	"github.com/A4Link/q2-jellyfin/internal/quality"
)

// Quality pages: Sync Quality and Streaming Quality (Settings → Quality and Storage) each open a
// list of quality.Choices (Original, the server's file as it is, then Opus from 320 kbps down), the
// current one marked Selected. A press chooses and toasts it; the page refreshes, so the mark
// follows. Every synced song is at Sync Quality: an item has no quality of its own.

const selectedMark = "Selected"

// choiceRows is one action row per choice: op with the quality as context.
func choiceRows(op, current string) []q2spot.Row {
	var rows []q2spot.Row
	for _, q := range quality.Choices() {
		r := act(op, choiceName(q), q)
		if q == current {
			r.Subtitle = selectedMark
		}
		rows = append(rows, r)
	}
	return rows
}

// choiceName is a choice's row title: Original says what it means.
func choiceName(q string) string {
	if quality.IsOriginal(q) {
		return "Original (no transcoding)"
	}
	return quality.Name(q)
}

// syncQualityPage lists the choices for Settings → Quality and Storage → Sync Quality.
func syncQualityPage(s settings) q2spot.Reply {
	return q2spot.Reply{Message: "refresh", Rows: choiceRows("setdefq", s.def())}
}

// streamQualityPage lists the choices for Settings → Quality and Storage → Streaming Quality.
func streamQualityPage(s settings) q2spot.Reply {
	return q2spot.Reply{Message: "refresh", Rows: choiceRows("setstreamq", s.stream())}
}
