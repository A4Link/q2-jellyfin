package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Each browse list's sort follows its search box: kind "sort" with the firmware's "sort" (the title
// bar's icon), a list row without it; both open the Sort page for that list.
func TestSortRow(t *testing.T) {
	for _, caps := range []string{"", "sort"} {
		d := newTestDaemon(t, &nav{albums: 2, songsPer: 1}, caps, true)
		for _, op := range []string{"albums", "artists", "playlists"} {
			r := d.do(op)
			want := q2spot.Row{Kind: "list", URI: "sort", Title: "Sort", Subtitle: "Name · A to Z", Context: op}
			if caps == "sort" {
				want.Kind = "sort"
			}
			if len(r.Rows) < 2 || r.Rows[0].Kind != "search" || r.Rows[1] != want {
				t.Errorf("caps %q, %s: rows %+v", caps, op, r.Rows)
			}
		}
	}
}

// The Sort page shows Sort By and Order with their current choices; each opens its choices with the
// current one marked; a choice is kept in settings.json, and a new Sort By starts in its natural
// direction.
func TestSortPage(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 1}, "", true)
	page := func() []q2spot.Row { return d.do("sort", "albums").Rows }
	want := func(by, order string) {
		t.Helper()
		r := page()
		if len(r) != 2 || r[0].Title != "Sort By" || r[0].Subtitle != by || r[1].Title != "Order" || r[1].Subtitle != order {
			t.Fatalf("Sort page %+v, want %s / %s", r, by, order)
		}
	}
	want("Name", "A to Z")
	d.do("setorder", "albums turned") // from no sort saved at all: Name, Z to A
	want("Name", "Z to A")
	d.do("setorder", "albums natural")
	r := d.do("sortby", "albums")
	if got := strings.Join(titles(r), "|"); got != "Name|Release Date|Date Added|Album Artist|Random" {
		t.Fatalf("Sort By choices %q", got)
	}
	if row, _ := find(r, "Name"); row.Subtitle != selectedMark {
		t.Errorf("current choice not marked: %+v", r.Rows)
	}
	if r := d.do("setsort", "albums released"); r.Message != "" || r.Status != "" {
		t.Errorf("a choice toasts or fails: %+v", r) // the page comes again, the mark moved
	}
	if row, _ := find(d.do("sortby", "albums"), "Release Date"); row.Subtitle != selectedMark {
		t.Errorf("mark not moved")
	}
	if row, _ := find(d.do("sortby", "albums"), "Name"); row.Subtitle != "" {
		t.Errorf("old choice still marked")
	}
	want("Release Date", "Newest first")
	r = d.do("sortorder", "albums")
	if got := strings.Join(titles(r), "|"); got != "Newest first|Oldest first" || r.Rows[0].Subtitle != selectedMark {
		t.Fatalf("Order choices %+v", r.Rows)
	}
	if r := d.do("setorder", "albums turned"); r.Message != "" || r.Status != "" {
		t.Errorf("a choice toasts or fails: %+v", r)
	}
	if r := d.do("sortorder", "albums"); r.Rows[0].Subtitle != "" || r.Rows[1].Subtitle != selectedMark {
		t.Errorf("Order mark not moved: %+v", r.Rows)
	}
	want("Release Date", "Oldest first")
	if s := d.settings().Sort["albums"]; s != (listSort{By: "released", Reverse: true}) {
		t.Errorf("settings %+v", s)
	}
	d.do("setsort", "albums released") // the current one again: nothing changes
	want("Release Date", "Oldest first")
	d.do("setsort", "albums added")
	want("Date Added", "Newest first")
	if got := strings.Join(titles(d.do("sortby", "playlists")), "|"); got != "Name|Date Added|Duration|Random" {
		t.Errorf("playlists choices %q", got)
	}
	if got := strings.Join(titles(d.do("sortby", "artists")), "|"); got != "Name|Date Added|Random" {
		t.Errorf("artists choices %q", got)
	}
	for _, c := range [][]string{{"sort", "songs"}, {"setsort", "albums duration"}, {"setorder", "albums sideways"}} {
		if r := d.do(c[0], c[1]); r.Status != q2spot.Bad {
			t.Errorf("%v: %+v", c, r)
		}
	}
}

