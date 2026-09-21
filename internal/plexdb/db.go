// Package plexdb reads and writes the markers Plex keeps in its SQLite library
// database.
//
// Plex has no HTTP API for intro or credits markers: its marker endpoint
// accepts bookmarks only, so every tool in this space edits "library.db"
// directly and so does this package. Everything here is written defensively.
// Every connection sets a busy timeout, writes run in BEGIN IMMEDIATE
// transactions, every operation is journalled before it happens, and an item
// whose live markers no longer match the plan it was built from is skipped
// rather than overwritten.
//
// Nothing here ever touches a real database during tests: the fixture builds a
// synthetic library.db with the verified Plex schema in a temporary directory.
package plexdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, driver name "sqlite"; no CGO on purpose

	"github.com/TheIntroDB/plex-integration/internal/model"
)

// Tag types Plex uses. Marker rows hang off a tag with TagTypeMarker, and the
// provider ids (tmdb://, imdb://, tvdb://) off a tag with TagTypeProviderID.
const (
	TagTypeMarker     = 12
	TagTypeProviderID = 314
)

// BusyTimeoutMS is the SQLite busy timeout set on every connection. A live
// Plex server writes to the same file, so a writer must be prepared to wait.
const BusyTimeoutMS = 30000

// DB is an open Plex library database.
type DB struct {
	path     string
	db       *sql.DB
	readOnly bool
}

// querier is the read/write surface shared by *sql.DB and *sql.Tx, so the read
// helpers can run both standalone and inside a transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// dsn builds the modernc.org/sqlite DSN. The busy timeout and the immediate
// transaction mode ride on the DSN so they are applied to every connection the
// pool opens, not just the first one. Paths are kept in SQLite URI form, which
// is what "mode=ro" needs.
func dsn(path string, readOnly bool) string {
	p := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	s := "file:" + p + "?mode=rwc&_pragma=busy_timeout(30000)&_txlock=immediate"
	if readOnly {
		s = "file:" + p + "?mode=ro&_pragma=busy_timeout(30000)&_txlock=immediate"
	}
	return s
}

// OpenDB opens the Plex database at path. When readOnly is true the connection
// is opened with mode=ro, so a read can never create journals or write locks.
func OpenDB(path string, readOnly bool) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("plexdb: empty database path")
	}
	if readOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("plexdb: open %s read-only: %w", path, err)
		}
	}

	raw, err := sql.Open("sqlite", dsn(path, readOnly))
	if err != nil {
		return nil, fmt.Errorf("plexdb: open %s: %w", path, err)
	}
	if readOnly {
		raw.SetMaxOpenConns(4)
	} else {
		// One writer connection: Plex is the other writer and SQLite
		// serialises writers anyway, so a pool would only add lock
		// contention to our own transactions.
		raw.SetMaxOpenConns(1)
	}
	raw.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("plexdb: ping %s: %w", path, err)
	}

	return &DB{path: path, db: raw, readOnly: readOnly}, nil
}

// Close releases the database handle.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// Path is the file this handle was opened against.
func (d *DB) Path() string { return d.path }

// SQL exposes the underlying handle for callers that need it (status output,
// read-only inventory queries). Writes should go through ApplyPlans.
func (d *DB) SQL() *sql.DB { return d.db }

// MarkerTagName is the name given to the marker tag this tool creates when Plex
// has not made one.
//
// The name is cosmetic. Plex's marker lookup filters on tag_type, and the marker
// kind lives in taggings.text, so the tag's own name is never read back. It is
// set to something a human reading the table will recognise.
const MarkerTagName = "Intro"

