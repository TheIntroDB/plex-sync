package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/TheIntroDB/plex-integration/internal/model"
)

// fakeNow is a fixed instant inside a UTC day, so day boundaries and TTLs are
// exact rather than approximately right.
var fakeNow = time.Date(2026, time.March, 1, 12, 30, 0, 0, time.UTC)

func openTest(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "state", "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	l.SetClock(func() time.Time { return fakeNow })
	return l
}

func TestOpenCreatesSchema(t *testing.T) {
	l := openTest(t)

	rows, err := l.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	for _, want := range []string{"applied", "lookups", "requests", "runs", "schema_version"} {
		if !got[want] {
			t.Errorf("table %q missing, have %v", want, got)
		}
	}

	var version int
	if err := l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("schema version = %d, want %d", version, schemaVersion)
	}

	// WAL is what lets a second process read while a sync writes.
	var mode string
	if err := l.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("Open(\"\") = nil error, want one")
	}
}

func TestStartOfUTCDay(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 1, 23, 59, 59, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		// 06:30 MDT on March 1 is 13:30 UTC, so the UTC day is March 1.
		{time.Date(2026, 3, 1, 6, 30, 0, 0, time.FixedZone("MDT", -7*3600)), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		// 20:00 MDT on March 1 is 03:00 UTC on March 2.
		{time.Date(2026, 3, 1, 20, 0, 0, 0, time.FixedZone("MDT", -7*3600)), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := StartOfUTCDay(tc.in); got != tc.want.Unix() {
			t.Errorf("StartOfUTCDay(%s) = %s, want %s",
				tc.in, time.Unix(got, 0).UTC(), tc.want.UTC())
		}
	}
}

func TestLookupRoundTripAndFreshness(t *testing.T) {
	l := openTest(t)

	key := "tmdb:1396:1:1"
	if _, found := l.Lookup(key); found {
		t.Fatal("Lookup on an empty ledger reported a hit")
	}

	body := `{"intro":{"start_ms":100,"end_ms":200}}`
	if err := l.PutLookup(key, 200, body, int64(30*24*time.Hour/time.Second), "episode"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}

	got, found := l.Lookup(key)
	if !found {
		t.Fatal("Lookup missed a stored entry")
	}
	if got.Status != 200 || got.Body != body || got.Kind != "episode" {
		t.Errorf("CachedLookup = %+v, want status 200, the stored body and kind episode", got)
	}
	if !got.FetchedAt.Equal(fakeNow) {
		t.Errorf("FetchedAt = %s, want %s", got.FetchedAt, fakeNow)
	}
	if want := fakeNow.Add(30 * 24 * time.Hour); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", got.ExpiresAt, want)
	}
	if !got.Fresh(fakeNow) {
		t.Error("entry is not fresh right after it was stored")
	}
	if got.Fresh(fakeNow.Add(29*24*time.Hour+23*time.Hour)) == false {
		t.Error("entry is not fresh one hour before its TTL")
	}
	if got.Fresh(fakeNow.Add(31 * 24 * time.Hour)) {
		t.Error("entry is still fresh a day past its TTL")
	}
	if age := got.Age(fakeNow.Add(time.Hour)); age != time.Hour {
		t.Errorf("Age = %s, want 1h", age)
	}

	// Replacing an answer replaces its body and its clock.
	if err := l.PutLookup(key, 404, "", int64(14*24*time.Hour/time.Second), "episode"); err != nil {
		t.Fatalf("PutLookup (replace): %v", err)
	}
	replaced, _ := l.Lookup(key)
	if replaced.Status != 404 || replaced.Body != "" {
		t.Errorf("replaced entry = %+v, want status 404 with an empty body", replaced)
	}
}

func TestLookupZeroTTLIsNeverFresh(t *testing.T) {
	l := openTest(t)
	if err := l.PutLookup("tmdb:1:movie", 200, `{"intro":null}`, 0, "movie"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}
	got, found := l.Lookup("tmdb:1:movie")
	if !found {
		t.Fatal("entry not stored")
	}
	if got.Fresh(fakeNow) {
		t.Error("a zero TTL entry must not be fresh")
	}
}

