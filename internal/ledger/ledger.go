// Package ledger is the tool's own durable state, kept in a SQLite database
// that is separate from Plex's.
//
// It holds four things:
//
//   - the TheIntroDB lookup cache, so a second run over the same library costs
//     no API requests while the answers are fresh. A cached hit and a cached
//     "no data" expire on different clocks, because a 404 today can become a
//     200 tomorrow once someone submits the timing;
//   - the markers the tool wrote, so a marker Plex later wiped can be re-added
//     and a marker the tool never wrote is never touched;
//   - the request log, which is what enforces the daily request budget. Every
//     request is written as it is made, so a killed run still counts;
//   - run history, for the status output.
//
// SQLite access is serialized through a single connection. Writes are short and
// the volume is a few thousand rows per library, so a pool of one costs nothing
// and removes any chance of SQLITE_BUSY between concurrent callers. WAL and a
// 30 second busy timeout are set on the connection as well, so a second process
// (an undo run, say) can read while a sync writes.
package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, name "sqlite"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// Request sources, used to scope the daily accounting.
const (
	// SourceTheIntroDB is the source recorded for TheIntroDB API requests.
	SourceTheIntroDB = "theintrodb"
	// SourcePlex is the source recorded for Plex API requests.
	SourcePlex = "plex"
	// SourceAny counts every source. It is the zero value, so a caller that
	// does not care can pass "".
	SourceAny = ""
)

// schemaVersion is bumped when a migration adds tables.
const schemaVersion = 1

