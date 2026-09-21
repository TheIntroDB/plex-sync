// Package planner turns what the sources know into a change set.
//
// It is pure: it takes library items, per-source segment sets, the markers Plex
// currently has and the markers the ledger says we wrote, and returns a plan.
// No network, no database, no filesystem.
package planner

import (
	"sort"

	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/model"
)

// Reasons a plan records for an item.
const (
	ReasonAdd     = "add"     // nothing of ours there yet
	ReasonRefresh = "refresh" // ours are there but the desired set changed
	ReasonReapply = "reapply" // Plex wiped our markers; put them back
	ReasonRetract = "retract" // our timings are no longer trustworthy; remove them
	ReasonNoop    = "noop"    // already correct
)

// Inputs is everything the planner needs about the library.
type Inputs struct {
	// Sources maps an item's rating key to the segment sets its sources
	// produced. A missing source simply has nothing for that item.
	Sources map[int]map[model.SourceName]model.SegmentSet
	// Existing is what the Plex database holds right now.
	Existing map[int][]model.ExistingMarker
	// Written is what the ledger recorded us writing. It is what distinguishes
	// our markers from ones Plex detected itself, and it is how a wipe is
	// detected.
	Written map[int][]model.Marker
	// Durations overrides the item's duration when the real file length is
	// known from the database and differs from what the API reported.
	Durations map[int]*int64
	// PALSpedUp marks items whose community timings cannot be trusted.
	PALSpedUp map[int]bool
}

// Build produces the change set for a whole library.
func Build(items []model.LibraryItem, in Inputs, cfg config.Config) model.Plan {
	plan := model.Plan{
		Stats:   map[string]int{},
		Sources: map[string]int{},
		Options: model.PlanOptions{
			Policy:   cfg.Apply.Policy,
			Intro:    cfg.Segments.Intro,
			Recap:    cfg.Segments.Recap,
			Credits:  cfg.Segments.Credits,
			Preview:  cfg.Segments.Preview,
			PALGuard: cfg.Apply.PALGuard,
			Sources:  cfg.Sources.Ordered(),
		},
	}

	for _, item := range items {
		plan.Items = append(plan.Items, planItem(item, in, cfg, plan.Stats))
	}
	return plan
}

// planItem decides what one item needs.
func planItem(item model.LibraryItem, in Inputs, cfg config.Config, stats map[string]int) model.ItemPlan {
	itemPlan := model.ItemPlan{Item: item, Sources: map[string]string{}}

	// Anything we cannot attribute to exactly one file is left alone: a marker
	// placed against the wrong cut lands in the wrong place, and that is worse
	// than no marker at all.
	if item.PartCount > 1 {
		itemPlan.Reason = "skip:multiple-parts"
		return itemPlan
	}
	if _, ok := item.LookupKey(); !ok {
		itemPlan.Reason = "skip:no-provider-id"
		return itemPlan
	}

	duration := item.BestDuration()
	if override, ok := in.Durations[item.RatingKey]; ok && override != nil {
		duration = override
	}

	spedUp := cfg.Apply.PALGuard && in.PALSpedUp[item.RatingKey]
	segments, sources := merge(item.RatingKey, in.Sources[item.RatingKey], cfg, spedUp)
	itemPlan.Sources = sources

	desired := desiredMarkers(segments, duration, cfg)

	existing := in.Existing[item.RatingKey]
	written := in.Written[item.RatingKey]

	add, remove, kept, reason := decide(desired, existing, written, cfg, spedUp)
	itemPlan.Desired = desired
	itemPlan.Add = add
	itemPlan.Remove = remove
	itemPlan.Kept = kept
	itemPlan.Reason = reason

	stats[reason]++
	if reason == ReasonAdd || reason == ReasonRefresh || reason == ReasonReapply {
		for _, m := range add {
			stats["markers:"+string(m.Text)]++
		}
	}
	for _, m := range add {
		plan := m.Origin()
		stats["origin:"+string(plan)]++
	}
	return itemPlan
}

