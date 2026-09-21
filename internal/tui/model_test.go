package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/logging"
	"github.com/TheIntroDB/plex-sync/internal/model"
)

// newTestModel opens a real application against a temporary state directory and
// a Plex server that is not running.
//
// That is the state a first-time user is in, and every screen has to render in
// it: the interface must stay usable when a service is down, not refuse to
// start.
func newTestModel(t *testing.T) *Model {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.TheIntroDB.BaseURL = "http://127.0.0.1:1" // nothing listens here
	cfg.Plex.URL = "http://127.0.0.1:1"

	application, err := app.Open(cfg, logging.Discard(), app.Options{})
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Errorf("close app: %v", err)
		}
	})

	m := newModel(context.Background(), Options{App: application})
	m.width, m.height = 120, 40
	return m
}

func TestViewRendersEveryScreen(t *testing.T) {
	m := newTestModel(t)

	for index, name := range screenNames {
		m.screen = screen(index)
		out := m.View()
		if strings.TrimSpace(out) == "" {
			t.Errorf("screen %q rendered nothing", name)
		}
		if !strings.Contains(out, name) {
			t.Errorf("screen %q does not name itself in its tabs", name)
		}
	}
}

func TestViewRendersBeforeAnyDataIsLoaded(t *testing.T) {
	m := newTestModel(t)
	// Nothing has been loaded yet: no readiness, no stats, no plan, no items.
	if m.readiness != nil || m.stats != nil || m.result != nil {
		t.Fatal("the model should start empty")
	}
	for index := range screenNames {
		m.screen = screen(index)
		out := m.View()
		if strings.TrimSpace(out) == "" {
			t.Errorf("screen %d rendered nothing before loading", index)
		}
	}
}

func TestNumberKeysSwitchScreens(t *testing.T) {
	m := newTestModel(t)
	for index := range screenNames {
		key := string(rune('1' + index))
		_, _ = m.handleKey(keyMsg(key))
		if int(m.screen) != index {
			t.Errorf("key %q selected screen %d, want %d", key, m.screen, index)
		}
	}
}

func TestTabCyclesScreens(t *testing.T) {
	m := newTestModel(t)
	m.screen = screenStatus
	for expected := 1; expected <= int(screenCount); expected++ {
		_, _ = m.handleKey(keyMsg("tab"))
		if int(m.screen) != expected%int(screenCount) {
			t.Fatalf("after %d tabs the screen is %d", expected, m.screen)
		}
	}
}

func TestQuit(t *testing.T) {
	m := newTestModel(t)
	updated, cmd := m.handleKey(keyMsg("q"))
	if !updated.(*Model).quit {
		t.Error("q must set the quit flag")
	}
	if cmd == nil {
		t.Error("q must return the quit command")
	}
	if out := m.View(); out != "" {
		t.Errorf("a quit model must render nothing, got %q", out)
	}
}

func TestApplyIsRefusedWithoutAPlan(t *testing.T) {
	m := newTestModel(t)
	if m.result != nil {
		t.Fatal("the model should start without a plan")
	}
	_, _ = m.handleKey(keyMsg("a"))
	if m.confirmApply {
		t.Error("applying without a plan must not ask for confirmation")
	}
	if m.err == nil {
		t.Error("applying without a plan must explain why it did nothing")
	}
}

func TestConfirmationSwallowsOtherKeys(t *testing.T) {
	m := newTestModel(t)
	m.confirmApply = true
	m.screen = screenStatus

	// Any key that is not an explicit confirmation cancels, so a stray
	// keystroke can never result in a write.
	_, cmd := m.handleKey(keyMsg("x"))
	if m.confirmApply {
		t.Error("an unrelated key must cancel the confirmation")
	}
	if cmd != nil {
		t.Errorf("cancelling must not start work, got a command")
	}
	if m.err != nil {
		t.Errorf("cancelling is not an error, got %v", m.err)
	}
}

func TestScrollingStaysInBounds(t *testing.T) {
	m := newTestModel(t)
	m.items = make([]model.LibraryItem, 3)
	m.height = 12

	for i := 0; i < 20; i++ {
		_, _ = m.handleKey(keyMsg("down"))
	}
	start, end := m.visible(3)
	if start < 0 || end > 3 || start >= end {
		t.Errorf("visible range %d:%d is out of bounds for 3 rows", start, end)
	}
	for i := 0; i < 20; i++ {
		_, _ = m.handleKey(keyMsg("up"))
	}
	if m.listOffset != 0 {
		t.Errorf("scrolling up past the top left the offset at %d", m.listOffset)
	}
}

func TestStatusMessageIsShown(t *testing.T) {
	m := newTestModel(t)
	m.setStatus("hello")
	if !strings.Contains(m.View(), "hello") {
		t.Error("the footer must show the status message")
	}
	m.setError(context.Canceled)
	if !strings.Contains(m.View(), context.Canceled.Error()) {
		t.Error("the footer must show an error")
	}
}

func TestHeaderShowsBusyStage(t *testing.T) {
	m := newTestModel(t)
	m.busy = "planning"
	if !strings.Contains(m.header(), "planning") {
		t.Error("the header must show what is running")
	}
}

// keyMsg builds a key message the way Bubble Tea delivers one.
func keyMsg(key string) tea.KeyMsg {
	if len(key) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	switch key {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}
