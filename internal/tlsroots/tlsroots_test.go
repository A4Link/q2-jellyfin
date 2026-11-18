package tlsroots

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeClock(t *testing.T) {
	t.Helper()
	nowFn = func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }
	SetDateURL("")
	t.Cleanup(func() { nowFn = time.Now; SetDateURL("") })
}

func dateServer(hits *atomic.Int32, tls bool) *httptest.Server {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
	})
	if tls {
		return httptest.NewTLSServer(h)
	}
	return httptest.NewServer(h)
}

func TestNowFromDate(t *testing.T) {
	fakeClock(t)
	var hits atomic.Int32
	srv := dateServer(&hits, true)
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	got := Now()
	if got.Year() != 2026 || got.Month() != 10 || got.Day() != 27 {
		t.Fatalf("Now() = %v", got)
	}
	Now()
	if hits.Load() != 1 {
		t.Fatalf("offset not cached: %d fetches", hits.Load())
	}
}

func TestNowFromDateOverVerifiedTLS(t *testing.T) {
	var hits atomic.Int32
	srv := dateServer(&hits, true)
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	resp, err := (&http.Client{Transport: dateTransport(roots, "127.0.0.1")}).Head(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// a server whose cert is not in the roots is refused even though expiry is ignored
	if _, err := (&http.Client{Transport: dateTransport(x509.NewCertPool(), "127.0.0.1")}).Head(srv.URL); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestClientVerifiesCert(t *testing.T) {
	var hits atomic.Int32
	srv := dateServer(&hits, true)
	defer srv.Close()
	bad := &http.Client{Transport: newTransport(x509.NewCertPool(), time.Second)}
	if resp, err := bad.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("cert outside the pool accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	good := &http.Client{Transport: newTransport(roots, time.Second)}
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// A new connection resumes the TLS session instead of a full handshake (slow on MIPS).
func TestClientResumesTLS(t *testing.T) {
	var hits atomic.Int32
	srv := dateServer(&hits, true)
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	tr := newTransport(roots, time.Second)
	cl := &http.Client{Transport: tr}
	for i := range 2 {
		resp, err := cl.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if i == 1 && !resp.TLS.DidResume {
			t.Fatal("second connection did not resume the TLS session")
		}
		tr.CloseIdleConnections()
	}
}

func TestNowFetchFailureFallsBack(t *testing.T) {
	fakeClock(t)
	SetDateURL("https://127.0.0.1:1/")
	start := time.Now()
	got := Now()
	if time.Since(start) > 3*time.Second {
		t.Fatal("Now stalled")
	}
	if got.Year() != 2000 {
		t.Fatalf("Now() = %v, want system time", got)
	}
}

func TestNowBackoff(t *testing.T) {
	fakeClock(t)
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header()["Date"] = nil // no Date at all
	}))
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	Now()
	Now()
	if hits.Load() != 1 {
		t.Fatalf("refetched within backoff: %d", hits.Load())
	}
	base := nowFn()
	nowFn = func() time.Time { return base.Add(retryAfter + time.Second) }
	Now()
	if hits.Load() != 2 {
		t.Fatalf("no retry after backoff: %d", hits.Load())
	}
}

func TestPool(t *testing.T) {
	if len(Pool().Subjects()) < 100 {
		t.Fatal("pool too small")
	}
}

// bundleDate is the day of cacert.pem's "as of" line, never later: a new bundle fails this until
// bundleDate moves with it.
func TestBundleDate(t *testing.T) {
	_, line, ok := strings.Cut(string(bundle), "Certificate data from Mozilla as of: ")
	line, _, _ = strings.Cut(line, "\n")
	asOf, err := time.Parse("Mon Jan _2 15:04:05 2006 MST", line)
	if !ok || err != nil {
		t.Fatalf("no as-of date in cacert.pem: %q %v", line, err)
	}
	if bundleDate.After(asOf) || asOf.Sub(bundleDate) >= 24*time.Hour {
		t.Fatalf("bundleDate %v, cacert.pem as of %v", bundleDate, asOf)
	}
}

// trustTestCert makes the Date fetch trust srv's certificate (httptest's: 127.0.0.1, ::1, example.com).
func trustTestCert(t *testing.T, srv *httptest.Server) {
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	dateRoots = func() *x509.CertPool { return roots }
	t.Cleanup(func() { dateRoots = Pool })
}

// M6: a server given by IP address is checked against the certificate's IPs too: a certificate
// for other names or IPs can't set the clock.
func TestDateFetchChecksIPHost(t *testing.T) {
	fakeClock(t)
	ln, err := net.Listen("tcp", "127.0.0.2:0") // not in httptest's certificate
	if err != nil {
		t.Skip("no 127.0.0.2:", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Mon, 01 Jan 2052 00:00:00 GMT")
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.StartTLS()
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	if got := Now(); got.Year() != 2000 {
		t.Fatalf("Now() = %v from a certificate without 127.0.0.2", got)
	}
}

// The Date is read only over HTTPS: a plain http:// server (the card's .jellyfin/server can say
// anything) has no certificate to vouch for it, so anyone on the Wi-Fi could set the clock.
func TestDateFetchHTTPSOnly(t *testing.T) {
	fakeClock(t)
	var hits atomic.Int32
	srv := dateServer(&hits, false)
	defer srv.Close()
	SetDateURL(srv.URL)
	if got := Now(); got.Year() != 2000 || Known() {
		t.Fatalf("Now() = %v from plain http", got)
	}
	if hits.Load() != 0 {
		t.Fatalf("plain http asked: %d", hits.Load())
	}
}

// M7: the Date fetch never follows a redirect (to http:// least of all); the 3xx's own Date is used.
func TestDateFetchNoRedirect(t *testing.T) {
	fakeClock(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Mon, 01 Jan 2052 00:00:00 GMT")
	}))
	defer plain.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	if got := Now(); got.Year() != 2026 {
		t.Fatalf("Now() = %v", got)
	}
}

// M8: while the first Date fetch runs, other callers wait for it rather than get the unset clock
// (which fails their handshake as "Certificate not trusted").
func TestNowWaitsForFetch(t *testing.T) {
	fakeClock(t)
	got := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(got)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
	}))
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	go Now()
	<-got
	if now := Now(); now.Year() != 2026 {
		t.Fatalf("second caller: %v", now)
	}
	if !Known() {
		t.Fatal("Known() false once the Date was read")
	}
}

