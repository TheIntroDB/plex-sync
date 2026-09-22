package plexdb

import (
	"context"
	"path/filepath"
	"testing"
)

// tagTriggers reads the FTS4 triggers on tags, by name and by the SQL that
// creates them.
//
// They are the reason this package has a withoutTagTriggers function at all:
// their bodies name a table built with Plex's own ICU tokenizer, so no SQLite
// outside Plex can prepare them. Whatever a write does to that table, these have
// to be exactly what they were afterwards, which is what these tests check —
// comparing the SQL rather than counting the triggers, because four triggers
// with the wrong bodies is not a restored schema.
func tagTriggers(t *testing.T, path string) map[string]string {
	t.Helper()
	db, err := OpenDB(path, true)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.db.QueryContext(context.Background(),
		`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'tags'`)
	if err != nil {
		t.Fatalf("read the tags triggers: %v", err)
	}
	defer func() { _ = rows.Close() }()

	triggers := map[string]string{}
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			t.Fatalf("read the tags triggers: %v", err)
		}
		triggers[name] = sql
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tags triggers: %v", err)
	}
	if len(triggers) == 0 {
		t.Fatal("this database has no triggers on tags, so nothing here is being tested")
	}
	return triggers
}

// sameTriggers reports the first difference between two sets of triggers, so a
// failure says what changed rather than only that something did.
func sameTriggers(before, after map[string]string) (string, bool) {
	for name, sql := range before {
		got, ok := after[name]
		switch {
		case !ok:
			return "the trigger " + name + " was not put back", false
		case got != sql:
			return "the trigger " + name + " came back with different SQL", false
			// The SQL is what matters, not the formatting of it, but any
			// difference at all here means the schema is not what it was.
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			return "a trigger called " + name + " appeared from nowhere", false
		}
	}
	return "", true
}

// TestLiveTheMarkerTagIsCreatedAndPutBackExactly is the test for the one thing
// this tool could not do for its first release, on a library that has never held
// a marker.
//
// A plain INSERT into tags cannot be prepared outside Plex: the table's FTS4
// triggers name a virtual table created with Plex's own ICU tokenizer
// ("tokenize=collating"), which only the SQLite inside Plex registers. That much
// is true, and the earlier conclusion drawn from it — that the row therefore has
// to come from Plex, and that Plex makes one only with Plex Pass — was wrong.
//
// Dropping a trigger does not need its body prepared. So the triggers come off
// for the duration of the write and go back from the SQL read out of the same
// database, inside one transaction, and a server without Plex Pass can be served
// after all. This checks the whole round trip on a real database: the row
// appears, the triggers are byte-for-byte what they were, and undo removes the
// row and leaves the schema alone.
func TestLiveTheMarkerTagIsCreatedAndPutBackExactly(t *testing.T) {
	path := liveCopy(t)

	before := tagTriggers(t, path)

	db, err := OpenDB(path, false)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}

	if id, err := db.MarkerTagID(); err == nil {
		_ = db.Close()
		t.Skipf("this database already has marker tag %d; creation is not what to test here", id)
	}

	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	journal, err := NewJournal(journalPath)
	if err != nil {
		_ = db.Close()
		t.Fatalf("open a journal: %v", err)
	}

	ctx := context.Background()
	id, err := db.MarkerTagIDOrCreate(ctx, journal)
	if err != nil {
		_ = db.Close()
		t.Fatalf("create the marker tag, which is the whole point of this test: %v", err)
	}
	if err := journal.Close(); err != nil {
		_ = db.Close()
		t.Fatalf("close the journal: %v", err)
	}
	if id <= 0 {
		t.Fatalf("created tag id %d", id)
	}

	var tagType int64
	var name string
	if err := db.db.QueryRowContext(ctx,
		`SELECT tag_type, tag FROM tags WHERE id = ?`, id).Scan(&tagType, &name); err != nil {
		_ = db.Close()
		t.Fatalf("the row that was supposedly created is not there: %v", err)
	}
	if tagType != TagTypeMarker {
		t.Errorf("created a tag with type %d, want %d", tagType, TagTypeMarker)
	}
	if name != MarkerTagName {
		t.Errorf("created a tag named %q, want %q", name, MarkerTagName)
	}

	// The second call is what every later run does, and it must not make a
	// second row.
	again, err := db.MarkerTagIDOrCreate(ctx, nil)
	if err != nil {
		t.Errorf("read the marker tag on a later run: %v", err)
	}
	if again != id {
		t.Errorf("a second run chose tag %d, want the existing %d", again, id)
	}
	var count int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_type = ?`, TagTypeMarker).Scan(&count); err != nil {
		t.Fatalf("count the marker tags: %v", err)
	}
	if count != 1 {
		t.Errorf("the tags table holds %d marker tags, want 1", count)
	}

	_ = db.Close()

	if reason, ok := sameTriggers(before, tagTriggers(t, path)); !ok {
		t.Errorf("creating the tag disturbed the schema: %s", reason)
	}

	// Undo has to delete the row, which meets the same trigger problem from the
	// other side: a DELETE fires the before-delete and after-delete triggers.
	if _, err := Undo(path, journalPath); err != nil {
		t.Fatalf("undo the created tag: %v", err)
	}

	after := tagTriggers(t, path)
	if reason, ok := sameTriggers(before, after); !ok {
		t.Errorf("undo disturbed the schema: %s", reason)
	}
	if len(after) != len(before) {
		t.Errorf("undo left %d triggers on tags, want %d", len(after), len(before))
	}

	check, err := OpenDB(path, true)
	if err != nil {
		t.Fatalf("reopen the copy: %v", err)
	}
	defer func() { _ = check.Close() }()
	if err := check.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("look for the tag after undo: %v", err)
	}
	if count != 0 {
		t.Errorf("the tag is still there after undo")
	}
}
