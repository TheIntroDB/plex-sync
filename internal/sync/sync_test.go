package sync

import (
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// changeable builds an item that needs a write.
func changeable(ratingKey int) model.ItemPlan {
	return model.ItemPlan{
		Item: model.LibraryItem{RatingKey: ratingKey, Title: "item"},
		Add: []model.Marker{{
			Text: model.MarkerIntro, StartMS: 0, EndMS: 1000, Source: string(model.SourceTheIntroDB),
		}},
		Reason: "add",
	}
}

func keys(items []model.ItemPlan) []int {
	var out []int
	for _, item := range items {
		out = append(out, item.Item.RatingKey)
	}
	return out
}

// A preview screen's selection has to reach the writer, or it is decoration.
func TestSelectedWorkAppliesThePlansOwnSelection(t *testing.T) {
	plan := model.Plan{Items: []model.ItemPlan{changeable(1), changeable(2), changeable(3)}}
	plan.EnsureSelection().Select(2, false)

	work := selectedWork(plan, Options{})
	if len(work) != 2 {
		t.Fatalf("selectedWork = %v, want items 1 and 3", keys(work))
	}
	if len(work) == 2 && (work[0].Item.RatingKey == 2 || work[1].Item.RatingKey == 2) {
		t.Errorf("selectedWork = %v, want item 2 left out", keys(work))
	}
}

func TestSelectedWorkHonoursTheCallersFlags(t *testing.T) {
	plan := model.Plan{Items: []model.ItemPlan{changeable(1), changeable(2), changeable(3)}}

	// --deselect removes an item without touching the plan.
	work := selectedWork(plan, Options{Deselected: []int{1}})
	if len(work) != 2 {
		t.Errorf("selectedWork = %v, want items 2 and 3", keys(work))
	}

	// --select narrows the write to exactly these keys.
	work = selectedWork(plan, Options{Only: []int{2}})
	if len(work) != 1 || work[0].Item.RatingKey != 2 {
		t.Errorf("selectedWork = %v, want only item 2", keys(work))
	}

	// It narrows rather than replaces: a plan that turned an item off keeps it
	// off, so a flag cannot silently reintroduce something the plan excluded.
	plan.EnsureSelection().Select(1, true)
	plan.EnsureSelection().Select(2, false)
	if got := selectedWork(plan, Options{Only: []int{1, 2}}); len(got) != 1 || got[0].Item.RatingKey != 1 {
		t.Errorf("selectedWork = %v, want only the still-selected item 1", keys(got))
	}

	// An empty plan stays empty rather than becoming an accident.
	if got := selectedWork(model.Plan{}, Options{}); len(got) != 0 {
		t.Errorf("selectedWork on an empty plan = %v, want nothing", keys(got))
	}
}

func TestRescanSetIgnoresBlanks(t *testing.T) {
	set := rescanSet([]string{"tmdb:1:movie", "  ", "tmdb:2:1:4"})
	if len(set) != 2 || !set["tmdb:1:movie"] || !set["tmdb:2:1:4"] {
		t.Errorf("rescanSet = %v, want the two real keys", set)
	}
	// A key's own whitespace is trimmed rather than kept as part of the key.
	if !rescanSet([]string{" tmdb:1:movie "})["tmdb:1:movie"] {
		t.Error("rescanSet did not trim a key")
	}
}
