package q2

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Network names the network the Q2 is on: its Wi-Fi's name (SSID), from wpa_supplicant, and its
// gateway's MAC address, from the kernel's routing and ARP tables. Either is "" when it can't be
// read. The gateway's MAC is the same on every access point and band of a home network, so another
// network (a hotel's, with the same 192.168.x addresses) has another. Neither is a secret (a MAC is
// in every Wi-Fi frame's clear header), so both are checked: a copy of both is a targeted attack.
type Network struct {
	SSID    string `json:"ssid,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// Known reports whether anything about the network could be read.
func (n Network) Known() bool { return n.SSID != "" || n.Gateway != "" }

// Same reports whether now is the network n was read on: n knows its gateway's MAC (a Wi-Fi name
// alone is anyone's to copy) and every part n knows matches now's. A part now can't read counts
// as different.
func (n Network) Same(now Network) bool {
	return n.Gateway != "" && n.Gateway == now.Gateway && (n.SSID == "" || n.SSID == now.SSID)
}

// Unsure reports whether now may still be n: no part read now differs from n's, but a part n
// knows can't be read now (the gateway's ARP answer late after Wi-Fi wakes, say).
func (n Network) Unsure(now Network) bool {
	return !n.Same(now) && (now.Gateway == "" || n.Gateway == "" || n.Gateway == now.Gateway) &&
		(now.SSID == "" || n.SSID == "" || n.SSID == now.SSID)
}

// Where the Q2's system keeps them: stock wpa_supplicant's control sockets (ctrl_interface in its
// wpa_supplicant.conf, which the stock player's own Wi-Fi page asks too), and the kernel's tables.
var (
	WPADir    = "/var/run/wpa_supplicant"
	RouteFile = "/proc/net/route"
	ARPFile   = "/proc/net/arp"
)

// CurrentNetwork reads the network the Q2 is on now.
func CurrentNetwork() Network {
	return Network{SSID: wifiSSID(), Gateway: gatewayMAC()}
}

var wpaSeq atomic.Int64

// wifiSSID is the SSID wpa_supplicant is connected to, asked over its control socket (STATUS), or
// "" when it isn't connected or can't be asked.
func wifiSSID() string {
	ents, _ := os.ReadDir(WPADir)
	for _, e := range ents { // a socket per interface: wlan0 on the Q2
		if s := wpaStatus(filepath.Join(WPADir, e.Name()))["ssid"]; s != "" {
			return s
		}
	}
	return ""
}

// wpaStatus is a control socket's STATUS answer as its key=value lines, or nil. Each request binds
// its own reply socket, so requests at once don't take each other's answers.
func wpaStatus(path string) map[string]string {
	local := filepath.Join(os.TempDir(), fmt.Sprintf("q2-jellyfin-wpa-%d-%d", os.Getpid(), wpaSeq.Add(1)))
	c, err := net.DialUnix("unixgram", &net.UnixAddr{Name: local, Net: "unixgram"}, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil
	}
	defer os.Remove(local)
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("STATUS")); err != nil {
		return nil
	}
	b := make([]byte, 4096)
	n, err := c.Read(b)
	if err != nil {
		return nil
	}
	st := map[string]string{}
	for _, line := range strings.Split(string(b[:n]), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			st[k] = v
		}
	}
	if st["wpa_state"] != "COMPLETED" { // not connected: its last ssid isn't this network's
		return nil
	}
	return st
}

// gatewayMAC is the default gateway's MAC address, or "". An entry not yet (or no longer) in the
// ARP table is asked for by sending the gateway one UDP packet (the discard port), which makes the
// kernel look its MAC up, waiting past the kernel's first resend.
func gatewayMAC() string {
	gw := defaultGateway()
	if gw == "" {
		return ""
	}
	if mac := arpMAC(gw); mac != "" {
		return mac
	}
	if c, err := net.DialTimeout("udp", net.JoinHostPort(gw, "9"), time.Second); err == nil {
		c.Write([]byte{0})
		c.Close()
	}
	for range 30 { // 1.5 s: the kernel sends its ARP request again after 1 s
		time.Sleep(50 * time.Millisecond)
		if mac := arpMAC(gw); mac != "" {
			return mac
		}
	}
	return ""
}

// defaultGateway is the IPv4 default route's gateway in /proc/net/route, or "".
func defaultGateway() string {
	f, err := os.Open(RouteFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() { // Iface Destination Gateway Flags ...: hex, the gateway's bytes in host (little-endian) order
		x := strings.Fields(sc.Text())
		if len(x) < 4 || x[1] != "00000000" {
			continue
		}
		g, err1 := strconv.ParseUint(x[2], 16, 32)
		fl, err2 := strconv.ParseUint(x[3], 16, 32)
		if err1 != nil || err2 != nil || fl&2 == 0 || g == 0 { // RTF_GATEWAY
			continue
		}
		return net.IPv4(byte(g), byte(g>>8), byte(g>>16), byte(g>>24)).String()
	}
	return ""
}

// arpMAC is ip's MAC in /proc/net/arp when the entry is complete, or "".
func arpMAC(ip string) string {
	f, err := os.Open(ARPFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() { // IP address, HW type, Flags, HW address, Mask, Device
		x := strings.Fields(sc.Text())
		if len(x) < 4 || x[0] != ip {
			continue
		}
		fl, err := strconv.ParseUint(strings.TrimPrefix(x[2], "0x"), 16, 32)
		if err == nil && fl&2 != 0 && x[3] != "00:00:00:00:00:00" { // ATF_COM
			return strings.ToLower(x[3])
		}
	}
	return ""
}
