package main

import "github.com/A4Link/q2-jellyfin/internal/q2spot"

// Swipe Between Sections (Settings). With Q2 Pod's "sections" the first page is the five sections,
// shown as pages a swipe moves between; turned off, or on firmware without "sections", they are
// rows of one main list, as before. On by default where the firmware offers it. A card moved to
// firmware without it, the setting turns off, so going back to firmware with it keeps the old
// list until the user turns it on again.

// reconcileSwipe brings s.Swipe in line with the firmware: the default stored as "on" the first
// time sections are offered, and "on" turned "off" when they no longer are. It reports a change.
func reconcileSwipe(s *settings, offered bool) bool {
	switch {
	case offered && s.Swipe == "":
		s.Swipe = "on"
	case !offered && s.Swipe == "on":
		s.Swipe = "off"
	default:
		return false
	}
	return true
}

// swipeOn reports whether the first page is sections, saving any change reconcileSwipe makes.
func (d *daemon) swipeOn() bool {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := d.settings()
	if reconcileSwipe(&s, d.caps.Sections) {
		d.saveSettings(s)
	}
	return d.caps.Sections && s.Swipe == "on"
}

// swipeRow is the setting's row: a toggle, or, on firmware without sections, an inert row saying so.
func (d *daemon) swipeRow(s settings) q2spot.Row {
	if !d.caps.Sections {
		return q2spot.Row{Kind: "info", Title: "Swipe Between Sections: Unavailable",
			Subtitle: "Needs Q2 Pod firmware with swipe support"}
	}
	state := "Off"
	if s.Swipe != "off" {
		state = "On"
	}
	return q2spot.Row{Kind: "action", URI: "swipe", Title: "Swipe Between Sections: " + state,
		Subtitle: "Applies when Jellyfin is next opened"}
}

// toggleSwipe is the row's press (settingsMu held).
func (d *daemon) toggleSwipe(s settings) q2spot.Reply {
	if !d.caps.Sections {
		return q2spot.Reply{Message: "This firmware can't swipe between sections"}
	}
	if s.Swipe == "off" {
		s.Swipe = "on"
	} else {
		s.Swipe = "off"
	}
	d.saveSettings(s)
	if s.Swipe == "on" {
		return q2spot.Reply{Message: "Swipe on: close and reopen Jellyfin"}
	}
	return q2spot.Reply{Message: "Swipe off: close and reopen Jellyfin"}
}
