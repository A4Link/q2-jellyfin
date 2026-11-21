package jellyfin

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

var (
	ErrAuth         = errors.New("wrong username or password")
	ErrUnreachable  = errors.New("server unreachable")
	ErrCert         = errors.New("certificate not trusted")
	ErrNotJellyfin  = errors.New("not a Jellyfin server")
	ErrInsecure     = errors.New("https required")
	ErrRedirect     = errors.New("redirect refused")                    // to plain http, other than a plain server to itself
	ErrOtherNetwork = errors.New("not the plain http server's network") // the app's check (cmd: homenet.go)
	ErrNotAllowed   = errors.New("not on the http allow list")          // the app's check (cmd: httpallow.go)
	ErrNoHTTPS      = errors.New("no https on this port")               // a plain http server's port typed as https
	// ErrAllowUnread is an allow list with lines but no address the app can read (UTF-16, a
	// typo); it is ErrNotAllowed too.
	ErrAllowUnread = fmt.Errorf("no address read in the http allow list: %w", ErrNotAllowed)
	// ErrQuickConnectOff is a server whose admin hasn't turned Quick Connect on.
	ErrQuickConnectOff = errors.New("quick connect is turned off on the server")
	// ErrQuickConnectExpired is a code the server no longer knows (it expired, or the server restarted).
	ErrQuickConnectExpired = errors.New("quick connect code expired")
	// ErrBadKey is an API key the server refuses; it is ErrAuth too.
	ErrBadKey = fmt.Errorf("API key not accepted: %w", ErrAuth)
	// ErrNoSuchUser is a user name the server doesn't know (API-key sign-in).
	ErrNoSuchUser = errors.New("no such user")
	// ErrDownloadsOff is a download the server refuses for this account; it is ErrAuth too, so the
	// syncer stops on it as on any other refusal about the account.
	ErrDownloadsOff = fmt.Errorf("downloads turned off for this account: %w", ErrAuth)
	// ErrStalled is a body that stopped arriving; it is ErrUnreachable too.
	ErrStalled = fmt.Errorf("server stopped sending: %w", ErrUnreachable)
	// ErrChanged is a list whose total changed between pages: an item may have been skipped.
	ErrChanged = errors.New("the library changed while it was read")
	// ErrNoMusic is an account that sees no music library: an empty list would read as every
	// song gone, and a sync would remove them all.
	ErrNoMusic = errors.New("no music library for this account")
)

// CodeGone is APIError.Code for an item the server says isn't there (HTTP 404 from Jellyfin
// itself on an item, a download or a stream). Jellyfin has no error codes of its own; the app's
// Details row and the syncer's no-retry rule read this one.
const CodeGone = 70

// APIError keeps what the Details row shows. It unwraps to one of the Err values when the
// failure has a known kind.
type APIError struct {
	Op   string
	HTTP int
	Code int
	Msg  string
	kind error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: http %d code %d: %s", e.Op, e.HTTP, e.Code, e.Msg)
}
func (e *APIError) Unwrap() error { return e.kind }

// statusError is the APIError for an HTTP status Jellyfin answered instead of success.
// TODO(verify): confirm 403 is what a user without the download permission gets from
// /Items/{id}/Download on the Jellyfin versions you support.
func statusError(op string, status int) *APIError {
	e := &APIError{Op: op, HTTP: status, Msg: http.StatusText(status)}
	switch {
	case status == http.StatusUnauthorized:
		e.kind = ErrAuth
	case status == http.StatusForbidden && op == "Download":
		e.kind = ErrDownloadsOff
	case status == http.StatusForbidden:
		e.kind = ErrAuth
	case status == http.StatusNotFound && (op == "Items" || op == "Download" || op == "stream" || op == "universal"):
		e.Code = CodeGone
	default:
		e.kind = ErrUnreachable
	}
	return e
}

// kindOn gives err the kind k when the server answered one of status: what that answer means to
// the one request that got it (a password refused, a Quick Connect code expired).
func kindOn(err, k error, status ...int) error {
	var ae *APIError
	if errors.As(err, &ae) && slices.Contains(status, ae.HTTP) {
		ae.kind = k
	}
	return err
}

// listErr is err from a list whose 404 says no item was deleted (an artist's albums, a library's
// folders): an HTTP error, not CodeGone, so its playlist or artist doesn't leave Sync.
func listErr(err error) error {
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == CodeGone {
		e := *ae
		e.Code, e.kind = 0, ErrUnreachable
		return &e
	}
	return err
}

