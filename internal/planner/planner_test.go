package planner

import (
	"testing"

	"github.com/TheIntroDB/plex-integration/internal/config"
	"github.com/TheIntroDB/plex-integration/internal/model"
)

func intPtr(v int) *int      { return &v }
func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

// episode builds an episode with a 40 minute file and a TMDb id.
func episode(ratingKey int, season, number int) model.LibraryItem {
	duration := int64(2_400_000)
	return model.LibraryItem{
		RatingKey:      ratingKey,
		Kind:           model.KindEpisode,
		Title:          "Episode",
		ShowTitle:      "Show",
		ShowRatingKey:  7,
		Season:         intPtr(season),
		Episode:        intPtr(number),
		DurationMS:     &duration,
		FileDurationMS: &duration,
		PartCount:      1,
		IDs:            model.ExternalIDs{TMDB: intPtr(1396)},
	}
}

func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Sources.Chapters = true
	return *cfg
}

func tidbSet(segments ...model.Segment) model.SegmentSet {
	set := model.SegmentSet{Source: model.SourceTheIntroDB}
	for i := range segments {
		segments[i].Source = model.SourceTheIntroDB
		set.Segments = append(set.Segments, segments[i])
	}
	return set
}

func TestFillKeepsPlexMarkersAndAddsWhatIsMissing(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	// Plex detected the intro itself; TheIntroDB has both an intro and credits.
	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
				model.Segment{Type: model.SegmentCredits, StartMS: i64(2_300_000), EndMS: i64(2_400_000)},
			)},
		},
		Existing: map[int][]model.ExistingMarker{
			1: {{TagID: 10, Text: "intro", StartMS: 61_000, EndMS: 88_000, Index: 0, Origin: "plex"}},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if got.Reason != ReasonAdd {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonAdd)
	}
	// The intro overlaps Plex's own marker, so it is dropped rather than
	// creating a second intro marker. Credits is added.
	if len(got.Add) != 1 {
		t.Fatalf("add = %+v, want exactly the credits marker", got.Add)
	}
	if got.Add[0].Text != model.MarkerCredits {
		t.Errorf("added %q, want credits", got.Add[0].Text)
	}
	if len(got.Remove) != 0 {
		t.Errorf("remove = %v, want nothing removed under fill", got.Remove)
	}
	if len(got.Kept) != 1 || got.Kept[0].TagID != 10 {
		t.Errorf("kept = %+v, want Plex's own marker preserved", got.Kept)
	}
	if !got.Desired[1].Final {
		t.Error("a credits marker reaching the end of the file must be marked final")
	}
}

func TestRefreshReplacesOurMarkersWhenTimingsChange(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	mine := model.Marker{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"}
	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(62_000), EndMS: i64(95_000)},
			)},
		},
		Existing: map[int][]model.ExistingMarker{
			1: {{TagID: 20, Text: "intro", StartMS: mine.StartMS, EndMS: mine.EndMS, Index: 0, Origin: "ours"}},
		},
		Written: map[int][]model.Marker{1: {mine}},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if got.Reason != ReasonRefresh {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonRefresh)
	}
	if len(got.Remove) != 1 || got.Remove[0] != 20 {
		t.Errorf("remove = %v, want [20]", got.Remove)
	}
	if len(got.Add) != 1 || got.Add[0].StartMS != 62_000 {
		t.Errorf("add = %+v, want the new timing", got.Add)
	}
}

func TestReapplyAfterPlexWipesOurMarkers(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	mine := model.Marker{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"}
	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
			)},
		},
		// The ledger says we wrote it, but nothing of ours is in Plex now:
		// Plex re-analysed the season and cleared custom markers.
		Written: map[int][]model.Marker{1: {mine}},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if got.Reason != ReasonReapply {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonReapply)
	}
	if len(got.Add) != 1 {
		t.Errorf("add = %+v, want the marker put back", got.Add)
	}
}