// MarkerTagIDOrCreate returns the marker tag, creating it when the database has
// none.
//
// Plex creates that row the first time it writes a marker itself, but writing
// markers is a Plex Pass feature: on a server without it, Plex never creates one
// and never will, and a tool like this is the only thing that ever would. So the
// row is created on request rather than leaving the tool unable to write
// anything at all on such a server.
//
// The stated rule is that Plex's schema is not ours to invent, which is why this
// is opt-in and why the row is journalled like any other change.
func (d *DB) MarkerTagIDOrCreate(ctx context.Context, j *Journal) (int64, error) {
	if id, err := d.MarkerTagID(); err == nil {
		return id, nil
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("plexdb: create the marker tag: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM tags`).Scan(&id); err != nil {
		return 0, fmt.Errorf("plexdb: next tag id: %w", err)
	}

	// Journal before the write, like every other change this tool makes, so an
	// interrupted run leaves nothing behind that cannot be undone.
	if j != nil {
		if err := j.Record(map[string]any{
			"op":       "tag_insert",
			"tag_id":   id,
			"tag_type": int64(TagTypeMarker),
		}); err != nil {
			return 0, err
		}
	}

	now := time.Now().UTC().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tags(id, tag_type, tag, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, TagTypeMarker, MarkerTagName, now, now,
	); err != nil {
		return 0, missingFTSModuleError(err, d.path)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("plexdb: create the marker tag: %w", err)
	}
	return id, nil
}

// missingFTSModuleError explains the one write this tool cannot make, and why
// there is no workaround to offer.
//
// Plex's tags table carries an FTS4 trigger, and that trigger's table is created
// with Plex's own ICU tokenizer:
//
//	CREATE VIRTUAL TABLE fts4_tag_titles_icu USING fts4(tag, tokenize=collating '...')
//
// Any write to tags has to prepare the trigger body, which means opening that
// table, which means resolving a tokenizer that only the SQLite built into Plex
// registers. So the statement fails before its WHEN clause is ever considered,
// and it fails for every SQLite outside Plex: this program's pure-Go build
// reports "no such module: fts4", and the system sqlite3 command reports
// "unknown tokenizer: collating". Switching to a CGO build with FTS4 compiled in
// would not help either.
//
// The row therefore has to come from Plex, which creates it the first time it
// writes a marker of its own. That is a Plex Pass feature. Everything else this
// tool does goes to taggings and media_parts, which carry no triggers at all.
func missingFTSModuleError(err error, dbPath string) error {
	message := err.Error()
	if !strings.Contains(message, "no such module") &&
		!strings.Contains(message, "unknown tokenizer") {
		return fmt.Errorf("plexdb: create the marker tag: %w", err)
	}
	return fmt.Errorf(
		"plexdb: cannot create the marker tag in %s, and no other tool can either.\n\n"+
			"Plex's tags table has an FTS4 trigger whose table uses Plex's own ICU tokenizer "+
			"(\"tokenize=collating\"), which only the SQLite inside Plex implements, so any write "+
			"to that table fails before the trigger's condition is even considered. The system "+
			"sqlite3 command fails the same way, and so would a build with FTS4 compiled in.\n\n"+
			"Let Plex create the row: it makes one the first time it writes a marker of its own, "+
			"which needs Plex Pass. After that, this tool never touches tags again and everything "+
			"works. Original error: %v", dbPath, err)
}

// MarkerTagID returns the marker tag Plex uses, when one exists.
//
// Plex has one tag row per marker kind ("intro", "credits", "commercial") but
// they all share tag_type 12, so the lowest id is returned. That is the id used
// as the default by every tool in this space, and a marker row's own kind comes
// from its text column, not from which tag it points at.
func (d *DB) MarkerTagID() (int64, error) {
	ctx := context.Background()
	var id int64
	err := d.db.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE tag_type = ? ORDER BY id LIMIT 1`, TagTypeMarker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("plexdb: %s has no marker tag (tag_type %d)", d.path, TagTypeMarker)
	}
	if err != nil {
		return 0, fmt.Errorf("plexdb: read marker tag: %w", err)
	}
	return id, nil
}

// ReadMarkers returns the marker rows Plex holds for one item, ordered by start
// time. When tagID is positive only rows carrying that tag are returned; pass 0
// to read every tag_type-12 row regardless of tag.
func (d *DB) ReadMarkers(ratingKey, tagID int64) ([]model.ExistingMarker, error) {
	return readMarkers(context.Background(), d.db, ratingKey, tagID)
}

// readMarkers is the transaction-safe core of ReadMarkers.
//
// taggings.id is the row identity a plan deletes by, so it lands in
// ExistingMarker.TagID; the tag the row hangs off is not interesting to a plan.
func readMarkers(ctx context.Context, q querier, ratingKey, tagID int64) ([]model.ExistingMarker, error) {
	query := `SELECT g.id, g.tag_id, COALESCE(g.text, ''), COALESCE(g.time_offset, 0),
       COALESCE(g.end_time_offset, 0), COALESCE(g."index", 0), COALESCE(g.extra_data, '')
FROM taggings g
JOIN tags t ON t.id = g.tag_id
WHERE g.metadata_item_id = ? AND t.tag_type = ?`
	args := []any{ratingKey, TagTypeMarker}
	if tagID > 0 {
		query += ` AND g.tag_id = ?`
		args = append(args, tagID)
	}
	query += ` ORDER BY g.time_offset ASC, g.id ASC`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("plexdb: read markers for %d: %w", ratingKey, err)
	}
	defer rows.Close()

	var out []model.ExistingMarker
	for rows.Next() {
		var m model.ExistingMarker
		if err := rows.Scan(&m.TagID, new(int64), &m.Text, &m.StartMS, &m.EndMS, &m.Index, &m.ExtraData); err != nil {
			return nil, fmt.Errorf("plexdb: scan marker for %d: %w", ratingKey, err)
		}
		m.Origin = string(model.OriginPlex)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plexdb: read markers for %d: %w", ratingKey, err)
	}
	return out, nil
}

// Part is one media file behind an item. An item with more than one part is
// ambiguous for marker placement and callers skip it.
type Part struct {
	ID          int64
	MediaItemID int64
	File        string
	Size        int64
	Duration    int64
	ExtraData   string
}

// Parts returns the live media parts of an item.
func (d *DB) Parts(ratingKey int64) ([]Part, error) {
	return parts(context.Background(), d.db, ratingKey)
}

// parts is the transaction-safe core of Parts.
func parts(ctx context.Context, q querier, ratingKey int64) ([]Part, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id, p.media_item_id, COALESCE(p.file, ''),
       COALESCE(p.size, 0), COALESCE(p.duration, 0), COALESCE(p.extra_data, '')
FROM media_parts p
JOIN media_items m ON m.id = p.media_item_id
WHERE m.metadata_item_id = ?
  AND (m.deleted_at IS NULL OR m.deleted_at = 0)
  AND (p.deleted_at IS NULL OR p.deleted_at = 0)
ORDER BY p.id ASC`, ratingKey)
	if err != nil {
		return nil, fmt.Errorf("plexdb: read parts for %d: %w", ratingKey, err)
	}
	defer rows.Close()

	var out []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.ID, &p.MediaItemID, &p.File, &p.Size, &p.Duration, &p.ExtraData); err != nil {
			return nil, fmt.Errorf("plexdb: scan part for %d: %w", ratingKey, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plexdb: read parts for %d: %w", ratingKey, err)
	}
	return out, nil
}

