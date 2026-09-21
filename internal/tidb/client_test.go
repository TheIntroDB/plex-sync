package tidb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/httpclient"
	"github.com/TheIntroDB/plex-sync/internal/ledger"
	"github.com/TheIntroDB/plex-sync/internal/model"
)

// testNow is a fixed instant inside a UTC day, so the budget boundary and the
// TTLs are exact.
var testNow = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// fakeClock is an injectable clock that advances when the client "sleeps"
// instead of blocking, so pacing and backoff tests are instant and exact.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
	return nil
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeClock) Slept() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

func (f *fakeClock) TotalSlept() time.Duration {
	var total time.Duration
	for _, d := range f.Slept() {
		total += d
	}
	return total
}

type recordedCall struct {
	Path  string
	Query url.Values
	Auth  string
}

// testServer is an httptest server that counts and records what it was asked.
type testServer struct {
	*httptest.Server

	respond func(call int, w http.ResponseWriter, r *http.Request)

	mu    sync.Mutex
	calls int
	seen  []recordedCall
}

func newTestServer(t *testing.T, respond func(call int, w http.ResponseWriter, r *http.Request)) *testServer {
	t.Helper()
	ts := &testServer{respond: respond}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.calls++
		call := ts.calls
		ts.seen = append(ts.seen, recordedCall{
			Path:  r.URL.Path,
			Query: r.URL.Query(),
			Auth:  r.Header.Get("Authorization"),
		})
		ts.mu.Unlock()
		respond(call, w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) Count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.calls
}

func (ts *testServer) Last() recordedCall {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.seen) == 0 {
		return recordedCall{}
	}
	return ts.seen[len(ts.seen)-1]
}

func (ts *testServer) Call(i int) recordedCall {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i < 1 || i > len(ts.seen) {
		return recordedCall{}
	}
	return ts.seen[i-1]
}

// harness wires a client to a test server and a file-backed ledger.
type harness struct {
	client *Client
	server *testServer
	ledger *ledger.Ledger
	clock  *fakeClock
}

func newHarness(t *testing.T, respond func(call int, w http.ResponseWriter, r *http.Request), tune func(*config.TheIntroDB)) *harness {
	t.Helper()

	srv := newTestServer(t, respond)

	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })

	cfg := config.Default().TheIntroDB
	cfg.BaseURL = srv.URL
	if tune != nil {
		tune(&cfg)
	}

	hc := httpclient.New(5*time.Second, false, "plex-sync-test")
	t.Cleanup(hc.Close)

	clock := newFakeClock(testNow)
	client := NewClient(cfg, led, hc)
	client.SetClock(clock.Now)
	client.SetSleeper(clock.Sleep)

	return &harness{client: client, server: srv, ledger: led, clock: clock}
}

