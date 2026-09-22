package plexdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// Plex 1.43 does not read markers out of taggings. It has a table of its own,
// metadata_item_setting_markers, and that is what its API serves: a row written
// there comes back from GET /library/metadata/{id}?includeMarkers=1 as a marker
// with a type, and a row written only to taggings does not come back at all.
//
// That was established by experiment rather than from documentation, because
// there is none: a marker was written there by hand, Plex was asked to re-read
// the item, and the answer came back. Everything below is measured the same way,
// and the numbers are not guessable.
//
// Marker types, by setting the column and reading back what Plex called it:
//
//	1 -> intro        2 -> commercial   3 -> bookmark
//	4 -> resume       5 -> credits      0, 6 and up -> no type at all
//
// The model has exactly two marker kinds (recap folds into intro, preview into
// credits), and they map to 1 and 5.
const (
	plexMarkerIntro   int64 = 1
	plexMarkerCredits int64 = 5
)

// markerTable is the table Plex keeps its own markers in, from schema revision
// 202309200911. Older servers do not have it, which is why everything here is
// conditional: the taggings write stays for them.
const markerTable = "metadata_item_setting_markers"

// markerSource marks the rows this tool wrote, so that a marker Plex detected
// itself is never taken for one of ours and overwritten. There is nowhere else
// to keep that distinction: the table has no source column.
const markerSource = "theintrodb"

// markerTypeFor returns the number Plex stores for a marker kind.
func markerTypeFor(text model.MarkerText) (int64, bool) {
	switch text {
	case model.MarkerIntro:
		return plexMarkerIntro, true
	case model.MarkerCredits:
		return plexMarkerCredits, true
	default:
		// Plex has no marker type for anything else, so there is nothing to
		// write. It is not an error: the plan may hold segment types that Plex
		// does not represent as markers.
		return 0, false
	}
}

// hasMarkerTable reports whether this Plex keeps markers in a table of its own.
func hasMarkerTable(ctx context.Context, q querier) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, markerTable).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("plexdb: look for %s: %w", markerTable, err)
	}
	return n > 0, nil
}

// ownerAccountID returns the account a marker is written for.
//
// The marker table hangs off metadata_item_settings, which is per account, so a
// marker has to belong to somebody. Plex itself used the lowest account id when
// it wrote playback state for the library owner, and the settings row it made is
// the one its API served a marker from, so that is the account used here.
func ownerAccountID(ctx context.Context, q querier) (int64, error) {
	var id sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MIN(id) FROM accounts`).Scan(&id); err != nil {
		// A database with no accounts table is not worth failing over: the
		// settings rows that matter are the ones that exist.
		if strings.Contains(err.Error(), "no such table") {
			return 1, nil
		}
		return 0, fmt.Errorf("plexdb: find the owner account: %w", err)
	}
	if !id.Valid || id.Int64 <= 0 {
		return 1, nil
	}
	return id.Int64, nil
}

// settingMarker is one row of the marker table, as it was before a change.
type settingMarker struct {
	ID      int64
	Setting int64
	Kind    int64
	StartMS int64
	EndMS   int64
	Title   string
	Extra   string
	Created int64
	Updated int64
	IsOurs  bool
}

// settingRow carries what a write needs: the settings row to hang markers off,
// and the item's guid.
type settingRow struct {
	ID      int64
	Account int64
	Guid    string
	Created bool
}

// ensureSettingRow finds the settings row for this item and account, making one
// when the item has never been played.
//
// Without it there is nothing for a marker to hang off: the foreign key is not
// optional. Plex creates these rows lazily as people watch things, so on a fresh
// library almost every item needs one, which is why this is not an error path.
func ensureSettingRow(ctx context.Context, tx querier, ratingKey int64, j *Journal) (settingRow, error) {
	var guid string
	if err := tx.QueryRowContext(ctx,
		`SELECT guid FROM metadata_items WHERE id = ?`, ratingKey).Scan(&guid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return settingRow{}, fmt.Errorf("plexdb: no metadata item %d to attach a marker to", ratingKey)
		}
		return settingRow{}, fmt.Errorf("plexdb: read guid for item %d: %w", ratingKey, err)
	}
	if guid == "" {
		return settingRow{}, fmt.Errorf(
			"plexdb: item %d has no guid, so there is no settings row to attach a marker to", ratingKey)
	}

	account, err := ownerAccountID(ctx, tx)
	if err != nil {
		return settingRow{}, err
	}

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM metadata_item_settings WHERE guid = ? AND account_id = ?`,
		guid, account).Scan(&id)
	switch {
	case err == nil:
		return settingRow{ID: id, Account: account, Guid: guid}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return settingRow{}, fmt.Errorf("plexdb: read settings row for %d: %w", ratingKey, err)
	}

	row := settingRow{Account: account, Guid: guid, Created: true}
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM metadata_item_settings`).Scan(&row.ID); err != nil {
		return settingRow{}, fmt.Errorf("plexdb: next settings id: %w", err)
	}
	now := time.Now().UTC().Unix()
	if j != nil {
		if err := j.Record(map[string]any{
			"op":         "settings_insert",
			"rating_key": ratingKey,
			"id":         row.ID,
			"account_id": account,
			"guid":       guid,
		}); err != nil {
			return settingRow{}, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO metadata_item_settings
		    (id, account_id, guid, created_at, updated_at, skip_count, changed_at)
		 VALUES (?, ?, ?, ?, ?, 0, 0)`,
		row.ID, account, guid, now, now); err != nil {
		return settingRow{}, fmt.Errorf("plexdb: create settings row for %d: %w", ratingKey, err)
	}
	return row, nil
}

