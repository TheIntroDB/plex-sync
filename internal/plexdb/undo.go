package plexdb

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Journal is the undo log of a write run: JSONL, one JSON object per line, one
// operation per object.
//
// Every operation is appended, and flushed, before the write it describes
// happens, so a crash between the two leaves an undo entry for a row that was
// never written, which is harmless, rather than a written row with no way back.
//
// The operations are:
//
//	{"op":"insert","rating_key":N,"id":NEW_TAGGING_ID,"tag_id":TAG_ID}
//	{"op":"delete","rating_key":N,"row":{...every original taggings column...}}
//	{"op":"index","rating_key":N,"id":TAGGING_ID,"old_index":N}
//	{"op":"extra","rating_key":N,"part_id":N,"old":PREVIOUS_EXTRA_DATA_OR_NULL}
//
// The tag_id key on an insert is recorded alongside the new taggings row id so
// the entry is complete either way a reader interprets it.
type Journal struct {
	path   string
	mu     sync.Mutex
	file   *os.File
	buf    *bufio.Writer
	closed bool
}

// NewJournal opens (or creates) a journal for appending.
func NewJournal(path string) (*Journal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("plexdb: empty journal path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("plexdb: create journal directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("plexdb: open journal %s: %w", path, err)
	}
	return &Journal{path: path, file: f, buf: bufio.NewWriter(f)}, nil
}

// Path is the file this journal writes to.
func (j *Journal) Path() string {
	if j == nil {
		return ""
	}
	return j.path
}

// Record appends one operation. It returns only after the line has reached the
// operating system, so the entry outlives a crash that follows it.
func (j *Journal) Record(op map[string]any) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errors.New("plexdb: journal is closed")
	}
	line, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("plexdb: encode journal operation: %w", err)
	}
	line = append(line, '\n')
	if _, err := j.buf.Write(line); err != nil {
		return fmt.Errorf("plexdb: write journal: %w", err)
	}
	if err := j.buf.Flush(); err != nil {
		return fmt.Errorf("plexdb: flush journal: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("plexdb: sync journal: %w", err)
	}
	return nil
}

// Close flushes and closes the journal.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.buf != nil {
		if err := j.buf.Flush(); err != nil {
			j.file.Close()
			return fmt.Errorf("plexdb: flush journal: %w", err)
		}
	}
	return j.file.Close()
}

// ReadJournal reads a journal back. Blank lines are ignored.
func ReadJournal(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plexdb: open journal %s: %w", path, err)
	}
	defer f.Close()

	var ops []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var op map[string]any
		if err := json.Unmarshal([]byte(text), &op); err != nil {
			return nil, fmt.Errorf("plexdb: journal %s line %d: %w", path, line, err)
		}
		if op == nil {
			return nil, fmt.Errorf("plexdb: journal %s line %d is not an object", path, line)
		}
		ops = append(ops, op)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("plexdb: read journal %s: %w", path, err)
	}
	return ops, nil
}