func js(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const sampleBody = `{"intro":{"start_ms":11300,"end_ms":74000},` +
	`"recap":null,` +
	`"credits":{"start_ms":null,"end_ms":1470000},` +
	`"preview":null}`

func movieItem(tmdb int, durationMS int64) model.LibraryItem {
	id, dur := tmdb, durationMS
	return model.LibraryItem{
		RatingKey:  tmdb,
		Kind:       model.KindMovie,
		Title:      fmt.Sprintf("Movie %d", tmdb),
		IDs:        model.ExternalIDs{TMDB: &id},
		DurationMS: &dur,
	}
}

func episodeItem(tmdb, season, episode int, durationMS int64) model.LibraryItem {
	id, se, ep, dur := tmdb, season, episode, durationMS
	return model.LibraryItem{
		RatingKey:  tmdb,
		Kind:       model.KindEpisode,
		Title:      fmt.Sprintf("Episode %d", episode),
		ShowTitle:  "Some Show",
		Season:     &se,
		Episode:    &ep,
		IDs:        model.ExternalIDs{TMDB: &id},
		DurationMS: &dur,
	}
}

func approx(t *testing.T, name string, got, want, tol time.Duration) {
	t.Helper()
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > tol {
		t.Errorf("%s = %s, want %s (±%s)", name, got, want, tol)
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func TestParseSegments(t *testing.T) {
	t.Run("single objects, an array and nulls", func(t *testing.T) {
		set, err := ParseSegments(`{"intro":{"start_ms":1000,"end_ms":45000},` +
			`"recap":null,` +
			`"credits":{"start_ms":null,"end_ms":null},` +
			`"preview":[{"start_ms":0,"end_ms":5000},{"start_ms":900000,"end_ms":950000}]}`)
		if err != nil {
			t.Fatalf("ParseSegments: %v", err)
		}
		if set.Source != model.SourceTheIntroDB {
			t.Errorf("Source = %q, want %q", set.Source, model.SourceTheIntroDB)
		}
		if len(set.Segments) != 4 {
			t.Fatalf("got %d segments, want 4: %+v", len(set.Segments), set.Segments)
		}
		intro := set.Of(model.SegmentIntro)
		if len(intro) != 1 || intro[0].StartMS == nil || *intro[0].StartMS != 1000 ||
			intro[0].EndMS == nil || *intro[0].EndMS != 45000 {
			t.Errorf("intro = %+v, want 1000-45000", intro)
		}
		if set.Has(model.SegmentRecap) {
			t.Error("a null recap produced segments")
		}
		credits := set.Of(model.SegmentCredits)
		if len(credits) != 1 {
			t.Fatalf("credits = %+v, want one", credits)
		}
		// Both bounds were null: they stay null, because a null start means
		// 0:00 and a null end means the end of the media. Neither is a zero.
		if credits[0].StartMS != nil || credits[0].EndMS != nil {
			t.Errorf("credits bounds = %v/%v, want both nil", credits[0].StartMS, credits[0].EndMS)
		}
		if got := set.Of(model.SegmentPreview); len(got) != 2 {
			t.Errorf("preview = %+v, want two segments from the array", got)
		}
		if types := set.Types(); len(types) != 3 {
			t.Errorf("Types() = %v, want intro, credits and preview", types)
		}
	})

	t.Run("null start resolves to zero against the file length", func(t *testing.T) {
		set, err := ParseSegments(`{"credits":{"start_ms":null,"end_ms":1470000}}`)
		if err != nil {
			t.Fatalf("ParseSegments: %v", err)
		}
		segs := set.Of(model.SegmentCredits)
		if len(segs) != 1 {
			t.Fatalf("credits = %+v", segs)
		}
		duration := int64(1470000)
		start, end, ok := segs[0].Resolve(&duration, true, true)
		if !ok || start != 0 || end != duration {
			t.Errorf("Resolve = %d-%d ok=%v, want 0-%d", start, end, ok, duration)
		}
	})

	t.Run("null end resolves to the file length", func(t *testing.T) {
		set, _ := ParseSegments(`{"intro":{"start_ms":11300,"end_ms":null}}`)
		segs := set.Of(model.SegmentIntro)
		duration := int64(2580000)
		start, end, ok := segs[0].Resolve(&duration, true, true)
		if !ok || start != 11300 || end != duration {
			t.Errorf("Resolve = %d-%d ok=%v, want 11300-%d", start, end, ok, duration)
		}
	})

	t.Run("absent and empty bodies mean no segments", func(t *testing.T) {
		for _, body := range []string{"", "   ", "null", "{}", `{"intro":null}`} {
			set, err := ParseSegments(body)
			if err != nil {
				t.Errorf("ParseSegments(%q): %v", body, err)
				continue
			}
			if len(set.Segments) != 0 {
				t.Errorf("ParseSegments(%q) = %+v, want no segments", body, set.Segments)
			}
		}
	})

	t.Run("malformed JSON is an error, not an empty answer", func(t *testing.T) {
		if _, err := ParseSegments(`{"intro":`); err == nil {
			t.Fatal("ParseSegments on malformed JSON returned no error")
		}
		if _, err := ParseSegments(`{"intro":"soon"}`); err == nil {
			t.Fatal("ParseSegments on a wrongly typed segment returned no error")
		}
	})

	t.Run("unknown keys and confidence are tolerated", func(t *testing.T) {
		set, err := ParseSegments(`{"intro":{"start_ms":1,"end_ms":2,"confidence":0.91,"who":"someone"},` +
			`"somethingelse":{"start_ms":1,"end_ms":2}}`)
		if err != nil {
			t.Fatalf("ParseSegments: %v", err)
		}
		segs := set.Of(model.SegmentIntro)
		if len(segs) != 1 {
			t.Fatalf("intro = %+v", segs)
		}
		if segs[0].Confidence == nil || *segs[0].Confidence != 0.91 {
			t.Errorf("Confidence = %v, want 0.91", segs[0].Confidence)
		}
	})
}

// ---------------------------------------------------------------------------
// Lookup
// ---------------------------------------------------------------------------

func TestLookupFreshThenCached(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, nil)

	item := movieItem(550, 1470000)

	set, res, err := h.client.Lookup(context.Background(), item)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Status != 200 || res.Cached || res.Reason != ReasonMiss {
		t.Errorf("first result = %+v, want a fresh 200 miss", res)
	}
	if !res.Data() || !res.OK() {
		t.Errorf("Data/OK = %v/%v, want both true", res.Data(), res.OK())
	}
	if res.Key != "tmdb:550:movie" {
		t.Errorf("Key = %q, want tmdb:550:movie", res.Key)
	}
	if res.Remaining != h.client.Config().DailyBudget-1 || !res.RemainingKnown {
		t.Errorf("Remaining = %d known=%v, want %d known", res.Remaining, res.RemainingKnown, h.client.Config().DailyBudget-1)
	}
	if !set.Has(model.SegmentIntro) || !set.Has(model.SegmentCredits) {
		t.Errorf("segments = %+v, want intro and credits", set.Segments)
	}
	if h.server.Count() != 1 {
		t.Fatalf("server saw %d requests, want 1", h.server.Count())
	}

	// The raw body is what was cached, so parsing can be redone later.
	cached, found := h.ledger.Lookup("tmdb:550:movie")
	if !found {
		t.Fatal("nothing was cached for the item")
	}
	if cached.Status != 200 || cached.Body != sampleBody || cached.Kind != "movie" {
		t.Errorf("cached = %+v, want the raw 200 body and kind movie", cached)
	}

	// The second lookup is answered from the ledger, with no network call.
	set2, res2, err := h.client.Lookup(context.Background(), item)
	if err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	if !res2.Cached || res2.Reason != ReasonHit || res2.Status != 200 {
		t.Errorf("second result = %+v, want a cached hit", res2)
	}
	if len(set2.Segments) != len(set.Segments) {
		t.Errorf("cached segments = %+v, want the same as the first answer", set2.Segments)
	}
	if h.server.Count() != 1 {
		t.Errorf("server saw %d requests, want 1: a fresh entry must not be re-fetched", h.server.Count())
	}

	usage := h.client.Usage()
	if usage.Requests != 1 || usage.Lookups != 2 || usage.CacheHits != 1 || usage.Data != 2 {
		t.Errorf("Usage = %+v, want 1 request, 2 lookups, 1 cache hit, 2 data", usage)
	}
	if usage.Today != 1 {
		t.Errorf("Usage.Today = %d, want 1", usage.Today)
	}
}

func TestLookup404IsCachedButExpires(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 404, `{"error":"no data"}`)
	}, func(cfg *config.TheIntroDB) { cfg.MissTTLDays = 14 })

	item := movieItem(999, 1000)

	set, res, err := h.client.Lookup(context.Background(), item)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Status != 404 || res.Reason != ReasonNoData || res.Cached {
		t.Errorf("first result = %+v, want a fresh 404 no-data", res)
	}
	if len(set.Segments) != 0 {
		t.Errorf("segments = %+v, want none: a miss is a miss", set.Segments)
	}
	if !res.OK() || res.Data() {
		t.Errorf("OK/Data = %v/%v, want true/false", res.OK(), res.Data())
	}
	if h.server.Count() != 1 {
		t.Fatalf("server saw %d requests, want 1", h.server.Count())
	}

	if _, res2, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("second Lookup: %v", err)
	} else if !res2.Cached || res2.Reason != ReasonNoData {
		t.Errorf("second result = %+v, want a cached no-data", res2)
	}
	if h.server.Count() != 1 {
		t.Errorf("a cached 404 made a network call")
	}

	// A 404 turns into a 200 the moment someone submits the timing, so it must
	// expire and be asked again.
	h.clock.Advance(15 * 24 * time.Hour)
	if _, res3, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("third Lookup: %v", err)
	} else if res3.Cached {
		t.Errorf("third result = %+v, want a fresh request after the miss TTL", res3)
	}
	if h.server.Count() != 2 {
		t.Errorf("server saw %d requests, want 2 after the miss TTL expired", h.server.Count())
	}
}