// settingMarkersOfKind reads the rows of one marker type on a settings row.
func settingMarkersOfKind(ctx context.Context, q querier, settingID, kind int64) ([]settingMarker, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, metadata_item_setting_id, marker_type, start_time_offset,
		        COALESCE(end_time_offset, 0), COALESCE(title, ''),
		        COALESCE(extra_data, ''), COALESCE(created_at, 0), COALESCE(updated_at, 0)
		   FROM `+markerTable+`
		  WHERE metadata_item_setting_id = ? AND marker_type = ?
		  ORDER BY id`, settingID, kind)
	if err != nil {
		return nil, fmt.Errorf("plexdb: read markers of type %d: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var out []settingMarker
	for rows.Next() {
		var m settingMarker
		if err := rows.Scan(&m.ID, &m.Setting, &m.Kind, &m.StartMS, &m.EndMS,
			&m.Title, &m.Extra, &m.Created, &m.Updated); err != nil {
			return nil, fmt.Errorf("plexdb: read markers of type %d: %w", kind, err)
		}
		m.IsOurs = strings.Contains(m.Extra, markerSource)
		out = append(out, m)
	}
	return out, rows.Err()
}

// markerExtra is the stamp written on a marker row so the next run can tell its
// own work from Plex's.
func markerExtra(text model.MarkerText) string {
	return fmt.Sprintf(`{"source":%q}`, markerSource+"-"+string(text))
}

// writeSettingMarkers brings the marker table in line with the plan for the two
// kinds Plex represents as markers.
//
// A row Plex detected itself is left alone unless the plan means to replace it:
// the planner reads the markers it can see in taggings, so its view of what
// exists does not include a marker Plex wrote straight into this table. Writing
// over one of those would throw away a better answer than this tool's own.
func (d *DB) writeSettingMarkers(
	ctx context.Context, tx querier, ratingKey int64,
	desired []model.Marker, replace bool, j *Journal,
) (int, error) {
	exists, err := hasMarkerTable(ctx, tx)
	if err != nil || !exists {
		// Plex older than the table: taggings is all it has, and the caller has
		// already written that.
		return 0, nil
	}

	row, err := ensureSettingRow(ctx, tx, ratingKey, j)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC().Unix()
	written := 0

	// Only the kinds the model can express, and only those the plan is about.
	for _, text := range []model.MarkerText{model.MarkerIntro, model.MarkerCredits} {
		kind, ok := markerTypeFor(text)
		if !ok {
			continue
		}
		want := markersOfText(desired, text)

		have, err := settingMarkersOfKind(ctx, tx, row.ID, kind)
		if err != nil {
			return written, err
		}

		if len(want) == 0 {
			// The plan leaves this kind alone unless it deliberately removed
			// one, in which case the taggings delete already recorded it. A row
			// of ours from an earlier run is the exception: if there is no
			// longer a marker to hold, it goes.
			for _, m := range have {
				if !m.IsOurs && !replace {
					continue
				}
				if err := deleteSettingMarker(ctx, tx, m, ratingKey, j); err != nil {
					return written, err
				}
			}
			continue
		}

		// One marker per kind is what Plex holds, so at most one row of ours can
		// be kept. Two questions decide what happens: does a row already hold the
		// range the plan wants, and is a row in the way one we are allowed to
		// remove?
		holds := false
		blocked := false
		for _, m := range have {
			switch {
			case m.StartMS == want[0].StartMS && m.EndMS == want[0].EndMS:
				// Ours or Plex's: the range asked for is already there.
				holds = true
			case m.IsOurs:
				// Ours, with a range the plan has moved on from. It goes, and
				// the new range is written below: deleting without inserting is
				// how a marker silently disappears.
				if err := deleteSettingMarker(ctx, tx, m, ratingKey, j); err != nil {
					return written, err
				}
			case replace:
				// Plex's own, and the configuration says this tool's answer
				// wins for this kind.
				if err := deleteSettingMarker(ctx, tx, m, ratingKey, j); err != nil {
					return written, err
				}
			default:
				// Plex's own, and it is not to be touched. Ours is not added
				// beside it either: the plan was built without seeing this row,
				// and Plex holds one marker per kind, so adding ours would mean
				// choosing between two answers this tool cannot compare.
				blocked = true
			}
		}
		if holds || blocked {
			continue
		}

		if err := insertSettingMarker(ctx, tx, row.ID, kind, want[0], ratingKey, now, j); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// insertSettingMarker adds one marker row, journal first.
func insertSettingMarker(
	ctx context.Context, tx querier, settingID, kind int64,
	marker model.Marker, ratingKey int64, now int64, j *Journal,
) error {
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM `+markerTable).Scan(&id); err != nil {
		return fmt.Errorf("plexdb: next marker id: %w", err)
	}
	if j != nil {
		if err := j.Record(map[string]any{
			"op":         "setting_marker_insert",
			"rating_key": ratingKey,
			"id":         id,
		}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+markerTable+`
		    (id, marker_type, metadata_item_setting_id, start_time_offset,
		     end_time_offset, title, created_at, updated_at, extra_data)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, kind, settingID, marker.StartMS, marker.EndMS,
		markerTitle(marker.Text), now, now, markerExtra(marker.Text)); err != nil {
		return fmt.Errorf("plexdb: insert marker of type %d for item %d: %w", kind, ratingKey, err)
	}
	return nil
}

// deleteSettingMarker removes one marker row, journal first, with the whole row
// recorded so undo can put it back exactly.
func deleteSettingMarker(ctx context.Context, tx querier, m settingMarker, ratingKey int64, j *Journal) error {
	if j != nil {
		if err := j.Record(map[string]any{
			"op":         "setting_marker_delete",
			"rating_key": ratingKey,
			"row": map[string]any{
				"id":          m.ID,
				"marker_type": m.Kind,
				"start_ms":    m.StartMS,
				"end_ms":      m.EndMS,
				"title":       m.Title,
				"extra_data":  m.Extra,
			},
		}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+markerTable+` WHERE id = ?`, m.ID); err != nil {
		return fmt.Errorf("plexdb: delete marker %d: %w", m.ID, err)
	}
	return nil
}

// markerTitle is what the marker is called in the interface: Plex shows the
// title beside the marker, and an empty one reads as a fault.
func markerTitle(text model.MarkerText) string {
	switch text {
	case model.MarkerIntro:
		return "Intro"
	case model.MarkerCredits:
		return "Credits"
	default:
		return string(text)
	}
}
