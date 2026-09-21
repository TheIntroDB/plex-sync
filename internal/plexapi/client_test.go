package plexapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/httpclient"
	"github.com/TheIntroDB/plex-sync/internal/model"
)

// ---------------------------------------------------------------------------
// fake Plex
// ---------------------------------------------------------------------------

const fakeToken = "test-token"

// fake is a tiny stand-in for Plex. It records the paths it saw and checks that
// every request carried the headers Plex needs.
type fake struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	token string
}

func newFake(t *testing.T, handler http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.token = r.Header.Get("X-Plex-Token")
		f.mu.Unlock()

		for _, h := range []string{
			"Accept", "X-Plex-Client-Identifier", "X-Plex-Product", "X-Plex-Version",
		} {
			if r.Header.Get(h) == "" {
				t.Errorf("request %s is missing header %s", r.URL.Path, h)
			}
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a client pointed at the fake, over the project's own HTTP layer.
func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	hc := httpclient.New(5*time.Second, false, UserAgent)
	t.Cleanup(hc.Close)
	return NewClient(config.Plex{URL: f.srv.URL, Token: fakeToken}, hc)
}

func (f *fake) saw(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if p == path {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ---------------------------------------------------------------------------
// bodies
// ---------------------------------------------------------------------------

const sectionsBody = `{"MediaContainer":{"size":3,"Directory":[
	{"key":"1","title":"Movies","type":"movie"},
	{"key":"2","title":"TV Shows","type":"show"},
	{"key":"3","title":"Music","type":"artist"}]}}`

// moviesBody mixes a string ratingKey with a numeric one, an item with three
// provider ids, an item with none, and a two-part item.
const moviesBody = `{"MediaContainer":{"size":2,"total":2,"Metadata":[
	{"ratingKey":"101","type":"movie","title":"Dune: Part Two","duration":166000000,
	 "addedAt":1700000000,"lastViewedAt":1700000100,
	 "Guid":[{"id":"imdb://tt15239678"},{"id":"tmdb://693134"},{"id":"tvdb://77"}],
	 "Media":[{"videoFrameRate":"23.976","Part":[{"duration":166500000,"file":"/media/dune.mkv"}]}]},
	{"ratingKey":102,"type":"movie","title":"No Identifiers","duration":5000000,
	 "Guid":[],
	 "Media":[{"videoFrameRate":"pal","Part":[
		 {"duration":5000000,"file":"/media/part1.mkv"},
		 {"duration":5000000,"file":"/media/part2.mkv"}]}]}]}}`

// episodesBody covers grandparentTitle (normal), a numeric grandparentRatingKey,
// and the legacy single-string "guid".
const episodesBody = `{"MediaContainer":{"size":3,"total":3,"Metadata":[
	{"ratingKey":"201","type":"episode","title":"Winter Is Coming","grandparentTitle":"Game of Thrones",
	 "parentIndex":1,"index":1,"duration":3600000,"grandparentRatingKey":"200","parentRatingKey":"2001",
	 "Guid":[{"id":"imdb://tt0944947"}]},
	{"ratingKey":"202","type":"episode","title":"The Kingsroad","grandparentTitle":"Game of Thrones",
	 "parentIndex":1,"index":2,"duration":3600000,"grandparentRatingKey":200,
	 "Guid":[{"id":"tvdb://121361"}]},
	{"ratingKey":"203","type":"episode","title":"Flattened","parentTitle":"Game of Thrones",
	 "parentIndex":2,"index":5,"duration":3600000,"grandparentRatingKey":200,
	 "guid":"tmdb://1399"}]}}`

// chaptersBody uses both spellings of every chapter field, plus one chapter that
// is not a real range and must be dropped.
const chaptersBody = `{"MediaContainer":{"Metadata":[{"ratingKey":301,"Media":[{"Part":[{"Chapter":[
	{"tag":"Intro","startTimeOffset":0,"endTimeOffset":52000},
	{"tag":"Chapter 2","startTime":52000,"endTime":90000},
	{"tag":"Broken","startTime":100,"endTime":100},
	{"title":"Only A Title","startTimeOffset":90000,"endTimeOffset":95000}]}]}]}]}}`

// chaptersPrecedenceBody has both name spellings on one chapter; tag wins.
const chaptersPrecedenceBody = `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"Chapter":[
	{"tag":"TagWins","title":"TitleLoses","startTimeOffset":10,"endTimeOffset":20}]}]}]}]}}`

const chaptersEmptyBody = `{"MediaContainer":{"Metadata":[{"ratingKey":303,"Media":[]}]}}`

const markersBody = `{"MediaContainer":{"size":2,"Marker":[
	{"id":55,"type":"intro","startTimeOffset":1000,"endTimeOffset":61000},
	{"id":"56","type":"credits","startTime":3000000,"endTime":3600000,"final":true}]}}`

// plexHandler serves the fixed fixture set above.
func plexHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/identity":
		writeJSON(w, http.StatusOK,
			`{"MediaContainer":{"machineIdentifier":"machine-1","version":"1.41.0"}}`)
	case r.URL.Path == "/library/sections":
		writeJSON(w, http.StatusOK, sectionsBody)
	case r.URL.Path == "/library/sections/1/all":
		if got := r.URL.Query().Get("type"); got != strconv.Itoa(metadataTypeMovie) {
			http.Error(w, "bad type "+got, http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("includeGuids") != "1" {
			http.Error(w, "missing includeGuids", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, moviesBody)
	case r.URL.Path == "/library/sections/2/all":
		if got := r.URL.Query().Get("type"); got != strconv.Itoa(metadataTypeEpisode) {
			http.Error(w, "bad type "+got, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, episodesBody)
	case r.URL.Path == "/status/sessions":
		writeJSON(w, http.StatusOK, `{"MediaContainer":{"size":2}}`)
	case strings.HasSuffix(r.URL.Path, "/markers"):
		switch r.URL.Path {
		case "/library/metadata/401/markers":
			writeJSON(w, http.StatusOK, markersBody)
		case "/library/metadata/402/markers":
			writeJSON(w, http.StatusNotFound, `{}`) // older PMS
		case "/library/metadata/403/markers":
			writeJSON(w, http.StatusBadRequest, `{}`) // no marker tag on the item
		default:
			writeJSON(w, http.StatusInternalServerError, `{"error":"boom"}`)
		}
	case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
		switch r.URL.Path {
		case "/library/metadata/301":
			if r.URL.Query().Get("includeChapters") != "1" {
				http.Error(w, "missing includeChapters", http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusOK, chaptersBody)
		case "/library/metadata/302":
			writeJSON(w, http.StatusOK, chaptersPrecedenceBody)
		case "/library/metadata/303":
			writeJSON(w, http.StatusOK, chaptersEmptyBody)
		default:
			writeJSON(w, http.StatusNotFound, `{}`)
		}
	default:
		writeJSON(w, http.StatusNotFound, `{}`)
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestIdentity(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)

	ctr, err := c.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if got := ctr["machineIdentifier"]; got != "machine-1" {
		t.Errorf("machineIdentifier = %v, want machine-1", got)
	}
	if got := ctr["version"]; got != "1.41.0" {
		t.Errorf("version = %v, want 1.41.0", got)
	}

	f.mu.Lock()
	token := f.token
	f.mu.Unlock()
	if token != fakeToken {
		t.Errorf("X-Plex-Token sent = %q, want %q", token, fakeToken)
	}
}

func TestRunning(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)

	running, err := c.Running(context.Background())
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !running {
		t.Error("Running = false, want true for a server that answered")
	}

	// A server that is gone must produce an error, not a quiet false: callers
	// fail closed.
	dead := newFake(t, plexHandler)
	deadClient := dead.client(t)
	dead.srv.Close()
	if _, err := deadClient.Running(context.Background()); err == nil {
		t.Error("Running on a closed server: want error, got nil")
	}
}

func TestSections(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)

	got, err := c.Sections(context.Background())
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	want := []Section{
		{Key: 1, Title: "Movies", Type: "movie"},
		{Key: 2, Title: "TV Shows", Type: "show"},
		{Key: 3, Title: "Music", Type: "artist"},
	}
	if len(got) != len(want) {
		t.Fatalf("Sections = %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Sections[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSectionsError(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, `{"error":"no token"}`)
	})
	c := f.client(t)

	_, err := c.Sections(context.Background())
	if err == nil {
		t.Fatal("Sections: want error on 401, got nil")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("Sections error = %v (%T), want *HTTPError", err, err)
	}
	if httpErr.Status != http.StatusUnauthorized {
		t.Errorf("HTTPError.Status = %d, want 401", httpErr.Status)
	}
	if !strings.Contains(httpErr.Error(), "401") {
		t.Errorf("HTTPError message %q does not mention the status", httpErr.Error())
	}
}

// asHTTPError is errors.As without the import noise in every test.
func asHTTPError(err error, dst **HTTPError) bool {
	for err != nil {
		if e, ok := err.(*HTTPError); ok {
			*dst = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestItems(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)

	items, err := c.Items(context.Background(), nil)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("Items = %d entries, want 5 (2 movies + 3 episodes)", len(items))
	}

	movieByKey := map[int]model.LibraryItem{}
	epByKey := map[int]model.LibraryItem{}
	for _, it := range items {
		switch it.Kind {
		case model.KindMovie:
			movieByKey[it.RatingKey] = it
		case model.KindEpisode:
			epByKey[it.RatingKey] = it
		default:
			t.Errorf("unexpected item kind %q", it.Kind)
		}
	}

	tmdb, imdb, tvdb := 693134, "tt15239678", 77
	t.Run("movie with ids and a lone part", func(t *testing.T) {
		it, ok := movieByKey[101]
		if !ok {
			t.Fatal("movie 101 missing")
		}
		if it.Title != "Dune: Part Two" {
			t.Errorf("Title = %q", it.Title)
		}
		if it.IDs.TMDB == nil || *it.IDs.TMDB != tmdb {
			t.Errorf("TMDB = %v, want %d", it.IDs.TMDB, tmdb)
		}
		if it.IDs.IMDb == nil || *it.IDs.IMDb != imdb {
			t.Errorf("IMDb = %v, want %s", it.IDs.IMDb, imdb)
		}
		if it.IDs.TVDB == nil || *it.IDs.TVDB != tvdb {
			t.Errorf("TVDB = %v, want %d", it.IDs.TVDB, tvdb)
		}
		if it.DurationMS == nil || *it.DurationMS != 166000000 {
			t.Errorf("DurationMS = %v", it.DurationMS)
		}
		if it.FileDurationMS == nil || *it.FileDurationMS != 166500000 {
			t.Errorf("FileDurationMS = %v, want the single part's 166500000", it.FileDurationMS)
		}
		if it.PartCount != 1 {
			t.Errorf("PartCount = %d, want 1", it.PartCount)
		}
		if it.FPS == nil || *it.FPS != 23.976 {
			t.Errorf("FPS = %v, want 23.976", it.FPS)
		}
		if it.AddedAt != 1700000000 || it.LastViewedAt != 1700000100 {
			t.Errorf("AddedAt/LastViewedAt = %d/%d", it.AddedAt, it.LastViewedAt)
		}
		if it.Season != nil || it.Episode != nil {
			t.Errorf("a movie must not carry season/episode, got %v/%v", it.Season, it.Episode)
		}
	})

	t.Run("movie with no ids and several parts", func(t *testing.T) {
		it, ok := movieByKey[102]
		if !ok {
			t.Fatal("movie 102 missing (a numeric ratingKey must parse too)")
		}
		if it.IDs.Any() {
			t.Errorf("IDs = %+v, want none", it.IDs)
		}
		if _, ok := it.LookupKey(); ok {
			t.Error("LookupKey must fail without an id")
		}
		if it.PartCount != 2 {
			t.Errorf("PartCount = %d, want 2", it.PartCount)
		}
		if it.FileDurationMS != nil {
			t.Errorf("FileDurationMS = %v, want nil for a multi-part item", *it.FileDurationMS)
		}
		if it.FPS == nil || *it.FPS != 25 {
			t.Errorf("FPS = %v, want 25 for \"pal\"", it.FPS)
		}
	})

	t.Run("episode with grandparentTitle", func(t *testing.T) {
		it, ok := epByKey[201]
		if !ok {
			t.Fatal("episode 201 missing")
		}
		if it.Title != "Winter Is Coming" || it.ShowTitle != "Game of Thrones" {
			t.Errorf("Title/ShowTitle = %q/%q", it.Title, it.ShowTitle)
		}
		if it.Season == nil || *it.Season != 1 || it.Episode == nil || *it.Episode != 1 {
			t.Errorf("Season/Episode = %v/%v, want 1/1", it.Season, it.Episode)
		}
		if it.ShowRatingKey != 200 {
			t.Errorf("ShowRatingKey = %d, want 200", it.ShowRatingKey)
		}
		if it.IDs.IMDb == nil || *it.IDs.IMDb != "tt0944947" {
			t.Errorf("IMDb = %v", it.IDs.IMDb)
		}
		if key, ok := it.LookupKey(); !ok || key != "imdb:tt0944947:1:1" {
			t.Errorf("LookupKey = %q, %v", key, ok)
		}
	})

	t.Run("episode with a numeric show key", func(t *testing.T) {
		it, ok := epByKey[202]
		if !ok {
			t.Fatal("episode 202 missing")
		}
		if it.IDs.TVDB == nil || *it.IDs.TVDB != 121361 {
			t.Errorf("TVDB = %v, want 121361", it.IDs.TVDB)
		}
		if it.ShowRatingKey != 200 {
			t.Errorf("ShowRatingKey = %d, want 200 from a numeric grandparentRatingKey", it.ShowRatingKey)
		}
	})

	t.Run("episode with the legacy guid string", func(t *testing.T) {
		it, ok := epByKey[203]
		if !ok {
			t.Fatal("episode 203 missing (a singular guid string must parse)")
		}
		if it.IDs.TMDB == nil || *it.IDs.TMDB != 1399 {
			t.Errorf("TMDB = %v, want 1399", it.IDs.TMDB)
		}
		if it.ShowTitle != "Game of Thrones" {
			t.Errorf("ShowTitle = %q, want the parentTitle fallback", it.ShowTitle)
		}
		if it.Season == nil || *it.Season != 2 || it.Episode == nil || *it.Episode != 5 {
			t.Errorf("Season/Episode = %v/%v, want 2/5", it.Season, it.Episode)
		}
		if it.FileDurationMS != nil {
			t.Errorf("FileDurationMS = %v, want nil when the item has no Media parts", *it.FileDurationMS)
		}
	})

	t.Run("section filter skips non-video sections", func(t *testing.T) {
		only, err := c.Items(context.Background(), []int{2})
		if err != nil {
			t.Fatalf("Items(section 2): %v", err)
		}
		if len(only) != 3 {
			t.Fatalf("Items(section 2) = %d entries, want 3", len(only))
		}
		for _, it := range only {
			if it.Kind != model.KindEpisode {
				t.Errorf("item %d kind = %q, want episode", it.RatingKey, it.Kind)
			}
		}

		none, err := c.Items(context.Background(), []int{3})
		if err != nil {
			t.Fatalf("Items(section 3): %v", err)
		}
		if len(none) != 0 {
			t.Errorf("Items(section 3) = %d entries, want 0 for a music section", len(none))
		}
		if !f.saw("/library/sections/3/all") {
			t.Log("note: music section was skipped before any request, as intended")
		}
	})
}

func TestItemsPaging(t *testing.T) {
	t.Parallel()
	const total = 5
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections":
			writeJSON(w, http.StatusOK,
				`{"MediaContainer":{"size":1,"Directory":[{"key":"7","title":"Movies","type":"movie"}]}}`)
		case "/library/sections/7/all":
			start, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Start"))
			size, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Size"))
			if size == 0 {
				size = 2 // the first answer is deliberately short
			}
			if start < 0 || start > total {
				start = total
			}
			end := start + size
			if end > total {
				end = total
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, `{"MediaContainer":{"size":%d,"total":%d,"Metadata":[`, end-start, total)
			for i := start; i < end; i++ {
				if i > start {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"ratingKey":%d,"type":"movie","title":"Movie %d"}`, 900+i, i)
			}
			sb.WriteString(`]}}`)
			writeJSON(w, http.StatusOK, sb.String())
		default:
			writeJSON(w, http.StatusNotFound, `{}`)
		}
	}
	f := newFake(t, handler)
	c := f.client(t)

	items, err := c.Items(context.Background(), nil)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != total {
		t.Fatalf("Items = %d entries, want %d across pages", len(items), total)
	}
	for i, it := range items {
		if it.RatingKey != 900+i {
			t.Errorf("items[%d].RatingKey = %d, want %d", i, it.RatingKey, 900+i)
		}
	}
}

func TestChapters(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)
	ctx := context.Background()

	t.Run("field-name variants", func(t *testing.T) {
		got, err := c.Chapters(ctx, 301)
		if err != nil {
			t.Fatalf("Chapters: %v", err)
		}
		want := []model.Chapter{
			{Name: "Intro", StartMS: 0, EndMS: 52000},
			{Name: "Chapter 2", StartMS: 52000, EndMS: 90000},
			{Name: "Only A Title", StartMS: 90000, EndMS: 95000},
		}
		if len(got) != len(want) {
			t.Fatalf("Chapters = %+v, want %d entries (the zero-length one is dropped)", got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Chapters[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("tag beats title", func(t *testing.T) {
		got, err := c.Chapters(ctx, 302)
		if err != nil {
			t.Fatalf("Chapters: %v", err)
		}
		if len(got) != 1 || got[0].Name != "TagWins" {
			t.Fatalf("Chapters = %+v, want one chapter named TagWins", got)
		}
		if got[0].LengthMS() != 10 {
			t.Errorf("LengthMS = %d, want 10", got[0].LengthMS())
		}
	})

	t.Run("no media means no chapters, no error", func(t *testing.T) {
		got, err := c.Chapters(ctx, 303)
		if err != nil {
			t.Fatalf("Chapters: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Chapters = %+v, want empty", got)
		}
	})

	t.Run("missing item is an error", func(t *testing.T) {
		if _, err := c.Chapters(ctx, 999); err == nil {
			t.Error("Chapters on a 404: want error, got nil")
		}
	})
}

func TestMarkers(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)
	ctx := context.Background()

	t.Run("markers are mapped with their index and origin", func(t *testing.T) {
		got, err := c.Markers(ctx, 401)
		if err != nil {
			t.Fatalf("Markers: %v", err)
		}
		want := []model.ExistingMarker{
			{TagID: 55, Text: "intro", StartMS: 1000, EndMS: 61000, Index: 0, Origin: "plex"},
			{TagID: 56, Text: "credits", StartMS: 3000000, EndMS: 3600000, Index: 1, Origin: "plex"},
		}
		if len(got) != len(want) {
			t.Fatalf("Markers = %+v, want %d entries", got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Markers[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
		if got[0].Key() != "intro:1000:61000" {
			t.Errorf("Key = %q", got[0].Key())
		}
	})

	for _, tc := range []struct {
		name   string
		key    int
		status int
	}{
		{name: "404 falls back to the database", key: 402, status: 404},
		{name: "400 falls back to the database", key: 403, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Markers(ctx, tc.key)
			if err != nil {
				t.Fatalf("Markers: %v, want no error on HTTP %d", err, tc.status)
			}
			if len(got) != 0 {
				t.Errorf("Markers = %+v, want an empty slice", got)
			}
			if got == nil {
				t.Error("Markers returned a nil slice, want an empty one")
			}
		})
	}

	t.Run("a real failure is still an error", func(t *testing.T) {
		if _, err := c.Markers(ctx, 500); err == nil {
			t.Error("Markers on a 500: want error, got nil")
		}
	})
}

func TestActiveSessions(t *testing.T) {
	t.Parallel()
	f := newFake(t, plexHandler)
	c := f.client(t)

	got, err := c.ActiveSessions(context.Background())
	if err != nil {
		t.Fatalf("ActiveSessions: %v", err)
	}
	if got != 2 {
		t.Errorf("ActiveSessions = %d, want 2", got)
	}

	// An empty container has no size field at all.
	empty := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"MediaContainer":{}}`)
	})
	got, err = empty.client(t).ActiveSessions(context.Background())
	if err != nil {
		t.Fatalf("ActiveSessions (empty): %v", err)
	}
	if got != 0 {
		t.Errorf("ActiveSessions (empty) = %d, want 0", got)
	}
}

func TestTokenFromPrefs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `<Preferences><Setting id="FriendlyName" value="plex"/>
  <Setting id="PlexOnlineToken" value="prefs-token-abc" />
  <Setting id="LastAutomaticMappedPort" value="0"/></Preferences>`
	if err := os.WriteFile(filepath.Join(dir, PreferencesFile), []byte(body), 0o600); err != nil {
		t.Fatalf("write Preferences.xml: %v", err)
	}
	if got := TokenFromPrefs(dir); got != "prefs-token-abc" {
		t.Errorf("TokenFromPrefs = %q, want prefs-token-abc", got)
	}

	missing := t.TempDir()
	if got := TokenFromPrefs(missing); got != "" {
		t.Errorf("TokenFromPrefs on a dir without Preferences.xml = %q, want \"\"", got)
	}
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, PreferencesFile),
		[]byte(`<Preferences><Setting id="FriendlyName" value="plex"/></Preferences>`), 0o600); err != nil {
		t.Fatalf("write Preferences.xml: %v", err)
	}
	if got := TokenFromPrefs(empty); got != "" {
		t.Errorf("TokenFromPrefs without a token = %q, want \"\"", got)
	}
}

