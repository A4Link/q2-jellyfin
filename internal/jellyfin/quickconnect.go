package jellyfin

import (
	"context"
	"errors"
	"net/url"
)

// Quick Connect: the Q2 shows a short code, the user approves it in any Jellyfin app where they are
// already signed in, and the Q2 swaps the secret for a token. No password crosses the Wi-Fi.
//
//	code, secret, err := c.QuickConnectInitiate(ctx)   // show code on screen
//	ok, err := c.QuickConnectPoll(ctx, secret)          // every few seconds until true
//	creds, err := c.QuickConnectSignIn(ctx, secret)     // keep creds on the card
//
// cmd/q2-jellyfin/login.go offers it as the first way to sign in. The server admin must enable
// Quick Connect (Dashboard → General → Quick Connect).

// QuickConnectInitiate starts a request and returns the code to show and the secret to poll with.
func (c *Client) QuickConnectInitiate(ctx context.Context) (code, secret string, err error) {
	var r struct {
		Code   string `json:"Code"`
		Secret string `json:"Secret"`
	}
	if err := c.call(ctx, "POST", "/QuickConnect/Initiate", nil, nil, &r); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.HTTP == 401 || ae.HTTP == 403) {
			ae.kind = ErrQuickConnectOff
		}
		return "", "", err
	}
	if r.Code == "" || r.Secret == "" {
		return "", "", &APIError{Op: "Initiate", Msg: "no code in the answer", kind: ErrNotJellyfin}
	}
	return r.Code, r.Secret, nil
}

// QuickConnectPoll reports whether the user has approved the code yet.
func (c *Client) QuickConnectPoll(ctx context.Context, secret string) (bool, error) {
	var r struct {
		Authenticated bool `json:"Authenticated"`
	}
	if err := c.call(ctx, "GET", "/QuickConnect/Connect", url.Values{"secret": {secret}}, nil, &r); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.HTTP == 404 { // "Unknown quick connect secret": it expired
			ae.kind = ErrQuickConnectExpired
		}
		return false, err
	}
	return r.Authenticated, nil
}

// QuickConnectSignIn swaps an approved secret for the sign-in to keep.
func (c *Client) QuickConnectSignIn(ctx context.Context, secret string) (Creds, error) {
	var r authResult
	if err := c.call(ctx, "POST", "/Users/AuthenticateWithQuickConnect", nil, map[string]string{"Secret": secret}, &r); err != nil {
		return Creds{}, err
	}
	if r.AccessToken == "" {
		return Creds{}, &APIError{Op: "AuthenticateWithQuickConnect", Msg: "no access token in the answer", kind: ErrNotJellyfin}
	}
	return c.keep(r), nil
}
