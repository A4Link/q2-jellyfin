package main

import (
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// walkWindow follows a window's replies as the firmware does ("pages" and a cursor ask for the
// next one) and returns its rows in order.
func walkWindow(t *testing.T, d *tdaemon, op, cursor string) []q2spot.Row {
	t.Helper()
	var rows []q2spot.Row
	for i := 0; ; i++ {
		args := []string{}
		if cursor != "" {
			args = append(args, cursor)
		}
		r := d.do(op, args...)
		if r.Status != "" && r.Status != "ok" {
			t.Fatalf("%s %q: %s %s", op, cursor, r.Status, r.Message)
		}
		if len(r.Rows) > q2spot.MaxRows {
			t.Fatalf("%d rows in one reply", len(r.Rows))
		}
		rows = append(rows, r.Rows...)
		if r.Cursor == "" {
			return rows
		}
		if r.Message != "pages" {
			t.Fatalf("a cursor without pages: %q", r.Message)
		}
		if i > 10 {
			t.Fatal("the window never ends")
		}
		cursor = r.Cursor
	}
}

// With the firmware's "window", Artists shows 200 rows at a time: Next page ends a window, its search
// box and sort (the title bar's icons) then Previous page start every later one, and the windows hold every row of the list once, in order.
func TestArtistsWindows(t *testing.T) {
	n := &nav{artists: 449}
	d := newTestDaemon(t, n, "sections art urls window", true) // 452 rows with the search box and sort
	var all []string
	cursor, prev := "", ""
	for w := 0; ; w++ {
		rows := walkWindow(t, d, "artists", cursor)
		if w > 0 {
			if rows[0].Kind != "search" || rows[1].URI != "sort" || rows[2].Kind != "page" || rows[2].URI != "prev" || rows[2].Context != prev {
				t.Fatalf("window %d starts with %+v", w, rows[:3])
			}
			rows = rows[3:]
		}
		last := rows[len(rows)-1]
		for _, r := range rows {
			if r.Kind == "page" && r != last {
				t.Fatalf("window %d: a page row inside it: %+v", w, r)
			}
		}
		prev = cursor
		if prev == "" {
			prev = "0"
		}
		if last.Kind != "page" {
			for _, r := range rows {
				all = append(all, r.Title)
			}
			if w != 2 {
				t.Fatalf("the list ended in window %d", w)
			}
			break
		}
		if last.URI != "next" || last.Context != strconv.Itoa((w+1)*windowRows) {
			t.Fatalf("window %d ends with %+v", w, last)
		}
		if len(rows)-1 != windowRows {
			t.Fatalf("window %d has %d rows", w, len(rows)-1)
		}
		for _, r := range rows[:len(rows)-1] {
			all = append(all, r.Title)
		}
		cursor = last.Context
	}
	if len(all) != 452 || all[0] != "Search Artists" || all[1] != "Sort" || all[2] != "Artist" || all[451] != "Artist 00450" {
		t.Fatalf("%d rows: %q … %q", len(all), all[:3], all[len(all)-1])
	}
	for i := 3; i < len(all); i++ {
		if want := "Artist " + pad5(i-1); all[i] != want {
			t.Fatalf("row %d is %q, want %q", i, all[i], want)
		}
	}
	// Later pages, those of later windows too, reuse the list the first page fetched.
	if k := n.artistReqs.Load(); k != 1 {
		t.Fatalf("%d artist list fetches", k)
	}
}

func pad5(i int) string { return fmt.Sprintf("%05d", i) }

// A section left mid-way through its second page, for another section, comes back on that page:
// Q2 Pod asks for a section it swipes to without a cursor and selects the row it left, so the page
// must be the one left, row for row, for that row to be the same one. One left on its first page,
// reached by Previous page, comes back the same too. Back to the main list (Swipe Between Sections
// off) opens each section at its start, as Q2 Pod opens it at its first row.
func TestSectionKeepsItsPage(t *testing.T) {
	for _, op := range []string{"albums", "artists", "playlists"} {
		d := newTestDaemon(t, &nav{albums: 260, songsPer: 1, artists: 449, playlists: 260}, "sections window", true)
		d.do("menu")
		d.do("toggle", "album:al0") // Sync Now on the first page
		first := walkWindow(t, d, op, "")
		second := walkWindow(t, d, op, "200")
		d.do("latest") // swiped to Newly Added
		if got := walkWindow(t, d, op, ""); !slices.Equal(got, second) {
			t.Fatalf("%s back from mid-way through page 2: %q…, want %q…", op, titles(q2spot.Reply{Rows: got[:4]}), titles(q2spot.Reply{Rows: second[:4]}))
		}
		if got := walkWindow(t, d, op, "0"); !slices.Equal(got, first) { // Previous page
			t.Fatalf("%s Previous page: %q…", op, titles(q2spot.Reply{Rows: got[:4]}))
		}
		d.do("latest")
		if got := walkWindow(t, d, op, ""); !slices.Equal(got, first) {
			t.Fatalf("%s back to page 1: %q…", op, titles(q2spot.Reply{Rows: got[:4]}))
		}
		walkWindow(t, d, op, "200")
		d.do("menu") // Back to the main list, or Jellyfin opened again
		if got := walkWindow(t, d, op, ""); !slices.Equal(got, first) {
			t.Fatalf("%s after the main list: %q…", op, titles(q2spot.Reply{Rows: got[:4]}))
		}
	}
}

// Without the cap, lists page as before: no page rows.
func TestNoWindowsWithoutTheCap(t *testing.T) {
	d := newTestDaemon(t, &nav{artists: 449}, "sections art urls", true)
	for _, r := range walkWindow(t, d, "artists", "") {
		if r.Kind == "page" {
			t.Fatalf("%+v", r)
		}
	}
}

// Albums page by the server: the windows cut it the same way.
func TestAlbumsWindows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 260, songsPer: 1}, "window", true)
	rows := walkWindow(t, d, "albums", "")
	if last := rows[len(rows)-1]; len(rows) != 201 || last.Kind != "page" || last.Context != "200" {
		t.Fatalf("%d rows, last %+v", len(rows), last)
	}
	rows = walkWindow(t, d, "albums", "200")
	if len(rows) != 65 || rows[0].Kind != "search" || rows[1].URI != "sort" || rows[2].URI != "prev" || rows[3].Title != "Album 198" || rows[64].Title != "Album 259" {
		t.Fatalf("%d rows: %q %q … %q", len(rows), rows[0].Title, rows[1].Title, rows[len(rows)-1].Title)
	}
}