func TestNewClientURLNormalisation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{in: "", want: DefaultURL},
		{in: "127.0.0.1:32400", want: "http://127.0.0.1:32400"},
		{in: "http://plex.local:32400/", want: "http://plex.local:32400"},
		{in: "https://plex.example.com", want: "https://plex.example.com"},
	} {
		c := NewClient(config.Plex{URL: tc.in, Token: "t"}, nil)
		if got := c.URL(); got != tc.want {
			t.Errorf("NewClient(%q).URL() = %q, want %q", tc.in, got, tc.want)
		}
		if c.identifier == "" {
			t.Errorf("NewClient(%q): empty client identifier", tc.in)
		}
		c.Close()
	}
}

func TestParseExternalIDs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		guids []string
		want  model.ExternalIDs
	}{
		{name: "tmdb only", guids: []string{"tmdb://1396"}, want: model.ExternalIDs{TMDB: ptr(1396)}},
		{name: "imdb only", guids: []string{"imdb://tt0944947"}, want: model.ExternalIDs{IMDb: ptr("tt0944947")}},
		{name: "tvdb only", guids: []string{"tvdb://121361"}, want: model.ExternalIDs{TVDB: ptr(121361)}},
		{name: "no ids", guids: nil, want: model.ExternalIDs{}},
		{name: "empty list", guids: []string{}, want: model.ExternalIDs{}},
		{
			name:  "all three, first of each wins",
			guids: []string{"tmdb://1", "imdb://tt1", "tvdb://2", "tmdb://999"},
			want:  model.ExternalIDs{TMDB: ptr(1), IMDb: ptr("tt1"), TVDB: ptr(2)},
		},
		{
			name:  "legacy agent form with a query string",
			guids: []string{"com.plexapp.agents.themoviedb://1396?lang=en"},
			want:  model.ExternalIDs{TMDB: ptr(1396)},
		},
		{
			name:  "unknown providers are ignored",
			guids: []string{"local://123", "com.plexapp.agents.none://x", "tvdb://9"},
			want:  model.ExternalIDs{TVDB: ptr(9)},
		},
		{
			name: "unparsable values are dropped", guids: []string{"tmdb://abc", "imdb://", ":", "://"},
			want: model.ExternalIDs{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseExternalIDs(tc.guids)
			if !sameIDs(got, tc.want) {
				t.Errorf("parseExternalIDs(%v) = %+v, want %+v", tc.guids, got, tc.want)
			}
		})
	}
}

func TestFlexInt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{name: "number", in: `{"v":42}`, want: 42},
		{name: "string", in: `{"v":"42"}`, want: 42},
		{name: "float", in: `{"v":42.0}`, want: 42},
		{name: "null", in: `{"v":null}`, want: 0},
		{name: "empty string", in: `{"v":""}`, want: 0},
		{name: "absent", in: `{}`, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body struct {
				V flexInt `json:"v"`
			}
			if err := json.Unmarshal([]byte(tc.in), &body); err != nil {
				t.Fatalf("decode %s: %v", tc.in, err)
			}
			if body.V.Int() != tc.want {
				t.Errorf("flexInt = %d, want %d", body.V.Int(), tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func sameIDs(a, b model.ExternalIDs) bool {
	return sameIntPtr(a.TMDB, b.TMDB) && sameIntPtr(a.TVDB, b.TVDB) && sameStrPtr(a.IMDb, b.IMDb)
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
