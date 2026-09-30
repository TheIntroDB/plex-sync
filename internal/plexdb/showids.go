package plexdb

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// showIDChunk is how many episode keys go into one statement.
//
// SQLite refuses a statement carrying more bound parameters than
// SQLITE_MAX_VARIABLE_NUMBER, which has been 32,766 since 3.32, and a TV
// library can hold more episodes than that. The whole query then fails, the
// caller treats that as non-fatal, and every episode silently keeps its own id
// instead of the show's -- a key TheIntroDB can never match, so the run spends
// its allowance on lookups that cannot succeed. Chunking keeps the
// one-query-per-library shape and never reaches the limit.
const showIDChunk = 5000

// ShowProviderIDs returns the provider id tags of the show each of these
// episodes belongs to.
//
// An episode carries ids of its own, and they are not the series id: Plex stores
// the series id on the show row, an episode id in the episode's own row, and a
// season id on the season row, all three in the same shape. TheIntroDB is asked
// for an episode by series id plus season and episode, so a lookup built from the
// episode's own id asks about something that does not exist. Measured on a real
// library: tmdb_id=125526 with season 1 episode 4 answers "media not found",
// while the show's own tmdb_id=1911, tvdb_id=75682 and imdb_id=tt0460627 all
// answer with the episode's timings.
//
// The walk is episode -> season -> show because that is the shape Plex stores:
// the episode's parent is its season, and the season's parent is the show. It is
// done in a handful of queries for a whole library rather than a request per
// episode.
func (d *DB) ShowProviderIDs(ctx context.Context, episodeKeys []int64) (map[int64][]string, error) {
	if len(episodeKeys) == 0 {
		return nil, nil
	}
	keys := uniqueKeys(episodeKeys)

	out := map[int64][]string{}
	for start := 0; start < len(keys); start += showIDChunk {
		end := start + showIDChunk
		if end > len(keys) {
			end = len(keys)
		}
		if err := d.showProviderIDsChunk(ctx, keys[start:end], len(keys), out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// uniqueKeys drops repeats, so an episode named twice cannot be reported twice
// and a key cannot be queried in two different chunks.
func uniqueKeys(keys []int64) []int64 {
	seen := make(map[int64]bool, len(keys))
	out := make([]int64, 0, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// showProviderIDsChunk runs one chunk of the walk and adds its rows to out.
// total is the size of the whole request, which is what the error reports.
func (d *DB) showProviderIDsChunk(ctx context.Context, keys []int64, total int, out map[int64][]string) error {
	placeholders := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		placeholders = append(placeholders, "?")
		args = append(args, key)
	}

	rows, err := d.db.QueryContext(ctx,
		`SELECT episode.id, tag.tag
		   FROM metadata_items episode
		   JOIN metadata_items season ON season.id = episode.parent_id
		   JOIN metadata_items show ON show.id = season.parent_id
		   JOIN taggings tagging ON tagging.metadata_item_id = show.id
		   JOIN tags tag ON tag.id = tagging.tag_id
		  WHERE episode.id IN (`+strings.Join(placeholders, ", ")+`)
		    AND tag.tag_type = ?
		  ORDER BY episode.id`,
		append(args, TagTypeProviderID)...)
	if err != nil {
		return fmt.Errorf("plexdb: read the shows for %d episode(s): %w", total, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key int64
		var tag string
		if err := rows.Scan(&key, &tag); err != nil {
			return fmt.Errorf("plexdb: read the shows for %d episode(s): %w", total, err)
		}
		out[key] = append(out[key], tag)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("plexdb: read the shows for %d episode(s): %w", total, err)
	}
	return nil
}

// IsEpisodeKey reports whether a rating key looks like an episode, by asking the
// database rather than by trusting the kind the API reported.
func (d *DB) IsEpisodeKey(ctx context.Context, key int64) (bool, error) {
	var kind int
	err := d.db.QueryRowContext(ctx,
		`SELECT metadata_type FROM metadata_items WHERE id = ?`, key).Scan(&kind)
	if err != nil {
		return false, fmt.Errorf("plexdb: read metadata type of %s: %w", strconv.FormatInt(key, 10), err)
	}
	return kind == metadataTypeEpisode, nil
}

// metadataTypeEpisode is Plex's metadata_type for an episode.
const metadataTypeEpisode = 4
