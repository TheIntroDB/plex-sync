package plexdb

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// Plex 1.43 does not read markers out of taggings. It keeps them in
// metadata_item_setting_markers, hanging off a per-account settings row, and that
// is what its API serves. This was found the hard way: a marker written to
// taggings and media_parts.extra_data -- the pair the older tools in this space
// use -- survived a Plex restart and a metadata reimport and was still never
// served, while a row written straight into the marker table came back from
// GET /library/metadata/{id}?includeMarkers=1 immediately.
//
// So the writer now writes both, and this is the test that it does: the markers
// reach the table Plex reads, the taggings copy is still there for servers older
// than that table, and undo puts everything back.

// settingMarkerCounts reads the marker table for an item: kind -> rows.
func settingMarkerCounts(t *testing.T, path string, ratingKey int64) map[int64][][2]int64 {
	t.Helper()
	db, err := OpenDB(path, true)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.db.QueryContext(context.Background(),
		`SELECT m.marker_type, m.start_time_offset, COALESCE(m.end_time_offset, 0)
		   FROM `+markerTable+` m
		   JOIN metadata_item_settings s ON s.id = m.metadata_item_setting_id
		  WHERE s.guid = (SELECT guid FROM metadata_items WHERE id = ?)
		  ORDER BY m.marker_type`, ratingKey)
	if err != nil {
		t.Fatalf("read the marker table: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int64][][2]int64{}
	for rows.Next() {
		var kind, start, end int64
		if err := rows.Scan(&kind, &start, &end); err != nil {
			t.Fatalf("scan a marker row: %v", err)
		}
		out[kind] = append(out[kind], [2]int64{start, end})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the marker table: %v", err)
	}
	return out
}

// TestLiveTheMarkersReachTheTablePlexReads is the whole point of this file.
func TestLiveTheMarkersReachTheTablePlexReads(t *testing.T) {
	path := liveCopy(t)

	probe, err := OpenDB(path, true)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	hasTable, err := hasMarkerTable(context.Background(), probe.db)
	_ = probe.Close()
	if err != nil {
		t.Fatalf("look for %s: %v", markerTable, err)
	}
	if !hasTable {
		t.Skipf("this Plex has no %s, so it reads taggings and nothing here applies", markerTable)
	}

	db, err := OpenDB(path, false)
	if err != nil {
		t.Fatalf("open the copy for writing: %v", err)
	}

	ctx := context.Background()
	tagID, err := db.MarkerTagIDOrCreate(ctx, nil)
	if err != nil {
		_ = db.Close()
		t.Fatalf("marker tag: %v", err)
	}
	ratingKey, _ := liveMovie(t, db)

	// Start from an item with no markers in either place. The real library this
	// runs against may already have some -- Plex detected them, or an earlier run
	// of this tool wrote them -- and both storages have to be emptied, not just
	// the new one: a marker left in taggings would be carried into the plan as
	// something to keep, and then this test would be measuring the writer's
	// handling of a kind that has two answers rather than the write itself.
	// Clearing is free because this is a copy.
	if _, err := db.db.ExecContext(ctx,
		`DELETE FROM `+markerTable+` WHERE metadata_item_setting_id IN (
		     SELECT id FROM metadata_item_settings
		      WHERE guid = (SELECT guid FROM metadata_items WHERE id = ?))`,
		ratingKey); err != nil {
		_ = db.Close()
		t.Fatalf("clear the marker table for the item: %v", err)
	}
	clearMarkerRows(t, db, ratingKey)
	before := settingMarkerCounts(t, path, ratingKey)

	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	journal, err := NewJournal(journalPath)
	if err != nil {
		_ = db.Close()
		t.Fatalf("journal: %v", err)
	}

	// A plan has to account for the rows already there: apply refuses one that
	// does not, which is how two runs cannot quietly disagree about an item.
	liveMarkers, err := db.ReadMarkers(ratingKey, tagID)
	if err != nil {
		_ = db.Close()
		t.Fatalf("read the markers already there: %v", err)
	}
	if len(liveMarkers) != 0 {
		_ = db.Close()
		t.Fatalf("the item still has %d marker row(s) after clearing it", len(liveMarkers))
	}
	added := []model.Marker{
		{Text: model.MarkerIntro, StartMS: 5_000, EndMS: 42_000, Source: "theintrodb"},
		{Text: model.MarkerCredits, StartMS: 900_000, EndMS: 960_000, Source: "theintrodb"},
	}
	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(ratingKey), Kind: model.KindMovie, Title: "marker table"},
		Kept:    liveMarkers,
		Desired: added,
		Add:     added,
	}

	if _, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 1, journal); err != nil {
		_ = db.Close()
		t.Fatalf("apply: %v", err)
	}
	if err := journal.Close(); err != nil {
		_ = db.Close()
		t.Fatalf("close the journal: %v", err)
	}
	_ = db.Close()

	after := settingMarkerCounts(t, path, ratingKey)

	// The two kinds, with the ranges that were asked for. 1 is intro and 5 is
	// credits; those numbers came out of Plex itself, not from a guess.
	intro, credits := after[plexMarkerIntro], after[plexMarkerCredits]
	if len(intro) != 1 || intro[0] != [2]int64{5_000, 42_000} {
		t.Errorf("intro markers in the table Plex reads = %v, want one at 5000-42000", intro)
	}
	if len(credits) != 1 || credits[0] != [2]int64{900_000, 960_000} {
		t.Errorf("credits markers in the table Plex reads = %v, want one at 900000-960000", credits)
	}
	if len(before[plexMarkerIntro]) == 0 && len(after[plexMarkerIntro]) != 1 {
		t.Errorf("an item with no intro marker gained %d", len(after[plexMarkerIntro]))
	}

	// The taggings copy stays, for a Plex older than the table.
	check, err := OpenDB(path, true)
	if err != nil {
		t.Fatalf("reopen the copy: %v", err)
	}
	live := liveMarkerRows(t, check, ratingKey, tagID)
	_ = check.Close()
	if len(live) == 0 {
		t.Error("nothing was written to taggings, which older servers read")
	}

	// And it is all reversible, which is the promise this tool makes.
	if _, err := Undo(path, journalPath); err != nil {
		t.Fatalf("undo: %v", err)
	}
	restored := settingMarkerCounts(t, path, ratingKey)
	for kind, want := range before {
		if len(restored[kind]) != len(want) {
			t.Errorf("after undo, kind %d has %d row(s), want %d", kind, len(restored[kind]), len(want))
		}
	}
	for _, kind := range []int64{plexMarkerIntro, plexMarkerCredits} {
		if len(restored[kind]) != len(before[kind]) {
			t.Errorf("after undo, kind %d has %v, want %v", kind, restored[kind], before[kind])
		}
	}
}

