package jellyfin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRequestsMatchOpenAPI checks every request the client can make against a Jellyfin OpenAPI
// document: the path and method exist, every query parameter is one the operation names (exact
// case), every enum value sent is one of the enum's, and every JSON body field is in its schema.
//
// It runs only when JELLYFIN_OPENAPI names the document, e.g. the server's own
// https://<server>/api-docs/openapi.json saved to a file:
//
//	JELLYFIN_OPENAPI=/path/to/jellyfin-openapi-stable.json go test ./internal/jellyfin -run OpenAPI
func TestRequestsMatchOpenAPI(t *testing.T) {
	file := os.Getenv("JELLYFIN_OPENAPI")
	if file == "" {
		t.Skip("set JELLYFIN_OPENAPI to a Jellyfin OpenAPI document to run")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var spec openAPI
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	reqs := exerciseClient(t)
	if len(reqs) < 15 {
		t.Fatalf("only %d requests recorded", len(reqs))
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		tmpl, op := spec.find(r.method, r.path)
		if op == nil {
			t.Errorf("%s %s: no such operation in the document", r.method, r.path)
			continue
		}
		seen[r.method+" "+tmpl] = true
		if op.Deprecated {
			t.Logf("%s %s is deprecated in the document", r.method, tmpl)
		}
		params := map[string]oaParam{}
		for _, p := range op.Parameters {
			if p.In == "query" {
				params[p.Name] = p
			}
		}
		for k, vs := range r.query {
			p, ok := params[k]
			if !ok {
				t.Errorf("%s %s: query parameter %q not in the document", r.method, tmpl, k)
				continue
			}
			if allowed := spec.enumOf(p.Schema); allowed != nil {
				for _, v := range vs {
					for _, one := range strings.Split(v, ",") {
						if !allowed[one] {
							t.Errorf("%s %s: %s=%q is not one of the enum's values", r.method, tmpl, k, one)
						}
					}
				}
			}
		}
		if len(r.body) > 0 {
			props := spec.bodyProps(op)
			for k := range r.body {
				if !props[k] {
					t.Errorf("%s %s: body field %q not in the document", r.method, tmpl, k)
				}
			}
		}
	}
	var ops []string
	for k := range seen {
		ops = append(ops, k)
	}
	sort.Strings(ops)
	t.Logf("checked %d requests over %d operations: %s", len(reqs), len(ops), strings.Join(ops, "; "))
}

type recorded struct {
	method, path string
	query        map[string][]string
	body         map[string]any
}

// exerciseClient calls every request the client can make against a server that answers each with
// just enough to go on, and returns them.
func exerciseClient(t *testing.T) []recorded {
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			json.Unmarshal(data, &rec.body)
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/System/Info/Public":
			w.Write([]byte(`{"ProductName":"Jellyfin Server"}`))
		case p == "/Users/AuthenticateByName" || p == "/Users/AuthenticateWithQuickConnect":
			w.Write([]byte(`{"AccessToken":"tok","User":{"Id":"u1","Name":"u"}}`))
		case p == "/QuickConnect/Initiate":
			w.Write([]byte(`{"Code":"123456","Secret":"sec"}`))
		case p == "/QuickConnect/Connect":
			w.Write([]byte(`{"Authenticated":true}`))
		case p == "/Users":
			w.Write([]byte(`[{"Id":"u1","Name":"u"}]`))
		case strings.HasSuffix(p, "/Lyrics"):
			w.Write([]byte(`{"Lyrics":[{"Text":"line","Start":10000000}]}`))
		case strings.HasPrefix(p, "/Audio/") || strings.HasSuffix(p, "/Download") || strings.Contains(p, "/Images/"):
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write([]byte("bytes"))
		case p == "/UserViews":
			w.Write([]byte(`{"Items":[{"Id":"lib1","Name":"Music","CollectionType":"music"}]}`))
		case strings.HasPrefix(p, "/Items/"): // a single item of the type the client asks for
			kind := map[string]string{"pl1": "Playlist", "al1": "MusicAlbum", "ar1": "MusicArtist"}[strings.TrimPrefix(p, "/Items/")]
			if kind == "" {
				kind = "Audio"
			}
			w.Write([]byte(`{"Id":"x","Name":"x","Type":"` + kind + `","AlbumId":"al1"}`))
		case strings.HasPrefix(p, "/Users/"):
			w.Write([]byte(`{"Id":"x","Name":"x"}`))
		default:
			w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	pw, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if _, err := pw.SignIn(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := New(Creds{Server: srv.URL, User: "u", Token: "tok", UserID: "u1", DeviceID: "dev"}, srv.Client())
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Ping(ctx))
	_, secret, err := c.QuickConnectInitiate(ctx)
	must(err)
	_, err = c.QuickConnectPoll(ctx, secret)
	must(err)
	_, err = c.QuickConnectSignIn(ctx, secret)
	must(err)
	key, _ := New(Creds{Server: srv.URL, Token: "KEY", Kind: KindAPIKey, UserID: "u1"}, srv.Client())
	_, err = key.SignInWithKey(ctx, "u")
	must(err)
	must(key.Ping(ctx))
	_, err = c.Playlists(ctx, "duration", true)
	must(err)
	_, _, err = c.Playlist(ctx, "pl1")
	must(err)
	_, err = c.Albums(ctx, 0, 50, "released", true)
	must(err)
	_, _, err = c.Album(ctx, "al1")
	must(err)
	_, err = c.Artists(ctx, "added", false)
	must(err)
	_, _, err = c.Artist(ctx, "ar1")
	must(err)
	_, _, _, err = c.Search(ctx, "x", 0)
	must(err)
	_, err = c.Song(ctx, "s1")
	must(err)
	_, err = c.Favorites(ctx)
	must(err)
	_, err = c.Latest(ctx)
	must(err)
	_, err = c.Lyrics(ctx, "s1")
	must(err)
	must(c.MarkPlayed(ctx, "s1", time.Now()))
	must(c.PlaybackStarted(ctx, "s1"))
	must(c.PlaybackProgress(ctx, "s1", time.Second, true))
	must(c.PlaybackStopped(ctx, "s1", time.Second))
	must(c.SetFavorite(ctx, "s1", true))
	must(c.SetFavorite(ctx, "s1", false))
	must(c.Logout(ctx))
	for _, u := range []string{c.MediaURL("s1", "original"), c.MediaURL("s1", "opus-320"), c.PlayURL("s1", "original"),
		c.PlayURL("s1", "opus-64"), c.MediaURL("s1", "mp3-160"), c.CoverURL("al1") + "&maxHeight=600&maxWidth=600"} {
		resp, err := c.Get(ctx, u, 0)
		must(err)
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]recorded(nil), reqs...)
}

