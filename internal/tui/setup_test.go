package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TheIntroDB/plex-sync/internal/sync"
)

// This used to be a screen of its own, which was the wrong shape: a sixth tab
// that means nothing once the tag exists, holding an action one level away from
// the rest of the Plex settings. The report was "We shouldnt show setup as a
// screen besides first boot. We have a settings page for that. We can add the
// create marker button to the settings page and make it a button instead."
//
// It is a row on Settings now: always present, showing what the database holds,
// and pressing enter asks before it writes.

// enterOnMarkerTag puts the cursor on the button and presses enter, the way a
// person would.
func enterOnMarkerTag(t *testing.T, m *Model) {
	t.Helper()
	m.screen = screenSettings
	m.cursor = markerTagRow()
	if got := settingsRows()[m.cursor].key; got != "plex.marker_tag" {
		t.Fatalf("the row at the marker tag position is %q", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

// TestTheMarkerTagIsARowOnSettings is the shape itself: the action has to be
// reachable with the arrow keys, not hidden behind a screen of its own.
func TestTheMarkerTagIsARowOnSettings(t *testing.T) {
	m := newTestModel(t)
	m.screen = screenSettings

	row := settingsRows()[markerTagRow()]
	if row.kind != settingAction {
		t.Errorf("the marker tag row is kind %v, want a button", row.kind)
	}
	if row.section != "Plex" {
		t.Errorf("the marker tag row is in section %q, want it with the other Plex rows", row.section)
	}
	if row.run == nil {
		t.Error("the marker tag row has nothing to run")
	}
	// And it is on the screen a person is looking at, not merely in the list.
	if !strings.Contains(m.View(), "marker tag") {
		t.Error("the settings screen does not show the marker tag row")
	}
}

// TestTheButtonSaysWhatTheDatabaseHolds, because a button with no value is a
// button nobody can decide about.
func TestTheButtonSaysWhatTheDatabaseHolds(t *testing.T) {
	m := newTestModel(t)
	row := settingsRows()[markerTagRow()]

	if got := row.display(m); got != "checking..." {
		t.Errorf("before the check the button says %q", got)
	}

	m.Update(setupMsg{tagError: "plexdb: no marker tag (tag_type 12)"})
	if got := row.display(m); !strings.Contains(got, "missing") {
		t.Errorf("with no marker tag the button says %q, want it to say so", got)
	}

	m.Update(setupMsg{tagID: 331})
	if got := row.display(m); !strings.Contains(got, "331") {
		t.Errorf("with marker tag 331 the button says %q, want the tag named", got)
	}
}

// TestPressingTheButtonShowsAConfirmation is the bug that was reported: the
// prompt was never drawn, because the view knew about two of the three
// confirmations, so pressing the key looked like it did nothing and then looked
// stuck on "cancelled".
func TestPressingTheButtonShowsAConfirmation(t *testing.T) {
	m := newTestModel(t)
	m.Update(setupMsg{tagError: "no marker tag"})

	enterOnMarkerTag(t, m)
	if !m.confirmSetup {
		t.Fatal("enter on the button did not ask for confirmation")
	}
	if view := m.View(); !strings.Contains(view, "Create the marker tag") {
		t.Errorf("no confirmation is drawn:\n%s", view)
	}
}

// TestPressingItRepeatedlyDoesNotWedge is the exact report: "I hit s a few too
// many times and its stuck on cancelled". A key that is not y cancels, which is
// right, and the button has to be pressable again afterwards.
func TestPressingItRepeatedlyDoesNotWedge(t *testing.T) {
	m := newTestModel(t)
	m.Update(setupMsg{tagError: "no marker tag"})

	enterOnMarkerTag(t, m)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if m.confirmSetup {
		t.Error("a stray key left the confirmation pending")
	}
	if strings.Contains(m.View(), "Create the marker tag") {
		t.Error("the confirmation is still drawn after it was cancelled")
	}

	// And it can be asked for again, as many times as the user likes.
	for i := 0; i < 3; i++ {
		enterOnMarkerTag(t, m)
		if !m.confirmSetup {
			t.Fatalf("the button stopped asking for confirmation after %d round(s)", i+1)
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	}
}

// TestDecliningTheButtonChangesNothing: it writes to the Plex database, so a no
// has to mean no.
func TestDecliningTheButtonChangesNothing(t *testing.T) {
	m := newTestModel(t)
	m.Update(setupMsg{tagError: "no marker tag"})

	enterOnMarkerTag(t, m)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})

	if m.setup.tagID != 0 {
		t.Error("declining still made a tag")
	}
	if m.busy == "setup" {
		t.Error("declining still started the work")
	}
}

// TestConfirmingRunsIt: the button has to reach the same code the command does,
// with the same backup and journal.
func TestConfirmingRunsIt(t *testing.T) {
	m := newTestModel(t)
	m.Update(setupMsg{tagError: "no marker tag"})

	enterOnMarkerTag(t, m)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("confirming produced no command, so nothing would ever happen")
	}
	if m.busy != "setup" {
		t.Errorf("busy = %q, want the setup stage while it runs", m.busy)
	}

	// What that command returns is what updates the button.
	m.Update(setupDoneMsg{tagID: 331, journal: "/tmp/undo.jsonl"})
	row := settingsRows()[markerTagRow()]
	if got := row.display(m); !strings.Contains(got, "331") {
		t.Errorf("after the tag was made the button says %q", got)
	}
	if !strings.Contains(m.status, "331") {
		t.Errorf("status %q does not mention the tag that was made", m.status)
	}
	if m.setup.journal != "/tmp/undo.jsonl" {
		t.Errorf("journal %q was not kept, so the change cannot be pointed at", m.setup.journal)
	}
}

// TestAFailedButtonPressSaysWhy: the common failure is Plex still running, and a
// silent one would look like the button doing nothing.
func TestAFailedButtonPressSaysWhy(t *testing.T) {
	m := newTestModel(t)
	m.Update(setupMsg{tagError: "no marker tag"})
	m.Update(setupDoneMsg{err: sync.ErrPlexRunning})

	if m.setup.err == nil {
		t.Fatal("the failure was not recorded")
	}
	if !strings.Contains(m.status, "could not make the marker tag") {
		t.Errorf("status %q does not report the failure", m.status)
	}
	// The button still has to be there to press again.
	if got := settingsRows()[markerTagRow()].display(m); got == "" {
		t.Error("the button says nothing after a failure")
	}
}

// TestTheFirstRunSaysWhereTheButtonIs: the button is no use to someone who does
// not know it is there, and the reason this was reported as broken is that
// nothing said so.
func TestTheFirstRunSaysWhereTheButtonIs(t *testing.T) {
	m := newTestModel(t)

	m.Update(setupMsg{tagError: "plexdb: no marker tag (tag_type 12)"})
	if !strings.Contains(m.status, "press 5") {
		t.Errorf("status %q does not say where the fix is", m.status)
	}
	if m.cursor != markerTagRow() {
		t.Errorf("cursor = %d, want it on the marker tag row (%d)", m.cursor, markerTagRow())
	}
	if !strings.Contains(m.View(), "cannot be written yet") {
		t.Error("the status screen does not say markers cannot be written")
	}
}

// TestALibraryThatCanTakeMarkersIsNotNagged, which is what went wrong with the
// screen: it appeared whether or not there was anything to do.
func TestALibraryThatCanTakeMarkersIsNotNagged(t *testing.T) {
	m := newTestModel(t)
	before := m.status

	m.Update(setupMsg{tagID: 331})
	if m.status != before {
		t.Errorf("status changed to %q on a library that is fine", m.status)
	}
	if strings.Contains(m.View(), "cannot be written yet") {
		t.Error("a library with a marker tag is told markers cannot be written")
	}
	if m.setup.tagID != 331 {
		t.Errorf("marker tag = %d, want 331", m.setup.tagID)
	}
}

// TestPressingTheButtonWhenTheTagExistsIsNotAnAction: a confirmation for a write
// that will not happen is how a button stops being believed.
func TestPressingTheButtonWhenTheTagExistsIsNotAnAction(t *testing.T) {
	m := newTestModel(t)
	m.Update(setupMsg{tagID: 331})

	enterOnMarkerTag(t, m)
	if m.confirmSetup {
		t.Error("pressing a button with nothing to do asked for confirmation")
	}
	if !strings.Contains(m.status, "already") {
		t.Errorf("status %q does not say the tag is already there", m.status)
	}
}

// TestTheMarkerTagCheckIsPartOfTheFirstLoad: the button only shows the truth if
// the interface asks the question at all.
func TestTheMarkerTagCheckIsPartOfTheFirstLoad(t *testing.T) {
	m := newTestModel(t)
	if m.Init() == nil {
		t.Fatal("Init returned nothing, so no check is ever made")
	}
	if m.setup.checked {
		t.Error("the model claims to have checked before anything ran")
	}
}
