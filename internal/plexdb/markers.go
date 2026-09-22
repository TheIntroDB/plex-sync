package plexdb

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// DefaultChunkSize is how many items one write transaction covers. Markers are
// cheap rows but Plex holds a write lock while it has one, so the transaction is
// kept short enough that a live server never notices it.
const DefaultChunkSize = 25

// ApplyOption adjusts one apply run.
type ApplyOption func(*applyConfig)

// applyConfig is what an option can change.
type applyConfig struct {
	// replace says the plan means to replace markers Plex detected itself,
	// which is apply.policy being "prefer-theintrodb". It only matters to the
	// marker table Plex 1.43 reads: a marker Plex wrote there is a better answer
	// than guessing, so it is kept unless the plan asked for it to go.
	replace bool
}

// WithReplacePolicy sets whether Plex's own markers may be overwritten.
func WithReplacePolicy(replace bool) ApplyOption {
	return func(c *applyConfig) { c.replace = replace }
}

// WriteStats summarises one ApplyPlans run.
type WriteStats struct {
	// Items is how many plans were looked at.
	Items int
	// Written is how many items were actually changed.
	Written int
	// Added and Removed count marker rows.
	Added   int
	Removed int
	// Indexed counts rows whose index was renumbered.
	Indexed int
	// PartsUpdated counts media_parts rows whose extra_data was rewritten.
	PartsUpdated int
	// Skipped counts items left alone because the database no longer matched
	// the plan; SkipReasons explains each one.
	Skipped     int
	SkipReasons []string
}

