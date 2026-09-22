package plexdb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// The tests in this file only run against a real Plex database, named by
// TIDB_LIVE_PLEX_DB. They are skipped otherwise, because a copy of a production
// library is not something to commit.
//
// Every defect this file guards against was found by running against a real
// database and could not be reproduced against the synthetic fixture:
//
//   - backups failed with "no such collation sequence: icu_root", because
//     VACUUM INTO rebuilds a schema that uses collations a Go build of SQLite
//     does not implement
//   - nested extra_data members were written as objects where Plex writes JSON
//     strings
//   - the per-row marker payloads used bare numbers where Plex quotes them
//   - the writer invented a final credits marker, leaving the two payloads
//     disagreeing about where an item ends
//
// The source database is never touched: it is copied first. Use
// scripts/live-e2e.sh to prepare a copy, including stripping the FTS4 objects
// that no SQLite outside Plex can parse.
func liveCopy(t *testing.T) string {
	t.Helper()
	source := os.Getenv("TIDB_LIVE_PLEX_DB")
	if source == "" {
		t.Skip("set TIDB_LIVE_PLEX_DB to a copy of a real Plex library.db to run this")
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatalf("TIDB_LIVE_PLEX_DB: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("TIDB_LIVE_PLEX_DB is a directory: %s", source)
	}

	dest := filepath.Join(t.TempDir(), "library.db")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	return dest
}

// liveMovie returns a movie with a real file length to place markers against.
func liveMovie(t *testing.T, db *DB) (ratingKey, durationMS int64) {
	t.Helper()
	err := db.SQL().QueryRow(`
		SELECT mi.metadata_item_id, MAX(mi.duration)
		FROM media_items mi
		JOIN metadata_items m ON m.id = mi.metadata_item_id
		WHERE m.metadata_type = 1 AND mi.duration > 600000 AND m.deleted_at IS NULL
		GROUP BY mi.metadata_item_id
		ORDER BY mi.metadata_item_id
		LIMIT 1`).Scan(&ratingKey, &durationMS)
	if err != nil {
		t.Skipf("no usable movie in this database: %v", err)
	}
	return ratingKey, durationMS
}

func liveExtraData(t *testing.T, db *DB, ratingKey int64) string {
	t.Helper()
	var out string
	err := db.SQL().QueryRow(`
		SELECT mp.extra_data
		FROM media_parts mp
		JOIN media_items mi ON mi.id = mp.media_item_id
		WHERE mi.metadata_item_id = ? AND mp.deleted_at IS NULL
		ORDER BY mp.id
		LIMIT 1`, ratingKey).Scan(&out)
	if err != nil {
		t.Fatalf("read a part's extra_data: %v", err)
	}
	return out
}

// clearMarkerRows removes every marker row an item has, on a copy.
//
// The tests below are about the writer: they say exactly what should end up in
// the database, which only holds if the item starts empty. A real library is not
// empty -- it has whatever Plex detected and whatever this tool wrote before --
// so each test empties its own copy of one item first, and asserts from there.
func clearMarkerRows(t *testing.T, db *DB, ratingKey int64) {
	t.Helper()
	if _, err := db.db.Exec(
		`DELETE FROM taggings WHERE metadata_item_id = ?
		  AND text IN ('intro', 'credits', 'recap', 'preview')`, ratingKey); err != nil {
		t.Fatalf("clear the markers on item %d: %v", ratingKey, err)
	}
}

// checkIntegrity fails the test unless the database passes Plex's own integrity
// check, and tolerates the one case that cannot be checked from here: a real Plex
// database uses collations that only Plex's own SQLite implements, so any check
// run through a different one stops at "no such collation sequence" before it can
// read a page. A copy prepared by scripts/live-e2e.sh has those objects stripped
// and is checked properly, which is what `make test-live` uses.
func checkIntegrity(t *testing.T, path, what string) {
	t.Helper()
	out, err := IntegrityCheck(path)
	if err != nil && strings.Contains(err.Error(), "collation") {
		t.Logf("%s: cannot be checked outside Plex, which is what registers those "+
			"collations (%v); run it against a copy from scripts/live-e2e.sh for the real check",
			what, err)
		return
	}
	if err != nil || out != "ok" {
		t.Errorf("%s: integrity check %q %v", what, out, err)
	}
}

func liveMarkerRows(t *testing.T, db *DB, ratingKey, tagID int64) []model.ExistingMarker {
	t.Helper()
	rows, err := db.ReadMarkers(ratingKey, tagID)
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	return rows
}

// TestLiveApplyWritesAndUndoesExactly is the whole write path against a real
// database: apply, check every byte that landed, undo, check the row is back to
// exactly what it was.
func TestLiveApplyWritesAndUndoesExactly(t *testing.T) {
	path := liveCopy(t)
	db, err := OpenDB(path, false)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer func() { _ = db.Close() }()

	tagID, err := db.MarkerTagID()
	if err != nil {
		t.Skipf("this database has no marker tag, so nothing can be written to it: %v", err)
	}
	ratingKey, durationMS := liveMovie(t, db)
	clearMarkerRows(t, db, ratingKey)

	before := liveExtraData(t, db, ratingKey)
	beforeRows := liveMarkerRows(t, db, ratingKey, tagID)

	// A plan built by hand rather than by the planner: this test is about the
	// writer, so the segments are fixed and the assertions are exact.
	intro := model.Marker{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"}
	credits := model.Marker{
		Text:    model.MarkerCredits,
		StartMS: durationMS - 120_000,
		EndMS:   durationMS - 10_000,
		Source:  "theintrodb",
	}
	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(ratingKey), Kind: model.KindMovie, Title: "live test"},
		Desired: []model.Marker{intro, credits},
		Add:     []model.Marker{intro, credits},
		Reason:  "add",
	}

	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	journal, err := NewJournal(journalPath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 1, journal)
	if closeErr := journal.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if stats.Added != 2 || stats.Written != 1 {
		t.Fatalf("stats = %+v, want 2 added across 1 item", stats)
	}

	// --- what landed -----------------------------------------------------------------

	rows := liveMarkerRows(t, db, ratingKey, tagID)
	if len(rows) != len(beforeRows)+2 {
		t.Fatalf("marker rows = %d, want %d more than the %d already there",
			len(rows), 2, len(beforeRows))
	}
	var ours []model.ExistingMarker
	for _, row := range rows {
		if row.Text == "intro" || row.Text == "credits" {
			ours = append(ours, row)
		}
	}
	if len(ours) != 2 {
		t.Fatalf("found %d of our markers, want 2", len(ours))
	}
	if ours[0].Text != "intro" || ours[0].StartMS != 60_000 || ours[0].EndMS != 90_000 {
		t.Errorf("intro row = %+v", ours[0])
	}
	if ours[1].EndMS != durationMS-10_000 {
		t.Errorf("credits row end = %d, want %d", ours[1].EndMS, durationMS-10_000)
	}

	// The index column is a single sequence ordered by start time across all
	// marker types, and Plex numbers it from zero.
	for i, row := range rows {
		if row.Index != i {
			t.Errorf("row %d has index %d, want %d", i, row.Index, i)
		}
	}

	// The per-row payloads must be the exact bytes Plex writes: values quoted.
	var introPayload, creditsPayload string
	if err := db.SQL().QueryRow(
		`SELECT extra_data FROM taggings WHERE metadata_item_id = ? AND text = 'intro' AND tag_id = ?`,
		ratingKey, tagID).Scan(&introPayload); err != nil {
		t.Fatalf("read the intro payload: %v", err)
	}
	if introPayload != extraIntro {
		t.Errorf("intro payload = %s, want %s", introPayload, extraIntro)
	}
	if err := db.SQL().QueryRow(
		`SELECT extra_data FROM taggings WHERE metadata_item_id = ? AND text = 'credits' AND tag_id = ?`,
		ratingKey, tagID).Scan(&creditsPayload); err != nil {
		t.Fatalf("read the credits payload: %v", err)
	}
	if creditsPayload != extraCredits {
		t.Errorf("credits payload = %s, want %s (the marker does not reach the end of the file)",
			creditsPayload, extraCredits)
	}

	// The part payload keeps every member Plex had, adds ours as JSON strings,
	// and rebuilds the url from all of them.
	after := liveExtraData(t, db, ratingKey)
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(after), &members); err != nil {
		t.Fatalf("the part payload is not valid JSON: %v", err)
	}
	for _, name := range []string{"pv:intros", "pv:credits"} {
		var asString string
		if err := json.Unmarshal(members[name], &asString); err != nil {
			t.Errorf("%s is not stored as a JSON string, which is what Plex writes: %v", name, err)
		}
		if asString == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if !contains(memberStringOf(t, members, "pv:credits"), "startTimeOffset") {
		t.Error("the credits member carries no marker range")
	}
	if memberStringOf(t, members, "pv:credits") != "" && contains(memberStringOf(t, members, "pv:credits"), "final") {
		t.Error("no marker was flagged final, so the payload must not claim one is")
	}

	// Nothing Plex had may have been lost.
	//
	// The two members this plan deliberately rewrote are the exception, and they
	// are checked above for what they should now hold: the point of the run is to
	// replace the intro and credits timings, so requiring those two to come back
	// unchanged would require the write not to have happened. Everything else in
	// the payload -- the chapters, the ma: fields Plex put there -- has to survive
	// untouched, which is what this loop is for.
	var beforeMembers map[string]json.RawMessage
	if err := json.Unmarshal([]byte(before), &beforeMembers); err != nil {
		t.Fatalf("the original payload is not valid JSON: %v", err)
	}
	for name, value := range beforeMembers {
		if name == "url" || name == extraIntroMember || name == extraCreditsMember {
			continue
		}
		got, present := members[name]
		if !present {
			t.Errorf("member %q was dropped from the part payload", name)
			continue
		}
		if string(got) != string(value) {
			t.Errorf("member %q changed: %s became %s", name, value, got)
		}
	}

	// --- undo ------------------------------------------------------------------------

	reverted, err := Undo(path, journalPath)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if reverted == 0 {
		t.Fatal("undo reverted nothing")
	}

	if got := liveMarkerRows(t, db, ratingKey, tagID); len(got) != len(beforeRows) {
		t.Errorf("after undo there are %d marker rows, want the original %d", len(got), len(beforeRows))
	}
	if got := liveExtraData(t, db, ratingKey); got != before {
		t.Errorf("the part payload was not restored byte for byte.\n before: %s\n after:  %s", before, got)
	}
	checkIntegrity(t, path, "after undo")
}