func TestLookup200UsesHitTTLNotMissTTL(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) {
		cfg.HitTTLDays = 30
		cfg.MissTTLDays = 14
	})

	item := movieItem(550, 1470000)
	if _, _, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// 15 days is past MissTTLDays but inside HitTTLDays: a 200 must still be
	// served from the ledger.
	h.clock.Advance(15 * 24 * time.Hour)
	if _, res, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("Lookup: %v", err)
	} else if !res.Cached {
		t.Errorf("result = %+v, want a cached hit at 15 days", res)
	}
	if h.server.Count() != 1 {
		t.Errorf("server saw %d requests, want 1", h.server.Count())
	}

	// 31 days is past HitTTLDays.
	h.clock.Advance(17 * 24 * time.Hour)
	if _, res, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("Lookup: %v", err)
	} else if res.Cached {
		t.Errorf("result = %+v, want a refresh past the hit TTL", res)
	}
	if h.server.Count() != 2 {
		t.Errorf("server saw %d requests, want 2", h.server.Count())
	}
}

func TestLookupCachedEntryDoesNotSpendBudget(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) { cfg.DailyBudget = 1 })

	item := movieItem(550, 1470000)
	if _, _, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// The budget is spent, but this answer is already cached, so it still works.
	if _, res, err := h.client.Lookup(context.Background(), item); err != nil {
		t.Fatalf("cached Lookup after the budget was spent: %v", err)
	} else if !res.Cached {
		t.Errorf("result = %+v, want a cached hit", res)
	}
}

func TestLookupBudgetRefusesTheRequest(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) { cfg.DailyBudget = 2 })

	for i := 0; i < 2; i++ {
		if _, _, err := h.client.Lookup(context.Background(), movieItem(100+i, 1000)); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}
	if n, err := h.ledger.RequestsToday(ledger.SourceTheIntroDB); err != nil || n != 2 {
		t.Fatalf("ledger counted %d requests today (err %v), want 2", n, err)
	}

	_, res, err := h.client.Lookup(context.Background(), movieItem(200, 1000))
	if err == nil {
		t.Fatal("the third lookup should have been refused by the budget")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != KindBudget {
		t.Fatalf("error = %v (%T), want an *Error of kind %s", err, err, KindBudget)
	}
	if apiErr.IsTerminal() {
		t.Error("a spent budget is not terminal for the run: it resets at midnight")
	}
	if !strings.Contains(apiErr.Error(), "budget") || !strings.Contains(apiErr.Error(), "00:00 UTC") {
		t.Errorf("budget error message = %q, want it to name the budget and the reset", apiErr.Error())
	}
	if res.Reason != ReasonBudget || res.Status != 0 {
		t.Errorf("result = %+v, want reason %s and no status", res, ReasonBudget)
	}
	if res.RemainingKnown && res.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", res.Remaining)
	}
	if h.server.Count() != 2 {
		t.Errorf("server saw %d requests, want 2: the refused one must not be sent", h.server.Count())
	}
	if n, _ := h.ledger.RequestsToday(ledger.SourceTheIntroDB); n != 2 {
		t.Errorf("ledger counted %d requests, want 2: a refused request is not a request", n)
	}

	// The next UTC day resets the allowance.
	h.clock.Advance(12 * time.Hour)
	if _, _, err := h.client.Lookup(context.Background(), movieItem(200, 1000)); err != nil {
		t.Fatalf("Lookup after UTC midnight: %v", err)
	}
	if h.server.Count() != 3 {
		t.Errorf("server saw %d requests, want 3 after the day rolled over", h.server.Count())
	}
}

