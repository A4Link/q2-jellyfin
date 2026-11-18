package main

import "github.com/A4Link/q2-jellyfin/internal/q2spot"

// Swipe Between Sections (Settings). With Q2 Pod's "sections" (1.7.3) the first page is the five
// sections, shown as pages a swipe moves between; turned off, they are rows of one main list. On
// by default.

// swipeOn reports whether the first page is sections.
func (d *daemon) swipeOn() bool { return d.settings().Swipe != "off" }

// swipeRow is the setting's row.
func (d *daemon) swipeRow(s settings) q2spot.Row {
	state := "Off"
	if s.Swipe != "off" {
		state = "On"
	}
	return q2spot.Row{Kind: "action", URI: "swipe", Title: "Swipe Between Sections: " + state,
		Subtitle: "Applies when Jellyfin is next opened"}
}

// toggleSwipe is the row's press (settingsMu held).
func (d *daemon) toggleSwipe(s settings) q2spot.Reply {
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