// Every Sort By choice but Random, either way round, is asked of the server as its own sortBy
// (a choice the client didn't know would show an error, not a list by name), and the list comes in
// the server's order; Random asks by name and shuffles.
func TestEveryChoiceIsAskedOfTheServer(t *testing.T) {
	n := &nav{albums: 3, songsPer: 1, playlists: 3, artists: 2}
	d := newTestDaemon(t, n, "", true)
	sent := map[string]func() any{"albums": n.albumSort.Load, "artists": n.artistSort.Load, "playlists": n.playlistSort.Load}
	for list, choices := range sortChoice {
		seen := map[string]string{}
		for _, c := range choices {
			for _, way := range []string{"natural", "turned"} {
				d.do("setsort", list+" "+c.key)
				if c != byRandom {
					d.do("setorder", list+" "+way)
				}
				r := d.do(list)
				if r.Status != "" || len(r.Rows) < 3 || r.Rows[2].Kind == "info" && r.Rows[2].Title != "No playlists" {
					t.Fatalf("%s %s %s: %+v", list, c.key, way, r.Rows)
				}
				got, _ := sent[list]().(string)
				by, _, _ := strings.Cut(got, " ")
				if c == byRandom {
					if by != "SortName" {
						t.Errorf("%s random asked %q, want the list by name to shuffle", list, got)
					}
					continue
				}
				if other, ok := seen[by]; ok && other != c.key {
					t.Errorf("%s: %s and %s both ask %q", list, c.key, other, by)
				}
				seen[by] = c.key
				if want := map[string]string{"natural": c.natural, "turned": c.turned}[way]; r.Rows[1].Subtitle != c.name+" · "+want {
					t.Errorf("%s: sort row %q, want %q", list, r.Rows[1].Subtitle, c.name+" · "+want)
				}
			}
		}
	}
	forget := func() { // lists come from the 1-minute cache (audit B4): ask the server again
		d.lists.mu.Lock()
		d.lists.cl = nil
		d.lists.mu.Unlock()
	}
	d.do("setsort", "playlists duration")
	d.do("setorder", "playlists turned")
	forget()
	d.do("playlists")
	if got := n.playlistSort.Load(); got != "Runtime,SortName Ascending,Ascending" {
		t.Errorf("shortest first asked %q", got)
	}
	d.do("setsort", "artists added")
	forget()
	d.do("artists")
	if got := n.artistSort.Load(); got != "DateCreated,SortName Descending,Ascending" {
		t.Errorf("artists newest first asked %q", got)
	}
	if r := d.do("playlists"); r.Rows[2].Title != "Favourites" {
		t.Errorf("Favourites not first: %q", titles(r))
	}
}

// A list keeps the server's order: what the server sends is shown as it comes.
func TestListsShowTheServersOrder(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1, playlists: 3, artists: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("setsort", "playlists duration")
	if got := items(d, "playlists"); !slices.Equal(got, []string{"Mix", "Mix 2", "Mix 3"}) { // the fake's order
		t.Errorf("playlists %q", got)
	}
	d.do("setsort", "artists added")
	if got := items(d, "artists"); !slices.Equal(got, []string{"Artist", "Artist 00002", "Artist 00003"}) {
		t.Errorf("artists %q", got)
	}
}

func items(d *tdaemon, op string) []string {
	var out []string
	for _, r := range d.do(op).Rows {
		if r.URI == "playlist" || r.URI == "artist" || r.URI == "album" {
			out = append(out, r.Title)
		}
	}
	return out
}

// Random shuffles Artists and Playlists too: every item once, the same order on the way back,
// another after Shuffled again.
func TestRandomArtistsAndPlaylists(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, playlists: 12, artists: 30}, "", true)
	for _, list := range []string{"artists", "playlists"} {
		byName := items(d, list)
		d.do("setsort", list+" random")
		first := items(d, list)
		if distinct(first) != len(byName) || len(first) != len(byName) || slices.Equal(first, byName) {
			t.Fatalf("%s: %d of %d, or not shuffled: %q", list, distinct(first), len(byName), first)
		}
		if again := items(d, list); !slices.Equal(again, first) {
			t.Errorf("%s: order changed on the way back", list)
		}
		d.do("sortorder", list)
		if again := items(d, list); slices.Equal(again, first) {
			t.Errorf("%s: same order after Shuffled again", list)
		}
	}
}

// A sort chosen while a Random album list is still being fetched doesn't change that request's
// sort half way (audit 3 #1: the second read took Date Added's branch and crashed).
func TestSortChangedWhileRandomAlbumsLoad(t *testing.T) {
	n := &nav{albums: 120, songsPer: 1, holdList: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("setsort", "albums random")
	done := make(chan q2spot.Reply)
	go func() { done <- d.do("albums") }()
	eventually(t, "the album list to be asked for", func() bool { s, _ := n.albumSort.Load().(string); return s != "" })
	d.do("setsort", "albums added")
	close(n.holdList)
	r := <-done
	if r.Status != "" || len(items2(r)) != 48 {
		t.Fatalf("%+v", r)
	}
	if r.Rows[1].Subtitle != "Random" { // the sort the request read, shown with it
		t.Errorf("sort row %q", r.Rows[1].Subtitle)
	}
}

func items2(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		if row.URI == "album" {
			out = append(out, row.Title)
		}
	}
	return out
}

