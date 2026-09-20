// Package model holds the shared vocabulary of the tool: library items,
// segments as sources report them, and markers as Plex stores them.
//
// Nothing in this package performs I/O. Every other package converts its
// inputs into these types, which keeps the planner pure and the tests cheap.
package model

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is the kind of library item a lookup is for.
type Kind string

// Item kinds. Plex stores these as metadata_type 1 and 4.
const (
	KindMovie   Kind = "movie"
	KindEpisode Kind = "episode"
)

// SegmentType is a segment as TheIntroDB names it.
type SegmentType string

// The segment types TheIntroDB can return.
const (
	SegmentIntro   SegmentType = "intro"
	SegmentRecap   SegmentType = "recap"
	SegmentCredits SegmentType = "credits"
	SegmentPreview SegmentType = "preview"
)

// SegmentTypes, in the order a plan reports them.
var SegmentTypes = []SegmentType{SegmentIntro, SegmentRecap, SegmentCredits, SegmentPreview}

// MarkerText is what Plex understands. Recap folds into intro, preview into
// credits: Plex has exactly two marker kinds.
type MarkerText string

// Plex marker kinds.
const (
	MarkerIntro   MarkerText = "intro"
	MarkerCredits MarkerText = "credits"
)

// SourceName identifies where a marker's timing came from.
type SourceName string

// Marker sources, strongest first. TheIntroDB always wins a segment type it
// answered; the others only fill gaps.
const (
	SourceTheIntroDB SourceName = "theintrodb"
	SourceChapters   SourceName = "chapters"
	SourceDetection  SourceName = "detection"
)

// SourceNames, strongest first.
var SourceNames = []SourceName{SourceTheIntroDB, SourceChapters, SourceDetection}

// Origin is the provenance reported for a marker already in Plex.
type Origin string

// Marker origins shown by the status and plan output.
const (
	OriginPlex       Origin = "plex"       // Plex detected it itself
	OriginTheIntroDB Origin = "theintrodb" // we wrote it from the API
	OriginChapters   Origin = "chapters"   // we wrote it from chapter names
	OriginDetection  Origin = "detection"  // we wrote it from local detection
	OriginMixed      Origin = "mixed"      // merged from more than one source
)

// ExternalIDs are the provider ids Plex matched for an item. Any of them may be
// missing; lookups prefer TMDb, which TheIntroDB keys its cut-aware data on.
type ExternalIDs struct {
	TMDB *int    `json:"tmdb,omitempty"`
	IMDb *string `json:"imdb,omitempty"`
	TVDB *int    `json:"tvdb,omitempty"`
}

// Any reports whether at least one id is present.
func (e ExternalIDs) Any() bool { return e.TMDB != nil || e.IMDb != nil || e.TVDB != nil }

// LookupKey returns the cache key for this item, and false when no id is known.
//
// The key encodes provider, id and episode coordinates, so a cached miss for
// one episode can never shadow another.
func (e ExternalIDs) LookupKey(season, episode *int) (string, bool) {
	var provider, value string
	switch {
	case e.TMDB != nil:
		provider, value = "tmdb", fmt.Sprint(*e.TMDB)
	case e.IMDb != nil && *e.IMDb != "":
		provider, value = "imdb", *e.IMDb
	case e.TVDB != nil:
		provider, value = "tvdb", fmt.Sprint(*e.TVDB)
	default:
		return "", false
	}
	if season == nil || episode == nil {
		return provider + ":" + value + ":movie", true
	}
	return fmt.Sprintf("%s:%s:%d:%d", provider, value, *season, *episode), true
}