func TestNoopWhenNothingChanges(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	mine := model.Marker{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"}
	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
			)},
		},
		Existing: map[int][]model.ExistingMarker{
			1: {{TagID: 30, Text: "intro", StartMS: 60_000, EndMS: 90_000, Index: 0, Origin: "ours"}},
		},
		Written: map[int][]model.Marker{1: {mine}},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	if got := plan.Items[0]; got.Reason != ReasonNoop {
		t.Errorf("reason = %q, want %q (%+v)", got.Reason, ReasonNoop, got)
	}
	if len(plan.Work()) != 0 {
		t.Errorf("work = %d items, want none", len(plan.Work()))
	}
}

func TestPreferTheIntroDBReplacesPlexMarker(t *testing.T) {
	cfg := baseConfig()
	cfg.Apply.Policy = "prefer-theintrodb"
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(50_000), EndMS: i64(95_000)},
			)},
		},
		Existing: map[int][]model.ExistingMarker{
			1: {{TagID: 40, Text: "intro", StartMS: 61_000, EndMS: 88_000, Index: 0, Origin: "plex"}},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if len(got.Remove) != 1 || got.Remove[0] != 40 {
		t.Errorf("remove = %v, want Plex's marker replaced", got.Remove)
	}
	if len(got.Add) != 1 || got.Add[0].StartMS != 50_000 {
		t.Errorf("add = %+v, want TheIntroDB's timing", got.Add)
	}
}

func TestTheIntroDBWinsOverChapters(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {
				model.SourceTheIntroDB: tidbSet(
					model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
				),
				model.SourceChapters: {
					Source: model.SourceChapters,
					Segments: []model.Segment{
						{Type: model.SegmentIntro, StartMS: i64(10_000), EndMS: i64(70_000)},
						{Type: model.SegmentCredits, StartMS: i64(2_300_000), EndMS: i64(2_400_000)},
					},
				},
			},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if got.Sources["intro"] != "theintrodb" {
		t.Errorf("intro source = %q, want theintrodb", got.Sources["intro"])
	}
	if got.Sources["credits"] != "chapters" {
		t.Errorf("credits source = %q, want chapters to fill the gap", got.Sources["credits"])
	}
	for _, m := range got.Add {
		if m.Text == model.MarkerIntro && m.StartMS != 60_000 {
			t.Errorf("intro start = %d, want TheIntroDB's 60000", m.StartMS)
		}
	}
}

func TestPALSpeedUpIgnoresCommunityTimings(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	mine := model.Marker{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"}
	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {
				model.SourceTheIntroDB: tidbSet(
					model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
				),
				model.SourceChapters: {
					Source: model.SourceChapters,
					Segments: []model.Segment{
						{Type: model.SegmentIntro, StartMS: i64(64_000), EndMS: i64(94_000)},
					},
				},
			},
		},
		Existing: map[int][]model.ExistingMarker{
			1: {{TagID: 50, Text: "intro", StartMS: 60_000, EndMS: 90_000, Index: 0, Origin: "ours"}},
		},
		Written:   map[int][]model.Marker{1: {mine}},
		PALSpedUp: map[int]bool{1: true},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if got.Sources["intro"] != "chapters" {
		t.Errorf("intro source = %q, want chapters only on a PAL speed-up", got.Sources["intro"])
	}
	if len(got.Remove) != 1 || got.Remove[0] != 50 {
		t.Errorf("remove = %v, want the untrustworthy marker removed", got.Remove)
	}
	for _, m := range got.Add {
		if m.StartMS == 60_000 {
			t.Error("the drifted community timing must not be written again")
		}
	}
}

func TestRetractWhenNothingCanReplaceOurMarkers(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	mine := model.Marker{Text: model.MarkerIntro, StartMS: 60_000, EndMS: 90_000, Source: "theintrodb"}
	in := Inputs{
		// A PAL speed-up with no chapters: the timings we wrote are wrong and
		// nothing else covers the segment, so they come out.
		Existing: map[int][]model.ExistingMarker{
			1: {{TagID: 60, Text: "intro", StartMS: 60_000, EndMS: 90_000, Index: 0, Origin: "ours"}},
		},
		Written:   map[int][]model.Marker{1: {mine}},
		PALSpedUp: map[int]bool{1: true},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if got.Reason != ReasonRetract {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonRetract)
	}
	if len(got.Remove) != 1 || len(got.Add) != 0 {
		t.Errorf("remove = %v add = %+v, want a retraction", got.Remove, got.Add)
	}
}