// A reply that is full with the Previous page row moves its last row to the next reply.
func TestWindowKeepsRepliesWithinLimits(t *testing.T) {
	d := &daemon{}
	d.caps.Window = true
	var rows []q2spot.Row
	for i := 0; i < q2spot.MaxRows; i++ {
		rows = append(rows, info("r"+strconv.Itoa(200+i)))
	}
	r := d.window(q2spot.Reply{Rows: rows, Cursor: "250"}, "200")
	if len(r.Rows) != q2spot.MaxRows || r.Rows[0].URI != "prev" || r.Cursor != "249" || r.Rows[49].Title != "r248" {
		t.Fatalf("%d rows, cursor %q, last %q", len(r.Rows), r.Cursor, r.Rows[len(r.Rows)-1].Title)
	}
	// The window's last reply: 50 rows from 350 reach 400; Next page takes the 50th row's room.
	r = d.window(q2spot.Reply{Rows: rows, Cursor: "400"}, "350")
	if len(r.Rows) != q2spot.MaxRows-1 || r.Cursor != "399" || r.Rows[48].Kind == "page" {
		t.Fatalf("%d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	r = d.window(q2spot.Reply{Rows: rows[:1], Cursor: "400"}, "399")
	if len(r.Rows) != 2 || r.Cursor != "" || r.Rows[1].URI != "next" || r.Rows[1].Context != "400" {
		t.Fatalf("%+v", r)
	}
}
