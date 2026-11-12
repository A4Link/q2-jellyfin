// Package quality names what a song is synced or streamed as: the server's file as it is
// ("original", the default: no transcoding), or an Opus transcode the Jellyfin server makes at one
// of Bitrates. MP3 transcodes exist only as a fallback for Q2 Pod firmware without the playlist
// extension, whose folder playback plays FLAC and MP3 alone; nobody chooses them.
//
// A quality is a string, kept in selection.json, settings.json and the manifest:
// "original", "opus-<kbps>" or "mp3-<kbps>".
package quality

import (
	"strconv"
	"strings"
)

const Original = "original"

// Bitrates are the Opus transcode's choices, in kbit/s, highest first as the pages list them.
var Bitrates = []int{320, 288, 256, 224, 192, 160, 128, 96, 64}

// Choices are what Sync Quality and Streaming Quality offer: Original first, then Opus from the
// highest bitrate down.
func Choices() []string {
	out := []string{Original}
	for _, b := range Bitrates {
		out = append(out, Opus(b))
	}
	return out
}

func Opus(kbps int) string { return "opus-" + strconv.Itoa(kbps) }
func MP3(kbps int) string  { return "mp3-" + strconv.Itoa(kbps) }

// Fallback is what an original the Q2 can't play (WavPack, Musepack, …) becomes.
var Fallback = Opus(320)

// Parse splits q into its codec ("" for Original) and bitrate. Anything it doesn't know is
// Original: the server's own file is never a wrong guess.
func Parse(q string) (codec string, kbps int) {
	c, n, ok := strings.Cut(q, "-")
	if !ok || (c != "opus" && c != "mp3") {
		return "", 0
	}
	k, err := strconv.Atoi(n)
	if err != nil || k < 32 || k > 320 {
		return "", 0
	}
	return c, k
}

// Normal is q as a known quality: Original for anything unknown.
func Normal(q string) string {
	c, k := Parse(q)
	if c == "" {
		return Original
	}
	return c + "-" + strconv.Itoa(k)
}

func IsOriginal(q string) bool { c, _ := Parse(q); return c == "" }

// Bitrate is the transcode's bits per second, 0 for Original.
func Bitrate(q string) int64 { _, k := Parse(q); return int64(k) * 1000 }

// Ext is a transcode's file extension ("opus", "mp3"); "" for Original, whose extension is its
// file's own.
func Ext(q string) string { c, _ := Parse(q); return c }

// Name is q for the screen: "Original", "Opus 160 kbps".
func Name(q string) string {
	c, k := Parse(q)
	switch c {
	case "opus":
		return "Opus " + strconv.Itoa(k) + " kbps"
	case "mp3":
		return "MP3 " + strconv.Itoa(k) + " kbps"
	}
	return "Original"
}

// Rank orders qualities: Original above every transcode, a transcode by its bitrate (Opus above
// MP3 at the same rate). "" is no quality at all (none chosen yet) and ranks below every one.
func Rank(q string) int {
	if q == "" {
		return -1
	}
	c, k := Parse(q)
	switch c {
	case "":
		return 1 << 20
	case "opus":
		return k*2 + 1
	}
	return k * 2
}

// Better reports whether a ranks above b.
func Better(a, b string) bool { return Rank(a) > Rank(b) }

// AsMP3 is q for folder playback on firmware without the playlist extension: a transcode at the
// same bitrate as MP3, Original unchanged.
func AsMP3(q string) string {
	if c, k := Parse(q); c != "" {
		return MP3(k)
	}
	return Original
}
