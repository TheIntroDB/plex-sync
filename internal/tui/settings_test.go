package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TheIntroDB/plex-sync/internal/config"
)

// settingsModel is a model whose configuration is a real file in a temporary
// directory, so a test can check what was actually written rather than only what
// is in memory.
func settingsModel(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	m.app.Cfg.Path = path
	m.screen = screenSettings
	return m
}

// press sends one key.
func press(m *Model, key string) {
	switch key {
	case "up":
		m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	case "down":
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	case "end":
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	case "home":
		m.handleKey(tea.KeyMsg{Type: tea.KeyHome})
	case "enter":
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	case "esc":
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	case "backspace":
		m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	case " ":
		m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
	default:
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	}
}

func typeText(m *Model, text string) {
	for _, r := range text {
		if r == ' ' {
			press(m, " ")
			continue
		}
		press(m, string(r))
	}
}

// cursorTo walks the cursor down onto a named row.
func cursorTo(t *testing.T, m *Model, key string) setting {
	t.Helper()
	for _, row := range settingsRows() {
		if row.key == key {
			rows := settingsRows()
			for i, candidate := range rows {
				if candidate.key == key {
					m.cursor = i
					return row
				}
			}
		}
	}
	t.Fatalf("no setting named %q", key)
	return setting{}
}

func TestArrowsMoveTheCursor(t *testing.T) {
	m := settingsModel(t)

	if m.cursor != 0 {
		t.Fatalf("cursor starts at %d, want the first row", m.cursor)
	}
	press(m, "down")
	press(m, "down")
	if m.cursor != 2 {
		t.Errorf("cursor = %d, want 2 after two downs", m.cursor)
	}
	press(m, "up")
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1 after one up", m.cursor)
	}

	// j and k are the same as the arrows, for people who expect vi keys.
	press(m, "j")
	if m.cursor != 2 {
		t.Errorf("cursor = %d, want 2 after j", m.cursor)
	}
	press(m, "k")
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1 after k", m.cursor)
	}
}

// Running off either end must stop rather than wrap: wrapping loses your place.
func TestCursorStopsAtBothEnds(t *testing.T) {
	m := settingsModel(t)

	press(m, "up")
	press(m, "up")
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want to stop at 0", m.cursor)
	}

	last := len(settingsRows()) - 1
	press(m, "end")
	if m.cursor != last {
		t.Errorf("end gave %d, want %d", m.cursor, last)
	}
	press(m, "down")
	press(m, "down")
	if m.cursor != last {
		t.Errorf("cursor = %d, want to stop at %d", m.cursor, last)
	}
	press(m, "home")
	if m.cursor != 0 {
		t.Errorf("home gave %d, want 0", m.cursor)
	}
}

// The whole point: enter toggles a boolean and it is saved.
func TestEnterTogglesABooleanAndSaves(t *testing.T) {
	m := settingsModel(t)
	row := cursorTo(t, m, "segments.preview")

	before := m.app.Cfg.Segments.Preview
	press(m, "enter")

	if m.app.Cfg.Segments.Preview == before {
		t.Fatalf("segments.preview did not change (still %v)", before)
	}

	// It has to be on disk, not only in memory.
	loaded, err := config.Load(m.app.Cfg.Path)
	if err != nil {
		t.Fatalf("the file it wrote does not load: %v", err)
	}
	if loaded.Segments.Preview != m.app.Cfg.Segments.Preview {
		t.Errorf("on disk preview = %v, in memory %v",
			loaded.Segments.Preview, m.app.Cfg.Segments.Preview)
	}
	if m.status == "" {
		t.Error("nothing was reported after a change")
	}
	_ = row
}

// Toggling off again has to work, so a mis-toggle is not permanent.
func TestEnterTogglesBack(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "apply.backup")

	original := m.app.Cfg.Apply.Backup
	press(m, "enter")
	press(m, "enter")

	if m.app.Cfg.Apply.Backup != original {
		t.Errorf("apply.backup = %v, want the original %v", m.app.Cfg.Apply.Backup, original)
	}
	loaded, err := config.Load(m.app.Cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Apply.Backup != original {
		t.Errorf("on disk = %v, want %v", loaded.Apply.Backup, original)
	}
}