func TestLookupBudgetCountsAgentIndependentRequests(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) { cfg.DailyBudget = 3 })

	// Two requests were already made today by an earlier run.
	h.clock.Advance(3 * time.Hour)
	for i := 0; i < 2; i++ {
		if err := h.ledger.RecordRequest(ledger.SourceTheIntroDB); err != nil {
			t.Fatalf("RecordRequest: %v", err)
		}
	}

	if _, _, err := h.client.Lookup(context.Background(), movieItem(1, 1000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, _, err := h.client.Lookup(context.Background(), movieItem(2, 1000)); err == nil {
		t.Fatal("the budget should count requests an earlier run made today")
	}
	if h.server.Count() != 1 {
		t.Errorf("server saw %d requests, want 1", h.server.Count())
	}
}

// ---------------------------------------------------------------------------
// Pacing and backoff
// ---------------------------------------------------------------------------

func TestLookupPacesRequests(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) {
		cfg.MaxPerWindow = 5
		cfg.WindowS = 10 // MinDelay 2s
	})
	if got := h.client.Config().MinDelay(); got != 2 {
		t.Fatalf("MinDelay = %v, want 2", got)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := h.client.Lookup(context.Background(), movieItem(i+1, 1000)); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}
	if h.server.Count() != 3 {
		t.Fatalf("server saw %d requests, want 3", h.server.Count())
	}
	slept := h.clock.Slept()
	if len(slept) != 2 {
		t.Fatalf("slept %v, want two waits between three requests", slept)
	}
	for i, d := range slept {
		approx(t, fmt.Sprintf("wait %d", i), d, 2*time.Second, 10*time.Millisecond)
	}
}

func TestConcurrentLookupsDoNotBurst(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) {
		cfg.MaxPerWindow = 5
		cfg.WindowS = 10 // MinDelay 2s
	})

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := h.client.Lookup(context.Background(), movieItem(1000+i, 1000))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Lookup %d: %v", i, err)
		}
	}
	if h.server.Count() != n {
		t.Fatalf("server saw %d requests, want %d", h.server.Count(), n)
	}
	// Five requests need four gaps of at least MinDelay between them. A burst
	// would produce far less.
	if total := h.clock.TotalSlept(); total < (n-1)*2*time.Second {
		t.Errorf("total wait = %s, want at least %s of spacing", total, (n-1)*2*time.Second)
	}
}

func TestRateLimitBackoffGrowsAndIsCappedAtFiveMinutes(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		js(w, 429, "slow down") // a rate-limit 429 has a non-JSON body
	}, func(cfg *config.TheIntroDB) {
		cfg.MaxPerWindow = 25
		cfg.WindowS = 10 // MinDelay 0.4s
	})

	want := []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second}
	for i, wantWait := range want {
		_, res, err := h.client.Lookup(context.Background(), movieItem(i+1, 1000))
		if err == nil {
			t.Fatalf("lookup %d: expected a rate-limit error", i)
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Kind != KindRateLimit {
			t.Fatalf("lookup %d: error = %v, want a rate-limit *Error", i, err)
		}
		if res.Reason != ReasonRateLimit || res.Status != 429 {
			t.Errorf("lookup %d: result = %+v, want a 429 rate-limited", i, res)
		}
		approx(t, fmt.Sprintf("lookup %d RetryAfter", i), res.RetryAfter, wantWait, time.Millisecond)
		if i == 0 && apiErr.IsTerminal() {
			t.Error("a rate limit is not terminal, it clears in seconds")
		}
	}

	// The waits between the requests grew as the 429s piled up.
	slept := h.clock.Slept()
	if len(slept) != 3 {
		t.Fatalf("slept %v, want three waits before requests 2, 3 and 4", slept)
	}
	approx(t, "wait before request 2", slept[0], 60*time.Second, time.Millisecond)
	approx(t, "wait before request 3", slept[1], 120*time.Second, time.Millisecond)
	approx(t, "wait before request 4", slept[2], 240*time.Second, time.Millisecond)

	if got := h.client.Usage().Consecutive429; got != 4 {
		t.Errorf("Consecutive429 = %d, want 4", got)
	}
}

