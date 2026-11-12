package quality

import (
	"strings"
	"testing"
)

func TestChoices(t *testing.T) {
	got := strings.Join(Choices(), ",")
	want := "original,opus-320,opus-288,opus-256,opus-224,opus-192,opus-160,opus-128,opus-96,opus-64"
	if got != want {
		t.Fatalf("%s", got)
	}
}

func TestParseAndNames(t *testing.T) {
	for q, want := range map[string]string{
		"original": "Original", "opus-160": "Opus 160 kbps", "mp3-320": "MP3 320 kbps",
		"": "Original", "opus-x": "Original", "opus-999": "Original", "flac-320": "Original", "../x": "Original",
	} {
		if got := Name(q); got != want {
			t.Errorf("%q: %q, want %q", q, got, want)
		}
	}
	if Normal("weird") != Original || Normal("opus-96") != "opus-96" {
		t.Fatal("Normal")
	}
	if Bitrate("opus-224") != 224000 || Bitrate(Original) != 0 || Ext("opus-64") != "opus" || Ext(Original) != "" {
		t.Fatal("Bitrate/Ext")
	}
}

func TestRank(t *testing.T) {
	order := []string{"", "mp3-64", "opus-64", "opus-128", "mp3-320", "opus-320", Original} // "": none chosen yet
	for i := 1; i < len(order); i++ {
		if !Better(order[i], order[i-1]) {
			t.Errorf("%s should rank above %s", order[i], order[i-1])
		}
	}
}

func TestAsMP3(t *testing.T) {
	if AsMP3("opus-160") != "mp3-160" || AsMP3(Original) != Original {
		t.Fatal(AsMP3("opus-160"))
	}
}
