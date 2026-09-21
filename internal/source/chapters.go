// Package source holds the alternate marker sources.
//
// TheIntroDB is the primary source and lives in internal/tidb. These fill in
// the segment types it did not answer for a given item, and can never override
// it for a type it did answer.
package source

import (
	"regexp"
	"strings"

	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/model"
)

// chapterPatterns match the chapter names Plex itself extracted from a file.
//
// Only unambiguous names count. "Studio Logo", "Scene 3" or "Part 01" say
// nothing about whether a segment is skippable, and a wrong intro marker is
// worse than no marker.
var chapterPatterns = map[model.SegmentType]*regexp.Regexp{
	model.SegmentIntro:   regexp.MustCompile(`(?i)^(intro|opening|opening credits|opening titles?|opening theme|title sequence|main titles?|theme|theme song|op)$`),
	model.SegmentRecap:   regexp.MustCompile(`(?i)^(recap|previously|previously on|last time|last time on)$`),
	model.SegmentCredits: regexp.MustCompile(`(?i)^(credits|end credits|ending credits|closing credits|end titles?|ending|outro|ed)$`),
	model.SegmentPreview: regexp.MustCompile(`(?i)^(preview|next episode|next time|next time on|coming up|coming up next|next week|next week on)$`),
}

// Plausibility windows. A chapter has to start where that segment plausibly
// lives and be a plausible length, or it is discarded.
const (
	minChapterMS = 5_000
	maxIntroMS   = 150_000
	maxRecapMS   = 300_000
	maxCreditsMS = 1_800_000
	// A recap or intro starting after this fraction of the item is not one.
	earlyFraction = 0.35
	// Credits and previews start after this fraction.
	lateFraction = 0.60
)

// Chapters turns chapter names into segments.
//
// This source is free: Plex already read the chapters out of the file, so there
// is no lookup and no media read, and the timings are exact for the file on
// disk. It is still only a fallback, because chapter names are what the release
// group chose to write: measured against TheIntroDB, credits chapters land
// within about two seconds but intro chapters are looser, around six.
func Chapters(item model.LibraryItem, chapters []model.Chapter, cfg config.Config) (model.SegmentSet, bool) {
	set := model.SegmentSet{Source: model.SourceChapters}
	if len(chapters) < 2 {
		// A single chapter says nothing: it is usually the whole file.
		return set, false
	}
	duration := item.BestDuration()
	if duration == nil || *duration <= 0 {
		return set, false
	}
	total := *duration

	found := map[model.SegmentType]bool{}
	for _, chapter := range chapters {
		name := strings.TrimSpace(chapter.Name)
		name = strings.TrimRight(name, ".…:!")
		if name == "" {
			continue
		}
		var segmentType model.SegmentType
		for _, candidate := range model.SegmentTypes {
			pattern, ok := chapterPatterns[candidate]
			if !ok || !pattern.MatchString(name) {
				continue
			}
			segmentType = candidate
			break
		}
		if segmentType == "" || found[segmentType] {
			continue
		}
		// A film's opening-credit chapter often runs over the first scene, so
		// movies only take credits and previews from chapters.
		if item.IsMovie() && (segmentType == model.SegmentIntro || segmentType == model.SegmentRecap) {
			continue
		}
		if !plausible(segmentType, chapter, total) {
			continue
		}
		start, end := chapter.StartMS, chapter.EndMS
		found[segmentType] = true
		set.Segments = append(set.Segments, model.Segment{
			Type:    segmentType,
			StartMS: &start,
			EndMS:   &end,
			Source:  model.SourceChapters,
		})
	}
	if len(set.Segments) == 0 {
		return set, false
	}
	return set, true
}

// plausible applies the length and position window for one segment type.
func plausible(t model.SegmentType, chapter model.Chapter, totalMS int64) bool {
	length := chapter.LengthMS()
	if length < minChapterMS {
		return false
	}
	early := chapter.StartMS <= int64(float64(totalMS)*earlyFraction)
	late := chapter.StartMS >= int64(float64(totalMS)*lateFraction)
	switch t {
	case model.SegmentIntro:
		return early && length <= maxIntroMS
	case model.SegmentRecap:
		return early && length <= maxRecapMS
	case model.SegmentCredits, model.SegmentPreview:
		return late && length <= maxCreditsMS
	}
	return false
}
