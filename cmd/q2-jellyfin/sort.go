package main

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/A4Link/q2-jellyfin/internal/q2spot"
)

// Sorting Albums, Artists and Playlists (Acceptance Criteria L7-L9). Each list's sort icon (a row
// of kind "sort" right after its search row, which Q2 Pod shows in the title bar; a list row
// elsewhere) opens the Sort page: Sort By and Order, each with its current choice under it. Each
// opens its choices, the current one marked Selected, as the Quality pages do; a choice has no
// toast: the page is asked again, so the mark moves to it. The choice is kept in settings.json, one
// per list. The server sorts every list (jellyfin orders); only Random is the app's, a shuffle of
// the list by name, as the server's Random is new at each request and can't be paged.

// sortOpt is one Sort By choice: key (kept in settings), its name, and the names of its two
// directions, its natural one first. Random has no direction.
type sortOpt struct{ key, name, natural, turned string }

var (
	byName     = sortOpt{"name", "Name", "A to Z", "Z to A"}
	byAdded    = sortOpt{"added", "Date Added", "Newest first", "Oldest first"}
	byRandom   = sortOpt{"random", "Random", "", ""}
	sortChoice = map[string][]sortOpt{
		"albums": {byName, {"released", "Release Date", "Newest first", "Oldest first"}, byAdded,
			{"albumartist", "Album Artist", "A to Z", "Z to A"}, byRandom},
		"artists":   {byName, byAdded, byRandom},
		"playlists": {byName, byAdded, {"duration", "Duration", "Longest first", "Shortest first"}, byRandom},
	}
)

// listSort is a list's sort as kept in settings.json: its Sort By key ("" is Name) and whether its
// Order is turned round from the natural one.
type listSort struct {
	By      string `json:"by,omitempty"`
	Reverse bool   `json:"reverse,omitempty"`
}

// sortOf is list's sort and its Sort By choice; none ("") is Name, its Order kept, and an unknown
// key is Name, A to Z.
func (s settings) sortOf(list string) (listSort, sortOpt) {
	o := s.Sort[list]
	for _, c := range sortChoice[list] {
		if c.key == cmp.Or(o.By, byName.key) {
			return o, c
		}
	}
	return listSort{}, byName
}

// orderName is the sort's Order for the screen.
func orderName(o listSort, c sortOpt) string {
	switch {
	case c == byRandom:
		return "Shuffled"
	case o.Reverse:
		return c.turned
	}
	return c.natural
}

// sortNow is a list's sort as one request reads it, once, so a choice made while the request
// runs can't change it half way (audit 3 #1).
type sortNow struct {
	listSort
	opt sortOpt
}

func (d *daemon) sortNow(list string) sortNow {
	o, c := d.settings().sortOf(list)
	return sortNow{o, c}
}

// query is the list's cache key and what to ask the server for: the sort's own order, or Random's
// list by name, which shuffled then shuffles.
func (s sortNow) query(list string) (key, order string, reverse bool) {
	if s.opt == byRandom {
		return list, "name", false
	}
	return fmt.Sprintf("%s %s %v", list, s.opt.key, s.Reverse), s.opt.key, s.Reverse
}

// sortRow is the list's sort icon: kind "sort" where the firmware has it, else a plain row.
func (d *daemon) sortRow(list string, s sortNow) q2spot.Row {
	o, c := s.listSort, s.opt
	kind := "list"
	if d.caps.Sort {
		kind = "sort"
	}
	sub := c.name
	if c != byRandom {
		sub += " · " + orderName(o, c)
	}
	return q2spot.Row{Kind: kind, URI: "sort", Title: "Sort", Subtitle: sub, Context: list}
}

// listTop is the rows every window of a browse list starts with: its search box, then its sort.
func (d *daemon) listTop(list string, s sortNow) []q2spot.Row {
	return []q2spot.Row{searchRow(searchTitles[list]), d.sortRow(list, s)}
}

// sortOp answers the Sort page and its choices. arg is the list's name, then a choice.
func (d *daemon) sortOp(op, arg string) q2spot.Reply {
	name, choice, _ := strings.Cut(arg, " ")
	if sortChoice[name] == nil {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := d.settings()
	o, c := s.sortOf(name)
	save := func(o listSort) {
		if s.Sort == nil {
			s.Sort = map[string]listSort{}
		}
		s.Sort[name] = o
		d.saveSettings(s)
		d.dropLists(name) // a new order: the list comes fresh from the server
	}
	switch op {
	case "sort": // the Sort page
		order := list("sortorder", "Order", orderName(o, c), name)
		if c == byRandom { // nothing to choose: a press shuffles again
			order = q2spot.Row{Kind: "action", URI: "sortorder", Title: "Order", Subtitle: "Shuffled", Context: name}
		}
		return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{list("sortby", "Sort By", c.name, name), order}}
	case "sortby":
		var rows []q2spot.Row
		for _, x := range sortChoice[name] {
			r := act("setsort", x.name, name+" "+x.key)
			if x == c {
				r.Subtitle = selectedMark
			}
			rows = append(rows, r)
		}
		return q2spot.Reply{Message: "refresh", Rows: rows}
	case "setsort":
		for _, x := range sortChoice[name] {
			if x.key == choice {
				if x == byRandom {
					d.reshuffle(name) // chosen again or not: a new shuffle, of a fresh list
					d.dropLists(name)
				}
				if x != c { // the current one again changes nothing: its Order is kept
					save(listSort{By: x.key}) // a new Sort By starts in its natural direction
				}
				return q2spot.Reply{} // no toast: the page comes again with the mark moved
			}
		}
		return q2spot.Fail(q2spot.Bad, "Bad request")
	case "sortorder":
		if c == byRandom {
			d.reshuffle(name)
			d.dropLists(name)
			return q2spot.Reply{Message: "Shuffled again"}
		}
		natural, turned := act("setorder", c.natural, name+" natural"), act("setorder", c.turned, name+" turned")
		if o.Reverse {
			turned.Subtitle = selectedMark
		} else {
			natural.Subtitle = selectedMark
		}
		return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{natural, turned}}
	case "setorder":
		if c == byRandom || choice != "natural" && choice != "turned" {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		if turned := choice == "turned"; turned != o.Reverse { // the current one again changes nothing
			o.Reverse = turned
			save(o)
		}
		return q2spot.Reply{}
	}
	return q2spot.Fail(q2spot.Bad, "Bad request")
}

// shuffleSeed is list's Random order: the same until Random is chosen again or Order is pressed, so
// scrolling and coming back keep the order; a new one each time the app starts.
func (d *daemon) shuffleSeed(list string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shuffles == nil {
		d.shuffles = map[string]uint64{}
	}
	if d.shuffles[list] == 0 {
		d.shuffles[list] = uint64(time.Now().UnixNano()) | 1
	}
	return d.shuffles[list]
}

func (d *daemon) reshuffle(list string) {
	d.mu.Lock()
	delete(d.shuffles, list)
	d.mu.Unlock()
}

// shuffled is xs, the list in its sort from the server, as list's sort s shows it: as it is, or for
// Random a shuffle of it (by name) in the order the list's seed gives, a copy.
func shuffled[T any](d *daemon, list string, s sortNow, xs []T) []T {
	if s.opt != byRandom {
		return xs
	}
	out := slices.Clone(xs)
	seed := d.shuffleSeed(list)
	rand.New(rand.NewPCG(seed, seed>>1)).Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