// TestLiveAMarkerPlexDetectedIsNotOverwritten: the planner reads what it can see
// in taggings, so it cannot know about a marker Plex wrote straight into its own
// table. Writing over one of those would replace a better answer with this tool's
// guess, unless the configuration asks for exactly that.
func TestLiveAMarkerPlexDetectedIsNotOverwritten(t *testing.T) {
	path := liveCopy(t)

	db, err := OpenDB(path, false)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	ctx := context.Background()
	if ok, err := hasMarkerTable(ctx, db.db); err != nil || !ok {
		_ = db.Close()
		t.Skip("no marker table on this Plex")
	}

	ratingKey, _ := liveMovie(t, db)
	row, err := ensureSettingRow(ctx, db.db, ratingKey, nil)
	if err != nil {
		_ = db.Close()
		t.Fatalf("settings row: %v", err)
	}

	// A marker as Plex would have written it: no provenance stamp of ours.
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO `+markerTable+`
		    (marker_type, metadata_item_setting_id, start_time_offset, end_time_offset,
		     title, created_at, updated_at, extra_data)
		 VALUES (?, ?, ?, ?, 'Intro', 1, 1, NULL)`,
		plexMarkerIntro, row.ID, 11_000, 33_000); err != nil {
		_ = db.Close()
		t.Fatalf("write a marker as Plex would: %v", err)
	}
	_ = db.Close()

	// A plan that wants a different intro, under the default policy.
	db, err = OpenDB(path, false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()
	tagID, err := db.MarkerTagIDOrCreate(ctx, nil)
	if err != nil {
		t.Fatalf("marker tag: %v", err)
	}
	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(ratingKey), Kind: model.KindMovie, Title: "kept"},
		Desired: []model.Marker{{Text: model.MarkerIntro, StartMS: 1_000, EndMS: 2_000, Source: "theintrodb"}},
		Add:     []model.Marker{{Text: model.MarkerIntro, StartMS: 1_000, EndMS: 2_000, Source: "theintrodb"}},
	}
	if _, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 1, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	_ = db.Close()

	got := settingMarkerCounts(t, path, ratingKey)[plexMarkerIntro]
	found := false
	for _, r := range got {
		if r == [2]int64{11_000, 33_000} {
			found = true
		}
		if r == [2]int64{1_000, 2_000} {
			t.Errorf("Plex's own marker was replaced by ours under the fill policy: %v", got)
		}
	}
	if !found {
		t.Errorf("Plex's own marker is gone: %v", got)
	}
}