// Undo replays a journal against a database in REVERSE order, inside a single
// transaction, and returns how many operations were undone.
//
// Reversal is what makes it correct: the last insert is removed before the
// index renumbering that followed it is restored, and the index renumbering is
// restored before the delete that preceded it puts its rows back.
func Undo(dbPath, journalPath string) (int, error) {
	ops, err := ReadJournal(journalPath)
	if err != nil {
		return 0, err
	}
	if len(ops) == 0 {
		return 0, nil
	}

	d, err := OpenDB(dbPath, false)
	if err != nil {
		return 0, err
	}
	defer d.Close()

	ctx := context.Background()
	tx, err := d.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE
	if err != nil {
		return 0, fmt.Errorf("plexdb: begin undo: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()

	applied := 0
	for i := len(ops) - 1; i >= 0; i-- {
		if err := undoOp(ctx, tx, ops[i]); err != nil {
			return applied, err
		}
		applied++
	}
	if err := tx.Commit(); err != nil {
		return applied, fmt.Errorf("plexdb: commit undo: %w", err)
	}
	done = true
	return applied, nil
}

// undoOp reverses one journalled operation.
func undoOp(ctx context.Context, tx *sql.Tx, op map[string]any) error {
	name, _ := op["op"].(string)
	ratingKey := rowInt(op, "rating_key")

	switch name {
	case "insert":
		id := rowInt(op, "id")
		if id == 0 {
			id = rowInt(op, "tag_id")
		}
		if id == 0 {
			return fmt.Errorf("plexdb: undo insert without a row id (rating_key %d)", ratingKey)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM taggings WHERE id = ?`, id); err != nil {
			return fmt.Errorf("plexdb: undo insert %d: %w", id, err)
		}
		return nil

	case "delete":
		row, _ := op["row"].(map[string]any)
		if len(row) == 0 {
			return fmt.Errorf("plexdb: undo delete without a row (rating_key %d)", ratingKey)
		}
		itemID := rowInt(row, "metadata_item_id")
		if itemID == 0 {
			itemID = ratingKey
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO taggings
    (id, metadata_item_id, tag_id, "index", text, time_offset, end_time_offset, thumb_url, created_at, extra_data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rowInt(row, "id"), itemID, rowInt(row, "tag_id"), rowInt(row, "index"),
			rowNullString(row, "text"), rowInt(row, "time_offset"), rowInt(row, "end_time_offset"),
			rowNullString(row, "thumb_url"), rowInt(row, "created_at"), rowNullString(row, "extra_data"),
		); err != nil {
			return fmt.Errorf("plexdb: undo delete %d: %w", rowInt(row, "id"), err)
		}
		return nil

	case "index":
		id := rowInt(op, "id")
		old, ok := firstInt(op, "old_index", "old")
		if id == 0 || !ok {
			return fmt.Errorf("plexdb: undo index without a row id or old index (rating_key %d)", ratingKey)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE taggings SET "index" = ? WHERE id = ?`, old, id); err != nil {
			return fmt.Errorf("plexdb: undo index %d: %w", id, err)
		}
		return nil

	case "tag_insert":
		id := rowInt(op, "tag_id")
		if id == 0 {
			return fmt.Errorf("plexdb: undo tag_insert without a tag id")
		}
		// Restricted to the marker tag type, so a wrong id in a journal can
		// never delete a tag that carries real metadata.
		//
		// Deleting needs the same care as creating: the FTS triggers on tags
		// cannot be prepared outside Plex, so they come off for the duration of
		// the delete and go back exactly as they were.
		return withoutTagTriggers(ctx, tx, func() error {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM tags WHERE id = ? AND tag_type = ?`, id, TagTypeMarker); err != nil {
				return fmt.Errorf("plexdb: undo tag_insert %d: %w", id, err)
			}
			return nil
		})

	case "settings_insert":
		// A settings row this tool made to hang markers off. Deleting it takes
		// the markers with it, which is what the foreign key is for.
		id := rowInt(op, "id")
		if id == 0 {
			return fmt.Errorf("plexdb: undo settings_insert without a row id")
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM metadata_item_settings WHERE id = ?`, id); err != nil {
			return fmt.Errorf("plexdb: undo settings_insert %d: %w", id, err)
		}
		return nil

	case "setting_marker_insert":
		id := rowInt(op, "id")
		if id == 0 {
			return fmt.Errorf("plexdb: undo setting_marker_insert without a row id")
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM metadata_item_setting_markers WHERE id = ?`, id); err != nil {
			return fmt.Errorf("plexdb: undo setting_marker_insert %d: %w", id, err)
		}
		return nil

	case "setting_marker_delete":
		row, _ := op["row"].(map[string]any)
		if row == nil {
			return fmt.Errorf("plexdb: undo setting_marker_delete without the row it removed")
		}
		// Put back every column, including the timestamps: this row may be one
		// Plex wrote itself, and a marker that looks like it was created now is
		// a lie about when it was detected.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO metadata_item_setting_markers
			    (id, marker_type, metadata_item_setting_id, start_time_offset,
			     end_time_offset, title, created_at, updated_at, extra_data)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rowInt(row, "id"), rowInt(row, "marker_type"), rowInt(row, "setting_id"),
			rowInt(row, "start_ms"), rowInt(row, "end_ms"),
			rowNullString(row, "title"), rowInt(row, "created_at"), rowInt(row, "updated_at"),
			rowNullString(row, "extra_data")); err != nil {
			return fmt.Errorf("plexdb: undo setting_marker_delete: %w", err)
		}
		return nil

	case "extra":
		partID := rowInt(op, "part_id")
		if partID == 0 {
			return fmt.Errorf("plexdb: undo extra without a part id (rating_key %d)", ratingKey)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_parts SET extra_data = ? WHERE id = ?`,
			rowNullString(op, "old"), partID); err != nil {
			return fmt.Errorf("plexdb: undo extra_data on part %d: %w", partID, err)
		}
		return nil

	default:
		return fmt.Errorf("plexdb: unknown journal operation %q", name)
	}
}

// firstInt returns the first key present as an integer.
func firstInt(row map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if v, ok := row[key]; ok && v != nil {
			return toInt64(v), true
		}
	}
	return 0, false
}

// rowInt is firstInt for a value that defaults to zero.
func rowInt(row map[string]any, key string) int64 {
	v, _ := firstInt(row, key)
	return v
}

// rowNullString renders a value as a SQL string, or NULL when it is absent or
// null. A journal entry that recorded a null extra_data restores a null.
func rowNullString(row map[string]any, key string) any {
	if row == nil {
		return nil
	}
	switch v := row[key].(type) {
	case nil:
		return nil
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(v)
	}
}

// toInt64 converts the numeric shapes that come out of encoding/json into an
// integer.
func toInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			f, ferr := t.Float64()
			if ferr != nil {
				return 0
			}
			return int64(f)
		}
		return n
	case string:
		var n int64
		if _, err := fmt.Sscan(t, &n); err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}
