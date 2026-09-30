package plexdb

import (
	"context"
	"testing"
)

// TestShowProviderIDsLargeLibrary covers a TV library bigger than SQLite's bound
// parameter limit.
//
// The walk binds one parameter per episode, and SQLITE_MAX_VARIABLE_NUMBER has
// been 32,766 since SQLite 3.32. Above that the whole statement fails, the
// caller treats the failure as non-fatal, and every episode keeps its own
// provider id -- an id TheIntroDB cannot match, so the run spends its allowance
// on lookups that cannot succeed and writes no markers. The library below is
// deliberately over the limit.
func TestShowProviderIDsLargeLibrary(t *testing.T) {
	t.Parallel()

	const episodes = 40_000
	const (
		showID   = int64(900001)
		seasonID = int64(900002)
	)

	f := newFixture(t)

	tmdbTag := f.addTag(TagTypeProviderID, "tmdb://236235")
	imdbTag := f.addTag(TagTypeProviderID, "imdb://tt1489211")

	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 2, NULL, 2, 1, 'The Gentlemen', 0, 1700000000, 1700000000, NULL)`, showID)
	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 3, ?, 2, 2, 'Season 2', 0, 1700000000, 1700000000, NULL)`, seasonID, showID)
	f.exec(`INSERT INTO taggings (metadata_item_id, tag_id, "index", created_at)
VALUES (?, ?, 1, 1700000000)`, showID, tmdbTag)
	f.exec(`INSERT INTO taggings (metadata_item_id, tag_id, "index", created_at)
VALUES (?, ?, 2, 1700000000)`, showID, imdbTag)

	keys := make([]int64, 0, episodes)
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 4, ?, 2, ?, 'Episode', 0, 1700000000, 1700000000, NULL)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = stmt.Close() }()
	for i := 1; i <= episodes; i++ {
		key := int64(i)
		if _, err := stmt.Exec(key, seasonID, i); err != nil {
			t.Fatalf("insert episode %d: %v", i, err)
		}
		keys = append(keys, key)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	db := f.open()
	got, err := db.ShowProviderIDs(context.Background(), keys)
	if err != nil {
		t.Fatalf("ShowProviderIDs over %d episodes: %v", episodes, err)
	}
	if len(got) != episodes {
		t.Fatalf("ShowProviderIDs returned %d episodes, want %d", len(got), episodes)
	}
	for _, key := range keys {
		tags := got[key]
		if len(tags) != 2 {
			t.Fatalf("episode %d carries %d tags, want the show's 2", key, len(tags))
		}
		if tags[0] != "tmdb://236235" || tags[1] != "imdb://tt1489211" {
			t.Fatalf("episode %d tags = %v, want the show's provider ids", key, tags)
		}
	}
}

// TestShowProviderIDsChunkBoundary checks the join between two chunks, and that a
// key named twice is not reported twice.
//
// The library is one key longer than a chunk, so the last key is read by a second
// statement than the first, and the repeat is appended after a whole chunk-sized
// group so deduplication is what keeps it out of the second one. A shorter
// library would put every key in the same statement and prove nothing about
// either.
func TestShowProviderIDsChunkBoundary(t *testing.T) {
	t.Parallel()

	const (
		showID   = int64(900001)
		seasonID = int64(900002)
	)

	f := newFixture(t)
	tag := f.addTag(TagTypeProviderID, "tmdb://1911")

	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 2, NULL, 2, 1, 'Game of Thrones', 0, 1700000000, 1700000000, NULL)`, showID)
	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 3, ?, 2, 1, 'Season 1', 0, 1700000000, 1700000000, NULL)`, seasonID, showID)
	f.exec(`INSERT INTO taggings (metadata_item_id, tag_id, "index", created_at)
VALUES (?, ?, 1, 1700000000)`, showID, tag)

	// One chunk and one more, so the walk has to join two statements.
	distinct := showIDChunk + 1
	keys := make([]int64, 0, distinct+1)
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 4, ?, 2, 1, 'Episode', 0, 1700000000, 1700000000, NULL)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = stmt.Close() }()
	for i := 1; i <= distinct; i++ {
		key := int64(i)
		if _, err := stmt.Exec(key, seasonID); err != nil {
			t.Fatalf("insert episode %d: %v", i, err)
		}
		keys = append(keys, key)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The first key again, after a whole chunk of others.
	keys = append(keys, 1)

	db := f.open()
	got, err := db.ShowProviderIDs(context.Background(), keys)
	if err != nil {
		t.Fatalf("ShowProviderIDs: %v", err)
	}
	if len(got) != distinct {
		t.Fatalf("ShowProviderIDs returned %d episodes, want %d", len(got), distinct)
	}
	for _, key := range keys {
		if tags := got[key]; len(tags) != 1 || tags[0] != "tmdb://1911" {
			t.Fatalf("episode %d tags = %v, want the show's id once", key, tags)
		}
	}
}