func TestUsageLimitWaitIsNotClampedToFiveMinutes(t *testing.T) {
	// The critical case: an exhausted daily allowance reports a reset of hours.
	// Clamping that to five minutes makes a scan wait, send one request, get
	// another 429 and wait again, so the whole library is skipped.
	const reset = 7200 // seconds, two hours

	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-UsageLimit-Reset", fmt.Sprint(reset))
		w.Header().Set("X-UsageLimit-Limit", "1000")
		w.Header().Set("X-UsageLimit-Remaining", "0")
		js(w, 429, `{"error":"daily limit reached","retry_after":"2 hours","code":"usage_limit_exceeded"}`)
	}, nil)

	_, res, err := h.client.Lookup(context.Background(), movieItem(1, 1000))
	if err == nil {
		t.Fatal("expected a usage-limit error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != KindUsageLimit {
		t.Fatalf("error = %v, want a usage-limit *Error", err)
	}
	if res.Reason != ReasonUsageLimit || res.Status != 429 {
		t.Errorf("result = %+v, want a 429 usage-limited", res)
	}
	approx(t, "RetryAfter", res.RetryAfter, 2*time.Hour, time.Second)
	if res.RetryAfter <= maxRateWait {
		t.Fatalf("RetryAfter = %s: a daily reset was clamped to the rate-limit ceiling", res.RetryAfter)
	}
	if hold := h.client.Usage().HoldFor; hold <= maxRateWait {
		t.Errorf("HoldFor = %s, want the full two hours", hold)
	}

	// The next send really does wait the full reset, not five minutes.
	_, _, err = h.client.Lookup(context.Background(), movieItem(2, 1000))
	if err == nil {
		t.Fatal("expected the second lookup to be rate limited too")
	}
	slept := h.clock.Slept()
	if len(slept) == 0 {
		t.Fatal("the second request did not wait at all")
	}
	if slept[0] <= maxRateWait {
		t.Errorf("the wait before the second request was %s, want about 2h", slept[0])
	}
	approx(t, "wait before request 2", slept[0], 2*time.Hour, time.Second)
}

func TestUsageLimitDetection(t *testing.T) {
	cases := []struct {
		name     string
		headers  map[string]string
		body     string
		limited  bool
		waitWant time.Duration
	}{
		{
			name:    "body code",
			body:    `{"error":"quota spent","retry_after":"3 hours","code":"usage_limit_exceeded"}`,
			limited: true, waitWant: 3 * time.Hour,
		},
		{
			name:    "specific media code",
			body:    `{"error":"quota spent","code":"specific_media_usage_limit_exceeded"}`,
			limited: true, waitWant: usageWaitDefault,
		},
		{
			name:    "reset header above the rate-limit floor",
			headers: map[string]string{"x-usagelimit-reset": "3000"},
			body:    "not json",
			limited: true, waitWant: 3000 * time.Second,
		},
		{
			name:    "reset header says 0, which is useless for pacing",
			headers: map[string]string{"x-usagelimit-reset": "0"},
			body:    `{"error":"slow down","code":""}`,
			limited: false, waitWant: 0,
		},
		{
			name:    "reset header just under the floor is a rate limit",
			headers: map[string]string{"x-usagelimit-reset": "300", "retry-after": "10"},
			body:    "slow down",
			limited: false, waitWant: 10 * time.Second,
		},
		{
			name:    "plain rate limit",
			headers: map[string]string{"retry-after": "45"},
			body:    "slow down",
			limited: false, waitWant: 45 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &httpclient.Response{
				Status:  429,
				Headers: map[string]string{},
				Body:    []byte(tc.body),
			}
			for k, v := range tc.headers {
				resp.Headers[strings.ToLower(k)] = v
			}
			if got := usageLimited(resp); got != tc.limited {
				t.Errorf("usageLimited = %v, want %v", got, tc.limited)
			}
			if tc.limited {
				if got := usageReset(resp); got != tc.waitWant {
					t.Errorf("usageReset = %s, want %s", got, tc.waitWant)
				}
				return
			}
			if got := retryAfter(resp); got != tc.waitWant {
				t.Errorf("retryAfter = %s, want %s", got, tc.waitWant)
			}
		})
	}
}

func TestUsageLimitWaitIsCappedAtADay(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		// A nonsense reset: trust it, but only up to a day.
		w.Header().Set("X-UsageLimit-Reset", "200000")
		js(w, 429, `{"code":"usage_limit_exceeded"}`)
	}, nil)

	if _, _, err := h.client.Lookup(context.Background(), movieItem(1, 1000)); err == nil {
		t.Fatal("expected a usage-limit error")
	}
	if hold := h.client.Usage().HoldFor; hold != maxUsageWait {
		t.Errorf("HoldFor = %s, want it capped at %s", hold, maxUsageWait)
	}
}

