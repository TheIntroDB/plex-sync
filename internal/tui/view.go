package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/TheIntroDB/plex-integration/internal/model"
)

var (
	styleTabActive = lipgloss.NewStyle().Bold(true).Padding(0, 1).Reverse(true)
	styleTabIdle   = lipgloss.NewStyle().Padding(0, 1).Faint(true)
	styleTitle     = lipgloss.NewStyle().Bold(true)
	styleDim       = lipgloss.NewStyle().Faint(true)
	styleGood      = lipgloss.NewStyle().Bold(true)
	styleBad       = lipgloss.NewStyle().Bold(true)
	styleWarn      = lipgloss.NewStyle()
	styleBox       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
	styleKey       = lipgloss.NewStyle().Bold(true)
)

// View renders the whole interface.
func (m *Model) View() string {
	if m.quit {
		return ""
	}
	var sections []string
	sections = append(sections, m.header())

	body := m.body()
	sections = append(sections, body)

	if m.confirmApply || m.confirmUndo {
		sections = append(sections, m.confirmation())
	} else {
		sections = append(sections, m.footer())
	}
	return strings.Join(sections, "\n")
}

func (m *Model) header() string {
	var tabs []string
	for i, name := range screenNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if screen(i) == m.screen {
			tabs = append(tabs, styleTabActive.Render(label))
		} else {
			tabs = append(tabs, styleTabIdle.Render(label))
		}
	}
	title := styleTitle.Render("TheIntroDB for Plex")
	if m.busy != "" {
		title += "  " + styleDim.Render("("+m.busy+")")
	}
	return title + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, tabs...) + "\n"
}

func (m *Model) body() string {
	switch m.screen {
	case screenStatus:
		return m.statusScreen()
	case screenLibrary:
		return m.libraryScreen()
	case screenPlan:
		return m.planScreen()
	case screenRuns:
		return m.runsScreen()
	case screenSettings:
		return m.settingsScreen()
	}
	return ""
}

// --- status ----------------------------------------------------------------

func (m *Model) statusScreen() string {
	var b strings.Builder
	cfg := m.app.Cfg

	b.WriteString(styleTitle.Render("Services") + "\n")
	if m.readiness == nil {
		b.WriteString("  " + styleDim.Render("checking...") + "\n")
	} else {
		b.WriteString("  Plex          " + m.ok(m.readiness.PlexOK, m.readiness.PlexVersion, m.readiness.PlexError) + "\n")
		status := "present"
		if cfg.TheIntroDB.APIKey == "" {
			status = "absent, 500 requests a day instead of 1000"
		}
		b.WriteString("  TheIntroDB    " + m.ok(m.readiness.TIDBOK, status, m.readiness.TIDBError) + "\n")
	}
	b.WriteString("  Plex database " + styleDim.Render(orUnknown(m.app.PlexDBPath(), "not configured")) + "\n")

	b.WriteString("\n" + styleTitle.Render("Requests") + "\n")
	used := 0
	if m.stats != nil {
		used = m.stats.RequestsToday
	}
	b.WriteString(fmt.Sprintf("  today         %d of %d (%.0f%%)\n",
		used, cfg.TheIntroDB.DailyBudget, percent(used, cfg.TheIntroDB.DailyBudget)))
	if m.usage.RemainingKnown {
		b.WriteString(fmt.Sprintf("  API reports   %d left\n", m.usage.Remaining))
	}
	if m.stats != nil {
		b.WriteString(fmt.Sprintf("  all time      %d request(s), %d lookup(s), %d hit(s)\n",
			m.stats.RequestsTotal, m.stats.Lookups, m.stats.LookupHits))
	}

	b.WriteString("\n" + styleTitle.Render("Markers written") + "\n")
	if m.stats == nil || m.stats.AppliedMarkers == 0 {
		b.WriteString("  " + styleDim.Render("none yet") + "\n")
	} else {
		b.WriteString(fmt.Sprintf("  %d marker(s) across %d item(s)\n",
			m.stats.AppliedMarkers, m.stats.AppliedItems))
	}
	if m.stats != nil && m.stats.LastRun != nil {
		b.WriteString(fmt.Sprintf("  last run      %s (%s, %s)\n",
			m.stats.LastRun.FinishedAt.Local().Format("2006-01-02 15:04"),
			m.stats.LastRun.Note, m.stats.LastRun.Status))
	} else {
		b.WriteString("  last run      " + styleDim.Render("never") + "\n")
	}

	b.WriteString("\n" + styleTitle.Render("Sources") + "\n")
	for _, name := range cfg.Sources.Ordered() {
		state := "off"
		if cfg.Sources.Enable(name) {
			state = "on"
		}
		note := ""
		switch name {
		case "theintrodb":
			note = "authoritative whenever it has data for a segment type"
		case "chapters":
			note = "chapter names Plex already extracted, free and exact for this file"
		case "detection":
			note = "local fingerprinting against a sibling episode, needs ffmpeg and fpcalc"
		}
		b.WriteString(fmt.Sprintf("  %-12s %-4s %s\n", name, state, styleDim.Render(note)))
	}
	return b.String()
}