// merge picks, for each segment type, the first enabled source that answered.
//
// TheIntroDB is authoritative wherever it has data: it is community verified
// and cut-aware, because it is given the real file length. The other sources
// only fill types TheIntroDB did not cover, which is why the source order is
// fixed rather than configurable.
func merge(
	ratingKey int,
	sets map[model.SourceName]model.SegmentSet,
	cfg config.Config,
	spedUp bool,
) ([]model.Segment, map[string]string) {
	sources := map[string]string{}
	if len(sets) == 0 {
		return nil, sources
	}

	order := cfg.Sources.Ordered()
	// A PAL speed-up plays 4.3% fast, so timings measured on a normal-speed
	// release drift by minutes over an episode. Only the file's own chapters
	// (and our own detection, which reads this exact file) can be trusted.
	if spedUp {
		order = []string{"chapters", "detection"}
	}

	var out []model.Segment
	for _, segmentType := range model.SegmentTypes {
		if !cfg.Segments.Enabled(string(segmentType)) {
			continue
		}
		for _, name := range order {
			if !cfg.Sources.Enable(name) {
				continue
			}
			set, ok := sets[model.SourceName(name)]
			if !ok {
				continue
			}
			segs := set.Of(segmentType)
			if len(segs) == 0 {
				continue
			}
			for i := range segs {
				segs[i].Source = model.SourceName(name)
			}
			out = append(out, segs...)
			sources[string(segmentType)] = name
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return startOf(out[i]) < startOf(out[j]) })
	return out, sources
}

func startOf(s model.Segment) int64 {
	if s.StartMS == nil {
		return 0
	}
	return *s.StartMS
}

// desiredMarkers resolves segments into the marker set the item should end up
// with, and applies the two rules that only the writer's format imposes.
func desiredMarkers(segments []model.Segment, duration *int64, cfg config.Config) []model.Marker {
	var (
		intros  []model.Marker
		credits []model.Marker
	)
	for _, seg := range segments {
		text, ok := markerTextFor(seg.Type, cfg)
		if !ok {
			continue
		}
		// An intro that starts at 0:00 is normal; a credits marker with no end
		// is the one that runs to the end of the media.
		start, end, ok := seg.Resolve(duration, true, true)
		if !ok {
			continue
		}
		if end-start < cfg.Segments.MinMarkerMS {
			continue
		}
		m := model.Marker{Text: text, StartMS: start, EndMS: end, Source: string(seg.Source)}
		if text == model.MarkerIntro {
			intros = append(intros, m)
		} else {
			credits = append(credits, m)
		}
	}

	intros = mergeGroup(intros, cfg.Segments.MinMarkerMS)
	credits = mergeGroup(credits, cfg.Segments.MinMarkerMS)

	// A credits marker that reaches the end of the file is Plex's "final"
	// marker: it is the one that raises the Up Next prompt.
	if duration != nil && *duration > 0 {
		for i := range credits {
			if credits[i].EndMS >= int64(*duration)-cfg.Segments.FinalSlackMS {
				credits[i].Final = true
			}
		}
	}

	out := append(intros, credits...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartMS != out[j].StartMS {
			return out[i].StartMS < out[j].StartMS
		}
		return out[i].EndMS < out[j].EndMS
	})
	// Plex rejects overlapping markers, and so does its own editor. Two
	// same-text markers that touch are one marker; a cross-text overlap is
	// trimmed, and dropped when nothing usable is left.
	return mergeGroup(out, cfg.Segments.MinMarkerMS)
}

// markerTextFor maps a segment type onto the two kinds Plex understands.
func markerTextFor(t model.SegmentType, cfg config.Config) (model.MarkerText, bool) {
	switch t {
	case model.SegmentIntro:
		return model.MarkerIntro, cfg.Segments.Intro
	case model.SegmentRecap:
		if !cfg.Segments.Recap || !cfg.Segments.MapRecap {
			return "", false
		}
		return model.MarkerIntro, true
	case model.SegmentCredits:
		return model.MarkerCredits, cfg.Segments.Credits
	case model.SegmentPreview:
		if !cfg.Segments.Preview || !cfg.Segments.MapPreview {
			return "", false
		}
		return model.MarkerCredits, true
	}
	return "", false
}