func TestLookupEmptyKey(t *testing.T) {
	l := openTest(t)
	if _, found := l.Lookup(""); found {
		t.Error("Lookup(\"\") reported a hit")
	}
	if err := l.PutLookup("", 200, "{}", 60, "movie"); err == nil {
		t.Error("PutLookup with an empty key should fail")
	}
}

func TestForgetLookupAndPurgeExpired(t *testing.T) {
	l := openTest(t)

	if err := l.PutLookup("tmdb:1:movie", 200, "{}", 60, "movie"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}
	if err := l.PutLookup("tmdb:2:movie", 200, "{}", 86400, "movie"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}

	if err := l.ForgetLookup("tmdb:1:movie"); err != nil {
		t.Fatalf("ForgetLookup: %v", err)
	}
	if _, found := l.Lookup("tmdb:1:movie"); found {
		t.Error("ForgetLookup left the entry behind")
	}

	n, err := l.PurgeExpired(fakeNow.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 0 {
		t.Errorf("PurgeExpired removed %d rows, want 0: the remaining entry is still fresh", n)
	}
	if err := l.PutLookup("tmdb:3:movie", 200, "{}", 30, "movie"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}
	n, err = l.PurgeExpired(fakeNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("PurgeExpired removed %d rows, want 1", n)
	}
	if _, found := l.Lookup("tmdb:3:movie"); found {
		t.Error("the expired entry survived the purge")
	}
}

func TestAppliedMarkersLifecycle(t *testing.T) {
	l := openTest(t)

	markers, err := l.Applied(42)
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(markers) != 0 {
		t.Errorf("Applied on an empty ledger = %v, want none", markers)
	}

	want := []model.Marker{
		{Text: model.MarkerIntro, StartMS: 0, EndMS: 60000, Source: string(model.SourceTheIntroDB)},
		{Text: model.MarkerCredits, StartMS: 2400000, EndMS: 2580000, Final: true, Source: string(model.SourceTheIntroDB)},
	}
	if err := l.ReplaceApplied(42, want); err != nil {
		t.Fatalf("ReplaceApplied: %v", err)
	}

	got, err := l.Applied(42)
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Applied returned %d markers, want 2", len(got))
	}
	if got[0].Text != model.MarkerIntro || got[0].StartMS != 0 || got[0].EndMS != 60000 {
		t.Errorf("first marker = %+v, want the intro at 0-60000", got[0])
	}
	if got[1].Text != model.MarkerCredits || !got[1].Final || got[1].EndMS != 2580000 {
		t.Errorf("second marker = %+v, want a final credits marker", got[1])
	}

	// Replacing is a replace, not an append.
	if err := l.ReplaceApplied(42, want[:1]); err != nil {
		t.Fatalf("ReplaceApplied: %v", err)
	}
	got, _ = l.Applied(42)
	if len(got) != 1 {
		t.Errorf("after replace: %d markers, want 1", len(got))
	}

	// Another item is untouched.
	if err := l.ReplaceApplied(43, []model.Marker{{Text: model.MarkerIntro, StartMS: 1, EndMS: 2}}); err != nil {
		t.Fatalf("ReplaceApplied: %v", err)
	}
	if got, _ := l.Applied(42); len(got) != 1 {
		t.Errorf("item 42 changed to %d markers while writing item 43", len(got))
	}

	// An empty set clears the item.
	if err := l.ReplaceApplied(42, nil); err != nil {
		t.Fatalf("ReplaceApplied(nil): %v", err)
	}
	if got, _ := l.Applied(42); len(got) != 0 {
		t.Errorf("after clearing: %d markers, want 0", len(got))
	}

	if err := l.ForgetApplied(43); err != nil {
		t.Fatalf("ForgetApplied: %v", err)
	}
	if got, _ := l.Applied(43); len(got) != 0 {
		t.Errorf("ForgetApplied left %d markers", len(got))
	}
}