// Item returns the metadata_items row for one rating key: kind, title and the
// rounded runtime Plex reports.
//
// The runtime here is the agent's rounded value, not the file length; marker
// placement must use media_items.duration through Parts instead.
func (d *DB) Item(ratingKey int64) (model.LibraryItem, error) {
	ctx := context.Background()
	var (
		item       model.LibraryItem
		metadataID int64
		metaType   sql.NullInt64
		title      sql.NullString
		duration   sql.NullInt64
		guid       sql.NullString
	)
	err := d.db.QueryRowContext(ctx, `SELECT id, metadata_type, title, duration, guid
FROM metadata_items WHERE id = ?`, ratingKey).
		Scan(&metadataID, &metaType, &title, &duration, &guid)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("plexdb: no metadata_items row for %d", ratingKey)
	}
	if err != nil {
		return item, fmt.Errorf("plexdb: read item %d: %w", ratingKey, err)
	}
	item.RatingKey = int(metadataID)
	item.Title = title.String
	switch metaType.Int64 {
	case 1:
		item.Kind = model.KindMovie
	case 4:
		item.Kind = model.KindEpisode
	}
	if duration.Valid {
		d := duration.Int64
		item.DurationMS = &d
	}
	return item, nil
}

// IntegrityCheck runs PRAGMA integrity_check and returns its result, "ok" when
// the file is sound.
func IntegrityCheck(dbPath string) (string, error) {
	d, err := OpenDB(dbPath, true)
	if err != nil {
		return "", err
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rows, err := d.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return "", fmt.Errorf("plexdb: integrity_check %s: %w", dbPath, err)
	}
	defer rows.Close()

	var msgs []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return "", fmt.Errorf("plexdb: scan integrity_check: %w", err)
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("plexdb: integrity_check %s: %w", dbPath, err)
	}
	if len(msgs) == 0 {
		return "ok", nil
	}
	return strings.Join(msgs, "; "), nil
}