func TestSkippedItems(t *testing.T) {
	cfg := baseConfig()

	multi := episode(1, 1, 1)
	multi.PartCount = 2
	noid := episode(2, 1, 2)
	noid.IDs = model.ExternalIDs{}

	plan := Build([]model.LibraryItem{multi, noid}, Inputs{}, cfg)
	if plan.Items[0].Reason != "skip:multiple-parts" {
		t.Errorf("reason = %q, want skip:multiple-parts", plan.Items[0].Reason)
	}
	if plan.Items[1].Reason != "skip:no-provider-id" {
		t.Errorf("reason = %q, want skip:no-provider-id", plan.Items[1].Reason)
	}
	if len(plan.Work()) != 0 {
		t.Error("skipped items must not produce work")
	}
}

func TestNullTimesResolveAgainstTheFile(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				// A null start means the intro begins at 0:00.
				model.Segment{Type: model.SegmentIntro, StartMS: nil, EndMS: i64(30_000)},
				// A null end means the credits run to the end of the media.
				model.Segment{Type: model.SegmentCredits, StartMS: i64(2_300_000), EndMS: nil},
			)},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if len(got.Add) != 2 {
		t.Fatalf("add = %+v, want both markers", got.Add)
	}
	if got.Add[0].StartMS != 0 || got.Add[0].EndMS != 30_000 {
		t.Errorf("intro = %d-%d, want 0-30000", got.Add[0].StartMS, got.Add[0].EndMS)
	}
	credits := got.Add[1]
	if credits.EndMS != 2_400_000 {
		t.Errorf("credits end = %d, want the file length", credits.EndMS)
	}
	if !credits.Final {
		t.Error("credits reaching the end of the media must be final")
	}
}

func TestSegmentPastTheEndIsDropped(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				// Measured on a longer cut than the file we hold.
				model.Segment{Type: model.SegmentCredits, StartMS: i64(2_600_000), EndMS: i64(2_700_000)},
			)},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	if got := plan.Items[0]; len(got.Add) != 0 {
		t.Errorf("add = %+v, want nothing: the segment starts past our file's end", got.Add)
	}
}

func TestShortMarkersAreDropped(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(61_000)},
			)},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	if got := plan.Items[0]; len(got.Add) != 0 {
		t.Errorf("add = %+v, want nothing under the minimum marker length", got.Add)
	}
}

func TestRecapFoldsIntoIntro(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentRecap, StartMS: i64(5_000), EndMS: i64(40_000)},
				model.Segment{Type: model.SegmentIntro, StartMS: i64(40_000), EndMS: i64(70_000)},
			)},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]

	if len(got.Add) != 1 {
		t.Fatalf("add = %+v, want the recap and intro joined into one marker", got.Add)
	}
	if got.Add[0].Text != model.MarkerIntro {
		t.Errorf("text = %q, want intro", got.Add[0].Text)
	}
	if got.Add[0].StartMS != 5_000 || got.Add[0].EndMS != 70_000 {
		t.Errorf("range = %d-%d, want 5000-70000", got.Add[0].StartMS, got.Add[0].EndMS)
	}
}

func TestPreviewIsOffByDefault(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentPreview, StartMS: i64(2_350_000), EndMS: i64(2_390_000)},
			)},
		},
	}
	plan := Build([]model.LibraryItem{item}, in, cfg)
	if got := plan.Items[0]; len(got.Add) != 0 {
		t.Errorf("add = %+v, want nothing while previews are disabled", got.Add)
	}
}

