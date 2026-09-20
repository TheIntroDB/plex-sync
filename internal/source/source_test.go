package source

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheIntroDB/plex-integration/internal/config"
	"github.com/TheIntroDB/plex-integration/internal/model"
)

func episode(season, number int) model.LibraryItem {
	duration := int64(2_400_000)
	s, e := season, number
	return model.LibraryItem{
		RatingKey:  100 + number,
		Kind:       model.KindEpisode,
		Title:      "Episode",
		ShowTitle:  "Show",
		Season:     &s,
		Episode:    &e,
		DurationMS: &duration,
		IDs:        model.ExternalIDs{TMDB: intPtr(1396)},
	}
}

func intPtr(v int) *int { return &v }

func cfgWithChapters(enabled bool) config.Config {
	cfg := config.Default()
	cfg.Sources.Chapters = enabled
	return *cfg
}

func TestChaptersRecognisesNames(t *testing.T) {
	item := episode(2, 3)
	cases := []struct {
		name     string
		chapters []model.Chapter
		want     []model.SegmentType
	}{
		{
			name: "intro and end credits",
			chapters: []model.Chapter{
				{Name: "Cold Open", StartMS: 0, EndMS: 60_000},
				{Name: "Intro", StartMS: 60_000, EndMS: 95_000},
				{Name: "Act One", StartMS: 95_000, EndMS: 1_200_000},
				{Name: "End Credits", StartMS: 2_200_000, EndMS: 2_400_000},
			},
			want: []model.SegmentType{model.SegmentIntro, model.SegmentCredits},
		},
		{
			name: "recap and next time",
			chapters: []model.Chapter{
				{Name: "Previously On", StartMS: 5_000, EndMS: 70_000},
				{Name: "Intro", StartMS: 70_000, EndMS: 100_000},
				{Name: "Next Time On", StartMS: 2_300_000, EndMS: 2_390_000},
			},
			want: []model.SegmentType{model.SegmentIntro, model.SegmentRecap, model.SegmentPreview},
		},
		{
			name: "uninformative names are ignored",
			chapters: []model.Chapter{
				{Name: "Scene 1", StartMS: 0, EndMS: 300_000},
				{Name: "Studio Logo", StartMS: 300_000, EndMS: 320_000},
				{Name: "Part 01", StartMS: 320_000, EndMS: 900_000},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, ok := Chapters(item, tc.chapters, cfgWithChapters(true))
			if tc.want == nil {
				if ok {
					t.Fatalf("expected no segments, got %+v", set.Segments)
				}
				return
			}
			if !ok {
				t.Fatalf("expected segments, got none")
			}
			got := set.Types()
			if len(got) != len(tc.want) {
				t.Fatalf("types = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("types = %v, want %v", got, tc.want)
				}
			}
			for _, seg := range set.Segments {
				if seg.Source != model.SourceChapters {
					t.Errorf("segment source = %q, want chapters", seg.Source)
				}
			}
		})
	}
}

func TestChaptersRejectsImplausibleRanges(t *testing.T) {
	item := episode(1, 1)
	cases := []struct {
		name    string
		chapter model.Chapter
		other   model.Chapter
	}{
		{
			name:    "intro that swallows the cold open",
			chapter: model.Chapter{Name: "Intro", StartMS: 0, EndMS: 200_000},
			other:   model.Chapter{Name: "Act One", StartMS: 200_000, EndMS: 2_000_000},
		},
		{
			name:    "recap that is really an act",
			chapter: model.Chapter{Name: "Previously On", StartMS: 0, EndMS: 700_000},
			other:   model.Chapter{Name: "Act One", StartMS: 700_000, EndMS: 2_000_000},
		},
		{
			name:    "credits chapter in the middle",
			chapter: model.Chapter{Name: "End Credits", StartMS: 600_000, EndMS: 700_000},
			other:   model.Chapter{Name: "Act Two", StartMS: 700_000, EndMS: 2_300_000},
		},
		{
			name:    "intro chapter that starts late",
			chapter: model.Chapter{Name: "Intro", StartMS: 1_400_000, EndMS: 1_460_000},
			other:   model.Chapter{Name: "Act One", StartMS: 0, EndMS: 1_400_000},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, ok := Chapters(item, []model.Chapter{tc.chapter, tc.other}, cfgWithChapters(true))
			if ok && len(set.Segments) > 0 {
				t.Fatalf("expected the chapter to be rejected, got %+v", set.Segments)
			}
		})
	}
}

