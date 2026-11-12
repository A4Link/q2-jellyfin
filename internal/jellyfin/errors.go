package jellyfin

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"

	"github.com/A4Link/q2-jellyfin/internal/tlsroots"
)

var (
	ErrAuth        = errors.New("wrong username or password")
	ErrUnreachable = errors.New("server unreachable")
	ErrCert        = errors.New("certificate not trusted")
	ErrNotJellyfin = errors.New("not a Jellyfin server")
	ErrInsecure    = errors.New("https required")
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
	case errors.As(err, &ae) && ae.Code == CodeGone:
		return "Not found on the server"
	case errors.As(err, &ae) && ae.HTTP != 0 && (ae.HTTP < 200 || ae.HTTP > 299) && errors.Is(err, ErrUnreachable):
		return fmt.Sprintf("The server answered HTTP %d", ae.HTTP)
	case errors.Is(err, ErrUnreachable):
		return "Can't reach " + host
	case errors.Is(err, ErrInsecure):
		return "HTTPS is required. Use an https:// address"
	}
	if errors.As(err, &ae) && ae.Msg != "" {
		return ae.Msg
	}
	return err.Error()
}