func TestPALSpedUpEpisodes(t *testing.T) {
	film := 23.976
	pal := 25.0

	// A season that is mostly film rate with one PAL episode.
	a := episode(1, 1, 1)
	a.FPS = f64(film)
	b := episode(2, 1, 2)
	b.FPS = f64(film)
	c := episode(3, 1, 3)
	c.FPS = f64(pal)

	// A season that is entirely PAL: a native production, left alone.
	d := episode(4, 2, 1)
	d.FPS = f64(pal)
	e := episode(5, 2, 2)
	e.FPS = f64(pal)

	// A movie 4% shorter than its listed runtime.
	movie := model.LibraryItem{
		RatingKey:      6,
		Kind:           model.KindMovie,
		Title:          "Film",
		DurationMS:     i64(7_200_000),
		FileDurationMS: i64(6_912_000),
		FPS:            f64(pal),
		PartCount:      1,
		IDs:            model.ExternalIDs{TMDB: intPtr(550)},
	}
	normalMovie := model.LibraryItem{
		RatingKey:      7,
		Kind:           model.KindMovie,
		Title:          "Other Film",
		DurationMS:     i64(7_200_000),
		FileDurationMS: i64(7_200_000),
		FPS:            f64(pal),
		PartCount:      1,
		IDs:            model.ExternalIDs{TMDB: intPtr(551)},
	}

	got := PALSpedUp([]model.LibraryItem{a, b, c, d, e, movie, normalMovie})

	if !got[3] {
		t.Error("the PAL episode in a mostly film-rate season must be flagged")
	}
	if got[1] || got[2] {
		t.Error("film-rate episodes must not be flagged")
	}
	if got[4] || got[5] {
		t.Error("an all-PAL season cannot be told apart from a speed-up and must be left alone")
	}
	if !got[6] {
		t.Error("a movie 4% shorter than its runtime must be flagged")
	}
	if got[7] {
		t.Error("a movie matching its runtime must not be flagged")
	}
}

func TestMergeGroupJoinsTouchingMarkersOfTheSameKind(t *testing.T) {
	cfg := baseConfig()
	item := episode(1, 1, 1)

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
				model.Segment{Type: model.SegmentIntro, StartMS: i64(85_000), EndMS: i64(120_000)},
			)},
		},
	}

	plan := Build([]model.LibraryItem{item}, in, cfg)
	got := plan.Items[0]
	if len(got.Add) != 1 {
		t.Fatalf("add = %+v, want the two intros joined", got.Add)
	}
	if got.Add[0].StartMS != 60_000 || got.Add[0].EndMS != 120_000 {
		t.Errorf("range = %d-%d, want 60000-120000", got.Add[0].StartMS, got.Add[0].EndMS)
	}
}

func TestMarkerOriginReportsMergedSources(t *testing.T) {
	m := model.Marker{Text: model.MarkerCredits, StartMS: 1, EndMS: 2, Source: "theintrodb+chapters"}
	if got := m.Origin(); got != model.OriginMixed {
		t.Errorf("origin = %q, want mixed", got)
	}
	single := model.Marker{Text: model.MarkerIntro, StartMS: 1, EndMS: 2, Source: "chapters"}
	if got := single.Origin(); got != model.OriginChapters {
		t.Errorf("origin = %q, want chapters", got)
	}
}

func TestPlanStatsCountWhatWillChange(t *testing.T) {
	cfg := baseConfig()
	items := []model.LibraryItem{episode(1, 1, 1), episode(2, 1, 2)}

	in := Inputs{
		Sources: map[int]map[model.SourceName]model.SegmentSet{
			1: {model.SourceTheIntroDB: tidbSet(
				model.Segment{Type: model.SegmentIntro, StartMS: i64(60_000), EndMS: i64(90_000)},
			)},
		},
	}

	plan := Build(items, in, cfg)
	if plan.Stats[ReasonAdd] != 1 {
		t.Errorf("add count = %d, want 1", plan.Stats[ReasonAdd])
	}
	if plan.Stats[ReasonNoop] != 1 {
		t.Errorf("noop count = %d, want 1 for the item with no data", plan.Stats[ReasonNoop])
	}
	if plan.Stats["markers:intro"] != 1 {
		t.Errorf("marker count = %d, want 1", plan.Stats["markers:intro"])
	}
	if plan.Stats["origin:theintrodb"] != 1 {
		t.Errorf("origin count = %d, want 1", plan.Stats["origin:theintrodb"])
	}
	if len(plan.Work()) != 1 {
		t.Errorf("work = %d, want 1", len(plan.Work()))
	}
}