func TestLowRateLimitRemainingHoldsTheNextSend(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) {
		cfg.MaxPerWindow = 5
		cfg.WindowS = 10 // MinDelay 2s
	})

	if _, _, err := h.client.Lookup(context.Background(), movieItem(1, 1000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// The window is nearly spent, so the client holds its next send a slot.
	if hold := h.client.Usage().HoldFor; hold != 2*time.Second {
		t.Errorf("HoldFor = %s, want one pacing slot of 2s", hold)
	}
}

func TestSuccessfulResponsesDoNotHold(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-UsageLimit-Reset", "0")
		w.Header().Set("X-UsageLimit-Remaining", "997")
		js(w, 200, sampleBody)
	}, nil)

	if _, _, err := h.client.Lookup(context.Background(), movieItem(1, 1000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	usage := h.client.Usage()
	if usage.HoldFor != 0 {
		t.Errorf("HoldFor = %s, want none after a 200", usage.HoldFor)
	}
	if !usage.RemainingKnown || usage.Remaining != 997 {
		t.Errorf("Remaining = %d known=%v, want 997 known from the header", usage.Remaining, usage.RemainingKnown)
	}
}

// ---------------------------------------------------------------------------
// Keys, auth and statuses
// ---------------------------------------------------------------------------

func TestLookupQueryAndAuthHeader(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, func(cfg *config.TheIntroDB) { cfg.APIKey = "secret-key" })

	if _, _, err := h.client.Lookup(context.Background(), episodeItem(1396, 2, 7, 2580000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	call := h.server.Last()
	if call.Path != mediaPath {
		t.Errorf("path = %q, want %q", call.Path, mediaPath)
	}
	if call.Auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want a bearer key", call.Auth)
	}
	wantQuery := map[string]string{
		"tmdb_id":     "1396",
		"season":      "2",
		"episode":     "7",
		"duration_ms": "2580000",
	}
	for k, want := range wantQuery {
		if got := call.Query.Get(k); got != want {
			t.Errorf("query %s = %q, want %q (query %v)", k, got, want, call.Query)
		}
	}
	if call.Query.Get("imdb_id") != "" || call.Query.Get("tvdb_id") != "" {
		t.Errorf("query carried a fallback id as well: %v", call.Query)
	}

	// A movie sends no season or episode.
	if _, _, err := h.client.Lookup(context.Background(), movieItem(550, 1470000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	movie := h.server.Last().Query
	if movie.Get("season") != "" || movie.Get("episode") != "" {
		t.Errorf("a movie query carried season/episode: %v", movie)
	}
	if movie.Get("duration_ms") != "1470000" {
		t.Errorf("movie duration_ms = %q, want 1470000", movie.Get("duration_ms"))
	}
}

func TestLookupFallsBackToIMDbAndTVDB(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, sampleBody)
	}, nil)

	imdb := "tt0137523"
	imdbItem := model.LibraryItem{
		RatingKey: 1, Kind: model.KindMovie, Title: "Fight Club",
		IDs: model.ExternalIDs{IMDb: &imdb},
	}
	if _, res, err := h.client.Lookup(context.Background(), imdbItem); err != nil {
		t.Fatalf("Lookup: %v", err)
	} else if res.Key != "imdb:tt0137523:movie" {
		t.Errorf("Key = %q, want imdb:tt0137523:movie", res.Key)
	}
	if got := h.server.Last().Query.Get("imdb_id"); got != imdb {
		t.Errorf("imdb_id = %q, want %q", got, imdb)
	}

	tvdb := 81189
	tvdbItem := model.LibraryItem{
		RatingKey: 2, Kind: model.KindMovie, Title: "Breaking Bad",
		IDs: model.ExternalIDs{TVDB: &tvdb},
	}
	if _, res, err := h.client.Lookup(context.Background(), tvdbItem); err != nil {
		t.Fatalf("Lookup: %v", err)
	} else if res.Key != "tvdb:81189:movie" {
		t.Errorf("Key = %q, want tvdb:81189:movie", res.Key)
	}
	if got := h.server.Last().Query.Get("tvdb_id"); got != "81189" {
		t.Errorf("tvdb_id = %q, want 81189", got)
	}

	// TMDb wins when several ids are present, matching the cache key.
	both := model.LibraryItem{
		RatingKey: 3, Kind: model.KindMovie, Title: "Both",
		IDs: model.ExternalIDs{TMDB: ptr(550), IMDb: &imdb},
	}
	if _, res, err := h.client.Lookup(context.Background(), both); err != nil {
		t.Fatalf("Lookup: %v", err)
	} else if res.Key != "tmdb:550:movie" {
		t.Errorf("Key = %q, want tmdb:550:movie", res.Key)
	}
}

func ptr[T any](v T) *T { return &v }

func TestLookupWithoutAnIDNeverCallsTheAPI(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		t.Error("the API was called for an item with no ids")
		js(w, 200, sampleBody)
	}, nil)

	item := model.LibraryItem{RatingKey: 5, Kind: model.KindMovie, Title: "Unknown"}
	set, res, err := h.client.Lookup(context.Background(), item)
	if err == nil {
		t.Fatal("expected an error for an item with no ids")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != KindNoIDs {
		t.Fatalf("error = %v, want a no-ids *Error", err)
	}
	if !apiErr.IsTerminal() || !IsTerminal(err) {
		t.Error("an item with no ids is terminal: it can never be looked up")
	}
	if res.Reason != ReasonError {
		t.Errorf("result = %+v, want reason %s", res, ReasonError)
	}
	if len(set.Segments) != 0 {
		t.Errorf("segments = %+v, want none", set.Segments)
	}
	if h.server.Count() != 0 {
		t.Errorf("server saw %d requests, want 0", h.server.Count())
	}
}