func TestRequestsSinceAndDayBoundary(t *testing.T) {
	l := openTest(t)

	for i := 0; i < 3; i++ {
		if err := l.RecordRequest(SourceTheIntroDB); err != nil {
			t.Fatalf("RecordRequest: %v", err)
		}
	}
	if err := l.RecordRequest(SourcePlex); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	dayStart := StartOfUTCDay(fakeNow)
	n, err := l.RequestsSince(dayStart, SourceTheIntroDB)
	if err != nil {
		t.Fatalf("RequestsSince: %v", err)
	}
	if n != 3 {
		t.Errorf("RequestsSince(day start, theintrodb) = %d, want 3", n)
	}
	if n, _ := l.RequestsSince(dayStart, SourceAny); n != 4 {
		t.Errorf("RequestsSince(day start, all) = %d, want 4", n)
	}
	if n, _ := l.RequestsSince(dayStart, SourcePlex); n != 1 {
		t.Errorf("RequestsSince(day start, plex) = %d, want 1", n)
	}
	// A window that starts after the requests saw nothing.
	if n, _ := l.RequestsSince(fakeNow.Unix()+1, SourceAny); n != 0 {
		t.Errorf("RequestsSince(future) = %d, want 0", n)
	}
	// Yesterday is excluded even though the rows exist.
	if n, _ := l.RequestsSince(dayStart-86400+1, SourceAny); n != 4 {
		t.Errorf("RequestsSince(previous day + 1s) = %d, want 4", n)
	}
	if n, err := l.RequestsToday(SourceAny); err != nil || n != 4 {
		t.Errorf("RequestsToday = %d (err %v), want 4", n, err)
	}

	// With the clock moved to the next UTC day, today's count resets while the
	// rows stay in the table.
	tomorrow := fakeNow.Add(24 * time.Hour)
	l.SetClock(func() time.Time { return tomorrow })
	if n, err := l.RequestsToday(SourceAny); err != nil || n != 0 {
		t.Errorf("RequestsToday after midnight = %d (err %v), want 0", n, err)
	}
	if n, _ := l.RequestsSince(0, SourceAny); n != 4 {
		t.Errorf("total requests = %d, want 4", n)
	}
}

