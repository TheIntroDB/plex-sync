package plexdb

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func intro(start, end int64) model.Marker {
	return model.Marker{Text: model.MarkerIntro, StartMS: start, EndMS: end, Source: string(model.SourceTheIntroDB)}
}

func credits(start, end int64, final bool) model.Marker {
	return model.Marker{
		Text:    model.MarkerCredits,
		StartMS: start,
		EndMS:   end,
		Final:   final,
		Source:  string(model.SourceTheIntroDB),
	}
}

// kept turns fixture rows into the Kept list a plan would carry.
func kept(rows []markerRow) []model.ExistingMarker {
	out := make([]model.ExistingMarker, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.ExistingMarker{
			TagID:   r.ID,
			Text:    r.Text,
			StartMS: r.Start,
			EndMS:   r.End,
			Index:   int(r.Index),
			Origin:  string(model.OriginPlex),
		})
	}
	return out
}

func extraString(m markerRow) string {
	s, _ := m.ExtraData.(string)
	return s
}

// assertQueryEncoded checks that a url member keeps only A-Z a-z 0-9 _ and -
// literal, and spells everything else as uppercase %XX.
func assertQueryEncoded(t *testing.T, query string) {
	t.Helper()
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '=' || c == '&' {
			continue
		}
		if c != '%' || i+2 >= len(query) {
			t.Fatalf("query %q carries a literal %q at %d", query, string(c), i)
		}
		hex := query[i+1 : i+3]
		if strings.ToUpper(hex) != hex || !isHex(hex) {
			t.Fatalf("query %q has a non-uppercase escape %%%s at %d", query, hex, i)
		}
		i += 2
	}
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// urlPairs splits a url member into its decoded key/value pairs.
func urlPairs(t *testing.T, query string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if query == "" {
		return out
	}
	for _, pair := range strings.Split(query, "&") {
		rawKey, rawValue, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("url member pair %q has no value", pair)
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			t.Fatalf("url member key %q: %v", rawKey, err)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			t.Fatalf("url member value %q: %v", rawValue, err)
		}
		out[key] = value
	}
	return out
}

// ---------------------------------------------------------------------------
// schema and reads
// ---------------------------------------------------------------------------