func TestLookup401IsTerminalAndNotCached(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 401, `{"error":"invalid api key"}`)
	}, nil)

	_, res, err := h.client.Lookup(context.Background(), movieItem(1, 1000))
	if err == nil {
		t.Fatal("expected a 401 to come back as an error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth {
		t.Fatalf("error = %v (%T), want an auth *Error", err, err)
	}
	if !apiErr.IsTerminal() || !IsTerminal(err) {
		t.Error("a rejected key is terminal for the run")
	}
	if !strings.Contains(apiErr.Error(), "invalid api key") {
		t.Errorf("error message = %q, want it to carry the server's reason", apiErr.Error())
	}
	if res.Status != 401 || res.Reason != ReasonError {
		t.Errorf("result = %+v, want a 401 error", res)
	}
	if _, found := h.ledger.Lookup("tmdb:1:movie"); found {
		t.Error("a 401 must not be cached as if it were an answer")
	}
	if h.server.Count() != 1 {
		t.Errorf("server saw %d requests, want 1", h.server.Count())
	}

	// 403 is the same story.
	h2 := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 403, `{"error":"key rejected"}`)
	}, nil)
	if _, _, err := h2.client.Lookup(context.Background(), movieItem(2, 1000)); err == nil || !IsTerminal(err) {
		t.Errorf("403 error = %v, want a terminal auth failure", err)
	}
}

func TestLookupUnexpectedStatusIsAnError(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 500, `{"error":"boom"}`)
	}, nil)

	_, res, err := h.client.Lookup(context.Background(), movieItem(1, 1000))
	if err == nil {
		t.Fatal("expected a 500 to come back as an error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != KindStatus || apiErr.Status != 500 {
		t.Errorf("error = %v, want a status *Error carrying 500", err)
	}
	if res.Reason != ReasonError || res.Status != 500 {
		t.Errorf("result = %+v, want a 500 error", res)
	}
	if _, found := h.ledger.Lookup("tmdb:1:movie"); found {
		t.Error("a 500 must not be cached")
	}
}

func TestLookupMalformedBodyFailsWithoutBurningTheAnswer(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 200, `{"intro":{"start_ms":`)
	}, nil)

	if _, _, err := h.client.Lookup(context.Background(), movieItem(1, 1000)); err == nil {
		t.Fatal("expected a malformed body to be an error")
	}
	if _, found := h.ledger.Lookup("tmdb:1:movie"); found {
		t.Error("an unparseable body must not be cached")
	}
	if got := h.client.Usage().Errors; got != 1 {
		t.Errorf("Errors = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// User stats and key validation
// ---------------------------------------------------------------------------

func TestUserStats(t *testing.T) {
	body := `{"total":42,"accepted":40,"pending":1,"rejected":1,"acceptance_rate":0.95,` +
		`"current_streak":3,"best_streak":9,"total_time_saved_ms":123456,"top_media":[]}`
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != statsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, statsPath)
		}
		js(w, 200, body)
	}, func(cfg *config.TheIntroDB) { cfg.APIKey = "secret-key" })

	stats, err := h.client.UserStats(context.Background())
	if err != nil {
		t.Fatalf("UserStats: %v", err)
	}
	if stats["total"] != float64(42) {
		t.Errorf("total = %v, want 42", stats["total"])
	}
	if got := h.server.Last().Auth; got != "Bearer secret-key" {
		t.Errorf("Authorization = %q", got)
	}

	if err := h.client.ValidateKey(context.Background()); err != nil {
		t.Errorf("ValidateKey with a good key: %v", err)
	}
}

func TestValidateKeyWithoutAKey(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 401, `{"error":"missing key"}`)
	}, nil)

	err := h.client.ValidateKey(context.Background())
	if err == nil {
		t.Fatal("ValidateKey with no key configured should fail")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth {
		t.Fatalf("error = %v, want an auth *Error", err)
	}
	if h.server.Count() != 0 {
		t.Errorf("server saw %d requests, want 0: there was no key to check", h.server.Count())
	}
}