// TestLiveApplyingTheSamePlanTwiceIsIdempotent is the property a nightly job
// depends on: a saved plan re-applied after it has already been applied must
// skip rather than add a second set of markers.
//
// This is why a plan file does not need to be fresh. The reconciliation compares
// every change against the rows actually in the database, so a plan that no
// longer matches what it assumed is skipped instead of duplicated.
func TestLiveApplyingTheSamePlanTwiceIsIdempotent(t *testing.T) {
	path := liveCopy(t)
	db, err := OpenDB(path, false)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer func() { _ = db.Close() }()

	tagID, err := db.MarkerTagID()
	if err != nil {
		t.Skipf("this database has no marker tag: %v", err)
	}
	ratingKey, _ := liveMovie(t, db)
	clearMarkerRows(t, db, ratingKey)
	before := len(liveMarkerRows(t, db, ratingKey, tagID))

	plan := model.ItemPlan{
		Item: model.LibraryItem{RatingKey: int(ratingKey), Kind: model.KindMovie, Title: "twice"},
		Desired: []model.Marker{
			{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"},
		},
		Add: []model.Marker{
			{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"},
		},
		Reason: "add",
	}

	journalDir := t.TempDir()
	apply := func(pass int) WriteStats {
		journal, err := NewJournal(filepath.Join(journalDir, "undo-"+strconv.Itoa(pass)+".jsonl"))
		if err != nil {
			t.Fatalf("journal: %v", err)
		}
		stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 1, journal)
		if closeErr := journal.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatalf("apply %d: %v", pass, err)
		}
		return stats
	}

	first := apply(1)
	if first.Added != 1 || first.Skipped != 0 {
		t.Fatalf("first pass added %d and skipped %d, want 1 and 0", first.Added, first.Skipped)
	}
	afterFirst := len(liveMarkerRows(t, db, ratingKey, tagID))
	if afterFirst != before+1 {
		t.Fatalf("after the first pass there are %d rows, want %d", afterFirst, before+1)
	}

	second := apply(2)
	if second.Added != 0 || second.Skipped != 1 {
		t.Errorf("second pass added %d and skipped %d, want 0 and 1: the same plan "+
			"must not be applied twice", second.Added, second.Skipped)
	}
	if got := len(liveMarkerRows(t, db, ratingKey, tagID)); got != afterFirst {
		t.Errorf("after the second pass there are %d rows, want the %d from the first", got, afterFirst)
	}
}

