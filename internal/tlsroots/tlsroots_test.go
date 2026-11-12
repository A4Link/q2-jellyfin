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
	srv := dateServer(&hits, false)
	defer srv.Close()
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

func TestNowFetchFailureFallsBack(t *testing.T) {
	fakeClock(t)
	SetDateURL("http://127.0.0.1:1/")
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header()["Date"] = nil // no Date at all
	}))
	defer srv.Close()
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(got)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
	}))
	defer srv.Close()
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
// before the build (any build is 2025 or later).
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
}
