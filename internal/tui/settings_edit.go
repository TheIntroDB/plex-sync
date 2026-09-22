package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// moveCursor walks the settings selection, stopping at both ends rather than
// wrapping: wrapping from the last row to the first loses your place.
func (m *Model) moveCursor(delta int) {
	rows := settingsRows()
	if len(rows) == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > len(rows)-1 {
		m.cursor = len(rows) - 1
	}
}

// activateSetting acts on the selected row: enter toggles or cycles a row that
// has no free text, and opens an input line for one that does.
func (m *Model) activateSetting() (tea.Model, tea.Cmd) {
	rows := settingsRows()
	if len(rows) == 0 {
		return m, nil
	}
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	row := rows[m.cursor]

	switch row.kind {
	case settingBool:
		current := row.get(m.app.Cfg) == "true"
		next := fmt.Sprintf("%t", !current)
		if err := applySettingValue(m.app.Cfg, row, next); err != nil {
			m.setError(fmt.Errorf("%s: %w", row.key, err))
			return m, nil
		}
		return m, m.saveSetting(row, fmt.Sprintf("%s %s", row.label, onOff(!current)))

	case settingChoice:
		next := nextChoice(row, row.get(m.app.Cfg))
		if err := applySettingValue(m.app.Cfg, row, next); err != nil {
			m.setError(fmt.Errorf("%s: %w", row.key, err))
			return m, nil
		}
		return m, m.saveSetting(row, fmt.Sprintf("%s %s", row.label, next))

	default:
		// Free text, a number or a secret: open the input line.
		m.editing = true
		m.buffer = row.text(m.app.Cfg)
		m.setStatus(fmt.Sprintf("%s: type a value, enter to save, esc to cancel", row.key))
		return m, nil
	}
}

// handleEditKey deals with the keyboard while a value is being typed.
func (m *Model) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = false
		m.buffer = ""
		m.setStatus("cancelled, nothing changed")
		return m, nil
	case "enter":
		return m.commitEdit()
	case "backspace", "ctrl+h":
		m.buffer = dropLastRune(m.buffer)
		return m, nil
	case "ctrl+u":
		m.buffer = ""
		return m, nil
	case "ctrl+c":
		m.editing = false
		m.quit = true
		return m, tea.Quit
	}

	// Anything printable is text. Keys with no runes of their own, such as the
	// arrows, are ignored rather than inserted.
	//
	// Space is its own key type rather than a rune, and it has to be typed:
	// every Plex path on macOS runs through "Application Support".
	switch {
	case msg.Type == tea.KeySpace:
		m.buffer += " "
	case msg.Type == tea.KeyRunes && len(msg.Runes) > 0:
		m.buffer += string(msg.Runes)
	}
	return m, nil
}

// commitEdit applies what was typed, saving only if the configuration accepts it.
//
// A rejected value leaves the input line open with the reason on the footer,
// because making someone retype a path because they mistyped one character is
// worse than a moment of confusion.
func (m *Model) commitEdit() (tea.Model, tea.Cmd) {
	rows := settingsRows()
	if m.cursor >= len(rows) {
		m.editing = false
		m.buffer = ""
		return m, nil
	}
	row := rows[m.cursor]
	typed := m.buffer

	if err := applySettingValue(m.app.Cfg, row, typed); err != nil {
		m.setError(fmt.Errorf("%s: %w", row.key, err))
		return m, nil
	}

	m.editing = false
	m.buffer = ""

	shown := typed
	if row.kind == settingSecret {
		// Never put a secret in the status line, and never in the scrollback
		// either: it would sit there for the rest of the session.
		if strings.TrimSpace(typed) == "" {
			shown = "cleared"
		} else {
			shown = "set"
		}
	}
	if shown == "" {
		shown = "cleared"
	}
	return m, m.saveSetting(row, fmt.Sprintf("%s %s", row.label, shown))
}

// saveSetting writes the configuration and reports honestly what it means.
func (m *Model) saveSetting(row setting, what string) tea.Cmd {
	cfg := m.app.Cfg
	path := cfg.WritePath()

	if err := cfg.Save(""); err != nil {
		// The value is in memory, so the running interface stays consistent,
		// but the user has to be told it will not survive a restart.
		m.setError(fmt.Errorf("%s, but the file could not be written: %w", what, err))
		return nil
	}

	if needsRestart(row.key) {
		m.setStatus(fmt.Sprintf("%s, saved to %s. Restart to pick it up.", what, path))
		return nil
	}
	m.setStatus(fmt.Sprintf("%s, saved to %s. Applies to the next run.", what, path))
	return nil
}

// dropLastRune removes one character, not one byte: paths and keys can hold
// anything, and slicing bytes would corrupt a multi-byte character.
func dropLastRune(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	return string(runes[:len(runes)-1])
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
