package q2

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Wipe leaves the file in place, as long as it was, all zeros.
func TestWipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(p, []byte("password=s3cret"), 0o600)
	if err := Wipe(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, make([]byte, len("password=s3cret"))) {
		t.Fatalf("%q", b)
	}
	if Wipe(filepath.Join(t.TempDir(), "none")) == nil {
		t.Fatal("no error for a missing file")
	}
}

// The user's Wi-Fi switch is [SYSSET] WIFI in stock's config.ini; anything else reads as on.
func TestWiFiSwitchedOff(t *testing.T) {
	old := ConfigPath
	t.Cleanup(func() { ConfigPath = old })
	ConfigPath = filepath.Join(t.TempDir(), "config.ini")
	for _, c := range []struct {
		text string
		off  bool
	}{
		{"[SYSSET]\nWIFI=0\n", true},
		{"[IPOD]\nLIGHT=2\r\n[SYSSET]\r\nTIMEZONE=22\r\nWIFI = 0\r\n", true},
		{"[SYSSET]\nWIFI=1\n", false},
		{"[Q2POD]\nWIFI=0\n[SYSSET]\nTIMEZONE=1\n", false}, // another section's key
		{"", false},
	} {
		os.WriteFile(ConfigPath, []byte(c.text), 0o644)
		if got := WiFiSwitchedOff(); got != c.off {
			t.Errorf("%q: %v", c.text, got)
		}
	}
	os.Remove(ConfigPath)
	if WiFiSwitchedOff() {
		t.Error("no file")
	}
}