// nextTaggingID returns the row id the next inserted tagging will get. It is
// only called while holding the write lock (BEGIN IMMEDIATE), which is what
// makes MAX(id)+1 safe and lets the journal name the row before it exists.
func nextTaggingID(ctx context.Context, q querier) (int64, error) {
	var id sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(id) FROM taggings`).Scan(&id); err != nil {
		return 0, fmt.Errorf("plexdb: read max tagging id: %w", err)
	}
	return id.Int64 + 1, nil
}

// markerTagIDFor resolves the tag a marker row should hang off. Plex keeps a
// separate tag_type-12 tag for each marker text, so the text wins; tagID is the
// fallback for libraries that only carry one.
func markerTagIDFor(ctx context.Context, q querier, text string, fallback int64) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM tags WHERE tag_type = ? AND tag = ? ORDER BY id LIMIT 1`,
		TagTypeMarker, text).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, sql.ErrNoRows):
		if fallback > 0 {
			return fallback, nil
		}
		return 0, fmt.Errorf("plexdb: no tag_type %d tag for text %q and no fallback tag id", TagTypeMarker, text)
	default:
		return 0, fmt.Errorf("plexdb: resolve marker tag %q: %w", text, err)
	}
}

// taggingRowColumns is the full column list of a marker-capable taggings row,
// in the order the journal and undo use.
var taggingRowColumns = []string{
	"id", "metadata_item_id", "tag_id", "index", "text",
	"time_offset", "end_time_offset", "thumb_url", "created_at", "extra_data",
}

// readTaggingRow reads every column of one taggings row so a delete can be
// undone byte for byte. Missing values are reported as nil.
func readTaggingRow(ctx context.Context, q querier, id int64) (map[string]any, error) {
	var (
		cid, cmid, ctag, cidx, start, end, created sql.NullInt64
		text, thumb, extra                         sql.NullString
	)
	err := q.QueryRowContext(ctx, `SELECT id, metadata_item_id, tag_id, "index", text,
       time_offset, end_time_offset, thumb_url, created_at, extra_data
FROM taggings WHERE id = ?`, id).
		Scan(&cid, &cmid, &ctag, &cidx, &text, &start, &end, &thumb, &created, &extra)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("plexdb: read tagging %d: %w", id, err)
	}
	row := map[string]any{
		"id":               cid.Int64,
		"metadata_item_id": cmid.Int64,
		"tag_id":           ctag.Int64,
		"index":            cidx.Int64,
		"text":             text.String,
		"time_offset":      start.Int64,
		"end_time_offset":  end.Int64,
		"created_at":       created.Int64,
	}
	if text.Valid {
		row["text"] = text.String
	} else {
		row["text"] = nil
	}
	if thumb.Valid {
		row["thumb_url"] = thumb.String
	} else {
		row["thumb_url"] = nil
	}
	if extra.Valid {
		row["extra_data"] = extra.String
	} else {
		row["extra_data"] = nil
	}
	return row, nil
}
