package model

import "testing"

// changeable builds an item that needs a write, so that Work and SelectedWork
// have something to act on.
func changeable(ratingKey int) ItemPlan {
	return ItemPlan{
		Item: LibraryItem{RatingKey: ratingKey, Title: "item"},
		Add: []Marker{{
			Text: MarkerIntro, StartMS: 0, EndMS: 1000, Source: string(SourceTheIntroDB),
		}},
		Reason: "add",
	}
}

func TestAnAbsentSelectionKeepsEverything(t *testing.T) {
	plan := Plan{Items: []ItemPlan{changeable(1), changeable(2)}}

	if len(plan.SelectedWork()) != 2 {
		t.Errorf("SelectedWork with no selection = %d, want 2: absent means everything",
			len(plan.SelectedWork()))
	}
	// A nil selection must answer safely, because a plan read from an older
	// file has none.
	var none *Selection
	if !none.Selected(7) {
		t.Error("a nil selection reported an item as unselected")
	}
	if len(none.RescanKeys()) != 0 {
		t.Error("a nil selection reported re-scan keys")
	}
}

func TestSelectingAndDeselecting(t *testing.T) {
	selection := &Selection{}

	selection.Select(2, false)
	if selection.Selected(2) {
		t.Error("item 2 was deselected but still reads as selected")
	}
	if !selection.Selected(3) {
		t.Error("deselecting item 2 turned off item 3")
	}

	// Deselecting twice must not record the key twice: a plan made the same way
	// twice should be identical.
	selection.Select(2, false)
	if len(selection.Unselected) != 1 {
		t.Errorf("unselected = %v, want one entry", selection.Unselected)
	}

	selection.Select(2, true)
	if !selection.Selected(2) || len(selection.Unselected) != 0 {
		t.Errorf("re-selecting left %v behind", selection.Unselected)
	}
}

func TestMarkingForRescan(t *testing.T) {
	selection := &Selection{}

	selection.MarkRescan("tmdb:1:movie", true)
	selection.MarkRescan("tmdb:2:1:4", true)
	if keys := selection.RescanKeys(); !keys["tmdb:1:movie"] || !keys["tmdb:2:1:4"] {
		t.Errorf("RescanKeys = %v, want both marked keys", keys)
	}

	// Marking twice is not two entries, and unmarking is exact.
	selection.MarkRescan("tmdb:1:movie", true)
	if len(selection.Rescan) != 2 {
		t.Errorf("rescan list = %v, want two entries", selection.Rescan)
	}
	selection.MarkRescan("tmdb:1:movie", false)
	if keys := selection.RescanKeys(); keys["tmdb:1:movie"] {
		t.Errorf("RescanKeys = %v, want the unmarked key gone", keys)
	}

	// A blank key is not a lookup that can happen, so it is not recorded.
	selection.MarkRescan("   ", true)
	if len(selection.Rescan) != 1 {
		t.Errorf("rescan list = %v, want the blank key ignored", selection.Rescan)
	}
}

func TestSelectedWorkLeavesItemsOut(t *testing.T) {
	plan := Plan{Items: []ItemPlan{changeable(1), changeable(2), changeable(3)}}
	plan.EnsureSelection().Select(2, false)

	work := plan.SelectedWork()
	if len(work) != 2 {
		t.Fatalf("SelectedWork = %d items, want 2", len(work))
	}
	for _, item := range work {
		if item.Item.RatingKey == 2 {
			t.Error("a deselected item is still in the work")
		}
	}
	if len(plan.Work()) != 3 {
		t.Errorf("Work = %d, want 3: deselecting must not hide items from the preview",
			len(plan.Work()))
	}
}

func TestEnsureSelectionIsStable(t *testing.T) {
	plan := Plan{}
	first := plan.EnsureSelection()
	second := plan.EnsureSelection()
	if first != second {
		t.Error("EnsureSelection replaced an existing selection")
	}
}
