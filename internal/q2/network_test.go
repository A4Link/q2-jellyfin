package q2

import (
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// sockDir is a new empty directory under /tmp for a test's Unix sockets, and TMPDIR (where
// wpaStatus binds its reply socket) for the test; removed after it. A socket's path holds at most
// 108 bytes, which t.TempDir() under a long TMPDIR goes past.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "qj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	t.Setenv("TMPDIR", d)
	return d
}

// The network is read from wpa_supplicant's STATUS (connected only) and the kernel's tables: the
// default route's gateway (little-endian hex) and its complete ARP entry.
func TestCurrentNetwork(t *testing.T) {
	dir := sockDir(t)
	RouteFile, ARPFile, WPADir = filepath.Join(dir, "route"), filepath.Join(dir, "arp"), filepath.Join(dir, "wpa")
	t.Cleanup(func() {
		RouteFile, ARPFile, WPADir = "/proc/net/route", "/proc/net/arp", "/var/run/wpa_supplicant"
	})
	os.WriteFile(RouteFile, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n"+
		"wlan0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n"+
		"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\n"), 0o644)
	os.WriteFile(ARPFile, []byte("IP address       HW type     Flags       HW address            Mask     Device\n"+
		"192.168.1.20     0x1         0x0         00:00:00:00:00:00     *        wlan0\n"+
		"192.168.1.1      0x1         0x2         AA:BB:CC:DD:EE:FF     *        wlan0\n"), 0o644)
	os.Mkdir(WPADir, 0o755)
	var state atomic.Value
	state.Store("COMPLETED")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(WPADir, "wlan0"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		b := make([]byte, 64)
		for {
			n, from, err := l.ReadFromUnix(b)
			if err != nil {
				return
			}
			if string(b[:n]) == "STATUS" {
				l.WriteToUnix([]byte("bssid=11:22:33:44:55:66\nssid=Home Net\nkey_mgmt=WPA2-PSK\nwpa_state="+state.Load().(string)+"\n"), from)
			}
		}
	}()
	if n := CurrentNetwork(); n != (Network{SSID: "Home Net", Gateway: "aa:bb:cc:dd:ee:ff"}) {
		t.Fatalf("%+v", n)
	}
	state.Store("SCANNING") // not connected: no SSID
	if n := CurrentNetwork(); n.SSID != "" || n.Gateway != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("%+v", n)
	}
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "q2-jellyfin-wpa-*"))
	if len(matches) != 0 {
		t.Fatalf("reply sockets left: %v", matches)
	}
}

// Same: every part the kept network knows must match, a part unread now counts as different, and
// a kept network that knows nothing matches nothing.
func TestNetworkSame(t *testing.T) {
	home := Network{SSID: "Home", Gateway: "aa:bb:cc:dd:ee:ff"}
	for now, want := range map[Network]bool{
		home: true,
		{SSID: "Home", Gateway: "11:22:33:44:55:66"}:  false, // a hotel's "Home"
		{SSID: "Hotel", Gateway: "aa:bb:cc:dd:ee:ff"}: false,
		{Gateway: "aa:bb:cc:dd:ee:ff"}:                false, // the SSID couldn't be read
		{}:                                            false,
	} {
		if home.Same(now) != want {
			t.Errorf("%+v: %v", now, !want)
		}
	}
	if gw := (Network{Gateway: "aa:bb:cc:dd:ee:ff"}); !gw.Same(Network{SSID: "Any", Gateway: "aa:bb:cc:dd:ee:ff"}) {
		t.Error("kept without an SSID: the gateway decides")
	}
	if (Network{}).Same(Network{}) {
		t.Error("nothing known matched")
	}
	if (Network{SSID: "Home"}).Same(Network{SSID: "Home", Gateway: "aa:bb:cc:dd:ee:ff"}) {
		t.Error("a Wi-Fi name alone matched")
	}
}

// Unsure: nothing read differs, but a kept part can't be read now.
func TestNetworkUnsure(t *testing.T) {
	home := Network{SSID: "Home", Gateway: "aa:bb:cc:dd:ee:ff"}
	for now, want := range map[Network]bool{
		{SSID: "Home"}:                 true, // the gateway's ARP answer late
		{Gateway: "aa:bb:cc:dd:ee:ff"}: true, // wpa_supplicant not answering
		{}:                             true,
		home:                           false, // the same: sure
		{SSID: "Hotel"}:                false, // a part read differs
		{SSID: "Home", Gateway: "11:22:33:44:55:66"}: false,
	} {
		if home.Unsure(now) != want {
			t.Errorf("%+v: %v", now, !want)
		}
	}
}

// Review 0.8.2 #3: a control socket nobody listens on (wpa_supplicant stopped) leaves no reply
// socket behind: the dial binds it before it fails.
func TestWPAStaleSocket(t *testing.T) {
	dir := sockDir(t)
	WPADir = filepath.Join(dir, "wpa")
	t.Cleanup(func() { WPADir = "/var/run/wpa_supplicant" })
	os.Mkdir(WPADir, 0o755)
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(WPADir, "wlan0"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	l.Close() // its file stays
	for range 3 {
		if s := wifiSSID(); s != "" {
			t.Fatal(s)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "q2-jellyfin-wpa-*")); len(left) != 0 {
		t.Fatalf("left %q", left)
	}
}
