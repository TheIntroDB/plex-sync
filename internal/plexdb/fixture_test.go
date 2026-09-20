package plexdb

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/TheIntroDB/plex-integration/internal/model"
)

// fixtureSchema is the Plex library schema this package is written against,
// reproduced faithfully. The parts that are not guessable and are easy to get
// wrong are all here: tag_type 12 for marker tags, the column literally named
// "index" in taggings, milliseconds in time_offset / end_time_offset,
// media_items.duration as the real file length while metadata_items.duration is
// only the agent's rounded runtime, and a frames_per_second for the PAL guard.
const fixtureSchema = `
CREATE TABLE metadata_items (
    id INTEGER PRIMARY KEY,
    metadata_type INTEGER,
    parent_id INTEGER,
    library_section_id INTEGER,
    "index" INTEGER,
    title TEXT,
    duration INTEGER,
    deleted_at INTEGER,
    added_at INTEGER,
    updated_at INTEGER,
    guid TEXT
);

CREATE TABLE media_items (
    id INTEGER PRIMARY KEY,
    metadata_item_id INTEGER,
    duration INTEGER,
    frames_per_second REAL,
    deleted_at INTEGER
);

CREATE TABLE media_parts (
    id INTEGER PRIMARY KEY,
    media_item_id INTEGER,
    file TEXT,
    size INTEGER,
    duration INTEGER,
    extra_data TEXT,
    deleted_at INTEGER
);

CREATE TABLE tags (
    id INTEGER PRIMARY KEY,
    tag_type INTEGER,
    tag TEXT
);

CREATE TABLE taggings (
    id INTEGER PRIMARY KEY,
    metadata_item_id INTEGER,
    tag_id INTEGER,
    "index" INTEGER,
    text TEXT,
    time_offset INTEGER,
    end_time_offset INTEGER,
    thumb_url TEXT,
    created_at INTEGER,
    extra_data TEXT
);
`

// fixturePartExtraData is a media_parts.extra_data blob as a real Plex server
// writes it after analysing a file: every member is repeated in the url member
// as a query string with both key and value percent-encoded, and a space-free
// value therefore looks like it does here. It is a literal on purpose: the
// fixture must not depend on this package's own encoder to look correct.
const fixturePartExtraData = `{"duration":"2825000","pv:version":"5","url":"duration=2825000&pv%3Aversion=5"}`

// fixtureLegacyPartExtraData is the pre-1.40 format, which carries no url
// member. Plex rewrites it itself and this package must leave it alone.
const fixtureLegacyPartExtraData = `{"some_legacy_key":"old value"}`

// fixtureItem is one library item plus the ids behind it.
type fixtureItem struct {
	RatingKey      int64
	MetadataItemID int64
	MediaItemID    int64
	PartID         int64
	RuntimeMS      int64
	FileDurationMS int64
}

// fixture is a synthetic Plex database in a temporary directory.
type fixture struct {
	t    *testing.T
	path string
	db   *sql.DB

	IntroTagID      int64
	CreditsTagID    int64
	CommercialTagID int64

	auxID int64
}

// newFixture creates the schema and the tags a real server carries: the three
// marker tags (tag_type 12) and the provider-id tags (tag_type 314).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "library.db")}
	db, err := sql.Open("sqlite", dsn(f.path, false))
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	f.db = db

	f.exec(fixtureSchema)
	f.IntroTagID = f.addTag(TagTypeMarker, string(model.MarkerIntro))
	f.CreditsTagID = f.addTag(TagTypeMarker, string(model.MarkerCredits))
	f.CommercialTagID = f.addTag(TagTypeMarker, "commercial")
	f.addTag(TagTypeProviderID, "tmdb://1396")
	f.addTag(TagTypeProviderID, "imdb://tt0944947")
	f.addTag(TagTypeProviderID, "tvdb://121361")
	return f
}