// ---- a minimal OpenAPI 3 reader ----

type openAPI struct {
	Paths      map[string]map[string]*oaOp `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type oaOp struct {
	Deprecated  bool      `json:"deprecated"`
	Parameters  []oaParam `json:"parameters"`
	RequestBody *struct {
		Content map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

type oaParam struct {
	Name   string          `json:"name"`
	In     string          `json:"in"`
	Schema json.RawMessage `json:"schema"`
}

type oaSchema struct {
	Ref        string                     `json:"$ref"`
	Type       string                     `json:"type"`
	Enum       []any                      `json:"enum"`
	Items      json.RawMessage            `json:"items"`
	AllOf      []json.RawMessage          `json:"allOf"`
	OneOf      []json.RawMessage          `json:"oneOf"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// find is the operation for method on a concrete path, and the template it matched. A literal
// template wins over one with placeholders, and fewer placeholders over more, as the server routes:
// /Artists/AlbumArtists is not /Artists/{name}, nor /Users/Me /Users/{userId}.
func (s *openAPI) find(method, path string) (string, *oaOp) {
	best, bestN := "", -1
	var bestOp *oaOp
	for tmpl, ops := range s.Paths {
		op := ops[strings.ToLower(method)]
		if op == nil {
			continue
		}
		re := "^" + regexp.MustCompile(`\\\{[^}]+\\\}`).ReplaceAllString(regexp.QuoteMeta(tmpl), `[^/]+`) + "$"
		if !regexp.MustCompile(re).MatchString(path) {
			continue
		}
		if n := strings.Count(tmpl, "{"); bestN < 0 || n < bestN || n == bestN && tmpl < best {
			best, bestN, bestOp = tmpl, n, op
		}
	}
	return best, bestOp
}

// resolve follows $ref, allOf and oneOf to the first schema with content.
func (s *openAPI) resolve(raw json.RawMessage) oaSchema {
	var sc oaSchema
	json.Unmarshal(raw, &sc)
	for i := 0; i < 10; i++ {
		switch {
		case sc.Ref != "":
			raw = s.Components.Schemas[sc.Ref[strings.LastIndex(sc.Ref, "/")+1:]]
		case len(sc.AllOf) > 0:
			raw = sc.AllOf[0]
		case len(sc.OneOf) > 0:
			raw = sc.OneOf[0]
		default:
			return sc
		}
		sc = oaSchema{}
		json.Unmarshal(raw, &sc)
	}
	return sc
}

// enumOf is the set of values a parameter (or an array of it) may take; nil when it is no enum.
func (s *openAPI) enumOf(raw json.RawMessage) map[string]bool {
	sc := s.resolve(raw)
	if sc.Type == "array" && sc.Items != nil {
		sc = s.resolve(sc.Items)
	}
	if len(sc.Enum) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, v := range sc.Enum {
		if str, ok := v.(string); ok {
			out[str] = true
		}
	}
	return out
}

// bodyProps is the set of field names the operation's JSON body may hold.
func (s *openAPI) bodyProps(op *oaOp) map[string]bool {
	out := map[string]bool{}
	if op.RequestBody == nil {
		return out
	}
	for ct, c := range op.RequestBody.Content {
		if strings.Contains(ct, "json") {
			for k := range s.resolve(c.Schema).Properties {
				out[k] = true
			}
		}
	}
	return out
}
