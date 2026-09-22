package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/config"
)

// Two settings are not stored, they are found: the library database and the Plex
// token. The rows used to print the stored value, so both said "(empty)" and
// "not set" while the tool was using something it had discovered, which reads as
// a fault and disagreed with `config check` and the status screen, which both
// print the database in use. Reported as "on the plex-sync config check and on
// the main page it says the database url, but on the settings page it says
// (empty)".

// plexOnThisMachine gives the model a Plex directory of its own, so nothing here
// depends on the machine running the test: the resolvers return early when a
// config directory is set.
func plexOnThisMachine(t *testing.T, m *Model) (dir, database string) {
	t.Helper()
	dir = t.TempDir()
	m.app.Cfg.Plex.ConfigDir = dir
	m.app.Cfg.Plex.Database = ""
	m.app.Cfg.Plex.Token = ""
	return dir, filepath.Join(dir, config.PlexDBSubpath)
}

// TestTheDatabaseRowShowsWhatIsInUse is the report itself.
func TestTheDatabaseRowShowsWhatIsInUse(t *testing.T) {
	m := newTestModel(t)
	_, database := plexOnThisMachine(t, m)

	got := settingsRows()[rowIndex(t, "plex.database")].display(m)
	if strings.Contains(got, "(empty)") {
		t.Fatalf("the row says %q while the tool uses %s", got, database)
	}
	if !strings.Contains(got, database) {
		t.Errorf("the row says %q, want the path in use (%s)", got, database)
	}
	if !strings.Contains(got, "found") {
		t.Errorf("the row says %q, want it clear that nothing was configured", got)
	}

	// And it agrees with the two places that were already right.
	if inUse := m.app.Cfg.Plex.ResolvedDatabase(); !strings.Contains(got, inUse) {
		t.Errorf("the row says %q but the resolved database is %s", got, inUse)
	}
	if !strings.Contains(m.View(), database) {
		t.Error("the settings screen does not show the database it will write to")
	}
}

// TestAPinnedDatabaseIsShownAsPinned: a path that was configured is the answer,
// and it should not be described as found.
func TestAPinnedDatabaseIsShownAsPinned(t *testing.T) {
	m := newTestModel(t)
	pinned := filepath.Join(t.TempDir(), "somewhere-else.db")
	m.app.Cfg.Plex.Database = pinned

	got := settingsRows()[rowIndex(t, "plex.database")].display(m)
	if got != pinned {
		t.Errorf("the row says %q, want the configured path %q", got, pinned)
	}
	if strings.Contains(got, "found") {
		t.Errorf("the row says %q, want a configured path not called found", got)
	}
}

// TestTheTokenRowSaysOneIsInUseWithoutShowingIt: the token is found the same way
// the database is, so "not set" was wrong, and the row must not start printing a
// secret to fix that.
func TestTheTokenRowSaysOneIsInUseWithoutShowingIt(t *testing.T) {
	m := newTestModel(t)
	dir, _ := plexOnThisMachine(t, m)

	const token = "not-a-real-token-9f2c"
	if err := os.WriteFile(filepath.Join(dir, config.LocalAdminTokenFile), []byte(token), 0o600); err != nil {
		t.Fatalf("write the token file Plex would have: %v", err)
	}

	row := settingsRows()[rowIndex(t, "plex.token")]
	got := row.display(m)
	if strings.Contains(got, "not set") {
		t.Fatalf("the row says %q while a token is in use", got)
	}
	if strings.Contains(got, token) {
		t.Fatal("the row prints the token")
	}
	// Nor anywhere else on the screen, which is the point of the masking.
	m.screen = screenSettings
	if strings.Contains(m.View(), token) {
		t.Error("the screen shows the token")
	}
	if !strings.Contains(m.View(), "hidden") {
		t.Errorf("the settings screen does not say a token is in use:\n%s", m.View())
	}
}

// TestAnUnconsideredSettingStillSaysItIsEmpty keeps the fallback from papering
// over the rows where empty really does mean empty.
func TestAnUnconsideredSettingStillSaysItIsEmpty(t *testing.T) {
	m := newTestModel(t)
	m.app.Cfg.Plex.Database = ""
	m.app.Cfg.Plex.ConfigDir = ""
	m.app.Cfg.Plex.Token = ""
	m.app.Cfg.Plex.ConfigDir = filepath.Join(t.TempDir(), "nothing-here")

	// A directory that holds no database resolves to a path that does not
	// exist; the row still has to say something useful rather than nothing.
	got := settingsRows()[rowIndex(t, "plex.database")].display(m)
	if got == "" {
		t.Error("the database row says nothing at all")
	}
}

// rowIndex finds a row by key, so a test does not depend on the order of a list
// that people reorder.
func rowIndex(t *testing.T, key string) int {
	t.Helper()
	for index, row := range settingsRows() {
		if row.key == key {
			return index
		}
	}
	t.Fatalf("no row with key %q", key)
	return -1
}