// TestFixtureSchemaMatchesPlex pins the parts of the schema that are not
// guessable: the marker tag type, the quoted index column, and the two
// different durations.
func TestFixtureSchemaMatchesPlex(t *testing.T) {
	f := newFixture(t)

	if got := f.count(`SELECT COUNT(*) FROM tags WHERE tag_type = ?`, TagTypeMarker); got != 3 {
		t.Fatalf("marker tags: got %d, want 3", got)
	}
	if got := f.count(`SELECT COUNT(*) FROM tags WHERE tag_type = ? AND tag LIKE '%://%'`, TagTypeProviderID); got != 3 {
		t.Fatalf("provider-id tags: got %d, want 3", got)
	}
	var provider string
	if err := f.db.QueryRow(`SELECT tag FROM tags WHERE tag_type = ? AND tag = 'tmdb://1396'`, TagTypeProviderID).Scan(&provider); err != nil {
		t.Fatalf("read provider tag: %v", err)
	}

	columns := map[string]bool{}
	rows, err := f.db.Query(`SELECT name FROM pragma_table_info('taggings')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		columns[name] = true
	}
	rows.Close()
	for _, want := range []string{"index", "text", "time_offset", "end_time_offset", "thumb_url", "created_at", "extra_data"} {
		if !columns[want] {
			t.Fatalf("taggings is missing the %q column", want)
		}
	}

	item := f.addEpisode(101, "A Show", 1, 1, 2820000, 2825000)
	if item.RuntimeMS == item.FileDurationMS {
		t.Fatal("fixture must keep the rounded runtime and the file length different")
	}
	var metadata, file int64
	if err := f.db.QueryRow(`SELECT duration FROM metadata_items WHERE id = ?`, item.RatingKey).Scan(&metadata); err != nil {
		t.Fatalf("read metadata duration: %v", err)
	}
	if err := f.db.QueryRow(`SELECT duration FROM media_items WHERE metadata_item_id = ?`, item.RatingKey).Scan(&file); err != nil {
		t.Fatalf("read media duration: %v", err)
	}
	if metadata != 2820000 || file != 2825000 {
		t.Fatalf("durations: metadata %d, file %d, want 2820000 and 2825000", metadata, file)
	}

	var fps float64
	if err := f.db.QueryRow(`SELECT frames_per_second FROM media_items WHERE metadata_item_id = ?`, item.RatingKey).Scan(&fps); err != nil {
		t.Fatalf("read frames_per_second: %v", err)
	}
	if fps <= 0 {
		t.Fatalf("frames_per_second: %v", fps)
	}
}

func TestOpenDBReadOnlyAndMarkerTag(t *testing.T) {
	f := newFixture(t)
	f.addEpisode(101, "A Show", 1, 1, 2820000, 2825000)

	ro, err := OpenDB(f.path, true)
	if err != nil {
		t.Fatalf("OpenDB read-only: %v", err)
	}
	defer ro.Close()

	if got, err := ro.MarkerTagID(); err != nil || got != f.IntroTagID {
		t.Fatalf("MarkerTagID: got %d, %v; want %d", got, err, f.IntroTagID)
	}
	if _, err := ro.SQL().Exec(`INSERT INTO tags (tag_type, tag) VALUES (12, 'x')`); err == nil {
		t.Fatal("a read-only handle accepted a write")
	}
	if _, err := OpenDB(filepath.Join(t.TempDir(), "missing.db"), true); err == nil {
		t.Fatal("opening a missing database read-only must fail")
	}
}

func TestReadMarkers(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(101, "A Show", 1, 1, 2820000, 2825000)
	f.addMarker(item.RatingKey, f.IntroTagID, "intro", 62000, 92000, 0, extraIntro)
	f.addMarker(item.RatingKey, f.CommercialTagID, "commercial", 1000000, 1030000, 1, extraCredits)
	f.addMarker(item.RatingKey, f.CreditsTagID, "credits", 2790000, 2825000, 2, extraCreditsFinal)

	db := f.open()
	markers, err := db.ReadMarkers(item.RatingKey, 0)
	if err != nil {
		t.Fatalf("ReadMarkers: %v", err)
	}
	if len(markers) != 3 {
		t.Fatalf("markers: got %d, want 3", len(markers))
	}
	wantText := []string{"intro", "commercial", "credits"}
	for i, m := range markers {
		if m.Text != wantText[i] {
			t.Fatalf("marker %d: text %q, want %q", i, m.Text, wantText[i])
		}
		if m.Index != i {
			t.Fatalf("marker %d (%s): index %d, want %d", i, m.Text, m.Index, i)
		}
		if m.Origin != string(model.OriginPlex) {
			t.Fatalf("marker %d: origin %q", i, m.Origin)
		}
		if m.TagID == 0 {
			t.Fatalf("marker %d: no row id", i)
		}
	}
	if markers[0].StartMS != 62000 || markers[0].EndMS != 92000 {
		t.Fatalf("intro range: %d-%d", markers[0].StartMS, markers[0].EndMS)
	}
	if markers[0].ExtraData != extraIntro {
		t.Fatalf("intro extra_data: %q", markers[0].ExtraData)
	}

	only, err := db.ReadMarkers(item.RatingKey, f.CreditsTagID)
	if err != nil {
		t.Fatalf("ReadMarkers filtered: %v", err)
	}
	if len(only) != 1 || only[0].Text != "credits" {
		t.Fatalf("filtered markers: %+v", only)
	}
}

// TestBusyTimeoutOnEveryConnection covers the safety requirement directly: the
// timeout rides on the DSN, so every connection the pool opens has it, not just
// the first one.
func TestBusyTimeoutOnEveryConnection(t *testing.T) {
	f := newFixture(t)
	run := func(db *DB, label string) {
		t.Helper()
		// The write handle pools a single connection, so each one is
		// taken, checked and released in turn.
		conn, err := db.SQL().Conn(context.Background())
		if err != nil {
			t.Fatalf("%s: connection: %v", label, err)
		}
		defer conn.Close()
		var ms int64
		if err := conn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&ms); err != nil {
			t.Fatalf("%s: busy_timeout: %v", label, err)
		}
		if ms != BusyTimeoutMS {
			t.Fatalf("%s: busy_timeout %d, want %d", label, ms, BusyTimeoutMS)
		}
	}

	write := f.open()
	for i := 0; i < 3; i++ {
		run(write, "write handle")
	}
	read, err := OpenDB(f.path, true)
	if err != nil {
		t.Fatalf("OpenDB read-only: %v", err)
	}
	defer read.Close()
	for i := 0; i < 3; i++ {
		run(read, "read-only handle")
	}
}

func TestPartsAndItemAndIntegrity(t *testing.T) {
	f := newFixture(t)
	item := f.addMovie(201, "A Film", 5400000, 5403000)

	db := f.open()
	parts, err := db.Parts(item.RatingKey)
	if err != nil {
		t.Fatalf("Parts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts: got %d, want 1", len(parts))
	}
	if parts[0].Duration != 5403000 {
		t.Fatalf("part duration: %d, want the file length 5403000", parts[0].Duration)
	}
	if parts[0].ExtraData != fixturePartExtraData {
		t.Fatalf("part extra_data: %q", parts[0].ExtraData)
	}

	got, err := db.Item(item.RatingKey)
	if err != nil {
		t.Fatalf("Item: %v", err)
	}
	if got.Kind != model.KindMovie || got.Title != "A Film" {
		t.Fatalf("item: %+v", got)
	}
	if got.DurationMS == nil || *got.DurationMS != 5400000 {
		t.Fatalf("item runtime: %v", got.DurationMS)
	}

	result, err := IntegrityCheck(f.path)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Fatalf("IntegrityCheck: %q", result)
	}
}

// ---------------------------------------------------------------------------
// extra_data encoding
// ---------------------------------------------------------------------------

// TestEncodeExtraMatchesPlexBytes is the one check that does not go through
// this package's own decoder: the expected bytes are the literal a real Plex
// server stores for a file it has analysed and found no markers in.
func TestEncodeExtraMatchesPlexBytes(t *testing.T) {
	cases := []struct {
		name string
		core map[string]string
		want string
	}{
		{
			name: "version only",
			core: map[string]string{"pv:version": "5"},
			want: `{"pv:version":"5","url":"pv%3Aversion=5"}`,
		},
		{
			name: "duration and version",
			core: map[string]string{"duration": "2825000", "pv:version": "5"},
			want: fixturePartExtraData,
		},
		{
			name: "final credits",
			core: map[string]string{"pv:final": "1", "pv:version": "4"},
			want: `{"pv:final":"1","pv:version":"4","url":"pv%3Afinal=1&pv%3Aversion=4"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeExtra(tc.core)
			if got != tc.want {
				t.Fatalf("EncodeExtra:\n got %s\nwant %s", got, tc.want)
			}
			var members map[string]string
			if err := json.Unmarshal([]byte(tc.want), &members); err != nil {
				t.Fatalf("expected value is not JSON: %v", err)
			}
			assertQueryEncoded(t, members["url"])
		})
	}
	if got := EncodeExtra(nil); got != "" {
		t.Fatalf("EncodeExtra(nil): %q", got)
	}
}

