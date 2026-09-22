package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/config"

	_ "modernc.org/sqlite"
)

// seedPlexDB writes the least a Plex database can be for this test: one table
// for the marker tag. Deliberately not a copy of the real schema, because what
// is being tested here is which connection the app hands out, not what it does
// with it.
func seedPlexDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "library.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`CREATE TABLE tags (
		id INTEGER PRIMARY KEY,
		tag_type INTEGER,
		tag TEXT,
		created_at INTEGER,
		updated_at INTEGER
	)`); err != nil {
		t.Fatalf("create the tags table: %v", err)
	}
	return path
}

// TestAWritableConnectionIsNeverServedByAReadOnlyOne is the regression test for
// the bug that made `plex-sync sync` write nothing at all, on every database,
// while reporting that it had finished.
//
// Read-only is fixed when a connection is opened, so the cached connection
// cannot be promoted to writable afterwards. One run asks for both: the survey
// reads existing markers through a read-only connection, and the apply that
// follows writes through a writable one. The cache used to return whichever was
// opened first, so the writer was handed a read-only connection and every insert
// failed with "attempt to write a readonly database" — or, before that, with a
// failure to create the marker tag, which is how it was found.
//
// Order matters here and is why the test opens read-only first: doing it the
// other way round passes without the fix.
func TestAWritableConnectionIsNeverServedByAReadOnlyOne(t *testing.T) {
	cfg := config.Default()
	cfg.Plex.Database = seedPlexDB(t)

	a := &App{Cfg: cfg}
	defer func() { _ = a.Close() }()

	if _, err := a.PlexDB(true); err != nil {
		t.Fatalf("open read-only, as the survey does: %v", err)
	}

	db, err := a.PlexDB(false)
	if err != nil {
		t.Fatalf("open writable, as the apply does: %v", err)
	}

	// The cheapest possible proof that the connection really is writable: the
	// write this package makes first in a real run.
	ctx := context.Background()
	id, err := db.MarkerTagIDOrCreate(ctx, nil)
	if err != nil {
		t.Fatalf("write through the connection the app handed out: %v", err)
	}
	if id <= 0 {
		t.Fatalf("created tag id %d", id)
	}

	// And a read-only request still gets a connection that works for reading.
	ro, err := a.PlexDB(true)
	if err != nil {
		t.Fatalf("go back to read-only: %v", err)
	}
	if _, err := ro.MarkerTagID(); err != nil {
		t.Fatalf("read through the read-only connection: %v", err)
	}
}

// TestTheDatabaseIsOpenedOncePerMode keeps the fix from becoming a reopen per
// call, which would be correct and slow.
func TestTheDatabaseIsOpenedOncePerMode(t *testing.T) {
	cfg := config.Default()
	cfg.Plex.Database = seedPlexDB(t)

	a := &App{Cfg: cfg}
	defer func() { _ = a.Close() }()

	first, err := a.PlexDB(false)
	if err != nil {
		t.Fatalf("open writable: %v", err)
	}
	second, err := a.PlexDB(false)
	if err != nil {
		t.Fatalf("open writable again: %v", err)
	}
	if first != second {
		t.Error("a second writable request reopened the database")
	}

	// A writable connection serves read-only callers as well: there is nothing
	// to gain by closing it and opening a weaker one.
	readOnly, err := a.PlexDB(true)
	if err != nil {
		t.Fatalf("read through the writable connection: %v", err)
	}
	if readOnly != first {
		t.Error("a read-only request after a writable one reopened the database")
	}
}