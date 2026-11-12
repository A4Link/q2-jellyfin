// Package q2 holds what the card add-ons share with Q2 Pod's side (q2-pod's patch/addon.c):
// the status file it reads, and card I/O that never keeps the card busy, so stock's umount for USB
// storage succeeds.
package q2

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// State is a key=value status file rewritten whole (a .tmp, renamed), so a reader never sees half of
// one. Values have newlines replaced, as the payload's field() reads a line.
type State struct {
	Path string
	mu   sync.Mutex
	last string
}

// Write replaces the file with kv, in key order, when it differs from the last one written.
func (s *State) Write(kv map[string]string) error {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + strings.NewReplacer("\n", " ", "\r", " ").Replace(kv[k]) + "\n")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.String() == s.last {
		return nil
	}
	if err := WriteFile(s.Path, []byte(b.String())); err != nil {
		return err
	}
	s.last = b.String()
	return nil
}

// Remove deletes the file, as an add-on does when it exits.
func (s *State) Remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(s.Path)
	s.last = ""
}

// WriteFile writes data aside and renames it into place, synced first when it is on the card.
func WriteFile(path string, data []byte) error {
	return WriteWith(path, func(w io.Writer) error { _, err := w.Write(data); return err })
}

// WriteWith is WriteFile with the data streamed by write, so a big file needn't be held whole.
func WriteWith(path string, write func(io.Writer) error) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil && OnCard(path) {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Wipe writes zeros over the file at path, in place, and syncs it: FAT frees a deleted or replaced
// file's clusters with its bytes still in them, so a secret is wiped before the file goes. Only
// for small files: the zeros are one buffer.
func Wipe(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err == nil {
		_, err = f.Write(make([]byte, fi.Size()))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Card is where the card is mounted; tests point it elsewhere.
var Card = "/mnt/mmc"

// OnCard reports whether path is under the card's mount point.
func OnCard(path string) bool {
	rel, err := filepath.Rel(Card, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// CardMounted reports whether the card is mounted: its mount point is on another device than its
// parent. While USB storage has it, /mnt/mmc is the rootfs's own empty directory.
func CardMounted() bool {
	var a, b syscall.Stat_t
	if syscall.Stat(Card, &a) != nil || syscall.Stat(filepath.Dir(Card), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}

// ErrNoCard is a card write or read while the card is away (USB storage, or no card).
var ErrNoCard = errors.New("the card is not mounted")

// Logger logs to path in /tmp (never the card: an open log would keep it busy), starting the file
// again once it passes max bytes.
type Logger struct {
	Path string
	Max  int64
	mu   sync.Mutex
}

func (l *Logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.Path); err == nil && st.Size()+int64(len(p)) > l.Max {
		os.Rename(l.Path, l.Path+".1")
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return len(p), nil
	}
	f.Write(p)
	f.Close()
	return len(p), nil
}

// SetupLog sends the standard logger to /tmp/<name>.log (256 KB, then one older file).
func SetupLog(name string) {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetOutput(&Logger{Path: "/tmp/" + name + ".log", Max: 256 << 10})
}

// Every runs fn every d until stop is closed.
func Every(d time.Duration, stop <-chan struct{}, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			fn()
		}
	}
}

// LocalIP is the Q2's Wi-Fi address: the first IPv4 address of an interface that is up and not the
// loopback, wlan first; "" without one.
func LocalIP() string {
	ifs, _ := net.Interfaces()
	sort.SliceStable(ifs, func(i, j int) bool {
		return strings.HasPrefix(ifs[i].Name, "wlan") && !strings.HasPrefix(ifs[j].Name, "wlan")
	})
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, ad := range addrs {
			if n, ok := ad.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String()
			}
		}
	}
	return ""
}

// ConfigPath is stock's settings file, where the user's Wi-Fi switch is kept as [SYSSET] WIFI.
var ConfigPath = "/mnt/data/config.ini"

// WiFiSwitchedOff reports whether the user turned Wi-Fi off (Quick Settings or the Wi-Fi page):
// WIFI=0 under [SYSSET]. Q2 Pod's Smart Wi-Fi never writes it, so a radio it has off for idling
// still reads as on. An unreadable file, or no key, reads as on.
func WiFiSwitchedOff() bool {
	b, err := os.ReadFile(ConfigPath)
	if err != nil {
		return false
	}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && section == "SYSSET" && strings.TrimSpace(k) == "WIFI" {
			return strings.TrimSpace(v) == "0"
		}
	}
	return false
}