// TestLiveBackupProducesAUsableCopy checks the backup path against a database
// that has the collations and FTS tables which broke VACUUM INTO.
func TestLiveBackupProducesAUsableCopy(t *testing.T) {
	source := liveCopy(t)
	destDir := filepath.Join(t.TempDir(), "backups")

	dest, err := Backup(source, destDir, 2)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the backup was not written: %v", err)
	}

	// The copy has to be a database, not just a file of the right size.
	db, err := OpenDB(dest, true)
	if err != nil {
		t.Fatalf("the backup is not a usable database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var rows int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM metadata_items`).Scan(&rows); err != nil {
		t.Fatalf("query the backup: %v", err)
	}
	if rows == 0 {
		t.Error("the backup has no rows in it")
	}
	checkIntegrity(t, dest, "the backup")
}

// TestLiveDatabaseHasNoTriggersOnTheTablesWeWrite is the assumption the whole
// design rests on. Plex's tags table cannot be written to from outside Plex
// because of an FTS4 trigger; taggings and media_parts must not have one.
func TestLiveDatabaseHasNoTriggersOnTheTablesWeWrite(t *testing.T) {
	path := liveCopy(t)
	db, err := OpenDB(path, true)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.SQL().Query(
		`SELECT tbl_name FROM sqlite_master WHERE type = 'trigger' AND tbl_name IN ('taggings', 'media_parts')`)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		found = append(found, name)
	}
	if len(found) > 0 {
		t.Fatalf("this database has triggers on the tables we write, so the write path "+
			"cannot be assumed to work: %v", found)
	}
}

// memberStringOf unwraps one member, or returns "" when it is absent.
func memberStringOf(t *testing.T, members map[string]json.RawMessage, name string) string {
	t.Helper()
	raw, present := members[name]
	if !present {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		t.Fatalf("%s is not a JSON string: %v", name, err)
	}
	return asString
}
