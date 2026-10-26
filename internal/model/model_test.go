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

// The key names the id the request is built from. TVDb comes first because it
// is the identity TheIntroDB stores its data under and the numbering Plex
// assigns by default; TMDb and IMDb only answer for items without one.
func TestLookupKeyUsesTVDbFirst(t *testing.T) {
	tmdb, tvdb := 615, 73871
	imdb := "tt0111161"
	season, episode := 7, 1

	cases := []struct {
		name string
		ids  ExternalIDs
		want string
	}{
		{"all three", ExternalIDs{TMDB: &tmdb, IMDb: &imdb, TVDB: &tvdb}, "tvdb:73871:7:1"},
		{"no tvdb", ExternalIDs{TMDB: &tmdb, IMDb: &imdb}, "tmdb:615:7:1"},
		{"imdb only", ExternalIDs{IMDb: &imdb}, "imdb:tt0111161:7:1"},
	}
	for _, tc := range cases {
		got, ok := tc.ids.LookupKey(&season, &episode)
		if !ok || got != tc.want {
			t.Errorf("%s: LookupKey = %q, %v, want %q", tc.name, got, ok, tc.want)
		}
	}

	if got, ok := (ExternalIDs{TVDB: &tvdb}).LookupKey(nil, nil); !ok || got != "tvdb:73871:movie" {
		t.Errorf("movie LookupKey = %q, %v, want tvdb:73871:movie", got, ok)
	}
	if _, ok := (ExternalIDs{}).LookupKey(&season, &episode); ok {
		t.Error("an item with no ids produced a key")
	}
}

// IDs is what tidb.buildQuery turns into the request, so its order and the
// parameter names it carries are a contract, not an implementation detail.
func TestIDsInRequestOrder(t *testing.T) {
	tmdb, tvdb := 615, 73871
	imdb := "tt0111161"

	got := ExternalIDs{TMDB: &tmdb, IMDb: &imdb, TVDB: &tvdb}.IDs()
	want := []ProviderID{
		{"tvdb", "tvdb_id", "73871"},
		{"tmdb", "tmdb_id", "615"},
		{"imdb", "imdb_id", "tt0111161"},
	}
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IDs()[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// An empty string is not an id: Plex writes those for an unmatched item.
	empty := ""
	if ids := (ExternalIDs{IMDb: &empty}).IDs(); len(ids) != 0 {
		t.Errorf("an empty IMDb id produced %v", ids)
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