func TestChaptersMoviesTakeCreditsOnly(t *testing.T) {
	duration := int64(7_200_000)
	movie := model.LibraryItem{
		RatingKey:  1,
		Kind:       model.KindMovie,
		Title:      "A Film",
		DurationMS: &duration,
		IDs:        model.ExternalIDs{TMDB: intPtr(550)},
	}
	chapters := []model.Chapter{
		{Name: "Opening Credits", StartMS: 30_000, EndMS: 150_000},
		{Name: "Act One", StartMS: 150_000, EndMS: 6_800_000},
		{Name: "End Credits", StartMS: 6_800_000, EndMS: 7_200_000},
	}
	set, ok := Chapters(movie, chapters, cfgWithChapters(true))
	if !ok {
		t.Fatal("expected credits chapter to be accepted")
	}
	if set.Has(model.SegmentIntro) {
		t.Error("a movie must not take an intro from chapters: opening credits often run over the first scene")
	}
	if !set.Has(model.SegmentCredits) {
		t.Error("expected a credits segment")
	}
}

// --- detection -------------------------------------------------------------

func TestParseFpcalc(t *testing.T) {
	frames := []uint32{0xdeadbeef, 0x00000000, 0xffffffff}
	raw := make([]byte, len(frames)*4)
	for i, f := range frames {
		binary.LittleEndian.PutUint32(raw[i*4:], f)
	}
	out := []byte(`{"duration": 12.34, "fingerprint": "` +
		base64.StdEncoding.EncodeToString(raw) + `"}`)

	got, duration, err := ParseFpcalc(out)
	if err != nil {
		t.Fatalf("ParseFpcalc: %v", err)
	}
	if duration != 12.34 {
		t.Errorf("duration = %v, want 12.34", duration)
	}
	if len(got) != len(frames) {
		t.Fatalf("frames = %d, want %d", len(got), len(frames))
	}
	for i := range frames {
		if got[i] != frames[i] {
			t.Errorf("frame %d = %#x, want %#x", i, got[i], frames[i])
		}
	}

	if _, _, err := ParseFpcalc([]byte(`{"duration":1}`)); err == nil {
		t.Error("expected an error for a missing fingerprint")
	}
	if _, _, err := ParseFpcalc([]byte(`not json`)); err == nil {
		t.Error("expected an error for malformed output")
	}
}

func TestCompareFindsOffset(t *testing.T) {
	// A distinctive reference, padded into a longer target at a known offset.
	reference := []uint32{0x1111aaaa, 0x2222bbbb, 0x3333cccc, 0x4444dddd, 0x5555eeee, 0x6666ffff}
	filler := []uint32{0xaaaaaaaa, 0xbbbbbbbb, 0xcccccccc, 0xdddddddd, 0xeeeeeeee}

	target := append([]uint32{}, filler...)
	target = append(target, reference...)
	target = append(target, filler...)

	match, ok := Compare(reference, target)
	if !ok {
		t.Fatal("expected a match")
	}
	if match.OffsetFrames != len(filler) {
		t.Errorf("offset = %d, want %d", match.OffsetFrames, len(filler))
	}
	if match.Distance != 0 {
		t.Errorf("distance = %v, want 0 for identical audio", match.Distance)
	}
	if want := float64(len(filler)) * FrameSeconds; match.OffsetSeconds != want {
		t.Errorf("offset seconds = %v, want %v", match.OffsetSeconds, want)
	}
}

func TestCompareRejectsUnrelatedAudio(t *testing.T) {
	reference := []uint32{0x11111111, 0x22222222, 0x33333333, 0x44444444, 0x55555555}
	// Alternating bit patterns: maximally distant from the reference.
	target := make([]uint32, 60)
	for i := range target {
		target[i] = 0xffffffff
	}
	match, ok := Compare(reference, target)
	if !ok {
		t.Fatal("expected Compare to run")
	}
	if match.Distance < 0.3 {
		t.Errorf("distance = %v, want a large distance for unrelated audio", match.Distance)
	}
}

func TestCompareNeedsRoomForTheReference(t *testing.T) {
	if _, ok := Compare([]uint32{1, 2, 3}, []uint32{1, 2}); ok {
		t.Error("a reference that cannot fit must not match")
	}
	if _, ok := Compare(nil, []uint32{1, 2, 3}); ok {
		t.Error("an empty reference must not match")
	}
}