// --- library ---------------------------------------------------------------

func (m *Model) libraryScreen() string {
	if len(m.items) == 0 {
		return styleDim.Render("Press l to read the library. Nothing is looked up until you plan.") + "\n"
	}
	start, end := m.visible(len(m.items))
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s  %d item(s)\n\n",
		styleTitle.Render("Library"), len(m.items)))
	b.WriteString(styleDim.Render(fmt.Sprintf("  %-46s %-22s %-10s %s",
		"item", "lookup key", "runtime", "markers")) + "\n")
	for _, item := range m.items[start:end] {
		key, ok := item.LookupKey()
		keyCell := key
		if !ok {
			keyCell = styleBad.Render("no id")
		}
		b.WriteString(fmt.Sprintf("  %-46s %-22s %-10s %s\n",
			truncate(item.Label(), 46),
			keyCell,
			formatRuntime(item.BestDuration()),
			m.markerState(item)))
	}
	return b.String()
}

// markerState describes what we know about an item's markers without looking
// anything up, so the library stays cheap to display.
func (m *Model) markerState(item model.LibraryItem) string {
	if markers, err := m.app.Ledger.Applied(int64(item.RatingKey)); err == nil && len(markers) > 0 {
		var kinds []string
		for _, marker := range markers {
			kinds = append(kinds, string(marker.Text))
		}
		return styleGood.Render("ours: " + strings.Join(dedupe(kinds), "+"))
	}
	return styleDim.Render("plex only or none")
}

// --- plan ------------------------------------------------------------------

func (m *Model) planScreen() string {
	if m.result == nil {
		return styleDim.Render("Press p to plan. A plan reads the library and asks TheIntroDB "+
			"for every item; nothing is written.") + "\n"
	}
	res := m.result
	work := res.Plan.Work()

	var b strings.Builder
	b.WriteString(styleTitle.Render("Plan") + "\n")
	b.WriteString(fmt.Sprintf("  examined %d item(s): %d with data, %d without, %d cached, %d lookup(s)\n",
		res.Survey.Items, res.Survey.WithData, res.Survey.NoData, res.Survey.Cached, res.Survey.Lookups))
	b.WriteString(fmt.Sprintf("  policy %s, %d item(s) to change\n\n",
		res.Plan.Options.Policy, len(work)))

	if len(work) == 0 {
		b.WriteString(styleGood.Render("  Nothing to do.") + "\n")
	} else {
		start, end := m.visible(len(work))
		for _, item := range work[start:end] {
			b.WriteString(fmt.Sprintf("  %-44s %-8s %s\n",
				truncate(item.Item.Label(), 44), item.Reason, describeItem(item)))
		}
		if end < len(work) {
			b.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more\n", len(work)-end)))
		}
	}

	if len(res.Survey.SkipReasons) > 0 {
		reasons := make([]string, 0, len(res.Survey.SkipReasons))
		for reason := range res.Survey.SkipReasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		b.WriteString("\n" + styleTitle.Render("Left alone") + "\n")
		for _, reason := range reasons {
			b.WriteString(fmt.Sprintf("  %-28s %d\n", strings.TrimPrefix(reason, "skip:"), res.Survey.SkipReasons[reason]))
		}
	}
	if len(res.Survey.Errors) > 0 {
		b.WriteString("\n" + styleTitle.Render("Problems") + "\n")
		for _, problem := range res.Survey.Errors {
			b.WriteString("  " + styleBad.Render(problem) + "\n")
		}
	}
	return b.String()
}