// Random is every album once, in an order kept across pages and a return to the list, until Order
// is pressed ("Shuffled again").
func TestRandomAlbums(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "", true)
	d.do("setsort", "albums random")
	walk := func() []string {
		var out []string
		for cursor := ""; ; {
			r := d.do("albums", cursor)
			for _, row := range r.Rows {
				if row.URI == "album" {
					out = append(out, row.Title)
				}
			}
			if cursor = r.Cursor; cursor == "" {
				return out
			}
		}
	}
	first := walk()
	if len(first) != 120 || distinct(first) != 120 {
		t.Fatalf("%d albums, some twice or missing", len(first))
	}
	if first[0] == "Album 000" && first[1] == "Album 001" && first[2] == "Album 002" {
		t.Errorf("not shuffled: %q", first[:5])
	}
	if again := walk(); !slices.Equal(again, first) {
		t.Errorf("order changed on the way back: %q, then %q", first[:5], again[:5])
	}
	order := d.do("sort", "albums").Rows[1]
	if order.Kind != "action" || order.Subtitle != "Shuffled" {
		t.Errorf("Order row %+v", order)
	}
	if m := d.do("sortorder", "albums").Message; m != "Shuffled again" {
		t.Errorf("toast %q", m)
	}
	if again := walk(); slices.Equal(again, first) {
		t.Error("the same order after Shuffled again")
	}
	if s := d.do("albums").Rows[1].Subtitle; s != "Random" {
		t.Errorf("sort row %q", s)
	}
}

// Random on a server that ignores the offset (every page the first) ends with the albums it has.
func TestRandomAlbumsServerIgnoresOffset(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 600, songsPer: 1, sameAlbums: true}, "", true)
	d.do("setsort", "albums random")
	if r := d.do("albums"); r.Status != "" || len(r.Rows) != 50 {
		t.Fatalf("%+v", r)
	}
}

func distinct(xs []string) int {
	seen := map[string]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return len(seen)
}

// Random on Albums answers in time however long every album takes to come: until it has, the page
// says so with its search and sort, live, and then shows the shuffled list (an Albums page set to
// Random timed out on a large library, every time it was opened).
func TestRandomAlbumsSlowServer(t *testing.T) {
	n := &nav{albums: 2000, songsPer: 1, listDelay: 100 * time.Millisecond} // 4 pages: 400 ms
	d := newTestDaemon(t, n, "", true)
	d.answerIn = 200 * time.Millisecond
	d.do("setsort", "albums random")
	start := time.Now()
	r := d.do("albums")
	if took := time.Since(start); took > d.answerIn {
		t.Errorf("answered in %v", took)
	}
	if r.Status != "" || r.Message != "refresh" || len(r.Rows) != 3 || r.Rows[1].URI != "sort" || r.Rows[2].Title != "Loading albums…" {
		t.Fatalf("%+v", r)
	}
	eventually(t, "the shuffled albums", func() bool { return len(items2(d.do("albums"))) == 48 })
	if r := d.do("albums"); r.Message != "pages" || r.Rows[1].URI != "sort" {
		t.Errorf("%+v", r)
	}
}

// A list that fails, in any sort, still shows its search and sort, so a sort that keeps failing
// can be changed from the page (an Albums page whose sort always failed could not be opened).
func TestFailedListKeepsSort(t *testing.T) {
	n := &nav{albums: 2, songsPer: 1, playlists: 2, artists: 2}
	d := newTestDaemon(t, n, "", true)
	n.rawStatus.Store(502)
	for list, choices := range sortChoice {
		for _, c := range choices {
			d.do("setsort", list+" "+c.key)
			r := d.do(list)
			if len(r.Rows) < 3 || r.Rows[0].Kind != "search" || r.Rows[1].URI != "sort" || r.Rows[2].Kind != "info" {
				t.Errorf("%s by %s: %+v", list, c.key, r.Rows)
			}
		}
	}
}

// A Random album fetch that fails between two asks of the live page is shown at the next ask (the
// error, with Search and Sort), not lost to a new fetch that leaves the page loading for ever; the
// ask after that tries again.
func TestRandomAlbumsFailedFetchIsShown(t *testing.T) {
	n := &nav{albums: 2000, songsPer: 1, listDelay: 150 * time.Millisecond, listErr: 70} // the second page fails
	d := newTestDaemon(t, n, "", true)
	d.answerIn = 100 * time.Millisecond
	d.do("setsort", "albums random")
	if r := d.do("albums"); r.Message != "refresh" {
		t.Fatalf("%+v", r)
	}
	time.Sleep(400 * time.Millisecond) // the fetch has failed: the firmware asks every 2 s
	r := d.do("albums")
	if r.Status != "" || r.Message != "" || len(r.Rows) < 3 || r.Rows[1].URI != "sort" || r.Rows[2].Kind != "info" ||
		r.Rows[2].Title == "Loading albums…" {
		t.Fatalf("%+v", r)
	}
	asked := n.albumLists.Load()
	if r := d.do("albums"); r.Message != "refresh" {
		t.Fatalf("again: %+v", r)
	}
	eventually(t, "a new fetch", func() bool { return n.albumLists.Load() > asked })
}

// Signing out stops a Random album fetch still running.
func TestSignOutStopsRandomAlbumsFetch(t *testing.T) {
	n := &nav{albums: 2000, songsPer: 1, listDelay: time.Minute}
	d := newTestDaemon(t, n, "", true)
	d.answerIn = 100 * time.Millisecond
	d.do("setsort", "albums random")
	d.do("albums")
	d.mu.Lock()
	f := d.albumsAll
	d.mu.Unlock()
	if f == nil {
		t.Fatal("no fetch running")
	}
	if err := d.signOut(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch still runs after Sign Out")
	}
}
