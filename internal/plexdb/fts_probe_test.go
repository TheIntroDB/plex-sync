package plexdb

import (
	"database/sql"
	"testing"
)

// Plex's database carries FTS4 tables and triggers on them. Whether this build
// of SQLite can even prepare a statement against them decides how the marker tag
// can be created, so it is probed rather than assumed.
func TestFtsModuleAvailability(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, module := range []string{"fts3", "fts4", "fts5"} {
		if _, err := db.Exec(`CREATE VIRTUAL TABLE probe_` + module + ` USING ` + module + `(x)`); err != nil {
			t.Logf("%s: NOT available (%v)", module, err)
			continue
		}
		t.Logf("%s: available", module)
	}
}

// A trigger that references an FTS4 table makes every write to its table fail to
// prepare when the module is missing, even if the trigger's WHEN clause means it
// would never run. Plex has exactly that on the tags table.
func TestFtsTriggerBlocksWritesToItsTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// The shape Plex uses: an FTS table, and a trigger that writes to it but
	// only for tag types that do not include the marker type.
	var available bool
	if _, err := db.Exec(`CREATE VIRTUAL TABLE fts4_probe_titles USING fts4(tag)`); err == nil {
		available = true
	}
	if !available {
		t.Skip("this build of SQLite has no FTS4, so the situation cannot be reproduced here")
	}

	stmts := []string{
		`CREATE TABLE tags(id INTEGER PRIMARY KEY, tag_type INTEGER, tag TEXT)`,
		`CREATE TRIGGER fts4_probe_after_insert AFTER INSERT ON tags
		   WHEN new.tag_type in (0, 1, 2, 4, 6, 207, 400)
		   BEGIN INSERT INTO fts4_probe_titles(docid, tag) VALUES(new.rowid, new.tag); END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	// tag_type 12 is not in the WHEN list, so the trigger body would not run.
	// If this insert fails, the module is missing and the statement cannot even
	// be prepared.
	_, err = db.Exec(`INSERT INTO tags(tag_type, tag) VALUES(12, 'Intro')`)
	t.Logf("insert with an FTS4 trigger present: %v", err)
}