// A row that takes text opens an input line, and what is typed is saved.
func TestTypingAValue(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "plex.url")

	press(m, "enter")
	if !m.editing {
		t.Fatal("enter on a text row did not open the input line")
	}

	// Clear what was there and type a new address.
	for range m.buffer {
		press(m, "backspace")
	}
	typeText(m, "http://192.168.1.50:32400")
	press(m, "enter")

	if m.editing {
		t.Error("the input line is still open after enter")
	}
	if got := m.app.Cfg.Plex.URL; got != "http://192.168.1.50:32400" {
		t.Fatalf("url in memory = %q", got)
	}
	loaded, err := config.Load(m.app.Cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plex.URL != "http://192.168.1.50:32400" {
		t.Errorf("url on disk = %q", loaded.Plex.URL)
	}
}

// Spaces are ordinary characters, and every Plex path on macOS contains them.
func TestTypingAPathWithSpaces(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "plex.database")

	press(m, "enter")
	for range m.buffer {
		press(m, "backspace")
	}
	typeText(m, "/Library/Application Support/Plex Media Server/library.db")
	press(m, "enter")

	const want = "/Library/Application Support/Plex Media Server/library.db"
	if got := m.app.Cfg.Plex.Database; got != want {
		t.Errorf("database = %q, want %q", got, want)
	}
}

// Escape abandons the edit and changes nothing.
func TestEscapeCancelsAnEdit(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "plex.url")

	before := m.app.Cfg.Plex.URL
	press(m, "enter")
	typeText(m, "rubbish")
	press(m, "esc")

	if m.editing {
		t.Error("escape did not close the input line")
	}
	if m.app.Cfg.Plex.URL != before {
		t.Errorf("url = %q, want the original %q", m.app.Cfg.Plex.URL, before)
	}
	if _, err := os.Stat(m.app.Cfg.Path); err == nil {
		t.Error("escape wrote a file")
	}
}

// A value the configuration refuses must not be applied, must not be written,
// and must leave the input line open so it can be corrected.
func TestARejectedValueIsNotApplied(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "theintrodb.daily_budget")

	before := m.app.Cfg.TheIntroDB.DailyBudget
	press(m, "enter")
	for range m.buffer {
		press(m, "backspace")
	}
	typeText(m, "99999") // above the server's ceiling
	press(m, "enter")

	if !m.editing {
		t.Error("a rejected value closed the input line")
	}
	if m.app.Cfg.TheIntroDB.DailyBudget != before {
		t.Errorf("budget = %d, want the original %d", m.app.Cfg.TheIntroDB.DailyBudget, before)
	}
	if m.err == nil {
		t.Error("nothing explained why the value was refused")
	}
	if _, err := os.Stat(m.app.Cfg.Path); err == nil {
		t.Error("a refused value was written to disk")
	}

	// Correcting it works without reopening the row.
	m.buffer = ""
	typeText(m, "800")
	press(m, "enter")
	if m.editing {
		t.Fatal("the corrected value did not save")
	}
	if m.app.Cfg.TheIntroDB.DailyBudget != 800 {
		t.Errorf("budget = %d, want 800", m.app.Cfg.TheIntroDB.DailyBudget)
	}
}

func TestARejectedCronIsNotApplied(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "schedule.cron")

	before := m.app.Cfg.Schedule.Cron
	press(m, "enter")
	for range m.buffer {
		press(m, "backspace")
	}
	typeText(m, "99 99 * * *")
	press(m, "enter")

	if m.editing != true {
		t.Error("a bad cron expression was accepted")
	}
	if m.app.Cfg.Schedule.Cron != before {
		t.Errorf("cron = %q, want %q", m.app.Cfg.Schedule.Cron, before)
	}
}

// Enter cycles a row that has a fixed set of values rather than opening an
// input line.
func TestEnterCyclesAChoice(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "apply.policy")

	if m.app.Cfg.Apply.Policy != "fill" {
		t.Fatalf("policy starts as %q", m.app.Cfg.Apply.Policy)
	}
	press(m, "enter")
	if m.editing {
		t.Error("a choice row opened an input line")
	}
	if m.app.Cfg.Apply.Policy != "prefer-theintrodb" {
		t.Errorf("policy = %q, want the next choice", m.app.Cfg.Apply.Policy)
	}
	press(m, "enter")
	if m.app.Cfg.Apply.Policy != "fill" {
		t.Errorf("policy = %q, want it to cycle back", m.app.Cfg.Apply.Policy)
	}
}

