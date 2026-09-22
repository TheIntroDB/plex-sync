package plexdb

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

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
// done in one query for a whole library rather than a request per episode.
func (d *DB) ShowProviderIDs(ctx context.Context, episodeKeys []int64) (map[int64][]string, error) {
	if len(episodeKeys) == 0 {
		return nil, nil
	}

	placeholders := make([]string, 0, len(episodeKeys))
	args := make([]any, 0, len(episodeKeys))
	for _, key := range episodeKeys {
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
		return nil, fmt.Errorf("plexdb: read the shows for %d episode(s): %w", len(episodeKeys), err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int64][]string{}
	for rows.Next() {
		var key int64
		var tag string
		if err := rows.Scan(&key, &tag); err != nil {
			return nil, fmt.Errorf("plexdb: read the shows for %d episode(s): %w", len(episodeKeys), err)
		}
		out[key] = append(out[key], tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plexdb: read the shows for %d episode(s): %w", len(episodeKeys), err)
	}
	return out, nil
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