// ApplyPlans writes a plan's changes to the database.
//
// Items are applied in transactions of chunk plans each, each opened with BEGIN
// IMMEDIATE and rolled back whole on error. Inside a transaction every item is
// re-read and compared with the rows the plan expects to find; an item that
// changed since it was planned is skipped and counted rather than overwritten,
// so a plan computed a while ago can never clobber a concurrent change.
//
// tagID is the marker tag to hang new rows off, used only when a library has no
// per-text tag. Every operation is written to j before it happens; j may be nil
// for a dry write with no undo log, which is rarely what you want.
func (d *DB) ApplyPlans(
	plans []model.ItemPlan, tagID int64, chunk int, j *Journal, opts ...ApplyOption,
) (WriteStats, error) {
	var cfg applyConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	var stats WriteStats
	if len(plans) == 0 {
		return stats, nil
	}
	if chunk <= 0 {
		chunk = DefaultChunkSize
	}

	ctx := context.Background()
	for start := 0; start < len(plans); start += chunk {
		end := start + chunk
		if end > len(plans) {
			end = len(plans)
		}
		if err := d.applyChunk(ctx, plans[start:end], tagID, j, &stats, cfg.replace); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// applyChunk applies one transaction's worth of plans. A failure anywhere rolls
// the whole chunk back, including the operations already journalled for it: an
// undo entry naming a row that was never written is harmless, a half-written
// chunk is not. Statistics are only added to the caller's totals once the chunk
// has committed, so a rolled-back chunk is never reported as written.
func (d *DB) applyChunk(ctx context.Context, batch []model.ItemPlan, tagID int64, j *Journal, stats *WriteStats, replace bool) error {
	tx, err := d.db.BeginTx(ctx, nil) // _txlock=immediate, so BEGIN IMMEDIATE
	if err != nil {
		return fmt.Errorf("plexdb: begin: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()

	var chunk WriteStats
	for i := range batch {
		if err := d.applyOne(ctx, tx, batch[i], tagID, j, &chunk, replace); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("plexdb: commit: %w", err)
	}
	done = true
	mergeStats(stats, chunk)
	return nil
}

// mergeStats folds one committed chunk's counts into the run totals.
func mergeStats(dst *WriteStats, src WriteStats) {
	dst.Items += src.Items
	dst.Written += src.Written
	dst.Added += src.Added
	dst.Removed += src.Removed
	dst.Indexed += src.Indexed
	dst.PartsUpdated += src.PartsUpdated
	dst.Skipped += src.Skipped
	dst.SkipReasons = append(dst.SkipReasons, src.SkipReasons...)
}

// applyOne applies a single item plan inside an open transaction.
func (d *DB) applyOne(
	ctx context.Context, tx querier, plan model.ItemPlan, tagID int64,
	j *Journal, stats *WriteStats, replace bool,
) error {
	stats.Items++

	ratingKey := int64(plan.Item.RatingKey)
	if ratingKey <= 0 {
		return fmt.Errorf("plexdb: plan for %q has no rating key", plan.Item.Label())
	}

	if !plan.Changes() {
		// Nothing to do in taggings, and something may still be owed to Plex's
		// own marker table: a library written by an earlier version of this tool
		// has its markers in taggings only, which Plex 1.43 ignores completely.
		// Syncing here is what turns those rows into working skip buttons
		// without re-writing anything a second time, which matters because the
		// alternative is telling every existing install to undo and start over.
		if _, err := d.writeSettingMarkers(ctx, tx, ratingKey, desiredMarkers(plan), replace, j); err != nil {
			return err
		}
		return nil
	}

	live, err := readMarkers(ctx, tx, ratingKey, 0)
	if err != nil {
		return err
	}
	if reason, ok := reconcile(live, plan); !ok {
		stats.Skipped++
		stats.SkipReasons = append(stats.SkipReasons, fmt.Sprintf("%d: %s", ratingKey, reason))
		return nil
	}

	partRows, err := parts(ctx, tx, ratingKey)
	if err != nil {
		return err
	}

	// Deletions first, so a replaced marker frees its index before the new row
	// takes one.
	affected := map[string]bool{}
	for _, id := range plan.Remove {
		row, err := readTaggingRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if row == nil {
			continue
		}
		if j != nil {
			if err := j.Record(map[string]any{
				"op":         "delete",
				"rating_key": ratingKey,
				"row":        row,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM taggings WHERE id = ?`, id); err != nil {
			return fmt.Errorf("plexdb: delete tagging %d: %w", id, err)
		}
		if text, ok := row["text"].(string); ok && text != "" {
			affected[text] = true
		}
		stats.Removed++
	}

	// Insertions.
	now := time.Now().UTC().Unix()
	for _, m := range plan.Add {
		if m.EndMS <= m.StartMS {
			return fmt.Errorf("plexdb: item %d: marker %s:%d:%d is empty", ratingKey, m.Text, m.StartMS, m.EndMS)
		}
		tag, err := markerTagIDFor(ctx, tx, string(m.Text), tagID)
		if err != nil {
			return err
		}
		newID, err := nextTaggingID(ctx, tx)
		if err != nil {
			return err
		}
		if j != nil {
			if err := j.Record(map[string]any{
				"op":         "insert",
				"rating_key": ratingKey,
				"id":         newID,
				"tag_id":     tag,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO taggings
    (id, metadata_item_id, tag_id, "index", text, time_offset, end_time_offset, thumb_url, created_at, extra_data)
VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
			newID, ratingKey, tag, 0, string(m.Text), m.StartMS, m.EndMS, now, MarkerExtraData(m)); err != nil {
			return fmt.Errorf("plexdb: insert marker for %d: %w", ratingKey, err)
		}
		affected[string(m.Text)] = true
		stats.Added++
	}

	// One marker can require renumbering the others: the index column holds the
	// row position ordered by start time across every marker type, so it is
	// rebuilt for the whole item whenever a row is added or removed.
	if len(plan.Add) > 0 || len(plan.Remove) > 0 {
		n, err := renumberIndex(ctx, tx, ratingKey, j)
		if err != nil {
			return err
		}
		stats.Indexed += n
	}

	// media_parts.extra_data, which is what Plex reads to decide whether it has
	// already analysed this file.
	if len(affected) > 0 {
		types := make([]string, 0, len(affected))
		for text := range affected {
			types = append(types, text)
		}
		sort.Strings(types)

		intros := markersOfText(desiredMarkers(plan), model.MarkerIntro)
		credits := markersOfText(desiredMarkers(plan), model.MarkerCredits)

		for _, part := range partRows {
			rewritten, changed := RewriteExtra(part.ExtraData, types, intros, credits)
			if !changed {
				continue
			}
			if j != nil {
				var prev any
				if part.ExtraData != "" {
					prev = part.ExtraData
				}
				if err := j.Record(map[string]any{
					"op":         "extra",
					"rating_key": ratingKey,
					"part_id":    part.ID,
					"old":        prev,
				}); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE media_parts SET extra_data = ? WHERE id = ?`,
				rewritten, part.ID); err != nil {
				return fmt.Errorf("plexdb: write part %d extra_data: %w", part.ID, err)
			}
			stats.PartsUpdated++
		}
	}

	if _, err := d.writeSettingMarkers(ctx, tx, ratingKey, desiredMarkers(plan),
		replace, j); err != nil {
		return err
	}

	stats.Written++
	return nil
}

// reconcile checks that the live marker rows are exactly the ones the plan was
// built from: the same count, the same row ids, the same ranges and the same
// index order. It reports why not when they are not.
func reconcile(live []model.ExistingMarker, plan model.ItemPlan) (string, bool) {
	want := len(plan.Kept) + len(plan.Remove)
	if len(live) != want {
		return fmt.Sprintf("marker count changed: live %d, planned %d", len(live), want), false
	}

	liveByID := make(map[int64]model.ExistingMarker, len(live))
	for _, m := range live {
		liveByID[m.TagID] = m
	}
	kept := make(map[int64]bool, len(plan.Kept))
	for _, k := range plan.Kept {
		kept[k.TagID] = true
		got, ok := liveByID[k.TagID]
		if !ok {
			return fmt.Sprintf("planned marker %d is gone", k.TagID), false
		}
		if got.Text != k.Text || got.StartMS != k.StartMS || got.EndMS != k.EndMS {
			return fmt.Sprintf("marker %d changed: live %s:%d:%d, planned %s:%d:%d",
				k.TagID, got.Text, got.StartMS, got.EndMS, k.Text, k.StartMS, k.EndMS), false
		}
		if got.Index != k.Index {
			return fmt.Sprintf("marker %d index changed: live %d, planned %d", k.TagID, got.Index, k.Index), false
		}
	}
	for _, id := range plan.Remove {
		if _, ok := liveByID[id]; !ok {
			return fmt.Sprintf("marker %d to remove is gone", id), false
		}
		if kept[id] {
			return fmt.Sprintf("marker %d is both kept and removed", id), false
		}
	}
	for _, m := range live {
		if _, ok := kept[m.TagID]; ok {
			continue
		}
		found := false
		for _, id := range plan.Remove {
			if id == m.TagID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("unexpected marker %d appeared", m.TagID), false
		}
	}
	return "", true
}

// renumberIndex rebuilds the index column for every marker row of an item,
// numbering by start time across all marker types, and journals each row whose
// index moves. It returns how many rows changed.
//
// The sequence is 0-based. That is not a guess: a verified third-party tool
// built its own zero-based position list from the same ORDER BY and found its
// values byte-identical to what Plex had stored, across a whole library.
func renumberIndex(ctx context.Context, q querier, ratingKey int64, j *Journal) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT g.id, COALESCE(g."index", 0)
FROM taggings g
JOIN tags t ON t.id = g.tag_id
WHERE g.metadata_item_id = ? AND t.tag_type = ?
ORDER BY g.time_offset ASC, g.id ASC`, ratingKey, TagTypeMarker)
	if err != nil {
		return 0, fmt.Errorf("plexdb: list markers for reindex of %d: %w", ratingKey, err)
	}
	type row struct {
		id    int64
		index int
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.index); err != nil {
			rows.Close()
			return 0, fmt.Errorf("plexdb: scan marker for reindex of %d: %w", ratingKey, err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("plexdb: list markers for reindex of %d: %w", ratingKey, err)
	}
	rows.Close()

	changed := 0
	for i, r := range list {
		if r.index == i {
			continue
		}
		if j != nil {
			if err := j.Record(map[string]any{
				"op":         "index",
				"rating_key": ratingKey,
				"id":         r.id,
				"old_index":  r.index,
			}); err != nil {
				return changed, err
			}
		}
		if _, err := q.ExecContext(ctx, `UPDATE taggings SET "index" = ? WHERE id = ?`, i, r.id); err != nil {
			return changed, fmt.Errorf("plexdb: reindex tagging %d: %w", r.id, err)
		}
		changed++
	}
	return changed, nil
}

// desiredMarkers returns the marker set the item should end up with. A plan
// normally carries it in Desired; when it does not, the kept rows plus the
// additions are used so the extra_data rewrite still describes the truth.
func desiredMarkers(plan model.ItemPlan) []model.Marker {
	if len(plan.Desired) > 0 {
		out := make([]model.Marker, len(plan.Desired))
		copy(out, plan.Desired)
		return out
	}
	out := make([]model.Marker, 0, len(plan.Kept)+len(plan.Add))
	for _, k := range plan.Kept {
		out = append(out, model.Marker{
			Text:    model.MarkerText(k.Text),
			StartMS: k.StartMS,
			EndMS:   k.EndMS,
		})
	}
	out = append(out, plan.Add...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartMS < out[j].StartMS })
	return out
}

// markersOfText filters a marker set down to one marker text.
func markersOfText(markers []model.Marker, text model.MarkerText) []model.Marker {
	var out []model.Marker
	for _, m := range markers {
		if m.Text == text {
			out = append(out, m)
		}
	}
	return out
}
