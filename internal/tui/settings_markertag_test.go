package tui

import (
	"strings"
	"testing"
)

// The marker tag used to be a button on this screen, and a line on the status
// screen nagging about it. Neither belongs in an interface.
//
// Making that tag is a first-run and debug step: it writes a row into Plex's
// schema, it is needed to write marker rows into a library that has never held
// one, and it is reached with --force-create-initial-tag when it is needed at
// all. A settings page invites people to press it who should never press it, and
// a warning about something a normal run does not need is noise.
//
// These tests exist so that the row and the nag do not come back by habit.

// TestTheMarkerTagIsNotOfferedInTheInterface is the decision itself.
func TestTheMarkerTagIsNotOfferedInTheInterface(t *testing.T) {
	m := newTestModel(t)
	m.screen = screenSettings

	for _, row := range settingsRows() {
		if row.key == "plex.marker_tag" || strings.Contains(strings.ToLower(row.label), "marker tag") {
			t.Errorf("the settings screen offers %q, which is a flag now", row.key)
		}
	}
	if strings.Contains(m.View(), "marker tag") {
		t.Errorf("the settings screen draws a marker tag row:\n%s", m.View())
	}
}

// TestTheStatusScreenDoesNotNagAboutTheMarkerTag: a missing tag does not stop a
// run from being useful, so it is not a fault to report on the screen people
// look at every day.
func TestTheStatusScreenDoesNotNagAboutTheMarkerTag(t *testing.T) {
	m := newTestModel(t)
	m.screen = screenStatus

	if view := strings.ToLower(m.View()); strings.Contains(view, "marker tag") ||
		strings.Contains(view, "cannot be written yet") {
		t.Errorf("the status screen still reports the marker tag:\n%s", m.View())
	}
}

// TestEverySettingsRowIsASetting is the structural half of the same decision:
// every row there reads and writes the configuration file, and nothing on that
// screen reaches into Plex's database.
func TestEverySettingsRowIsASetting(t *testing.T) {
	for _, row := range settingsRows() {
		if row.get == nil {
			t.Errorf("row %q has no value of its own, so it is not a setting", row.key)
		}
		if row.set == nil {
			t.Errorf("row %q cannot be changed, so it is not a setting", row.key)
		}
	}
}