func describeItem(item model.ItemPlan) string {
	var parts []string
	for _, marker := range item.Add {
		parts = append(parts, fmt.Sprintf("%s %s-%s [%s]",
			marker.Text,
			model.FormatMS(marker.StartMS),
			model.FormatMS(marker.EndMS),
			marker.Source))
	}
	if len(item.Remove) > 0 {
		parts = append(parts, fmt.Sprintf("remove %d", len(item.Remove)))
	}
	return strings.Join(parts, "  ")
}

// --- runs ------------------------------------------------------------------

func (m *Model) runsScreen() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Runs") + "\n\n")
	if len(m.runs) == 0 {
		b.WriteString(styleDim.Render("  No runs recorded yet.") + "\n")
	} else {
		b.WriteString(styleDim.Render(fmt.Sprintf("  %-16s %-12s %-6s %6s %6s %6s %6s %5s",
			"finished", "kind", "status", "items", "added", "removed", "skipped", "err")) + "\n")
		start, end := m.visible(len(m.runs))
		for _, run := range m.runs[start:end] {
			b.WriteString(fmt.Sprintf("  %-16s %-12s %-6s %6d %6d %6d %6d %5d\n",
				run.FinishedAt.Local().Format("01-02 15:04:05"),
				truncate(run.Note, 12), run.Status,
				run.Items, run.Added, run.Removed, run.Skipped, run.Errors))
		}
	}

	journals, err := undoJournals(m.app.UndoDir())
	if err == nil && len(journals) > 0 {
		b.WriteString("\n" + styleTitle.Render("Undo journals") + "\n")
		b.WriteString("  " + styleDim.Render("u reverts the most recent one") + "\n")
		for i, journal := range journals {
			if i >= 5 {
				break
			}
			marker := "  "
			if i == len(journals)-1 {
				marker = styleKey.Render("> ")
			}
			b.WriteString(marker + filepath.Base(journal) + "\n")
		}
	}
	return b.String()
}

// --- settings --------------------------------------------------------------

func (m *Model) settingsScreen() string {
	cfg := m.app.Cfg
	path := cfg.Path
	if path == "" {
		path = "(defaults, environment and flags)"
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render("Configuration") + "\n\n")
	b.WriteString(fmt.Sprintf("  file            %s\n", path))
	b.WriteString(fmt.Sprintf("  state directory %s\n", cfg.StateDir))
	b.WriteString(fmt.Sprintf("  Plex            %s\n", cfg.Plex.URL))
	b.WriteString(fmt.Sprintf("  token           %s\n", setOrNot(cfg.Plex.Token != "")))
	b.WriteString(fmt.Sprintf("  database        %s\n", orUnknown(cfg.Plex.ResolvedDatabase(), "not configured")))
	b.WriteString(fmt.Sprintf("  TheIntroDB      %s\n", cfg.TheIntroDB.BaseURL))
	b.WriteString(fmt.Sprintf("  api key         %s\n", setOrNot(cfg.TheIntroDB.APIKey != "")))
	b.WriteString(fmt.Sprintf("  daily budget    %d requests, %.2f s apart\n",
		cfg.TheIntroDB.DailyBudget, cfg.TheIntroDB.MinDelay()))

	b.WriteString("\n" + styleTitle.Render("Segments") + "\n")
	for _, kind := range []string{"intro", "recap", "credits", "preview"} {
		state := "do not write"
		if cfg.Segments.Enabled(kind) {
			state = "write"
		}
		b.WriteString(fmt.Sprintf("  %-8s %s\n", kind, state))
	}
	b.WriteString(fmt.Sprintf("  recap folds into %s, preview folds into %s\n",
		fold(cfg.Segments.MapRecap, "intro"), fold(cfg.Segments.MapPreview, "credits")))

	b.WriteString("\n" + styleTitle.Render("Writing") + "\n")
	b.WriteString(fmt.Sprintf("  policy          %s\n", cfg.Apply.Policy))
	b.WriteString(fmt.Sprintf("  backup          %s, keeping %d\n", yesNo(cfg.Apply.Backup), cfg.Apply.KeepBackups))
	b.WriteString(fmt.Sprintf("  allow live      %s\n", yesNo(cfg.Apply.AllowLive)))
	b.WriteString(fmt.Sprintf("  PAL guard       %s\n", yesNo(cfg.Apply.PALGuard)))
	b.WriteString(fmt.Sprintf("  chunk size      %d item(s) per transaction\n", cfg.Apply.ChunkSize))

	b.WriteString("\n" + styleDim.Render("Edit the file above, or use environment variables, then restart.") + "\n")
	return b.String()
}