// open opens the fixture through the package's own handle.
func (f *fixture) open() *DB {
	f.t.Helper()
	db, err := OpenDB(f.path, false)
	if err != nil {
		f.t.Fatalf("OpenDB(%s): %v", f.path, err)
	}
	f.t.Cleanup(func() { db.Close() })
	return db
}

// exec runs statements against the fixture connection.
func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("fixture exec %q: %v", query, err)
	}
}

// insert runs one statement and returns the rowid it created.
func (f *fixture) insert(query string, args ...any) int64 {
	f.t.Helper()
	res, err := f.db.Exec(query, args...)
	if err != nil {
		f.t.Fatalf("fixture insert %q: %v", query, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("fixture insert %q: last insert id: %v", query, err)
	}
	return id
}

func (f *fixture) addTag(tagType int, tag string) int64 {
	f.t.Helper()
	return f.insert(`INSERT INTO tags (tag_type, tag) VALUES (?, ?)`, tagType, tag)
}

// addMovie creates a movie (metadata_type 1) with one media file.
func (f *fixture) addMovie(ratingKey int64, title string, runtimeMS, fileDurationMS int64) fixtureItem {
	f.t.Helper()
	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 1, NULL, 1, 1, ?, ?, ?, ?, ?)`,
		ratingKey, title, runtimeMS, 1700000000, 1700000000,
		"com.plexapp.agents.imdb://tt0944947?lang=en")
	return f.addFile(ratingKey, runtimeMS, fileDurationMS)
}

// addEpisode creates a show (metadata_type 2), its season (metadata_type 3) and
// an episode (metadata_type 4) under it, exactly as Plex nests them, then one
// media file for the episode.
//
// Show and season rows take explicit ids from a range well above the rating
// keys in use, so an auto-assigned rowid can never collide with the rating key
// of an item created later.
func (f *fixture) addEpisode(ratingKey int64, showTitle string, season, episode int, runtimeMS, fileDurationMS int64) fixtureItem {
	f.t.Helper()
	f.auxID++
	showID := 900000 + f.auxID
	f.auxID++
	seasonID := 900000 + f.auxID
	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 2, NULL, 2, 1, ?, 0, 1700000000, 1700000000, ?)`,
		showID, showTitle, "com.plexapp.agents.thetvdb://121361?lang=en")
	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 3, ?, 2, ?, ?, 0, 1700000000, 1700000000, ?)`,
		seasonID, showID, season, "Season "+itoa(int64(season)),
		"com.plexapp.agents.thetvdb://121361/"+itoa(int64(season))+"?lang=en")
	f.exec(`INSERT INTO metadata_items
    (id, metadata_type, parent_id, library_section_id, "index", title, duration, added_at, updated_at, guid)
VALUES (?, 4, ?, 2, ?, ?, ?, 1700000000, 1700000000, ?)`,
		ratingKey, seasonID, episode, "Episode "+itoa(int64(episode)), runtimeMS,
		"com.plexapp.agents.thetvdb://121361/"+itoa(int64(season))+"/"+itoa(int64(episode))+"?lang=en")
	return f.addFile(ratingKey, runtimeMS, fileDurationMS)
}

// addFile creates the media_items and media_parts rows behind an item. The file
// length is the real one and is deliberately different from the rounded runtime,
// which is the trap the writer must not fall into.
func (f *fixture) addFile(metadataItemID, runtimeMS, fileDurationMS int64) fixtureItem {
	f.t.Helper()
	mediaItemID := f.insert(`INSERT INTO media_items (metadata_item_id, duration, frames_per_second, deleted_at)
VALUES (?, ?, 23.976, NULL)`, metadataItemID, fileDurationMS)
	partID := f.insert(`INSERT INTO media_parts (media_item_id, file, size, duration, extra_data, deleted_at)