// schemaStatements build the database. Every one is idempotent, so Open works
// on a fresh file and on an existing one alike.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER NOT NULL,
		applied_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS lookups (
		key        TEXT    PRIMARY KEY,
		status     INTEGER NOT NULL,
		body       TEXT    NOT NULL,
		kind       TEXT    NOT NULL DEFAULT '',
		fetched_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_lookups_expires_at ON lookups (expires_at)`,
	`CREATE TABLE IF NOT EXISTS applied (
		rating_key INTEGER NOT NULL,
		marker_key TEXT    NOT NULL,
		text       TEXT    NOT NULL,
		start_ms   INTEGER NOT NULL,
		end_ms     INTEGER NOT NULL,
		is_final   INTEGER NOT NULL DEFAULT 0,
		source     TEXT    NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (rating_key, marker_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_applied_rating_key ON applied (rating_key)`,
	`CREATE TABLE IF NOT EXISTS requests (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		source TEXT    NOT NULL DEFAULT '',
		ts     INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests (ts)`,
	`CREATE TABLE IF NOT EXISTS runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		started_at  INTEGER NOT NULL,
		finished_at INTEGER NOT NULL,
		source      TEXT    NOT NULL DEFAULT '',
		status      TEXT    NOT NULL DEFAULT '',
		note        TEXT    NOT NULL DEFAULT '',
		items       INTEGER NOT NULL DEFAULT 0,
		lookups     INTEGER NOT NULL DEFAULT 0,
		hits        INTEGER NOT NULL DEFAULT 0,
		misses      INTEGER NOT NULL DEFAULT 0,
		no_data     INTEGER NOT NULL DEFAULT 0,
		added       INTEGER NOT NULL DEFAULT 0,
		removed     INTEGER NOT NULL DEFAULT 0,
		skipped     INTEGER NOT NULL DEFAULT 0,
		errors      INTEGER NOT NULL DEFAULT 0
	)`,
}

// Ledger is the open state database.
type Ledger struct {
	db   *sql.DB
	path string

	mu  sync.Mutex
	now func() time.Time
}

// CachedLookup is one stored TheIntroDB answer.
type CachedLookup struct {
	// Key is the lookup key (provider:id[:season:episode]).
	Key string
	// Status is the HTTP status the body was fetched with, 200 or 404.
	Status int
	// Body is the raw response body, kept verbatim so parsing can be redone
	// without another request.
	Body string
	// Kind is the library kind the answer was for: movie or episode.
	Kind string
	// FetchedAt and ExpiresAt are when the answer was stored and when it stops
	// being trusted.
	FetchedAt time.Time
	ExpiresAt time.Time
}

// Fresh reports whether the entry may still be served at now.
//
// An entry with no expiry (TTL zero or negative) is never fresh.
func (c CachedLookup) Fresh(now time.Time) bool {
	if c.ExpiresAt.IsZero() {
		return false
	}
	return now.Before(c.ExpiresAt)
}

// Age is how long ago the entry was fetched, at now.
func (c CachedLookup) Age(now time.Time) time.Duration {
	if c.FetchedAt.IsZero() {
		return 0
	}
	if d := now.Sub(c.FetchedAt); d > 0 {
		return d
	}
	return 0
}

// Run is one completed run of the tool.
type Run struct {
	ID         int64     `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Source     string    `json:"source,omitempty"`
	Status     string    `json:"status,omitempty"`
	Note       string    `json:"note,omitempty"`
	Items      int       `json:"items"`
	Lookups    int       `json:"lookups"`
	Hits       int       `json:"hits"`
	Misses     int       `json:"misses"`
	NoData     int       `json:"no_data"`
	Added      int       `json:"added"`
	Removed    int       `json:"removed"`
	Skipped    int       `json:"skipped"`
	Errors     int       `json:"errors"`
}

// Duration is how long the run took.
func (r Run) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	if d := r.FinishedAt.Sub(r.StartedAt); d > 0 {
		return d
	}
	return 0
}

// Stats is the summary the status screen shows.
type Stats struct {
	DatabasePath    string `json:"database_path"`
	Lookups         int    `json:"lookups"`
	LookupHits      int    `json:"lookup_hits"`
	LookupMisses    int    `json:"lookup_misses"`
	AppliedItems    int    `json:"applied_items"`
	AppliedMarkers  int    `json:"applied_markers"`
	RequestsTotal   int    `json:"requests_total"`
	RequestsToday   int    `json:"requests_today"`
	Runs            int    `json:"runs"`
	LastRun         *Run   `json:"last_run,omitempty"`
	SchemaVersion   int    `json:"schema_version"`
	DatabaseBytes   int64  `json:"database_bytes"`
	LastRequestTime int64  `json:"last_request_at,omitempty"`
}

// Open opens, creating if needed, the ledger at path.
//
// The parent directory is created when missing. path may be ":memory:" for a
// throwaway database, which the tests use.
func Open(path string) (*Ledger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("ledger: empty database path")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("ledger: create state directory %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	// One connection, kept for the life of the process: every write is short,
	// and this makes SQLITE_BUSY between our own goroutines impossible.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	for _, pragma := range []string{
		"PRAGMA busy_timeout=30000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("ledger: %s: %w", pragma, err)
		}
	}

	l := &Ledger{db: db, path: path, now: time.Now}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// dsn builds a modernc.org/sqlite connection string with the pragmas that must
// be set on every connection, not just the first one.
func dsn(path string) string {
	base := path
	switch {
	case strings.HasPrefix(path, "file:"):
	case path == ":memory:":
		base = "file::memory:"
	default:
		base = "file:" + path
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep +
		"_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// migrate creates the schema.
func (l *Ledger) migrate() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, stmt := range schemaStatements {
		if _, err := l.db.Exec(stmt); err != nil {
			return fmt.Errorf("ledger: migrate: %w", err)
		}
	}
	var have int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&have); err != nil {
		return fmt.Errorf("ledger: read schema version: %w", err)
	}
	if have == 0 {
		if _, err := l.db.Exec(
			`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
			schemaVersion, l.now().Unix(),
		); err != nil {
			return fmt.Errorf("ledger: record schema version: %w", err)
		}
	}
	return nil
}

// Close releases the database.
func (l *Ledger) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.db.Close()
}

// Path is the database file the ledger was opened on.
func (l *Ledger) Path() string { return l.path }

// SetClock replaces the clock used to stamp rows.
//
// The default is time.Now. Tests set it so that the daily budget boundary and
// the cache TTLs are deterministic; a caller must not change it while lookups
// are in flight.
func (l *Ledger) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

func (l *Ledger) clock() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now()
}

// StartOfUTCDay is the Unix time of the most recent UTC midnight at or before t.
//
// TheIntroDB's daily allowance resets at UTC midnight, so the request budget is
// counted from here.
func StartOfUTCDay(t time.Time) int64 {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// ---------------------------------------------------------------------------
// Lookup cache
// ---------------------------------------------------------------------------

// Lookup returns the cached answer for key, whether it is still fresh or not.
//
// The expiry is deliberately not applied here: a caller that wants a stale
// entry's body (to re-parse it, say) can have it, and a caller that wants a
// network-free answer checks Fresh first. The bool reports whether a row exists
// at all.
func (l *Ledger) Lookup(key string) (CachedLookup, bool) {
	if strings.TrimSpace(key) == "" {
		return CachedLookup{}, false
	}
	var (
		out       CachedLookup
		fetchedAt int64
		expiresAt int64
	)
	err := l.db.QueryRow(
		`SELECT key, status, body, kind, fetched_at, expires_at FROM lookups WHERE key = ?`,
		key,
	).Scan(&out.Key, &out.Status, &out.Body, &out.Kind, &fetchedAt, &expiresAt)
	if err != nil {
		return CachedLookup{}, false
	}
	out.FetchedAt = time.Unix(fetchedAt, 0)
	out.ExpiresAt = time.Unix(expiresAt, 0)
	return out, true
}

// PutLookup stores an answer, replacing any earlier one.
//
// ttlSeconds is how long the entry stays fresh: hits are kept for a long time
// because the timing does not change, misses for a short one because a 404
// turns into a 200 the moment someone submits the intro. A non-positive TTL
// stores the body but marks it stale immediately.
func (l *Ledger) PutLookup(key string, status int, body string, ttlSeconds int64, kind string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("ledger: empty lookup key")
	}
	fetchedAt := l.clock()
	expiresAt := fetchedAt
	if ttlSeconds > 0 {
		expiresAt = fetchedAt.Add(time.Duration(ttlSeconds) * time.Second)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.db.Exec(
		`INSERT INTO lookups (key, status, body, kind, fetched_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
			status     = excluded.status,
			body       = excluded.body,
			kind       = excluded.kind,
			fetched_at = excluded.fetched_at,
			expires_at = excluded.expires_at`,
		key, status, body, kind, fetchedAt.Unix(), expiresAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("ledger: store lookup %s: %w", key, err)
	}
	return nil
}

// ForgetLookup drops one cached answer.
func (l *Ledger) ForgetLookup(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.db.Exec(`DELETE FROM lookups WHERE key = ?`, key); err != nil {
		return fmt.Errorf("ledger: forget lookup %s: %w", key, err)
	}
	return nil
}

// PurgeExpired deletes cached answers that stopped being fresh at or before
// now, and reports how many rows went.
func (l *Ledger) PurgeExpired(now time.Time) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`DELETE FROM lookups WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("ledger: purge lookups: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Markers we wrote
// ---------------------------------------------------------------------------

// Applied returns the markers the tool recorded writing for an item.
func (l *Ledger) Applied(ratingKey int64) ([]model.Marker, error) {
	rows, err := l.db.Query(
		`SELECT text, start_ms, end_ms, is_final, source
		   FROM applied WHERE rating_key = ?
		  ORDER BY start_ms, text`,
		ratingKey,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: read applied markers for %d: %w", ratingKey, err)
	}
	defer rows.Close()

	var out []model.Marker
	for rows.Next() {
		var (
			m     model.Marker
			final int
		)
		if err := rows.Scan(&m.Text, &m.StartMS, &m.EndMS, &final, &m.Source); err != nil {
			return nil, fmt.Errorf("ledger: scan applied marker for %d: %w", ratingKey, err)
		}
		m.Final = final != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: read applied markers for %d: %w", ratingKey, err)
	}
	return out, nil
}

// ReplaceApplied records the complete set of markers for an item, replacing
// whatever was recorded before. An empty slice clears the item: it is what a
// run that removed the markers leaves behind.
func (l *Ledger) ReplaceApplied(ratingKey int64, markers []model.Marker) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	tx, err := l.db.Begin()
	if err != nil {
		return fmt.Errorf("ledger: record applied markers for %d: %w", ratingKey, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.Exec(`DELETE FROM applied WHERE rating_key = ?`, ratingKey); err != nil {
		return fmt.Errorf("ledger: clear applied markers for %d: %w", ratingKey, err)
	}
	updated := l.now().Unix()
	for _, m := range markers {
		final := 0
		if m.Final {
			final = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO applied
				(rating_key, marker_key, text, start_ms, end_ms, is_final, source, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(rating_key, marker_key) DO UPDATE SET
				text       = excluded.text,
				start_ms   = excluded.start_ms,
				end_ms     = excluded.end_ms,
				is_final   = excluded.is_final,
				source     = excluded.source,
				updated_at = excluded.updated_at`,
			ratingKey, m.Key(), string(m.Text), m.StartMS, m.EndMS, final, m.Source, updated,
		); err != nil {
			return fmt.Errorf("ledger: record applied marker %s for %d: %w", m.Key(), ratingKey, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: record applied markers for %d: %w", ratingKey, err)
	}
	return nil
}

// ForgetApplied drops the record of what we wrote for an item, after Plex has
// been put back the way we found it.
func (l *Ledger) ForgetApplied(ratingKey int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.db.Exec(`DELETE FROM applied WHERE rating_key = ?`, ratingKey); err != nil {
		return fmt.Errorf("ledger: forget applied markers for %d: %w", ratingKey, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request accounting
// ---------------------------------------------------------------------------

// RecordRequest writes one request to the log as it is made, so that a run that
// is killed mid-flight still counts against the daily budget.
func (l *Ledger) RecordRequest(source string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.db.Exec(
		`INSERT INTO requests (source, ts) VALUES (?, ?)`,
		source, l.now().Unix(),
	); err != nil {
		return fmt.Errorf("ledger: record request: %w", err)
	}
	return nil
}

// RequestsSince counts requests stamped at or after ts, optionally for one
// source ("" counts every source).
func (l *Ledger) RequestsSince(ts int64, source string) (int, error) {
	var (
		n   int
		err error
	)
	if source == "" {
		err = l.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE ts >= ?`, ts).Scan(&n)
	} else {
		err = l.db.QueryRow(
			`SELECT COUNT(*) FROM requests WHERE ts >= ? AND source = ?`, ts, source,
		).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: count requests since %d: %w", ts, err)
	}
	return n, nil
}

// RequestsToday counts requests since the most recent UTC midnight.
func (l *Ledger) RequestsToday(source string) (int, error) {
	return l.RequestsSince(StartOfUTCDay(l.clock()), source)
}

// ---------------------------------------------------------------------------
// Run history
// ---------------------------------------------------------------------------

// RecordRun stores a finished run and returns its id.
func (l *Ledger) RecordRun(run Run) (int64, error) {
	now := l.clock()
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}
	if run.FinishedAt.IsZero() {
		run.FinishedAt = now
	}
	if run.FinishedAt.Before(run.StartedAt) {
		run.FinishedAt = run.StartedAt
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(
		`INSERT INTO runs
			(started_at, finished_at, source, status, note,
			 items, lookups, hits, misses, no_data,
			 added, removed, skipped, errors)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.StartedAt.Unix(), run.FinishedAt.Unix(), run.Source, run.Status, run.Note,
		run.Items, run.Lookups, run.Hits, run.Misses, run.NoData,
		run.Added, run.Removed, run.Skipped, run.Errors,
	)
	if err != nil {
		return 0, fmt.Errorf("ledger: record run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ledger: record run: %w", err)
	}
	return id, nil
}

// Runs returns the most recent runs, newest first. A limit of zero or less
// defaults to ten.
func (l *Ledger) Runs(limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := l.db.Query(
		`SELECT id, started_at, finished_at, source, status, note,
		        items, lookups, hits, misses, no_data,
		        added, removed, skipped, errors
		   FROM runs ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: read runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var (
			r          Run
			startedAt  int64
			finishedAt int64
		)
		if err := rows.Scan(
			&r.ID, &startedAt, &finishedAt, &r.Source, &r.Status, &r.Note,
			&r.Items, &r.Lookups, &r.Hits, &r.Misses, &r.NoData,
			&r.Added, &r.Removed, &r.Skipped, &r.Errors,
		); err != nil {
			return nil, fmt.Errorf("ledger: scan run: %w", err)
		}
		r.StartedAt = time.Unix(startedAt, 0)
		r.FinishedAt = time.Unix(finishedAt, 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: read runs: %w", err)
	}
	return out, nil
}

// Stats summarizes the ledger for the status output.
func (l *Ledger) Stats() (Stats, error) {
	out := Stats{DatabasePath: l.path, SchemaVersion: schemaVersion}

	count := func(query string, dst *int) error {
		if err := l.db.QueryRow(query).Scan(dst); err != nil {
			return fmt.Errorf("ledger: stats: %w", err)
		}
		return nil
	}
	if err := count(`SELECT COUNT(*) FROM lookups`, &out.Lookups); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM lookups WHERE status = 200`, &out.LookupHits); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM lookups WHERE status = 404`, &out.LookupMisses); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(DISTINCT rating_key) FROM applied`, &out.AppliedItems); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM applied`, &out.AppliedMarkers); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM requests`, &out.RequestsTotal); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM runs`, &out.Runs); err != nil {
		return out, err
	}
	today, err := l.RequestsToday(SourceAny)
	if err != nil {
		return out, err
	}
	out.RequestsToday = today

	var last *int64
	if err := l.db.QueryRow(`SELECT MAX(ts) FROM requests`).Scan(&last); err == nil && last != nil {
		out.LastRequestTime = *last
	}

	if out.LastRun, err = l.lastRun(); err != nil {
		return out, err
	}
	if st, err := os.Stat(l.path); err == nil && !st.IsDir() {
		out.DatabaseBytes = st.Size()
	}
	return out, nil
}

func (l *Ledger) lastRun() (*Run, error) {
	runs, err := l.Runs(1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return &runs[0], nil
}