func TestPercentEncodeExtraEscapesPunctuation(t *testing.T) {
	cases := map[string]string{
		"pv:version":               "pv%3Aversion",
		"a b":                      "a%20b",
		"a.b":                      "a%2Eb",
		"a~b":                      "a%7Eb",
		"a!b*c'd(e)f":              "a%21b%2Ac%27d%28e%29f",
		"keep-this_and_this09AZaz": "keep-this_and_this09AZaz",
		"{\"a\":[1,true]}":         "%7B%22a%22%3A%5B1%2Ctrue%5D%7D",
	}
	for in, want := range cases {
		if got := percentEncodeExtra(in); got != want {
			t.Fatalf("percentEncodeExtra(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestRewriteExtraLeavesLegacyAlone(t *testing.T) {
	markers := []model.Marker{intro(1000, 30000)}
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"not json", "definitely not json"},
		{"json array", `[1,2,3]`},
		{"object without url", fixtureLegacyPartExtraData},
		{"json null", "null"},
		{"truncated", `{"duration":"1","url":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := RewriteExtra(tc.raw, []string{"intro", "credits"}, markers, nil)
			if changed {
				t.Fatalf("RewriteExtra rewrote %q", tc.raw)
			}
			if got != tc.raw {
				t.Fatalf("RewriteExtra changed the value: %q -> %q", tc.raw, got)
			}
		})
	}

	// A valid blob with no affected type is also left alone.
	got, changed := RewriteExtra(fixturePartExtraData, []string{"recap"}, markers, nil)
	if changed || got != fixturePartExtraData {
		t.Fatalf("untouched types: changed=%v value=%q", changed, got)
	}
}

func TestRewriteExtraBuildsMarkerMembers(t *testing.T) {
	intros := []model.Marker{intro(62000, 92000)}
	creditMarkers := []model.Marker{credits(2790000, 2825000, true)}

	out, changed := RewriteExtra(fixturePartExtraData, []string{"intro", "credits"}, intros, creditMarkers)
	if !changed {
		t.Fatal("RewriteExtra did not rewrite a valid blob")
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("rewritten extra_data is not JSON: %v (%s)", err, out)
	}

	wantOrder := []string{`"duration":`, `"pv:credits":`, `"pv:intros":`, `"pv:version":`, `"url":`}
	last := -1
	for _, key := range wantOrder {
		at := strings.Index(out, key)
		if at < 0 {
			t.Fatalf("rewritten extra_data is missing %s: %s", key, out)
		}
		if at < last {
			t.Fatalf("member %s is out of sorted order in %s", key, out)
		}
		last = at
	}

	// The untouched member survives byte for byte.
	if string(decoded["pv:version"]) != `"5"` {
		t.Fatalf("pv:version member: %s", decoded["pv:version"])
	}

	var introMember struct {
		MediaPartMarkersArray struct {
			AttributeName   string `json:"attributeName"`
			Version         int    `json:"version"`
			MediaPartMarker []struct {
				StartTimeOffset int64 `json:"startTimeOffset"`
				EndTimeOffset   int64 `json:"endTimeOffset"`
			} `json:"MediaPartMarker"`
		} `json:"MediaPartMarkersArray"`
	}
	if err := json.Unmarshal([]byte(memberString(t, decoded["pv:intros"])), &introMember); err != nil {
		t.Fatalf("pv:intros: %v (%s)", err, decoded["pv:intros"])
	}
	array := introMember.MediaPartMarkersArray
	if array.AttributeName != "intros" || array.Version != 5 || len(array.MediaPartMarker) != 1 {
		t.Fatalf("pv:intros: %+v", array)
	}
	if array.MediaPartMarker[0].StartTimeOffset != 62000 || array.MediaPartMarker[0].EndTimeOffset != 92000 {
		t.Fatalf("pv:intros marker: %+v", array.MediaPartMarker[0])
	}

	var creditsMember struct {
		MediaPartMarkersArray struct {
			AttributeName   string `json:"attributeName"`
			Version         int    `json:"version"`
			MediaPartMarker []struct {
				StartTimeOffset int64 `json:"startTimeOffset"`
				EndTimeOffset   int64 `json:"endTimeOffset"`
				Final           bool  `json:"final"`
			} `json:"MediaPartMarker"`
		} `json:"MediaPartMarkersArray"`
	}
	if err := json.Unmarshal([]byte(memberString(t, decoded["pv:credits"])), &creditsMember); err != nil {
		t.Fatalf("pv:credits: %v (%s)", err, decoded["pv:credits"])
	}
	cm := creditsMember.MediaPartMarkersArray
	if cm.AttributeName != "credits" || cm.Version != 4 || len(cm.MediaPartMarker) != 1 {
		t.Fatalf("pv:credits: %+v", cm)
	}
	if !cm.MediaPartMarker[0].Final {
		t.Fatalf("pv:credits marker is not final: %+v", cm.MediaPartMarker[0])
	}

	// The url member repeats every other member as an encoded query string.
	var urlMember string
	if err := json.Unmarshal(decoded["url"], &urlMember); err != nil {
		t.Fatalf("url member: %v (%s)", err, decoded["url"])
	}
	assertQueryEncoded(t, urlMember)
	pairs := urlPairs(t, urlMember)
	if len(pairs) != 4 {
		t.Fatalf("url member has %d pairs, want 4: %s", len(pairs), urlMember)
	}
	if pairs["duration"] != "2825000" || pairs["pv:version"] != "5" {
		t.Fatalf("url member scalars: %+v", pairs)
	}
	if pairs["pv:intros"] != memberString(t, decoded["pv:intros"]) {
		t.Fatalf("url member pv:intros %q does not match the member %s", pairs["pv:intros"], decoded["pv:intros"])
	}
	if pairs["pv:credits"] != memberString(t, decoded["pv:credits"]) {
		t.Fatalf("url member pv:credits %q does not match the member %s", pairs["pv:credits"], decoded["pv:credits"])
	}
}

func TestRewriteExtraEmptyMarkerSets(t *testing.T) {
	out, changed := RewriteExtra(fixturePartExtraData, []string{"intro", "credits"}, nil, nil)
	if !changed {
		t.Fatal("RewriteExtra did not rewrite")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("rewritten extra_data is not JSON: %v", err)
	}

	var introMember struct {
		MediaPartMarkersArray struct {
			AttributeName   string `json:"attributeName"`
			Version         int    `json:"version"`
			MediaPartMarker any    `json:"MediaPartMarker"`
		} `json:"MediaPartMarkersArray"`
	}
	if err := json.Unmarshal([]byte(memberString(t, decoded["pv:intros"])), &introMember); err != nil {
		t.Fatalf("pv:intros: %v", err)
	}
	if marker, ok := introMember.MediaPartMarkersArray.MediaPartMarker.(string); !ok || marker != "" {
		t.Fatalf("empty intro marker list should be an empty string, got %#v", introMember.MediaPartMarkersArray.MediaPartMarker)
	}

	var creditsMember map[string]json.RawMessage
	if err := json.Unmarshal([]byte(memberString(t, decoded["pv:credits"])), &creditsMember); err != nil {
		t.Fatalf("pv:credits: %v", err)
	}
	array, ok := creditsMember["MediaPartMarkersArray"]
	if !ok {
		t.Fatalf("pv:credits has no array: %s", decoded["pv:credits"])
	}
	var arrayMembers map[string]json.RawMessage
	if err := json.Unmarshal(array, &arrayMembers); err != nil {
		t.Fatalf("pv:credits array: %v", err)
	}
	if len(arrayMembers) != 2 {
		t.Fatalf("an empty credits array carries just a name and a version: %s", array)
	}
	if string(arrayMembers["attributeName"]) != `"credits"` || string(arrayMembers["version"]) != "4" {
		t.Fatalf("empty credits array: %s", array)
	}
	if _, ok := arrayMembers["MediaPartMarker"]; ok {
		t.Fatalf("empty credits array should carry no marker list: %s", array)
	}
}

func TestMarkerExtraDataBytes(t *testing.T) {
	if got := MarkerExtraData(intro(0, 1)); got != extraIntro {
		t.Fatalf("intro payload: %s", got)
	}
	if got := MarkerExtraData(credits(0, 1, false)); got != extraCredits {
		t.Fatalf("credits payload: %s", got)
	}
	if got := MarkerExtraData(credits(0, 1, true)); got != extraCreditsFinal {
		t.Fatalf("final credits payload: %s", got)
	}
}

// ---------------------------------------------------------------------------
// the write path
// ---------------------------------------------------------------------------

func TestApplyPlansInsertsMarkers(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(101, "A Show", 1, 1, 2820000, 2825000)
	db := f.open()

	tagID, err := db.MarkerTagID()
	if err != nil {
		t.Fatalf("MarkerTagID: %v", err)
	}
	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	j, err := NewJournal(journalPath)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}

	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode, Title: "Pilot"},
		Desired: []model.Marker{intro(62000, 92000), credits(2790000, 2825000, true)},
		Add:     []model.Marker{intro(62000, 92000), credits(2790000, 2825000, true)},
		Reason:  "add",
	}
	stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, j)
	if err != nil {
		t.Fatalf("ApplyPlans: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("journal close: %v", err)
	}

	if stats.Items != 1 || stats.Written != 1 || stats.Added != 2 || stats.Removed != 0 || stats.Skipped != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if stats.PartsUpdated != 1 {
		t.Fatalf("parts updated: %d, want 1", stats.PartsUpdated)
	}

	rows := f.markers(item.RatingKey)
	if len(rows) != 2 {
		t.Fatalf("marker rows: got %d, want 2", len(rows))
	}
	if rows[0].Text != "intro" || rows[0].Start != 62000 || rows[0].End != 92000 || rows[0].Index != 0 {
		t.Fatalf("intro row: %+v", rows[0])
	}
	if rows[1].Text != "credits" || rows[1].Start != 2790000 || rows[1].End != 2825000 || rows[1].Index != 1 {
		t.Fatalf("credits row: %+v", rows[1])
	}
	// Each marker hangs off the tag_type-12 tag for its own text.
	if rows[0].TagID != f.IntroTagID || rows[1].TagID != f.CreditsTagID {
		t.Fatalf("marker tags: %d and %d, want %d and %d", rows[0].TagID, rows[1].TagID, f.IntroTagID, f.CreditsTagID)
	}
	// The payloads are the byte-exact literals Plex expects.
	if extraString(rows[0]) != extraIntro {
		t.Fatalf("intro payload: %s", extraString(rows[0]))
	}
	if extraString(rows[1]) != extraCreditsFinal {
		t.Fatalf("credits payload: %s", extraString(rows[1]))
	}
	// thumb_url is the literal empty string, not null.
	if thumb, ok := rows[0].ThumbURL.(string); !ok || thumb != "" {
		t.Fatalf("thumb_url: %#v", rows[0].ThumbURL)
	}

	part, valid := f.partExtra(item.PartID)
	if !valid {
		t.Fatal("part extra_data went null")
	}
	if part == fixturePartExtraData {
		t.Fatal("part extra_data was not rewritten")
	}
	decoded := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(part), &decoded); err != nil {
		t.Fatalf("part extra_data: %v (%s)", err, part)
	}
	if _, ok := decoded["pv:intros"]; !ok {
		t.Fatalf("part extra_data has no pv:intros: %s", part)
	}
	if _, ok := decoded["pv:credits"]; !ok {
		t.Fatalf("part extra_data has no pv:credits: %s", part)
	}
	if string(decoded["duration"]) != `"2825000"` {
		t.Fatalf("part extra_data lost an existing member: %s", part)
	}

	ops, err := ReadJournal(journalPath)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	counts := map[string]int{}
	for _, op := range ops {
		name, _ := op["op"].(string)
		counts[name]++
	}
	if counts["insert"] != 2 || counts["extra"] != 1 || counts["index"] != 1 || counts["delete"] != 0 {
		t.Fatalf("journal operations: %+v (%d lines)", counts, len(ops))
	}
}

// TestApplyPlansRenumbersAcrossMarkerTypes is the case the schema note warns
// about: inserting one marker has to renumber the rows that come after it, and
// markers of other types count too.
func TestApplyPlansRenumbersAcrossMarkerTypes(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(102, "A Show", 1, 1, 2820000, 2825000)
	commercialID := f.addMarker(item.RatingKey, f.CommercialTagID, "commercial", 2790000, 2825000, 0, extraCredits)

	db := f.open()
	tagID, _ := db.MarkerTagID()
	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	j, err := NewJournal(journalPath)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()

	before := f.markers(item.RatingKey)
	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode},
		Desired: []model.Marker{intro(62000, 92000)},
		Add:     []model.Marker{intro(62000, 92000)},
		Kept:    kept(before),
		Reason:  "add",
	}
	stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, j)
	if err != nil {
		t.Fatalf("ApplyPlans: %v", err)
	}
	if stats.Skipped != 0 || stats.Added != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if stats.Indexed != 1 {
		t.Fatalf("indexed: %d, want the commercial row renumbered", stats.Indexed)
	}

	rows := f.markers(item.RatingKey)
	if len(rows) != 2 {
		t.Fatalf("marker rows: %d", len(rows))
	}
	if rows[0].Text != "intro" || rows[0].Index != 0 {
		t.Fatalf("intro row: %+v", rows[0])
	}
	if rows[1].ID != commercialID || rows[1].Index != 1 {
		t.Fatalf("commercial row: %+v", rows[1])
	}
	if rows[1].ExtraData == nil || extraString(rows[1]) != extraCredits {
		t.Fatalf("the untouched row's payload changed: %#v", rows[1].ExtraData)
	}

	ops, err := ReadJournal(journalPath)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	found := false
	for _, op := range ops {
		if op["op"] == "index" && int64(op["id"].(float64)) == commercialID {
			found = true
			if int64(op["old_index"].(float64)) != 0 {
				t.Fatalf("index operation: %+v", op)
			}
		}
	}
	if !found {
		t.Fatalf("no index operation for the renumbered row: %+v", ops)
	}
}

func TestApplyPlansRemovesAndRenumbers(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(103, "A Show", 1, 1, 2820000, 2825000)
	f.addMarker(item.RatingKey, f.CommercialTagID, "commercial", 2790000, 2825000, 0, extraCredits)
	introID := f.addMarker(item.RatingKey, f.IntroTagID, "intro", 62000, 92000, 1, extraIntro)

	db := f.open()
	tagID, _ := db.MarkerTagID()
	rows := f.markers(item.RatingKey)
	if len(rows) != 2 {
		t.Fatalf("fixture markers: %d", len(rows))
	}

	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode},
		Desired: []model.Marker{intro(62000, 92000)},
		Remove:  []int64{rows[1].ID},
		Kept:    kept([]markerRow{rows[0]}),
		Reason:  "refresh",
	}
	stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, nil)
	if err != nil {
		t.Fatalf("ApplyPlans: %v", err)
	}
	if stats.Removed != 1 || stats.Added != 0 || stats.Indexed != 1 || stats.Skipped != 0 {
		t.Fatalf("stats: %+v", stats)
	}

	after := f.markers(item.RatingKey)
	if len(after) != 1 || after[0].ID != introID || after[0].Index != 0 {
		t.Fatalf("after removal: %+v", after)
	}
	if got, _ := f.partExtra(item.PartID); got != fixturePartExtraData {
		t.Fatalf("a removal of a commercial marker should not rewrite the part: %q", got)
	}
}

func TestApplyPlansSkipsChangedItems(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(104, "A Show", 1, 1, 2820000, 2825000)
	introID := f.addMarker(item.RatingKey, f.IntroTagID, "intro", 62000, 92000, 0, extraIntro)

	db := f.open()
	tagID, _ := db.MarkerTagID()
	live := f.markers(item.RatingKey)

	t.Run("range changed", func(t *testing.T) {
		stale := kept(live)
		stale[0].EndMS = 99999
		plan := model.ItemPlan{
			Item:   model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode},
			Add:    []model.Marker{credits(2790000, 2825000, true)},
			Kept:   stale,
			Reason: "refresh",
		}
		stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, nil)
		if err != nil {
			t.Fatalf("ApplyPlans: %v", err)
		}
		if stats.Skipped != 1 || stats.Written != 0 || stats.Added != 0 {
			t.Fatalf("stats: %+v", stats)
		}
		if len(stats.SkipReasons) != 1 || !strings.Contains(stats.SkipReasons[0], "104") {
			t.Fatalf("skip reasons: %+v", stats.SkipReasons)
		}
		if rows := f.markers(item.RatingKey); len(rows) != 1 || rows[0].ID != introID {
			t.Fatalf("the skipped item was written: %+v", rows)
		}
	})

	t.Run("index order changed", func(t *testing.T) {
		stale := kept(live)
		stale[0].Index = 7
		plan := model.ItemPlan{
			Item:   model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode},
			Add:    []model.Marker{credits(2790000, 2825000, true)},
			Kept:   stale,
			Reason: "refresh",
		}
		stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, nil)
		if err != nil {
			t.Fatalf("ApplyPlans: %v", err)
		}
		if stats.Skipped != 1 {
			t.Fatalf("stats: %+v", stats)
		}
	})

	t.Run("a marker appeared", func(t *testing.T) {
		f.addMarker(item.RatingKey, f.CreditsTagID, "credits", 2790000, 2825000, 1, extraCreditsFinal)
		plan := model.ItemPlan{
			Item:   model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode},
			Add:    []model.Marker{credits(2790000, 2825000, true)},
			Kept:   kept(live),
			Reason: "refresh",
		}
		stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, nil)
		if err != nil {
			t.Fatalf("ApplyPlans: %v", err)
		}
		if stats.Skipped != 1 {
			t.Fatalf("stats: %+v", stats)
		}
		if rows := f.markers(item.RatingKey); len(rows) != 2 {
			t.Fatalf("the skipped item was written: %+v", rows)
		}
	})
}

func TestApplyPlansRollsBackAWholeChunk(t *testing.T) {
	f := newFixture(t)
	good := f.addEpisode(105, "A Show", 1, 1, 2820000, 2825000)
	bad := f.addEpisode(106, "A Show", 1, 2, 2820000, 2825000)

	db := f.open()
	tagID, _ := db.MarkerTagID()

	plans := []model.ItemPlan{
		{
			Item:    model.LibraryItem{RatingKey: int(good.RatingKey), Kind: model.KindEpisode},
			Desired: []model.Marker{intro(62000, 92000)},
			Add:     []model.Marker{intro(62000, 92000)},
			Reason:  "add",
		},
		{
			Item:    model.LibraryItem{RatingKey: int(bad.RatingKey), Kind: model.KindEpisode},
			Desired: []model.Marker{intro(92000, 62000)},
			Add:     []model.Marker{intro(92000, 62000)},
			Reason:  "add",
		},
	}
	stats, err := db.ApplyPlans(plans, tagID, 10, nil)
	if err == nil {
		t.Fatal("a marker with no duration must be refused")
	}
	if stats.Written != 0 || stats.Added != 0 {
		t.Fatalf("a rolled back chunk must not be reported as written: %+v", stats)
	}
	if rows := f.markers(good.RatingKey); len(rows) != 0 {
		t.Fatalf("the first item of the chunk was left behind: %+v", rows)
	}
	if got, _ := f.partExtra(good.PartID); got != fixturePartExtraData {
		t.Fatalf("the part was left behind: %q", got)
	}
}

// ---------------------------------------------------------------------------
// journal and undo
// ---------------------------------------------------------------------------

func TestJournalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "undo.jsonl")
	j, err := NewJournal(path)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	op := map[string]any{"op": "insert", "rating_key": 101, "id": 7, "tag_id": 1}
	if err := j.Record(op); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// The line must already be readable: an entry that only lands on Close is
	// useless as an undo log for a run that was killed.
	ops, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(ops) != 1 || ops[0]["op"] != "insert" {
		t.Fatalf("journal: %+v", ops)
	}
	if err := j.Record(map[string]any{"op": "delete", "rating_key": 101}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := j.Record(op); err == nil {
		t.Fatal("a closed journal accepted a record")
	}
	if ops, _ := ReadJournal(path); len(ops) != 2 {
		t.Fatalf("journal: %+v", ops)
	}
}

func TestUndoReplaysTheJournalInReverse(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(107, "A Show", 1, 1, 2820000, 2825000)
	f.addMarker(item.RatingKey, f.CommercialTagID, "commercial", 2790000, 2825000, 1, extraCredits)
	introID := f.addMarker(item.RatingKey, f.IntroTagID, "intro", 62000, 92000, 0, extraIntro)

	before := f.snapshotMarkers()
	beforePart, beforeValid := f.partExtra(item.PartID)

	db := f.open()
	tagID, _ := db.MarkerTagID()
	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	j, err := NewJournal(journalPath)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}

	live := f.markers(item.RatingKey)
	plan := model.ItemPlan{
		Item:    model.LibraryItem{RatingKey: int(item.RatingKey), Kind: model.KindEpisode},
		Desired: []model.Marker{intro(62000, 92000), credits(2790000, 2825000, true)},
		Add:     []model.Marker{credits(2790000, 2825000, true)},
		Remove:  []int64{live[0].ID},
		Kept:    kept([]markerRow{live[1]}),
		Reason:  "refresh",
	}
	stats, err := db.ApplyPlans([]model.ItemPlan{plan}, tagID, 10, j)
	if err != nil {
		t.Fatalf("ApplyPlans: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stats.Added != 1 || stats.Removed != 1 {
		t.Fatalf("stats: %+v", stats)
	}

	ops, err := ReadJournal(journalPath)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(ops) == 0 {
		t.Fatal("nothing was journalled")
	}

	applied, err := Undo(f.path, journalPath)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if applied != len(ops) {
		t.Fatalf("undo applied %d of %d operations", applied, len(ops))
	}

	after := f.snapshotMarkers()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("undo did not restore the markers:\n before %+v\n after %+v", before, after)
	}
	afterPart, afterValid := f.partExtra(item.PartID)
	if afterPart != beforePart || afterValid != beforeValid {
		t.Fatalf("undo did not restore extra_data: %q (%v) -> %q (%v)", beforePart, beforeValid, afterPart, afterValid)
	}
	if after[0].Index != 0 && after[1].Index != 0 {
		t.Fatalf("indices were not restored: %+v", after)
	}
	_ = introID
}

// TestUndoRestoresNullExtraData covers the journal entry a hand-written or
// older log can carry: an extra_data that was null before the write.
func TestUndoRestoresNullExtraData(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(108, "A Show", 1, 1, 2820000, 2825000)
	markerID := f.addMarker(item.RatingKey, f.IntroTagID, "intro", 62000, 92000, 3, extraIntro)
	f.setPartExtra(item.PartID, nil)

	journalPath := filepath.Join(t.TempDir(), "undo.jsonl")
	body := `{"op":"extra","rating_key":108,"part_id":` + itoa(item.PartID) + `,"old":null}` + "\n" +
		`{"op":"index","rating_key":108,"id":` + itoa(markerID) + `,"old_index":0}` + "\n"
	if err := os.WriteFile(journalPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write journal: %v", err)
	}

	applied, err := Undo(f.path, journalPath)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if applied != 2 {
		t.Fatalf("undo applied %d, want 2", applied)
	}

	if _, valid := f.partExtra(item.PartID); valid {
		t.Fatal("the part extra_data should be NULL again")
	}
	rows := f.markers(item.RatingKey)
	if len(rows) != 1 || rows[0].Index != 0 {
		t.Fatalf("index not restored: %+v", rows)
	}
}

// ---------------------------------------------------------------------------
// backup
// ---------------------------------------------------------------------------

func TestBackupCopiesInOneStepAndPrunes(t *testing.T) {
	f := newFixture(t)
	item := f.addEpisode(109, "A Show", 1, 1, 2820000, 2825000)
	f.addMarker(item.RatingKey, f.IntroTagID, "intro", 62000, 92000, 0, extraIntro)

	dir := filepath.Join(t.TempDir(), "backups")
	var made []string
	for i := 0; i < 3; i++ {
		path, err := Backup(f.path, dir, 2)
		if err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
		made = append(made, path)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, BackupPrefix) {
			t.Fatalf("backup name %q does not start with %q", base, BackupPrefix)
		}
		stamp := strings.TrimPrefix(base, BackupPrefix)
		if strings.ContainsAny(stamp, ":.") {
			t.Fatalf("backup stamp %q carries a colon or a dot", stamp)
		}
	}

	kept, err := Backups(dir)
	if err != nil {
		t.Fatalf("Backups: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d backups, want 2: %+v", len(kept), kept)
	}
	if _, err := os.Stat(made[0]); !os.IsNotExist(err) {
		t.Fatalf("the oldest backup survived the prune: %v", err)
	}
	if !sort.StringsAreSorted([]string{kept[1], kept[0]}) {
		t.Fatalf("backups are not listed newest first: %+v", kept)
	}

	restored, err := OpenDB(kept[0], true)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()
	markers, err := restored.ReadMarkers(item.RatingKey, 0)
	if err != nil {
		t.Fatalf("read backup markers: %v", err)
	}
	if len(markers) != 1 || markers[0].Text != "intro" {
		t.Fatalf("backup markers: %+v", markers)
	}
	if result, err := IntegrityCheck(kept[0]); err != nil || result != "ok" {
		t.Fatalf("IntegrityCheck on the backup: %q, %v", result, err)
	}

	if _, err := Backup(filepath.Join(t.TempDir(), "nope.db"), dir, 2); err == nil {
		t.Fatal("backing up a missing database must fail")
	}
	if _, err := Backup(f.path, dir, 0); err != nil {
		t.Fatalf("Backup with keep 0: %v", err)
	}
	if kept, _ := Backups(dir); len(kept) != 3 {
		t.Fatalf("keep 0 should keep everything, got %d", len(kept))
	}
}

// TestApplyPlansMultipleItemsAndChunks exercises more than one transaction.
func TestApplyPlansMultipleItemsAndChunks(t *testing.T) {
	f := newFixture(t)
	first := f.addEpisode(110, "A Show", 1, 1, 2820000, 2825000)
	second := f.addEpisode(111, "A Show", 1, 2, 2820000, 2820000)

	db := f.open()
	tagID, _ := db.MarkerTagID()

	plans := []model.ItemPlan{
		{
			Item:    model.LibraryItem{RatingKey: int(first.RatingKey), Kind: model.KindEpisode},
			Desired: []model.Marker{intro(62000, 92000)},
			Add:     []model.Marker{intro(62000, 92000)},
			Reason:  "add",
		},
		{
			Item:    model.LibraryItem{RatingKey: int(second.RatingKey), Kind: model.KindEpisode},
			Desired: []model.Marker{credits(2790000, 2820000, true)},
			Add:     []model.Marker{credits(2790000, 2820000, true)},
			Reason:  "add",
		},
	}
	stats, err := db.ApplyPlans(plans, tagID, 1, nil)
	if err != nil {
		t.Fatalf("ApplyPlans: %v", err)
	}
	if stats.Items != 2 || stats.Written != 2 || stats.Added != 2 {
		t.Fatalf("stats: %+v", stats)
	}
	if rows := f.markers(first.RatingKey); len(rows) != 1 || rows[0].Text != "intro" {
		t.Fatalf("first item: %+v", rows)
	}
	if rows := f.markers(second.RatingKey); len(rows) != 1 || rows[0].Text != "credits" {
		t.Fatalf("second item: %+v", rows)
	}

	// A plan with nothing to do is counted but not written.
	stats, err = db.ApplyPlans([]model.ItemPlan{{
		Item:   model.LibraryItem{RatingKey: int(first.RatingKey), Kind: model.KindEpisode},
		Reason: "noop",
	}}, tagID, 1, nil)
	if err != nil {
		t.Fatalf("ApplyPlans: %v", err)
	}
	if stats.Items != 1 || stats.Written != 0 || stats.Skipped != 0 {
		t.Fatalf("noop stats: %+v", stats)
	}
}
