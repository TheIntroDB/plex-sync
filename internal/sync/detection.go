package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TheIntroDB/plex-integration/internal/model"
	"github.com/TheIntroDB/plex-integration/internal/plexdb"
	"github.com/TheIntroDB/plex-integration/internal/source"
)

// detect runs local fingerprint detection for episodes nothing else covered.
//
// It is the last source consulted and the most expensive, so it is asked about
// as little as possible: an episode is only examined once TheIntroDB and the
// chapter names have both come up empty for its intro, and it is only accepted
// when the match is unambiguous against a sibling whose intro is already known.
//
// Detection reads the media files, which is why it is off by default.
func (r *Runner) detect(
	ctx context.Context,
	items []model.LibraryItem,
	sets map[int]map[model.SourceName]model.SegmentSet,
	db *plexdb.DB,
	opts Options,
) (int, error) {
	cfg := r.app.Cfg
	if db == nil {
		return 0, nil
	}

	fingerprinter := source.NewFingerprinter(cfg.Sources.FFmpegPath, cfg.Sources.FpcalcPath, nil)
	if err := fingerprinter.Available(); err != nil {
		return 0, fmt.Errorf(
			"local detection is enabled but its tools are missing; install ffmpeg and "+
				"chromaprint (fpcalc), or set sources.detection = false: %w", err)
	}

	store, err := source.NewStore(cfg.FingerprintDir())
	if err != nil {
		return 0, err
	}
	tmpDir := cfg.TempDir()
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return 0, err
	}

	detector := &source.Detector{
		Fingerprinter: fingerprinter,
		Store:         store,
		WindowSeconds: cfg.Sources.DetectionSampleS,
		ToleranceS:    cfg.Sources.DetectionToleranceS,
	}

	// A season is the unit that matters: an intro is only shared among episodes
	// of the same season of the same show.
	type seasonKey struct {
		show   int
		season int
	}
	seasons := map[seasonKey][]model.LibraryItem{}
	for _, item := range items {
		if item.Kind != model.KindEpisode || item.Season == nil {
			continue
		}
		key := seasonKey{show: item.ShowRatingKey, season: *item.Season}
		seasons[key] = append(seasons[key], item)
	}

	detected := 0
	for key, siblings := range seasons {
		if err := ctx.Err(); err != nil {
			return detected, err
		}
		reference, ok := r.referenceFor(siblings, sets, db)
		if !ok {
			// Nothing in this season has a known intro, so there is nothing to
			// match against and no amount of reading would help.
			continue
		}
		for _, item := range siblings {
			if covered(sets[item.RatingKey]) {
				continue
			}
			path, size, err := r.mediaFile(db, item.RatingKey)
			if err != nil {
				r.app.Log.Debug("detection: no readable file", "item", item.Label(), "error", err)
				continue
			}
			result, err := detector.DetectIntro(ctx, path, size, reference, tmpDir)
			if err != nil {
				r.app.Log.Debug("detection failed", "item", item.Label(), "error", err)
				continue
			}
			if result == nil {
				// A miss stays a miss. No marker is invented from a weak match.
				continue
			}
			set := result.SegmentSet(item)
			if len(set.Segments) == 0 {
				continue
			}
			if sets[item.RatingKey] == nil {
				sets[item.RatingKey] = map[model.SourceName]model.SegmentSet{}
			}
			sets[item.RatingKey][model.SourceDetection] = set
			detected++
			r.app.Log.Info("detected an intro locally",
				"item", item.Label(),
				"season", key.season,
				"start", model.FormatMS(result.StartMS),
				"distance", result.Match.Distance,
			)
		}
	}
	return detected, nil
}

// referenceFor picks the sibling whose intro is already known.
//
// A known intro is one TheIntroDB answered for, or one we wrote ourselves from
// it. A detection of our own is deliberately not used as a reference for
// another episode: an unverified timing must not become the basis for more.
func (r *Runner) referenceFor(
	siblings []model.LibraryItem, sets map[int]map[model.SourceName]model.SegmentSet, db *plexdb.DB,
) (source.Reference, bool) {
	for _, item := range siblings {
		itemSets := sets[item.RatingKey]
		if len(itemSets) == 0 {
			continue
		}
		for _, name := range []model.SourceName{model.SourceTheIntroDB, model.SourceChapters} {
			set, ok := itemSets[name]
			if !ok {
				continue
			}
			intros := set.Of(model.SegmentIntro)
			if len(intros) == 0 {
				continue
			}
			segment := intros[0]
			if segment.StartMS == nil || segment.EndMS == nil {
				continue
			}
			path, size, err := r.mediaFile(db, item.RatingKey)
			if err != nil {
				continue
			}
			return source.Reference{
				Path:    path,
				Size:    size,
				StartMS: *segment.StartMS,
				EndMS:   *segment.EndMS,
				Source:  string(name),
			}, true
		}
	}
	return source.Reference{}, false
}

// mediaFile returns the first part of an item, which is the file to read.
func (r *Runner) mediaFile(db *plexdb.DB, ratingKey int) (string, int64, error) {
	parts, err := db.Parts(int64(ratingKey))
	if err != nil {
		return "", 0, err
	}
	for _, part := range parts {
		if part.File == "" {
			continue
		}
		size := part.Size
		if size == 0 {
			// A zero size would make the fingerprint cache key ambiguous, so
			// fall back to the file's real size rather than reusing a stale
			// entry for a different file.
			if info, err := os.Stat(part.File); err == nil {
				size = info.Size()
			} else {
				size = -1
			}
		}
		return part.File, size, nil
	}
	return "", 0, fmt.Errorf("no media part for rating key %d", ratingKey)
}

// covered reports whether an item already has data from a source that can
// answer for its intro.
func covered(sets map[model.SourceName]model.SegmentSet) bool {
	for _, name := range []model.SourceName{model.SourceTheIntroDB, model.SourceChapters} {
		if set, ok := sets[name]; ok && set.Has(model.SegmentIntro) {
			return true
		}
	}
	return false
}

// relativePath is a small helper for log output, so a full library path does
// not have to be printed to say which file was read.
func relativePath(path string) string {
	if base := filepath.Base(path); base != "" {
		return base
	}
	return path
}