// --- chrome ----------------------------------------------------------------

func (m *Model) confirmation() string {
	var prompt string
	if m.confirmApply {
		work := 0
		added := 0
		if m.result != nil {
			work = len(m.result.Plan.Work())
			for _, item := range m.result.Plan.Work() {
				added += len(item.Add)
			}
		}
		prompt = fmt.Sprintf(
			"Write %d marker(s) across %d item(s) to the Plex database?", added, work)
	}
	if m.confirmUndo {
		prompt = "Revert the most recent run, restoring the previous marker rows?"
	}
	body := prompt + "\n\n" +
		styleDim.Render("The database is backed up first and every change is journalled.") + "\n\n" +
		styleKey.Render("y") + " confirm    " + styleKey.Render("n") + " cancel"
	return styleBox.Render(body)
}

func (m *Model) footer() string {
	var b strings.Builder
	if m.busy != "" && m.progress.Total > 0 {
		b.WriteString(fmt.Sprintf("  %s %d/%d %s\n", m.progress.Stage, m.progress.Done, m.progress.Total,
			truncate(m.progress.Label, 40)))
	}
	if m.status != "" {
		if m.err != nil {
			b.WriteString(styleBad.Render("  "+truncate(m.status, 120)) + "\n")
		} else {
			b.WriteString(styleDim.Render("  "+truncate(m.status, 120)) + "\n")
		}
	}
	keys := "1-5 screens   tab next   r refresh   l library   p plan   a apply   u undo   q quit"
	b.WriteString(styleDim.Render("  " + keys))
	return b.String()
}

func (m *Model) ok(good bool, note, problem string) string {
	if !good {
		return styleBad.Render("unreachable") + "  " + styleDim.Render(problem)
	}
	if note == "" {
		return styleGood.Render("ok")
	}
	return styleGood.Render("ok") + "  " + styleDim.Render(note)
}

// --- helpers ---------------------------------------------------------------

// undoJournals lists the journals in a directory, oldest first.
func undoJournals(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "undo-") && strings.HasSuffix(name, ".jsonl") {
			names = append(names, filepath.Join(dir, name))
		}
	}
	sort.Strings(names)
	return names, nil
}

func truncate(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(text) <= width {
		return text
	}
	if width == 1 {
		return "…"
	}
	return text[:width-1] + "…"
}

func percent(part, whole int) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

func formatRuntime(ms *int64) string {
	if ms == nil || *ms <= 0 {
		return "unknown"
	}
	total := *ms / 1000
	if total >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", total/3600, (total/60)%60, total%60)
	}
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func orUnknown(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func setOrNot(present bool) string {
	if present {
		return "set, hidden"
	}
	return "not set"
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func fold(enabled bool, target string) string {
	if !enabled {
		return "nothing"
	}
	return target
}

var _ = time.Now
