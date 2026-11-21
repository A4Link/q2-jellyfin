package main

import (
	"bufio"
	"bytes"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/A4Link/q2-jellyfin/internal/q2"
)

// The allow list (U54), allowFile on the card, editable from a computer: the addresses that may be
// plain http. A range (CIDR) is where a bare IP and port typed on the Q2 means http://; an IP and
// port is a server the user said Yes to (the Q2 adds it after warning that it is served over HTTP,
// so each new one is asked once). An http:// typed in full may be any IPv4 address and port, asked
// the same way, with a stronger warning outside the private ranges. A missing file is written
// again with the defaults; a file with no address at all turns plain http off.

const allowFile = "http-allow.txt"

const allowDefaults = `# Plain HTTP (NOT encrypted) is allowed only to the addresses below. One per line:
#   a range, e.g. 192.168.0.0/16: an IP and port in it typed on the Q2 means http://
#   an IP and port, e.g. 192.168.1.10:8096: a server you said Yes to on the Q2 (it adds them)
# Each new server is asked once on the Q2, with a warning. Lines starting with # are ignored.
# Leave no address at all to turn plain HTTP off. Delete the file to get these defaults back.
10.0.0.0/8
172.16.0.0/12
192.168.0.0/16
`

// privateNets are the ranges whose servers get the ordinary warning: home networks (RFC 1918) and
// Tailscale's (CGNAT), which the Q2 reaches through the user's own router (it can't run Tailscale).
// Anywhere else is the internet.
var privateNets = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10")}

type allowList struct {
	ranges  []netip.Prefix
	servers map[netip.AddrPort]bool
	skipped bool // a line was skipped (UTF-16, a typo)
}

// off reports whether the file allows nothing: plain http is turned off.
func (a allowList) off() bool { return len(a.ranges) == 0 && len(a.servers) == 0 }

// offMsg is why plain http is off: turned off, or no line could be read.
func (a allowList) offMsg() string {
	if a.skipped {
		return "http-allow.txt has no address the app can read"
	}
	return plainOff
}

// inRangeAddr reports whether ip is in a range of the list.
func (a allowList) inRangeAddr(ip netip.Addr) bool {
	for _, p := range a.ranges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// inRange reports whether a bare IP and port typed means http://.
func (a allowList) inRange(ap netip.AddrPort) bool {
	return a.servers[ap] || a.inRangeAddr(ap.Addr())
}

// allowList is allowFile as last read, read again when its time or size changes (edited on a
// computer while the app runs) or the last read failed. A missing file whose defaults couldn't
// be written is tried once: its defaults are kept until the file is there.
func (d *daemon) allowList() allowList {
	d.allowMu.Lock()
	defer d.allowMu.Unlock()
	fi, err := os.Stat(filepath.Join(d.dir, allowFile))
	gone := os.IsNotExist(err)
	if d.allow == nil || gone && !d.allowGone || !gone && (d.allowFI == nil || err != nil || !fi.ModTime().Equal(d.allowFI.ModTime()) || fi.Size() != d.allowFI.Size()) {
		a, ok, unwritten := d.readAllow()
		d.allow, d.allowFI, d.allowGone = &a, nil, unwritten
		if ok && err == nil {
			d.allowFI = fi
		}
	}
	return *d.allow
}

// readAllow reads allowFile, writing the defaults first when it is missing, and reports whether
// it could be read and whether it was missing and the defaults couldn't be written. A line that
// isn't a range or an IPv4 address and port is logged and skipped.
func (d *daemon) readAllow() (a allowList, ok, unwritten bool) {
	p := filepath.Join(d.dir, allowFile)
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		b = []byte(allowDefaults)
		if err := q2.WriteFile(p, b); err != nil {
			d.log.Printf("http allow list: can't write the defaults: %v", err)
			unwritten = true
		}
	} else if err != nil {
		d.log.Printf("http allow list: can't read it: %v", err)
		return allowList{}, false, false // plain http off until it can be read
	}
	b = bytes.TrimPrefix(b, []byte("\uFEFF")) // a BOM (Notepad's "UTF-8 with BOM")
	a = allowList{servers: map[netip.AddrPort]bool{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pr, err := netip.ParsePrefix(line); err == nil && pr.Addr().Is4() {
			a.ranges = append(a.ranges, pr.Masked())
		} else if ap, err := netip.ParseAddrPort(line); err == nil && ap.Addr().Is4() {
			a.servers[ap] = true
		} else if ip, err := netip.ParseAddr(line); err == nil && ip.Is4() {
			a.ranges = append(a.ranges, netip.PrefixFrom(ip, 32))
		} else {
			d.log.Printf("http allow list: line %d skipped: not an IPv4 range, address or address:port", n)
			a.skipped = true
		}
	}
	return a, true, unwritten
}

// allowServer adds plain http server's IP and port to allowFile, once.
func (d *daemon) allowServer(server string) error {
	ap, ok := addrPort(server)
	if !ok || d.allowList().servers[ap] {
		return nil
	}
	p := filepath.Join(d.dir, allowFile)
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) { // deleted, or its defaults couldn't be written: keep them
		b = []byte(allowDefaults)
	} else if err != nil { // can't be read: leave it as it is
		return err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	err = q2.WriteFile(p, append(b, ap.String()+"\n"...))
	d.allowMu.Lock()
	d.allow = nil // read again
	d.allowMu.Unlock()
	return err
}

// allowed reports whether plain http server is a server said Yes to.
func (d *daemon) allowed(server string) bool { return d.allowList().has(server) }

// has reports whether plain http server is a server said Yes to in the list.
func (a allowList) has(server string) bool {
	ap, ok := addrPort(server)
	return ok && a.servers[ap]
}

// addrPort is a server address's IPv4 address and port, as the allow list writes them.
func addrPort(server string) (netip.AddrPort, bool) {
	u, err := url.Parse(server)
	if err != nil {
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(u.Host)
	return ap, err == nil && ap.Addr().Is4()
}

// onInternet reports whether a plain http server is outside privateNets.
func onInternet(server string) bool {
	ap, ok := addrPort(server)
	if !ok {
		return true
	}
	for _, p := range privateNets {
		if p.Contains(ap.Addr()) {
			return false
		}
	}
	return true
}
