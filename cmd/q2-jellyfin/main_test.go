package main

import (
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// oldPage is the update page as the firmware reads it off the socket.
const oldPage = "Q2SPOT/1\n7\tok\t\t\ninfo\t\t" + oldFirmware + "\t\t\t\n"

// askSock sends req to the app at sock, once it listens, and returns the reply.
func askSock(t *testing.T, sock, req string) string {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 400; i++ { // until it listens
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("Q2SPOT/1\n7\t" + req + "\n"))
	c.(*net.UnixConn).CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// U45: on Q2 Pod before 1.7.3 (no "urls" in its caps) every request, the first page's or any
// other, is answered with the one page asking for an update, over the socket as the firmware asks:
// a plain info row, which that firmware draws.
func TestOldFirmwarePage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	go serveOld(sock)
	if oldFirmware != "Needs Q2 Pod 1.7.3 or later. Update Q2 Pod to use Jellyfin." {
		t.Fatalf("text %q", oldFirmware)
	}
	for _, req := range []string{"menu", "settings", "pwlogin", "qc", "sync", "play\talbum:al0 0", "clearcache"} {
		if got := askSock(t, sock, req); got != oldPage {
			t.Errorf("%q: %q", req, got)
		}
	}
}

// U45: main itself, run as the app with Q2 Pod 1.7.2's caps file, serves the update page and
// writes nothing to the card. The test binary runs again as the app (Q2_GATE_CARD set).
func TestOldFirmwareGate(t *testing.T) {
	if card := os.Getenv("Q2_GATE_CARD"); card != "" {
		flag.CommandLine = flag.NewFlagSet("q2-jellyfin", flag.ExitOnError)
		os.Args = []string{"q2-jellyfin", "-card", card, "-dir", filepath.Join(card, ".jellyfin"), "-tmp", os.Getenv("Q2_GATE_TMP")}
		main()
		return
	}
	card, tmp := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "q2-addon.caps"), []byte("playlist progress pages library goto sleep game audio\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := exec.Command(os.Args[0], "-test.run", "^TestOldFirmwareGate$")
	app.Env = append(os.Environ(), "Q2_GATE_CARD="+card, "Q2_GATE_TMP="+tmp)
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { app.Process.Kill(); app.Wait() }()
	if got := askSock(t, filepath.Join(tmp, "q2-jellyfin.browse.sock"), "menu"); got != oldPage {
		t.Fatalf("menu: %q", got)
	}
	if e, err := os.ReadDir(card); err != nil || len(e) != 0 {
		t.Fatalf("card %v %v", e, err)
	}
}