// LibraryItem is one Plex movie or episode, reduced to what a lookup needs.
type LibraryItem struct {
	RatingKey     int         `json:"rating_key"`
	Kind          Kind        `json:"kind"`
	Title         string      `json:"title"`
	ShowTitle     string      `json:"show_title,omitempty"`
	ShowRatingKey int         `json:"show_rating_key,omitempty"`
	Season        *int        `json:"season,omitempty"`
	Episode       *int        `json:"episode,omitempty"`
	IDs           ExternalIDs `json:"ids"`

	// DurationMS is the runtime Plex reports for the item.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	// FileDurationMS is the real file length from media_items.duration. Half a
	// large library differs from DurationMS by more than 30 seconds, and a
	// credits marker placed against the wrong length lands in the wrong place.
	FileDurationMS *int64 `json:"file_duration_ms,omitempty"`
	// PartCount above one means the item has several files: ambiguous, skipped.
	PartCount int `json:"part_count"`
	// FPS feeds the PAL speed-up guard.
	FPS *float64 `json:"fps,omitempty"`

	AddedAt      int64 `json:"added_at,omitempty"`
	LastViewedAt int64 `json:"last_viewed_at,omitempty"`
}

// IsMovie reports whether the item is a movie.
func (i LibraryItem) IsMovie() bool { return i.Kind == KindMovie }

// LookupKey returns the TheIntroDB cache key for this item.
func (i LibraryItem) LookupKey() (string, bool) { return i.IDs.LookupKey(i.Season, i.Episode) }

// BestDuration returns the file length when known, else the reported runtime.
func (i LibraryItem) BestDuration() *int64 {
	if i.FileDurationMS != nil && *i.FileDurationMS > 0 {
		return i.FileDurationMS
	}
	return i.DurationMS
}

// Label is a human-readable name, used by filters and log output.
func (i LibraryItem) Label() string {
	if i.Kind == KindEpisode && i.ShowTitle != "" {
		return fmt.Sprintf("%s S%02dE%02d %s", i.ShowTitle, deref(i.Season), deref(i.Episode), i.Title)
	}
	return i.Title
}