// A secret must never be put on the screen, not even after it is saved.
func TestSecretsAreNeverDisplayed(t *testing.T) {
	const secret = "theintrodb:user_abc:deadbeef"

	m := settingsModel(t)
	row := cursorTo(t, m, "theintrodb.api_key")

	press(m, "enter")
	if m.buffer != "" {
		t.Errorf("the input line was prefilled with %q", m.buffer)
	}
	typeText(m, secret)
	press(m, "enter")

	if m.app.Cfg.TheIntroDB.APIKey != secret {
		t.Fatalf("the key was not stored: %q", m.app.Cfg.TheIntroDB.APIKey)
	}
	if strings.Contains(m.status, secret) {
		t.Error("the status line contains the key")
	}
	if strings.Contains(m.View(), secret) {
		t.Error("the screen contains the key")
	}
	if got := row.display(m); got != "set, hidden" {
		t.Errorf("the row shows %q, want it hidden", got)
	}
}

// While typing, the keyboard belongs to the input line: q types a q.
func TestTypingSwallowsShortcutKeys(t *testing.T) {
	m := settingsModel(t)
	cursorTo(t, m, "plex.url")

	press(m, "enter")
	for range m.buffer {
		press(m, "backspace")
	}
	typeText(m, "q1pa")

	if m.quit {
		t.Error("q quit the interface while typing")
	}
	if m.screen != screenSettings {
		t.Errorf("screen changed to %d while typing", m.screen)
	}
	if m.buffer != "q1pa" {
		t.Errorf("buffer = %q, want %q", m.buffer, "q1pa")
	}
}

// The screen has to say that the value is on disk, and whether it needs a
// restart, because "saved" alone reads as "take effect now".
func TestTheStatusSaysWhetherARestartIsNeeded(t *testing.T) {
	m := settingsModel(t)

	cursorTo(t, m, "theintrodb.api_key")
	press(m, "enter")
	typeText(m, "k")
	press(m, "enter")
	if !strings.Contains(m.status, "Restart to pick it up") {
		t.Errorf("status = %q, want it to mention a restart", m.status)
	}

	cursorTo(t, m, "segments.recap")
	press(m, "enter")
	// Match the phrasing rather than the word: the temporary directory in the
	// message is named after this test, so "Restart" appears in the path.
	if strings.Contains(m.status, "Restart to pick it up") {
		t.Errorf("status = %q, want no restart for a setting read per run", m.status)
	}
	if !strings.Contains(m.status, "next run") {
		t.Errorf("status = %q, want it to say when it applies", m.status)
	}
}

// Every row must render and be reachable, including the last.
func TestEveryRowRendersAndIsSelectable(t *testing.T) {
	m := settingsModel(t)
	rows := settingsRows()
	if len(rows) == 0 {
		t.Fatal("there are no settings rows")
	}
	for i, row := range rows {
		m.cursor = i
		out := m.View()
		if !strings.Contains(out, row.label) {
			t.Errorf("row %q is not rendered", row.key)
		}
		// The selected row's help is shown, so its meaning is never a guess.
		if row.help != "" && !strings.Contains(out, wrapHelp(row)) {
			t.Errorf("row %q shows no help for its selection", row.key)
		}
	}
}

// The cursor marker must be present without colour, for terminals that have
// none.
func TestTheSelectedRowIsMarked(t *testing.T) {
	m := settingsModel(t)
	m.cursor = 3
	out := m.View()
	if !strings.Contains(out, "> ") {
		t.Error("no cursor marker on the settings screen")
	}
}

// The old screen told people to edit the file by hand and restart. It should not
// still say that.
func TestTheScreenNoLongerTellsYouToEditTheFileYourself(t *testing.T) {
	m := settingsModel(t)
	out := m.View()
	if strings.Contains(out, "Edit the file above") {
		t.Error("the screen still points at the file instead of offering to edit")
	}
}