func transportKind(err error) error {
	switch {
	case errors.Is(err, ErrInsecure):
		return ErrInsecure
	case errors.Is(err, ErrRedirect):
		return ErrRedirect
	case errors.Is(err, ErrOtherNetwork):
		return ErrOtherNetwork
	case errors.Is(err, ErrAllowUnread):
		return ErrAllowUnread
	case errors.Is(err, ErrNotAllowed):
		return ErrNotAllowed
	case noHTTPS(err):
		return ErrNoHTTPS
	case isCert(err):
		var ci x509.CertificateInvalidError
		if errors.As(err, &ci) && ci.Reason == x509.Expired && !timeKnown() {
			return ErrUnreachable // the clock is unset and the server's time couldn't be read
		}
		return ErrCert
	}
	return ErrUnreachable
}

// noHTTPS reports whether err is a server that answered the TLS hello in plain HTTP: Jellyfin's own
// port 8096, say, typed by name (which means https://).
func noHTTPS(err error) bool {
	var rh tls.RecordHeaderError
	return errors.Is(err, http.ErrSchemeMismatch) || errors.As(err, &rh)
}

// timeKnown is tlsroots.Known, injectable for tests.
var timeKnown = tlsroots.Known

// isCert reports whether err is a certificate verification failure.
func isCert(err error) bool {
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	var cv *tls.CertificateVerificationError
	return errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &he) || errors.As(err, &cv)
}

// Message maps err to the user-facing copy.
func Message(err error, host string) string {
	var ae *APIError
	switch {
	case errors.Is(err, ErrDownloadsOff):
		return "Downloads are turned off for this account on the server"
	case errors.Is(err, ErrQuickConnectOff):
		return "Quick Connect is off on this server. Sign in with a password instead"
	case errors.Is(err, ErrQuickConnectExpired):
		return "The Quick Connect code expired. Try again"
	case errors.Is(err, ErrBadKey):
		return "API key not accepted, or not an admin key"
	case errors.Is(err, ErrNoSuchUser):
		return "No user by that name on the server"
	case errors.As(err, &ae) && (ae.HTTP == http.StatusUnauthorized || ae.HTTP == http.StatusForbidden) && ae.Op != "AuthenticateByName":
		// a token the server stopped taking (the Q2 revoked, the server rebuilt): no password was
		// typed, and only a new sign-in helps. A list of a library the account lost answers 401
		// too; the Sync preview then asks CheckMusic and says ErrNoMusic when none is left
		return "Server refused this Q2. Sign in again: Settings → Account → Sign Out"
	case errors.Is(err, ErrAuth):
		return "Wrong username or password"
	case errors.Is(err, ErrCert):
		return "Certificate not trusted"
	case errors.Is(err, ErrStalled):
		return "The server stopped sending"
	case errors.Is(err, ErrNotJellyfin):
		return "Not a Jellyfin server"
	case errors.Is(err, ErrChanged):
		return "The library changed while it was read. Try again"
	case errors.Is(err, ErrNoMusic):
		return "No music library on the server for this account"
	case errors.As(err, &ae) && ae.Code == CodeGone:
		return "Not found on the server"
	case errors.As(err, &ae) && ae.HTTP != 0 && (ae.HTTP < 200 || ae.HTTP > 299) && errors.Is(err, ErrUnreachable):
		return fmt.Sprintf("The server answered HTTP %d", ae.HTTP)
	case errors.Is(err, ErrUnreachable):
		return "Can't reach " + host
	case errors.Is(err, ErrInsecure):
		return "HTTPS is required. Use an https:// address"
	case errors.Is(err, ErrRedirect):
		return "The server redirected to another address. Check the address and port"
	case errors.Is(err, ErrOtherNetwork):
		return "Not this server's home Wi-Fi. New Wi-Fi at home? Type its address again there"
	case errors.Is(err, ErrAllowUnread):
		return "http-allow.txt has no address the app can read"
	case errors.Is(err, ErrNotAllowed):
		return "Not on http-allow.txt: nothing sent"
	case errors.Is(err, ErrNoHTTPS):
		return "This port has no HTTPS. At home, type the server's IP and port"
	}
	if errors.As(err, &ae) && ae.Msg != "" {
		return ae.Msg
	}
	return err.Error()
}