func TestUserStats401(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		js(w, 401, `{"error":"invalid api key"}`)
	}, func(cfg *config.TheIntroDB) { cfg.APIKey = "bad" })

	_, err := h.client.UserStats(context.Background())
	if err == nil || !IsTerminal(err) {
		t.Fatalf("UserStats error = %v, want a terminal failure", err)
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %q, want the server's reason", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Usage accounting
// ---------------------------------------------------------------------------

func TestUsageCountsEverything(t *testing.T) {
	h := newHarness(t, func(call int, w http.ResponseWriter, r *http.Request) {
		switch {
		case call == 1:
			js(w, 200, sampleBody)
		case call == 2:
			js(w, 404, `{"error":"no data"}`)
		default:
			js(w, 401, `{"error":"nope"}`)
		}
	}, nil)

	ctx := context.Background()
	if _, _, err := h.client.Lookup(ctx, movieItem(1, 1000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, _, err := h.client.Lookup(ctx, movieItem(2, 1000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// A cached answer, which spends nothing.
	if _, _, err := h.client.Lookup(ctx, movieItem(1, 1000)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, _, err := h.client.Lookup(ctx, movieItem(3, 1000)); err == nil {
		t.Fatal("expected the 401 to fail")
	}

	usage := h.client.Usage()
	if usage.Requests != 3 {
		t.Errorf("Requests = %d, want 3", usage.Requests)
	}
	if usage.Lookups != 4 {
		t.Errorf("Lookups = %d, want 4", usage.Lookups)
	}
	if usage.CacheHits != 1 {
		t.Errorf("CacheHits = %d, want 1", usage.CacheHits)
	}
	if usage.Data != 2 {
		t.Errorf("Data = %d, want 2 (one fresh, one cached)", usage.Data)
	}
	if usage.NoData != 1 {
		t.Errorf("NoData = %d, want 1", usage.NoData)
	}
	if usage.Errors != 1 {
		t.Errorf("Errors = %d, want 1", usage.Errors)
	}
	if usage.Today != 3 {
		t.Errorf("Today = %d, want 3", usage.Today)
	}
	if usage.LastStatus != 401 {
		t.Errorf("LastStatus = %d, want 401", usage.LastStatus)
	}
	if usage.Budget != h.client.Config().DailyBudget {
		t.Errorf("Budget = %d, want %d", usage.Budget, h.client.Config().DailyBudget)
	}
}

// ---------------------------------------------------------------------------
// Small pieces
// ---------------------------------------------------------------------------

func TestParseDelay(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3 hours", 3 * time.Hour, true},
		{"1 hour", time.Hour, true},
		{"2 hours 5 minutes", 2*time.Hour + 5*time.Minute, true},
		{"45 minutes", 45 * time.Minute, true},
		{"120 seconds", 120 * time.Second, true},
		{"90", 90 * time.Second, true},
		{"2h", 2 * time.Hour, true},
		{"", 0, false},
		{"soon", 0, false},
		{"00:00", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseDelay(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseDelay(%q) = %s, %v; want %s, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSleepContext(t *testing.T) {
	if err := SleepContext(context.Background(), 0); err != nil {
		t.Errorf("SleepContext(0) = %v, want nil", err)
	}
	if err := SleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("SleepContext(1ms) = %v, want nil", err)
	}
	if err := SleepContext(nil, time.Millisecond); err != nil {
		t.Errorf("SleepContext(nil ctx) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("SleepContext(cancelled) = %v, want context.Canceled", err)
	}
}

func TestNewClientDefaults(t *testing.T) {
	// A zero-value config still produces a usable client, with the pacing
	// default rather than a division by zero.
	client := NewClient(config.TheIntroDB{}, nil, nil)
	if client.minDelay() <= 0 {
		t.Errorf("minDelay = %s, want the built-in default", client.minDelay())
	}
	if client.clock == nil || client.sleep == nil {
		t.Error("a client built from a zero config must still have a clock and a sleeper")
	}
	if client.Usage().RemainingKnown {
		t.Error("with no ledger and no header there is no remaining figure to report")
	}
	// A config with a window and a cap uses them.
	client2 := NewClient(config.TheIntroDB{MaxPerWindow: 5, WindowS: 10}, nil, nil)
	if got := client2.minDelay(); got != 2*time.Second {
		t.Errorf("minDelay = %s, want 2s", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if IsNotFound(&Error{Kind: KindStatus, Status: 404}) != true {
		t.Error("a 404 should read as not found")
	}
	if IsNotFound(&Error{Kind: KindStatus, Status: 500}) {
		t.Error("a 500 is not a 404")
	}
	if IsNotFound(errors.New("boom")) {
		t.Error("a plain error is not a 404")
	}
}

// ---------------------------------------------------------------------------
// Live smoke test
// ---------------------------------------------------------------------------

// TestLiveLookup is the only test that talks to the real API. It is skipped
// unless TIDB_LIVE=1, so an ordinary `go test ./...` never reaches the network.
func TestLiveLookup(t *testing.T) {
	if os.Getenv("TIDB_LIVE") != "1" {
		t.Skip("live API test: set TIDB_LIVE=1 to run it")
	}

	cfg := config.Default().TheIntroDB
	if v := strings.TrimSpace(os.Getenv("TIDB_API_URL")); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("TIDB_API_KEY")); v != "" {
		cfg.APIKey = v
	}

	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()

	hc := httpclient.New(20*time.Second, false, DefaultUserAgent)
	defer hc.Close()

	client := NewClient(cfg, led, hc)
	ctx := context.Background()

	if cfg.APIKey != "" {
		if err := client.ValidateKey(ctx); err != nil {
			t.Fatalf("ValidateKey: %v", err)
		}
		stats, err := client.UserStats(ctx)
		if err != nil {
			t.Fatalf("UserStats: %v", err)
		}
		t.Logf("user stats: total=%v accepted=%v pending=%v",
			stats["total"], stats["accepted"], stats["pending"])
	}

	// TMDb 550 is Fight Club; the answer may legitimately be a 404.
	item := movieItem(550, 139*60*1000)
	set, res, err := client.Lookup(ctx, item)
	if err != nil {
		if IsNotFound(err) {
			t.Logf("no data for %s, which is a valid answer", item.Title)
			return
		}
		t.Fatalf("Lookup: %v", err)
	}
	t.Logf("result: status=%d cached=%v reason=%s remaining=%d(%v)",
		res.Status, res.Cached, res.Reason, res.Remaining, res.RemainingKnown)
	for _, seg := range set.Segments {
		t.Logf("segment %s start=%v end=%v", seg.Type, seg.StartMS, seg.EndMS)
	}
}