func TestFramesForRange(t *testing.T) {
	frames := make([]uint32, 1000)
	// Ten frames at 0.1238 s each is 1.238 s of audio.
	got := FramesForRange(frames, 0, 1_238)
	if len(got) != 10 {
		t.Errorf("frames = %d, want 10", len(got))
	}
	if got := FramesForRange(frames, 0, 12_380); len(got) != 100 {
		t.Errorf("frames = %d, want 100", len(got))
	}
	if got := FramesForRange(frames, 500_000, 600_000); got != nil {
		t.Errorf("a range past the end must return nothing, got %d frames", len(got))
	}
	if got := FramesForRange(frames, 10_000, 5_000); got != nil {
		t.Errorf("an inverted range must return nothing, got %d frames", len(got))
	}
}

type stubRunner struct {
	frames   []uint32
	failNext bool
	commands []string
}

func (s *stubRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	s.commands = append(s.commands, name)
	if s.failNext {
		return nil, errors.New("stub failure")
	}
	if filepath.Base(name) == "fpcalc" {
		raw := make([]byte, len(s.frames)*4)
		for i, f := range s.frames {
			binary.LittleEndian.PutUint32(raw[i*4:], f)
		}
		return []byte(`{"duration": 30, "fingerprint": "` +
			base64.StdEncoding.EncodeToString(raw) + `"}`), nil
	}
	return nil, nil
}

func TestDetectIntroMatchesSibling(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "fp"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// A realistic intro: about 3.7 seconds of distinctive audio. Anything
	// under a second is refused, because a short window matches too easily.
	intro := make([]uint32, 30)
	for i := range intro {
		intro[i] = uint32(0x10000000 * (i + 1))
	}
	padding := make([]uint32, 40)
	for i := range padding {
		padding[i] = 0xffff0000
	}
	// The sibling and the target both carry the same intro, 40 frames (about
	// five seconds) into the file.
	targetFrames := append(append([]uint32{}, padding...), intro...)
	targetFrames = append(targetFrames, padding...)

	introStartMS := int64(float64(len(padding)) * FrameSeconds * 1000)
	introLengthMS := int64(float64(len(intro)) * FrameSeconds * 1000)

	runner := &stubRunner{frames: targetFrames}
	detector := &Detector{
		Fingerprinter: NewFingerprinter("ffmpeg", "fpcalc", runner),
		Store:         store,
		WindowSeconds: 16,
		ToleranceS:    5,
	}

	refPath := filepath.Join(dir, "sibling.mkv")
	targetPath := filepath.Join(dir, "target.mkv")
	for _, p := range []string{refPath, targetPath} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reference := Reference{
		Path:    refPath,
		Size:    1,
		StartMS: introStartMS,
		EndMS:   introStartMS + introLengthMS,
		Source:  "theintrodb",
	}
	result, err := detector.DetectIntro(context.Background(), targetPath, 1, reference, dir)
	if err != nil {
		t.Fatalf("DetectIntro: %v", err)
	}
	if result == nil {
		t.Fatal("expected a detection")
	}
	if result.StartMS != introStartMS {
		t.Errorf("start = %d ms, want %d ms", result.StartMS, introStartMS)
	}
	if want := introStartMS + introLengthMS; result.EndMS != want {
		t.Errorf("end = %d ms, want %d ms", result.EndMS, want)
	}
}

func TestDetectIntroSkipsWhenBinariesFail(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "fp"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &stubRunner{failNext: true}
	detector := &Detector{
		Fingerprinter: NewFingerprinter("ffmpeg", "fpcalc", runner),
		Store:         store,
	}
	if _, err := detector.DetectIntro(context.Background(), "a", 1,
		Reference{Path: "b", Size: 1, StartMS: 0, EndMS: 1000}, dir); err == nil {
		t.Error("a decode failure must be reported, not silently treated as no match")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "fp"))
	if err != nil {
		t.Fatal(err)
	}
	key := Key("/media/show/s01e01.mkv", 1234)
	if _, _, ok := store.Get(key); ok {
		t.Fatal("expected an empty store")
	}
	frames := []uint32{1, 2, 3, 4}
	if err := store.Put(key, "/media/show/s01e01.mkv", 1234, 42.5, frames); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, duration, ok := store.Get(key)
	if !ok {
		t.Fatal("expected the fingerprint to be stored")
	}
	if duration != 42.5 {
		t.Errorf("duration = %v, want 42.5", duration)
	}
	if len(got) != len(frames) {
		t.Fatalf("frames = %d, want %d", len(got), len(frames))
	}
	for i := range frames {
		if got[i] != frames[i] {
			t.Errorf("frame %d = %d, want %d", i, got[i], frames[i])
		}
	}
	// A different size means a different file, and therefore a different key.
	if _, _, ok := store.Get(Key("/media/show/s01e01.mkv", 9999)); ok {
		t.Error("a changed file size must not reuse the old fingerprint")
	}
}