func deref(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// Segment is one range as a source reported it.
//
// Either bound may be nil: TheIntroDB uses a null start for "begins at 0:00" and
// a null end for "runs to the end of the media". Resolve turns that into a
// concrete range against the file length.
type Segment struct {
	Type       SegmentType `json:"type"`
	StartMS    *int64      `json:"start_ms,omitempty"`
	EndMS      *int64      `json:"end_ms,omitempty"`
	Source     SourceName  `json:"source"`
	Confidence *float64    `json:"confidence,omitempty"`
}

// Resolve returns the concrete range for this segment, and false when it cannot
// be placed on the file Plex holds.
func (s Segment) Resolve(durationMS *int64, openStart, openEnd bool) (start, end int64, ok bool) {
	if s.StartMS != nil {
		start = *s.StartMS
	} else if openStart {
		start = 0
	} else {
		return 0, 0, false
	}

	switch {
	case s.EndMS != nil:
		end = *s.EndMS
	case openEnd && durationMS != nil && *durationMS > 0:
		end = *durationMS
	default:
		return 0, 0, false
	}

	if durationMS != nil && *durationMS > 0 {
		if start >= *durationMS {
			// The segment starts past the end of our file, so this is a
			// different cut of the episode and the timing cannot be trusted.
			return 0, 0, false
		}
		if end > *durationMS {
			end = *durationMS
		}
	}
	if end <= start {
		return 0, 0, false
	}
	return start, end, true
}

// Chapter is a chapter Plex extracted from the file.
type Chapter struct {
	Name    string `json:"name"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// LengthMS is the chapter's duration.
func (c Chapter) LengthMS() int64 { return c.EndMS - c.StartMS }

// SegmentSet is everything one source knows about one item.
type SegmentSet struct {
	Source   SourceName `json:"source"`
	Segments []Segment  `json:"segments"`
}

// Of returns the segments of one type.
func (s SegmentSet) Of(t SegmentType) []Segment {
	var out []Segment
	for _, seg := range s.Segments {
		if seg.Type == t {
			out = append(out, seg)
		}
	}
	return out
}

// Has reports whether the set covers a segment type.
func (s SegmentSet) Has(t SegmentType) bool { return len(s.Of(t)) > 0 }

// Types lists the covered segment types, in canonical order.
func (s SegmentSet) Types() []SegmentType {
	var out []SegmentType
	for _, t := range SegmentTypes {
		if s.Has(t) {
			out = append(out, t)
		}
	}
	return out
}

// Marker is a marker as it will exist in Plex.
type Marker struct {
	Text    MarkerText `json:"text"`
	StartMS int64      `json:"start_ms"`
	EndMS   int64      `json:"end_ms"`
	// Final marks the credits marker Plex treats as the end of the item, which
	// is what raises the Up Next prompt.
	Final  bool   `json:"final,omitempty"`
	Source string `json:"source"`
}

// Key identifies a marker by its text and range.
func (m Marker) Key() string {
	return fmt.Sprintf("%s:%d:%d", m.Text, m.StartMS, m.EndMS)
}

// Origin reports which sources produced this marker; mixed when several merged.
func (m Marker) Origin() Origin {
	parts := strings.Split(m.Source, "+")
	var found []SourceName
	for _, want := range SourceNames {
		for _, got := range parts {
			if string(want) == got {
				found = append(found, want)
			}
		}
	}
	switch len(found) {
	case 0:
		return OriginTheIntroDB
	case 1:
		return Origin(found[0])
	default:
		return OriginMixed
	}
}

// ExistingMarker is a marker row read back out of the Plex database.
type ExistingMarker struct {
	TagID   int64  `json:"tag_id"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Index   int    `json:"index"`
	// Origin is "ours" when the ledger says we wrote this row, else "plex".
	Origin    string `json:"origin"`
	ExtraData string `json:"extra_data,omitempty"`
	// Ours reports whether the ledger recorded this exact marker.
	Ours bool `json:"ours,omitempty"`
}

// Key identifies an existing marker by its text and range.
func (m ExistingMarker) Key() string {
	return fmt.Sprintf("%s:%d:%d", m.Text, m.StartMS, m.EndMS)
}

// ItemPlan is the change set for one library item.
type ItemPlan struct {
	Item LibraryItem `json:"item"`
	// Desired is what the item's markers should look like afterwards, sorted by
	// start time across both kinds, ready to be indexed.
	Desired []Marker `json:"desired"`
	// Add are the markers to insert.
	Add []Marker `json:"add,omitempty"`
	// Remove are taggings ids to delete.
	Remove []int64 `json:"remove,omitempty"`
	// Kept are existing rows left untouched.
	Kept []ExistingMarker `json:"kept,omitempty"`
	// Sources records which source supplied each segment type.
	Sources map[string]string `json:"sources,omitempty"`
	// Reason is one of add, refresh, reapply, noop or skip:<why>.
	Reason string `json:"reason"`
}

// Changes reports whether this item needs a write.
func (p ItemPlan) Changes() bool { return len(p.Add) > 0 || len(p.Remove) > 0 }

// Skipped reports whether the item was left alone deliberately.
func (p ItemPlan) Skipped() bool { return strings.HasPrefix(p.Reason, "skip:") }

// MarkerCounts counts the desired markers by text.
func (p ItemPlan) MarkerCounts() (intro, credits int) {
	for _, m := range p.Desired {
		if m.Text == MarkerIntro {
			intro++
		} else {
			credits++
		}
	}
	return intro, credits
}

// Plan is a whole-library change set.
type Plan struct {
	Items   []ItemPlan     `json:"items"`
	Stats   map[string]int `json:"stats"`
	Sources map[string]int `json:"sources"`
	// Options echoes the planner settings the plan was built with.
	Options PlanOptions `json:"options"`
}

// PlanOptions records the knobs a plan was produced with, so an old plan file
// stays interpretable.
type PlanOptions struct {
	Policy   string   `json:"policy"`
	Intro    bool     `json:"intro"`
	Recap    bool     `json:"recap"`
	Credits  bool     `json:"credits"`
	Preview  bool     `json:"preview"`
	PALGuard bool     `json:"pal_guard"`
	Sources  []string `json:"sources"`
}

// Work returns the items that need a change.
func (p Plan) Work() []ItemPlan {
	var out []ItemPlan
	for _, it := range p.Items {
		if it.Changes() {
			out = append(out, it)
		}
	}
	return out
}

// SortByLabel orders items by their display label, for stable output.
func SortByLabel(items []LibraryItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].Label() < items[j].Label() })
}

// FormatMS renders a millisecond offset as m:ss for human output.
func FormatMS(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%d:%02d", ms/60000, (ms/1000)%60)
}
