package jellyfin

import (
	"context"
	"errors"
	"net/url"
	"time"
)

// ---- playback reporting ----
//
// Checked on a live 12.4.0 server (TestLiveReport): POST /Sessions/Playing counts a play of an
// audio item at once (PlayCount + 1, Played, LastPlayedDate now); /Sessions/Playing/Stopped then
// changes no count; POST /UserPlayedItems/{id}?datePlayed= counts one more play on every call and
// keeps datePlayed as the last-played date; an item the server doesn't know is 404 (an id it
// can't read, 400).

// MarkPlayed records a play of item id at at (POST /UserPlayedItems/{id}): a song played away
// from the server, reported later. Every call counts one more play.
func (c *Client) MarkPlayed(ctx context.Context, id string, at time.Time) error {
	q := url.Values{"datePlayed": {at.UTC().Format(time.RFC3339)}}
	if c.c.UserID != "" {
		q.Set("userId", c.c.UserID)
	}
	return c.call(ctx, "POST", "/UserPlayedItems/"+url.PathEscape(id), q, nil, nil)
}

// LastPlayed is when the user last played item id, as the server has it (GET /Items/{id}): zero
// when never, or when the date can't be read. A play marked with MarkPlayed has its datePlayed
// here, to the second. The date may come without a zone: it is UTC.
func (c *Client) LastPlayed(ctx context.Context, id string) (time.Time, error) {
	var it struct {
		UserData struct{ LastPlayedDate string }
	}
	q := url.Values{}
	if c.c.UserID != "" {
		q.Set("userId", c.c.UserID)
	}
	if err := c.call(ctx, "GET", "/Items/"+url.PathEscape(id), q, nil, &it); err != nil {
		return time.Time{}, err
	}
	s := it.UserData.LastPlayedDate
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.UTC)
	}
	return t, nil
}

// SetFavorite makes item id the user's favourite (POST /UserFavoriteItems/{id}), or not (DELETE),
// whatever it is now: the call is the same however often it is made. A server that doesn't know
// the item answers 404.
func (c *Client) SetFavorite(ctx context.Context, id string, on bool) error {
	q := url.Values{}
	if c.c.UserID != "" {
		q.Set("userId", c.c.UserID)
	}
	method := "DELETE"
	if on {
		method = "POST"
	}
	return c.call(ctx, method, "/UserFavoriteItems/"+url.PathEscape(id), q, nil, nil)
}

type playbackInfo struct {
	ItemID        string `json:"ItemId"`
	PositionTicks int64  `json:"PositionTicks"`
	PlayMethod    string `json:"PlayMethod,omitempty"`
	IsPaused      bool   `json:"IsPaused,omitempty"`
}

// PlaybackStarted tells the server item id starts playing now on this device (POST
// /Sessions/Playing), as Jellyfin's own apps do: it shows in the server's sessions, and the play
// counts. A server API key belongs to no user, so with one the play is marked instead (MarkPlayed).
func (c *Client) PlaybackStarted(ctx context.Context, id string) error {
	if c.KeyAuth() {
		return c.MarkPlayed(ctx, id, time.Now())
	}
	return c.call(ctx, "POST", "/Sessions/Playing", nil, playbackInfo{ItemID: id, PlayMethod: "DirectStream"}, nil)
}

// PlaybackProgress tells the server item id, started with PlaybackStarted, is at pos, paused or
// not (POST /Sessions/Playing/Progress): its session shows it; no count changes.
func (c *Client) PlaybackProgress(ctx context.Context, id string, pos time.Duration, paused bool) error {
	if c.KeyAuth() {
		return nil
	}
	return c.call(ctx, "POST", "/Sessions/Playing/Progress", nil, playbackInfo{ItemID: id, PositionTicks: int64(pos / 100), IsPaused: paused}, nil)
}

// PlaybackStopped tells the server item id stopped at pos (POST /Sessions/Playing/Stopped).
func (c *Client) PlaybackStopped(ctx context.Context, id string, pos time.Duration) error {
	if c.KeyAuth() {
		return nil
	}
	return c.call(ctx, "POST", "/Sessions/Playing/Stopped", nil, playbackInfo{ItemID: id, PositionTicks: int64(pos / 100)}, nil)
}

// Refused reports whether err is the server's final word on a report: an item it doesn't know
// (404) or a request it can't read (400). Sending it again changes nothing. A refusal of the
// sign-in (401, 403), an error of the server's own (5xx) or no answer may pass.
func Refused(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.HTTP == 400 || ae.HTTP == 404)
}