// testCert makes a self-signed certificate for 127.0.0.1, valid from notBefore to notAfter.
func testCert(t *testing.T, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// certDateServer serves date as its Date header over HTTPS with cert, and makes the Date fetch
// trust cert.
func certDateServer(t *testing.T, cert tls.Certificate, date string) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date)
		w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	dateRoots = func() *x509.CertPool { return roots }
	t.Cleanup(func() { dateRoots = Pool })
	return srv
}

// With the clock unset, a Date is taken only from a certificate valid at that Date, and never one
// before the CA bundle (any build is later).
func TestPastDateNeedsCertValidThen(t *testing.T) {
	fakeClock(t)
	day := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	good := certDateServer(t, testCert(t, day(2025), day(2030)), "Tue, 27 Oct 2026 10:00:00 GMT")
	SetDateURL(good.URL)
	if got := Now(); got.Year() != 2026 {
		t.Fatalf("a valid certificate's Date refused: %v", got)
	}
	expired := certDateServer(t, testCert(t, day(2020), day(2021)), "Tue, 27 Oct 2026 10:00:00 GMT")
	SetDateURL(expired.URL)
	if got := Now(); got.Year() != 2000 || Known() {
		t.Errorf("Date taken from a certificate expired in 2021: %v", got)
	}
	early := certDateServer(t, testCert(t, day(2000), day(2030)), "Mon, 01 Jan 2001 00:00:00 GMT")
	SetDateURL(early.URL)
	if got := Now(); got.Year() != 2000 || Known() {
		t.Errorf("a Date before the build taken: %v", got)
	}
	// An expired certificate (its key may have leaked) can't set the clock back to when it was
	// valid, even after 2025: no Date before the CA bundle is believed.
	old := certDateServer(t, testCert(t, day(2025), day(2026)), "Thu, 01 May 2025 00:00:00 GMT")
	SetDateURL(old.URL)
	if got := Now(); got.Year() != 2000 || Known() {
		t.Errorf("a Date before the CA bundle taken: %v", got)
	}
}

// A clock set before the CA bundle was made is as good as unset: every build is later.
func TestClockBeforeBundleUnset(t *testing.T) {
	fakeClock(t)
	nowFn = func() time.Time { return time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC) }
	if Known() {
		t.Fatal("a clock before the CA bundle is trusted")
	}
}

// An answer's headers are capped at 64 KB (Go's default is 10 MB): a 100 KB header fails the request.
func TestBigHeaderRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("a", 100<<10))
	}))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	for _, tr := range []*http.Transport{newTransport(roots, time.Second), dateTransport(roots, "127.0.0.1")} {
		if resp, err := (&http.Client{Transport: tr}).Get(srv.URL); err == nil {
			resp.Body.Close()
			t.Fatal("a 100 KB header accepted")
		}
	}
}
