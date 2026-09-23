package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/model"
	"github.com/TheIntroDB/plex-sync/internal/sync"
)

// withPlan puts a plan of n items on the model and shows the preview screen, so
// the selection keys have something to act on without a library or a server.
func withPlan(t *testing.T, n int) *Model {
	t.Helper()
	m := newTestModel(t)

	var plan model.Plan
	for i := 1; i <= n; i++ {
		plan.Items = append(plan.Items, model.ItemPlan{
			Item: model.LibraryItem{
				RatingKey: i,
				Title:     fmt.Sprintf("episode %d", i),
				Kind:      model.KindEpisode,
				Season:    intPtr(1),
				Episode:   intPtr(i),
				IDs:       model.ExternalIDs{TMDB: intPtr(1000 + i)},
			},
			Add: []model.Marker{{
				Text: model.MarkerIntro, StartMS: 0, EndMS: 1000,
				Source: string(model.SourceTheIntroDB),
			}},
			Reason: "add",
		})
	}
	m.result = &sync.Result{Plan: plan}
	m.screen = screenPlan
	return m
}

func intPtr(v int) *int { return &v }

// pressKeys sends keys to the interface, failing if one of them quits it.
func pressKeys(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, _ = m.handleKey(keyMsg(key)); m.quit {
			t.Fatalf("key %q quit the interface", key)
		}
	}
}

func TestArrowsMoveThePlanCursor(t *testing.T) {
	m := withPlan(t, 3)

	if m.planCursor != 0 {
		t.Fatalf("cursor starts at %d, want 0", m.planCursor)
	}
	pressKeys(t, m, "down", "down")
	if m.planCursor != 2 {
		t.Errorf("cursor after two downs = %d, want 2", m.planCursor)
	}
	// It stops at the ends rather than wrapping, so a held key cannot come back
	// round to the top without the person noticing.
	pressKeys(t, m, "down", "down")
	if m.planCursor != 2 {
		t.Errorf("cursor past the last row = %d, want 2", m.planCursor)
	}
	pressKeys(t, m, "up")
	if m.planCursor != 1 {
		t.Errorf("cursor after one up = %d, want 1", m.planCursor)
	}
	pressKeys(t, m, "up", "up")
	if m.planCursor != 0 {
		t.Errorf("cursor past the first row = %d, want 0", m.planCursor)
	}
	pressKeys(t, m, "end")
	if m.planCursor != 2 {
		t.Errorf("cursor after end = %d, want 2", m.planCursor)
	}
	pressKeys(t, m, "home")
	if m.planCursor != 0 {
		t.Errorf("cursor after home = %d, want 0", m.planCursor)
	}
}

func TestSpaceSelectsAndUnselectsTheItemUnderTheCursor(t *testing.T) {
	m := withPlan(t, 3)

	pressKeys(t, m, "down", " ") // item 2
	if len(m.result.Plan.SelectedWork()) != 2 {
		t.Fatalf("selected work = %d, want 2 after turning one off",
			len(m.result.Plan.SelectedWork()))
	}
	if m.result.Plan.Selection.Selected(2) {
		t.Error("item 2 is still selected")
	}
	if !m.result.Plan.Selection.Selected(1) || !m.result.Plan.Selection.Selected(3) {
		t.Error("deselecting one item turned off another")
	}
	if !strings.Contains(m.status, "1 left out") {
		t.Errorf("status = %q, want it to say how many were left out", m.status)
	}

	// The same key turns it back on, which is what unselect means.
	pressKeys(t, m, " ")
	if len(m.result.Plan.SelectedWork()) != 3 {
		t.Errorf("selected work = %d, want 3 after turning it back on",
			len(m.result.Plan.SelectedWork()))
	}
}

func TestSelectAllAndNone(t *testing.T) {
	m := withPlan(t, 3)

	pressKeys(t, m, "N")
	if len(m.result.Plan.SelectedWork()) != 0 {
		t.Errorf("selected work = %d, want none after N", len(m.result.Plan.SelectedWork()))
	}
	if !strings.Contains(m.status, "3 left out") {
		t.Errorf("status = %q, want it to say all three were left out", m.status)
	}

	pressKeys(t, m, "A")
	if len(m.result.Plan.SelectedWork()) != 3 {
		t.Errorf("selected work = %d, want all three after A", len(m.result.Plan.SelectedWork()))
	}
}

func TestRescanIsMarkedAndShown(t *testing.T) {
	m := withPlan(t, 2)

	pressKeys(t, m, "R")
	keys := m.result.Plan.Selection.RescanKeys()
	if !keys["tmdb:1001:1:1"] {
		t.Fatalf("rescan keys = %v, want the first item's lookup key", keys)
	}
	if !strings.Contains(m.status, "press p to plan again") {
		t.Errorf("status = %q, want it to say the re-scan happens on the next plan", m.status)
	}
	if !strings.Contains(m.View(), "[re-scan]") {
		t.Error("the preview does not show that an item is marked for a re-scan")
	}

	// Pressing it again unmarks.
	pressKeys(t, m, "R")
	if len(m.result.Plan.Selection.RescanKeys()) != 0 {
		t.Errorf("rescan keys = %v, want none after unmarking",
			m.result.Plan.Selection.RescanKeys())
	}
}

func TestThePreviewMarksTheCursorAndTheSelection(t *testing.T) {
	m := withPlan(t, 3)

	out := m.View()
	if !strings.Contains(out, "> [x]") {
		t.Errorf("the preview does not mark the cursor row:\n%s", out)
	}

	pressKeys(t, m, "down", "down", " ")
	out = m.View()
	if !strings.Contains(out, "[ ]") {
		t.Errorf("the preview does not show an unselected item:\n%s", out)
	}
	if !strings.Contains(out, "3 item(s) to change, 2 selected") {
		t.Errorf("the preview does not count the selection:\n%s", out)
	}
}

func TestThePlanScreenKeepsItsCursorOnScreen(t *testing.T) {
	m := withPlan(t, 60)
	m.height = 20

	for i := 0; i < 40; i++ {
		pressKeys(t, m, "down")
	}
	start, end := m.visible(m.planLen())
	if m.planCursor < start || m.planCursor >= end {
		t.Errorf("cursor %d is outside the visible rows %d-%d", m.planCursor, start, end)
	}
}

func TestThePreviewKeysDoNothingWithoutAPlan(t *testing.T) {
	m := newTestModel(t)
	m.screen = screenPlan

	// Nothing here may panic or write: there is no plan to select from.
	pressKeys(t, m, "down", " ", "A", "N", "R")
	if m.result != nil {
		t.Error("a plan appeared from nowhere")
	}
}