VALUES (?, ?, 123456789, ?, ?, NULL)`,
		mediaItemID, "/library/media/"+itoa(metadataItemID)+".mkv", fileDurationMS, fixturePartExtraData)
	return fixtureItem{
		RatingKey:      metadataItemID,
		MetadataItemID: metadataItemID,
		MediaItemID:    mediaItemID,
		PartID:         partID,
		RuntimeMS:      runtimeMS,
		FileDurationMS: fileDurationMS,
	}
}

// addMarker writes a marker row straight into taggings, the way Plex would.
func (f *fixture) addMarker(metadataItemID, tagID int64, text string, startMS, endMS int64, index int, extraData string) int64 {
	f.t.Helper()
	var thumb any
	var extra any
	if extraData != "" {
		extra = extraData
	}
	return f.insert(`INSERT INTO taggings
    (metadata_item_id, tag_id, "index", text, time_offset, end_time_offset, thumb_url, created_at, extra_data)
VALUES (?, ?, ?, ?, ?, ?, ?, 1700000000, ?)`,
		metadataItemID, tagID, index, text, startMS, endMS, thumb, extra)
}

// markerRow is one taggings row read back with every column.
type markerRow struct {
	ID        int64
	ItemID    int64
	TagID     int64
	Index     int64
	Text      string
	Start     int64
	End       int64
	ThumbURL  any
	CreatedAt int64
	ExtraData any
}

// markers reads every marker row of an item, ordered the way Plex orders them.
func (f *fixture) markers(metadataItemID int64) []markerRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT g.id, g.metadata_item_id, g.tag_id, g."index", g.text,
       g.time_offset, g.end_time_offset, g.thumb_url, g.created_at, g.extra_data
FROM taggings g JOIN tags t ON t.id = g.tag_id
WHERE g.metadata_item_id = ? AND t.tag_type = ?
ORDER BY g.time_offset ASC, g.id ASC`, metadataItemID, TagTypeMarker)
	if err != nil {
		f.t.Fatalf("fixture read markers: %v", err)
	}
	defer rows.Close()
	var out []markerRow
	for rows.Next() {
		var m markerRow
		if err := rows.Scan(&m.ID, &m.ItemID, &m.TagID, &m.Index, &m.Text, &m.Start, &m.End,
			&m.ThumbURL, &m.CreatedAt, &m.ExtraData); err != nil {
			f.t.Fatalf("fixture scan marker: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("fixture read markers: %v", err)
	}
	return out
}

// partExtra reads media_parts.extra_data as a nullable string.
func (f *fixture) partExtra(partID int64) (string, bool) {
	f.t.Helper()
	var extra sql.NullString
	if err := f.db.QueryRow(`SELECT extra_data FROM media_parts WHERE id = ?`, partID).Scan(&extra); err != nil {
		f.t.Fatalf("fixture read part %d extra_data: %v", partID, err)
	}
	return extra.String, extra.Valid
}

// setPartExtra overwrites media_parts.extra_data, including with NULL.
func (f *fixture) setPartExtra(partID int64, extra any) {
	f.t.Helper()
	f.exec(`UPDATE media_parts SET extra_data = ? WHERE id = ?`, extra, partID)
}

// count runs one scalar count query.
func (f *fixture) count(query string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("fixture count %q: %v", query, err)
	}
	return n
}

// snapshotMarkers returns every taggings column of every marker row, for
// comparing a database before and after an undo.
func (f *fixture) snapshotMarkers() []markerRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT g.id, g.metadata_item_id, g.tag_id, g."index", g.text,
       g.time_offset, g.end_time_offset, g.thumb_url, g.created_at, g.extra_data
FROM taggings g JOIN tags t ON t.id = g.tag_id
WHERE t.tag_type = ?
ORDER BY g.id ASC`, TagTypeMarker)
	if err != nil {
		f.t.Fatalf("fixture snapshot markers: %v", err)
	}
	defer rows.Close()
	var out []markerRow
	for rows.Next() {
		var m markerRow
		if err := rows.Scan(&m.ID, &m.ItemID, &m.TagID, &m.Index, &m.Text, &m.Start, &m.End,
			&m.ThumbURL, &m.CreatedAt, &m.ExtraData); err != nil {
			f.t.Fatalf("fixture scan snapshot: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("fixture snapshot markers: %v", err)
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