func TestRunHistoryAndStats(t *testing.T) {
	runs, err := openTest(t).Runs(5)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("Runs on an empty ledger = %v, want none", runs)
	}

	l := openTest(t)
	l.SetClock(func() time.Time { return fakeNow })

	first := Run{
		StartedAt:  fakeNow.Add(-5 * time.Minute),
		FinishedAt: fakeNow.Add(-4 * time.Minute),
		Source:     "sync",
		Status:     "ok",
		Note:       "small library",
		Items:      10,
		Lookups:    8,
		Hits:       5,
		Misses:     3,
		NoData:     2,
		Added:      4,
		Removed:    1,
		Skipped:    5,
	}
	id, err := l.RecordRun(first)
	if err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	if id != 1 {
		t.Errorf("first run id = %d, want 1", id)
	}
	if _, err := l.RecordRun(Run{Source: "sync", Status: "failed"}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	got, err := l.Runs(10)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Runs returned %d rows, want 2", len(got))
	}
	// Newest first.
	if got[0].Status != "failed" || got[1].Status != "ok" {
		t.Errorf("run order = %q, %q, want newest first", got[0].Status, got[1].Status)
	}
	last := got[1]
	if last.Items != 10 || last.Lookups != 8 || last.Hits != 5 || last.Misses != 3 || last.NoData != 2 ||
		last.Added != 4 || last.Removed != 1 || last.Skipped != 5 {
		t.Errorf("counts did not survive the round trip: %+v", last)
	}
	if !last.StartedAt.Equal(first.StartedAt) || !last.FinishedAt.Equal(first.FinishedAt) {
		t.Errorf("times did not survive: got %s..%s, want %s..%s",
			last.StartedAt, last.FinishedAt, first.StartedAt, first.FinishedAt)
	}
	if last.Duration() != time.Minute {
		t.Errorf("Duration = %s, want 1m", last.Duration())
	}

	// A run with no explicit times is stamped now.
	id, err = l.RecordRun(Run{Source: "sync"})
	if err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	if id == 0 {
		t.Error("RecordRun returned id 0")
	}
	got, _ = l.Runs(1)
	if len(got) != 1 || !got[0].StartedAt.Equal(fakeNow) {
		t.Errorf("undated run stamp = %+v, want %s", got, fakeNow)
	}

	if err := l.PutLookup("tmdb:1:movie", 200, "{}", 86400, "movie"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}
	if err := l.PutLookup("tmdb:2:movie", 404, "", 86400, "movie"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}
	if err := l.ReplaceApplied(7, []model.Marker{
		{Text: model.MarkerIntro, StartMS: 0, EndMS: 1000},
		{Text: model.MarkerCredits, StartMS: 2000, EndMS: 3000},
	}); err != nil {
		t.Fatalf("ReplaceApplied: %v", err)
	}
	if err := l.RecordRequest(SourceTheIntroDB); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	stats, err := l.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Lookups != 2 || stats.LookupHits != 1 || stats.LookupMisses != 1 {
		t.Errorf("lookup stats = %+v, want 2 lookups split 1 hit / 1 miss", stats)
	}
	if stats.AppliedItems != 1 || stats.AppliedMarkers != 2 {
		t.Errorf("applied stats = %+v, want 1 item and 2 markers", stats)
	}
	if stats.RequestsTotal != 1 || stats.RequestsToday != 1 {
		t.Errorf("request stats = %+v, want 1 total and 1 today", stats)
	}
	if stats.Runs != 3 {
		t.Errorf("Runs = %d, want 3", stats.Runs)
	}
	if stats.LastRun == nil || !stats.LastRun.StartedAt.Equal(fakeNow) {
		t.Errorf("LastRun = %+v, want the undated run", stats.LastRun)
	}
	if stats.DatabasePath != l.Path() {
		t.Errorf("DatabasePath = %q, want %q", stats.DatabasePath, l.Path())
	}
	if stats.DatabaseBytes <= 0 {
		t.Errorf("DatabaseBytes = %d, want a positive file size", stats.DatabaseBytes)
	}
	if stats.LastRequestTime != fakeNow.Unix() {
		t.Errorf("LastRequestTime = %d, want %d", stats.LastRequestTime, fakeNow.Unix())
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "ledger.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first.SetClock(func() time.Time { return fakeNow })
	if err := first.PutLookup("tmdb:1396:1:1", 200, `{"intro":{"start_ms":1,"end_ms":2}}`,
		int64(30*24*time.Hour/time.Second), "episode"); err != nil {
		t.Fatalf("PutLookup: %v", err)
	}
	if err := first.RecordRequest(SourceTheIntroDB); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	second.SetClock(func() time.Time { return fakeNow })

	cached, found := second.Lookup("tmdb:1396:1:1")
	if !found {
		t.Fatal("the cached answer did not survive the reopen")
	}
	if cached.Body != `{"intro":{"start_ms":1,"end_ms":2}}` {
		t.Errorf("body after reopen = %q", cached.Body)
	}
	if !cached.Fresh(fakeNow) {
		t.Error("the cached answer is not fresh after a reopen")
	}
	if n, err := second.RequestsToday(SourceTheIntroDB); err != nil || n != 1 {
		t.Errorf("requests today after reopen = %d (err %v), want 1", n, err)
	}
}

func TestClockDefaultsToNow(t *testing.T) {
	l := openTest(t)
	l.SetClock(nil) // must fall back to time.Now, not panic or freeze
	before := time.Now()
	if err := l.RecordRequest(SourceTheIntroDB); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	after := time.Now()

	if n, err := l.RequestsSince(before.Unix(), SourceAny); err != nil || n != 1 {
		t.Errorf("request stamped outside [%s, %s]: count %d (err %v)",
			before, after, n, err)
	}
}