// mergeGroup joins touching or overlapping markers and trims cross-text
// overlaps, mirroring what Plex's own marker format allows.
func mergeGroup(in []model.Marker, minMS int64) []model.Marker {
	var out []model.Marker
	for _, m := range in {
		if m.EndMS-m.StartMS < minMS {
			continue
		}
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		if m.StartMS > prev.EndMS {
			out = append(out, m)
			continue
		}
		if prev.Text == m.Text {
			if m.EndMS > prev.EndMS {
				prev.EndMS = m.EndMS
			}
			prev.Source = combineSources(prev.Source, m.Source)
			prev.Final = prev.Final || m.Final
			continue
		}
		// Different kinds overlap: push this one past the previous marker, and
		// drop it if the remaining range is not worth writing.
		shifted := m
		shifted.StartMS = prev.EndMS + 1
		if shifted.EndMS-shifted.StartMS < minMS {
			continue
		}
		out = append(out, shifted)
	}
	return out
}

func combineSources(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	parts := map[string]bool{}
	var ordered []string
	for _, s := range append(splitPlus(a), splitPlus(b)...) {
		if !parts[s] {
			parts[s] = true
			ordered = append(ordered, s)
		}
	}
	sort.Strings(ordered)
	out := ""
	for i, s := range ordered {
		if i > 0 {
			out += "+"
		}
		out += s
	}
	return out
}

