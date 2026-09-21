package planfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

func samplePlan() *model.Plan {
	duration := int64(2_612_738)
	return &model.Plan{
		Items: []model.ItemPlan{
			{
				Item: model.LibraryItem{
					RatingKey:  13,
					Kind:       model.KindEpisode,
					Title:      "The Man in the Bear",
					ShowTitle:  "Bones",
					DurationMS: &duration,
				},
				Desired: []model.Marker{
					{Text: model.MarkerIntro, StartMS: 0, EndMS: 37_000, Source: "theintrodb"},
				},
				Add: []model.Marker{
					{Text: model.MarkerIntro, StartMS: 0, EndMS: 37_000, Source: "theintrodb"},
				},
				Reason: "add",
			},
		},
		Stats:   map[string]int{"add": 1},
		Sources: map[string]int{"theintrodb": 1},
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	original := samplePlan()
	meta := Meta{
		CreatedAt: time.Date(2026, time.September, 20, 21, 0, 0, 0, time.UTC),
		Tool:      "plex-sync",
		Version:   "0.1.0",
		Database:  "/plex/com.plexapp.plugins.library.db",
		Plex:      "http://127.0.0.1:32400",
	}

	if err := Save(path, original, meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, gotMeta, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(loaded.Items))
	}
	got := loaded.Items[0]
	if got.Item.RatingKey != 13 || got.Item.ShowTitle != "Bones" {
		t.Errorf("the item did not survive: %+v", got.Item)
	}
	if len(got.Desired) != 1 || got.Desired[0].EndMS != 37_000 {
		t.Errorf("the markers did not survive: %+v", got.Desired)
	}
	if got.Reason != "add" {
		t.Errorf("reason = %q, want add", got.Reason)
	}
	if len(loaded.Work()) != 1 {
		t.Errorf("Work() = %d, want 1", len(loaded.Work()))
	}

	if gotMeta.Database != meta.Database {
		t.Errorf("database = %q, want %q", gotMeta.Database, meta.Database)
	}
	if !gotMeta.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("created at = %s, want %s", gotMeta.CreatedAt, meta.CreatedAt)
	}
	// Items is filled from the plan, so a file can be described without being
	// interpreted.
	if gotMeta.Items != 1 {
		t.Errorf("meta.Items = %d, want 1", gotMeta.Items)
	}
}

// The write has to be atomic: an interrupted save must not leave a half-written
// plan where something else could pick it up and apply it.
func TestSaveReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")

	if err := Save(path, samplePlan(), Meta{}); err != nil {
		t.Fatal(err)
	}
	second := samplePlan()
	second.Items = nil // a plan with nothing to do
	if err := Save(path, second, Meta{}); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load after replace: %v", err)
	}
	if len(loaded.Items) != 0 {
		t.Errorf("the file still holds the first plan: %d items", len(loaded.Items))
	}

	// No temporary files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "plan.json" {
			t.Errorf("left a file behind: %s", entry.Name())
		}
	}
}

func TestLoadRefusesSomethingElse(t *testing.T) {
	dir := t.TempDir()

	notJSON := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(notJSON, []byte("cron = \"30 7 * * *\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(notJSON); err == nil {
		t.Error("a TOML file was accepted as a plan")
	}

	// JSON, but not a plan: no format marker.
	otherJSON := filepath.Join(dir, "other.json")
	if err := os.WriteFile(otherJSON, []byte(`{"items":[{"reason":"add"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(otherJSON); err == nil {
		t.Error("JSON without a format marker was accepted as a plan")
	}

	// A plan from a format this build does not know.
	future := filepath.Join(dir, "future.json")
	body, err := json.Marshal(map[string]any{"format": Format + 1, "plan": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(future, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(future); err == nil {
		t.Error("a plan from a newer format was accepted")
	}

	if _, _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}

// A plan with nothing to do is a legitimate plan, not an error.
func TestEmptyPlanIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := Save(path, &model.Plan{}, Meta{}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := Load(path)
	if err != nil {
		t.Fatalf("an empty plan was rejected: %v", err)
	}
	if len(loaded.Work()) != 0 {
		t.Errorf("Work() = %d, want 0", len(loaded.Work()))
	}
}

func TestSaveNeedsAPlan(t *testing.T) {
	if err := Save(filepath.Join(t.TempDir(), "plan.json"), nil, Meta{}); err == nil {
		t.Error("saving nothing was accepted")
	}
}

func TestAgeAndDescribe(t *testing.T) {
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)

	made := Meta{CreatedAt: now.Add(-3 * time.Hour)}
	if got := made.Age(now); got != 3*time.Hour {
		t.Errorf("age = %s, want 3h", got)
	}
	// A plan that did not record when it was made reports nothing rather than a
	// nonsense age measured from the zero time.
	if got := (Meta{}).Age(now); got != 0 {
		t.Errorf("age of an undated plan = %s, want 0", got)
	}
	if got := (Meta{}).Describe(); got == "" {
		t.Error("Describe says nothing about an undated plan")
	}
	if got := made.Describe(); got == "" {
		t.Error("Describe says nothing")
	}
}

func TestSaveCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "plan.json")
	if err := Save(path, samplePlan(), Meta{}); err != nil {
		t.Fatalf("Save into a missing directory: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the plan was not written: %v", err)
	}
}