func splitPlus(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '+' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// decide compares the desired markers with what is actually in Plex.
//
// The distinction that matters is ours versus Plex's. A marker the ledger
// recorded is ours, and may be replaced or removed. A marker Plex detected
// itself is left alone unless the policy says otherwise, because that marker is
// what the user sees today and silently rewriting it would be a surprise.
func decide(
	desired []model.Marker,
	existing []model.ExistingMarker,
	written []model.Marker,
	cfg config.Config,
	spedUp bool,
) (add []model.Marker, remove []int64, kept []model.ExistingMarker, reason string) {
	ours := map[string]bool{}
	for _, m := range written {
		ours[m.Key()] = true
	}

	wanted := map[model.MarkerText]bool{}
	for _, m := range desired {
		wanted[m.Text] = true
	}

	removeSet := map[int64]bool{}
	for _, row := range existing {
		if !isMarkerText(row.Text) {
			// Commercials and bookmarks are never ours to touch.
			kept = append(kept, row)
			continue
		}
		isOurs := ours[row.Key()]
		isWanted := false
		for _, m := range desired {
			if m.Key() == row.Key() {
				isWanted = true
				break
			}
		}

		switch {
		case spedUp && isOurs:
			// These came from a source we can no longer trust for this file.
			// Whatever the chapters or our own detection produced replaces
			// them, and if they produced nothing the markers are retracted
			// rather than left to skip the wrong scene.
			removeSet[row.TagID] = true
		case isOurs && !isWanted:
			// Ours, and no longer what the sources say: replace it.
			removeSet[row.TagID] = true
		case cfg.Apply.Policy == "prefer-theintrodb" && !isOurs &&
			wanted[model.MarkerText(row.Text)]:
			// Plex's own marker for a kind we have data for: replace it too.
			removeSet[row.TagID] = true
		default:
			kept = append(kept, row)
		}
	}

	// Anything desired that is not already there, and does not collide with
	// something that is, gets written.
	keptKeys := map[string]bool{}
	for _, row := range kept {
		keptKeys[row.Key()] = true
	}
	var pending []model.Marker
	for _, m := range desired {
		if keptKeys[m.Key()] {
			continue
		}
		if overlaps(m, kept) {
			continue
		}
		pending = append(pending, m)
	}

	// The desired set is what the item holds afterwards: the rows that survive
	// plus what we add, in the index order the writer needs.
	final := make([]model.Marker, 0, len(kept)+len(pending))
	for _, row := range kept {
		if !isMarkerText(row.Text) {
			continue
		}
		final = append(final, model.Marker{
			Text:    model.MarkerText(row.Text),
			StartMS: row.StartMS,
			EndMS:   row.EndMS,
			Final:   rowHasFinal(row.ExtraData),
			Source:  "plex",
		})
	}
	final = append(final, pending...)
	sort.SliceStable(final, func(i, j int) bool { return final[i].StartMS < final[j].StartMS })

	switch {
	case len(pending) == 0 && len(removeSet) == 0:
		reason = ReasonNoop
	case len(pending) == 0:
		reason = ReasonRetract
	case len(written) > 0 && !hadOurs(existing, ours):
		// We wrote markers for this item before, none of them survive, and we
		// are writing again: Plex re-analysed the item and wiped them, so this
		// is putting them back rather than a first add.
		reason = ReasonReapply
	case len(written) == 0:
		reason = ReasonAdd
	default:
		reason = ReasonRefresh
	}

	remove = make([]int64, 0, len(removeSet))
	for id := range removeSet {
		remove = append(remove, id)
	}
	sort.Slice(remove, func(i, j int) bool { return remove[i] < remove[j] })
	return pending, remove, kept, reason
}

// hadOurs reports whether any row currently in Plex is one we wrote.
func hadOurs(existing []model.ExistingMarker, ours map[string]bool) bool {
	for _, row := range existing {
		if ours[row.Key()] {
			return true
		}
	}
	return false
}

func isMarkerText(text string) bool {
	return text == string(model.MarkerIntro) || text == string(model.MarkerCredits)
}

func rowHasFinal(extraData string) bool {
	return extraData != "" && contains(extraData, "final")
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func overlaps(m model.Marker, kept []model.ExistingMarker) bool {
	for _, row := range kept {
		if !isMarkerText(row.Text) {
			continue
		}
		if m.StartMS <= row.EndMS && row.StartMS <= m.EndMS {
			return true
		}
	}
	return false
}

// PALSpedUp finds the items whose community timings cannot be trusted.
//
// Some releases are PAL speed-ups of film-rate content: they play 4.3% fast, so
// a timing measured on the normal-speed release drifts by about five seconds at
// two minutes and by two minutes at fifty. An episode at 25 or 50 fps is a
// speed-up when at least half of its season is at film rate; a movie is one
// when its file is between 2.5% and 6% shorter than its listed runtime. Seasons
// that are entirely PAL (native European productions) cannot be told apart from
// a speed-up, so they are left alone.
func PALSpedUp(items []model.LibraryItem) map[int]bool {
	out := map[int]bool{}
	seasons := map[[2]int][]*float64{}
	for i := range items {
		item := items[i]
		if item.Kind == model.KindEpisode && item.FPS != nil {
			key := [2]int{item.ShowRatingKey, derefInt(item.Season)}
			seasons[key] = append(seasons[key], item.FPS)
		}
	}

	for i := range items {
		item := items[i]
		if item.FPS == nil || !isPAL(*item.FPS) {
			continue
		}
		switch item.Kind {
		case model.KindEpisode:
			key := [2]int{item.ShowRatingKey, derefInt(item.Season)}
			siblings := seasons[key]
			if len(siblings) < 2 {
				continue
			}
			film := 0
			for _, fps := range siblings {
				if fps != nil && isFilm(*fps) {
					film++
				}
			}
			if film*2 >= len(siblings) {
				out[item.RatingKey] = true
			}
		case model.KindMovie:
			if item.DurationMS == nil || item.FileDurationMS == nil || *item.DurationMS <= 0 {
				continue
			}
			ratio := float64(*item.FileDurationMS) / float64(*item.DurationMS)
			if ratio >= 0.94 && ratio <= 0.975 {
				out[item.RatingKey] = true
			}
		}
	}
	return out
}

func isPAL(fps float64) bool  { return near(fps, 25) || near(fps, 50) }
func isFilm(fps float64) bool { return near(fps, 23.976) || near(fps, 24) }

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.05
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
