// Package tui is the interactive terminal interface.
//
// It is the default way to use the tool: a terminal, a keyboard, and no
// browser. Everything it does is also available as a command, so it never
// becomes the only way to reach a feature.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/ledger"
	"github.com/TheIntroDB/plex-sync/internal/model"
	"github.com/TheIntroDB/plex-sync/internal/sync"
	"github.com/TheIntroDB/plex-sync/internal/tidb"
)

// screen identifies a page of the interface.
type screen int

const (
	screenStatus screen = iota
	screenLibrary
	screenPlan
	screenRuns
	screenSettings
	screenCount
)

var screenNames = []string{"Status", "Library", "Preview", "Runs", "Settings"}

// Options configures the interface.
type Options struct {
	App *app.App
	// Filter and Limit mirror the command line's --show and --limit, so a
	// long run can be piloted from the interface too.
	Filter string
	Limit  int
}

// Run starts the interface and blocks until the user quits.
func Run(ctx context.Context, opts Options) error {
	if opts.App == nil {
		return fmt.Errorf("tui: no application")
	}
	m := newModel(ctx, opts)
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	m.program = program
	_, err := program.Run()
	return err
}

// Model is the application state.
type Model struct {
	ctx    context.Context
	app    *app.App
	runner *sync.Runner
	opts   Options

	program *tea.Program

	width  int
	height int
	screen screen
	quit   bool

	// busy is the stage currently running, empty when idle.
	busy     string
	progress sync.Event

	// Loaded data.
	readiness *app.Readiness
	stats     *ledger.Stats
	runs      []ledger.Run
	usage     tidb.Usage
	items     []model.LibraryItem
	result    *sync.Result

	// schedulerRunning is true when the scheduler PID file points at a live process.
	schedulerRunning bool

	// Confirmation state for the actions that touch the database.
	confirmApply bool
	confirmUndo  bool

	// status is the one-line message at the bottom of the screen.
	status  string
	statusT time.Time
	err     error

	// listOffset scrolls the library and plan lists.
	listOffset int
	// planCursor is the row the preview screen's selection is on, as an index
	// into the plan's work.
	planCursor int

	// pending counts the loads the current busy stage is waiting for, and busy
	// names that stage. A refresh is two loads (the ledger and the services),
	// everything else is one, so clearing the stage on whichever result arrived
	// first would stop saying "refreshing" while half of it was still running.
	pending int

	// cursor is the selected row on the settings screen.
	cursor int
	// editing is true while a setting's value is being typed. The keyboard
	// belongs to it while it is set, so a key like q types rather than quits.
	editing bool
	// buffer holds what has been typed so far.
	buffer string
}

func newModel(ctx context.Context, opts Options) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Model{
		ctx:    ctx,
		app:    opts.App,
		runner: sync.New(opts.App),
		opts:   opts,
		screen: screenStatus,
		status: "loading",
	}
}

// Messages.

type readinessMsg struct{ readiness app.Readiness }

type statsMsg struct {
	stats *ledger.Stats
	runs  []ledger.Run
	usage tidb.Usage
}

type schedulerMsg struct{ running bool }

type inventoryMsg struct {
	items []model.LibraryItem
	err   error
}

type planMsg struct {
	result *sync.Result
	err    error
}

type applyMsg struct {
	result *sync.Result
	err    error
}

type undoMsg struct {
	count int
	err   error
}

type progressMsg struct{ event sync.Event }

type statusMsg struct{ text string }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.loadStatus(), m.loadReadiness(), m.checkScheduler())
}

// --- commands --------------------------------------------------------------

func (m *Model) loadReadiness() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		return readinessMsg{readiness: m.app.Ready(ctx)}
	}
}

func (m *Model) loadStatus() tea.Cmd {
	return func() tea.Msg {
		out := statsMsg{usage: m.app.TIDB.Usage()}
		if stats, err := m.app.Ledger.Stats(); err == nil {
			out.stats = &stats
		}
		if runs, err := m.app.Ledger.Runs(25); err == nil {
			out.runs = runs
		}
		return out
	}
}

// checkScheduler reads the PID file and reports whether the scheduler is alive.
func (m *Model) checkScheduler() tea.Cmd {
	return func() tea.Msg {
		running, _ := schedulerRunning(m.app.Cfg.StateDir)
		return schedulerMsg{running: running}
	}
}

func (m *Model) loadInventory() tea.Cmd {
	return func() tea.Msg {
		items, err := m.runner.Inventory(m.ctx, sync.Options{
			Filter: m.opts.Filter,
			Limit:  m.opts.Limit,
		})
		return inventoryMsg{items: items, err: err}
	}
}

// runPlan plans. It reports progress into the program so the interface stays
// responsive: a plan is one lookup per item, which is minutes of work on a
// large library.
//
// Items marked for a re-scan on the preview screen are carried into the plan, so
// pressing p again is what makes a marked item actually be asked about again:
// nothing is re-scanned on its own.
func (m *Model) runPlan() tea.Cmd {
	return func() tea.Msg {
		opts := sync.Options{
			Filter: m.opts.Filter,
			Limit:  m.opts.Limit,
			DryRun: true,
			Progress: func(event sync.Event) {
				if m.program != nil {
					m.program.Send(progressMsg{event: event})
				}
			},
		}
		if m.result != nil && m.result.Plan.Selection != nil {
			opts.Rescan = append(opts.Rescan, m.result.Plan.Selection.Rescan...)
		}
		result, err := m.runner.Plan(m.ctx, opts)
		return planMsg{result: result, err: err}
	}
}

func (m *Model) runApply() tea.Cmd {
	return func() tea.Msg {
		if m.result == nil {
			return applyMsg{err: fmt.Errorf("plan first: press p")}
		}
		opts := sync.Options{
			Confirm:  true,
			DryRun:   false,
			Filter:   m.opts.Filter,
			Limit:    m.opts.Limit,
			Progress: func(event sync.Event) { m.sendProgress(event) },
		}
		// Only the items left on are written. The plan says what could change,
		// the preview screen's selection says what was agreed to.
		if selection := m.result.Plan.Selection; selection != nil {
			opts.Deselected = append(opts.Deselected, selection.Unselected...)
		}
		err := m.runner.Apply(m.ctx, m.result, opts)
		return applyMsg{result: m.result, err: err}
	}
}

func (m *Model) runUndo() tea.Cmd {
	return func() tea.Msg {
		journals, err := undoJournals(m.app.UndoDir())
		if err != nil {
			return undoMsg{err: err}
		}
		if len(journals) == 0 {
			return undoMsg{err: fmt.Errorf("there are no undo journals yet")}
		}
		// Only the newest journal is offered: reverting an older one after a
		// newer run would overwrite the newer changes.
		count, err := m.runner.Undo(m.ctx, journals[len(journals)-1], sync.Options{Confirm: true})
		return undoMsg{count: count, err: err}
	}
}

func (m *Model) sendProgress(event sync.Event) {
	if m.program != nil {
		m.program.Send(progressMsg{event: event})
	}
}

// --- update ----------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case readinessMsg:
		readiness := msg.readiness
		m.readiness = &readiness
		if !readiness.PlexOK {
			m.setError(fmt.Errorf("Plex did not answer: %s", readiness.PlexError))
		} else if !readiness.TIDBOK {
			m.setError(fmt.Errorf("TheIntroDB did not answer: %s", readiness.TIDBError))
		} else if m.status == "loading" {
			// Only on the way in. Reporting "ready" after a refresh would
			// overwrite the result of whatever the refresh was for.
			m.setStatus("ready")
		}
		m.finishLoad()
		return m, nil

	case statsMsg:
		m.stats = msg.stats
		m.runs = msg.runs
		m.usage = msg.usage
		m.finishLoad()
		return m, nil

	case schedulerMsg:
		m.schedulerRunning = msg.running
		m.finishLoad()
		return m, nil

	case inventoryMsg:
		m.finishLoad()
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.items = msg.items
		m.setStatus(fmt.Sprintf("%d item(s) in the library", len(msg.items)))
		return m, nil

	case progressMsg:
		m.progress = msg.event
		return m, nil

	case planMsg:
		m.finishLoad()
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		if msg.result == nil {
			// Nothing to show and nothing to explain it. Refusing is better
			// than a crash in the draw loop.
			m.setError(fmt.Errorf("planning returned nothing"))
			return m, nil
		}
		m.result = msg.result
		m.screen = screenPlan
		m.listOffset = 0
		m.planCursor = 0
		m.setStatus(fmt.Sprintf("plan ready: %d item(s) to change, %d selected",
			len(m.result.Plan.Work()), len(m.result.Plan.SelectedWork())))
		return m, m.loadStatus()

	case applyMsg:
		m.finishLoad()
		if msg.err != nil {
			m.setError(msg.err)
			return m, m.loadStatus()
		}
		stats := msg.result.Stats
		m.setStatus(fmt.Sprintf("wrote %d marker(s) across %d item(s), skipped %d",
			stats.Added, stats.Written, stats.Skipped))
		// The plan is stale now: Plex holds what it asked for, so the next
		// view of it would be misleading.
		m.result = nil
		return m, m.loadStatus()

	case undoMsg:
		m.finishLoad()
		if msg.err != nil {
			m.setError(msg.err)
			return m, m.loadStatus()
		}
		m.setStatus(fmt.Sprintf("reverted %d operation(s)", msg.count))
		return m, m.loadStatus()
	}
	return m, nil
}

// startLoad marks a stage busy and records how many results it will produce.
func (m *Model) startLoad(stage string, loads int) {
	m.busy = stage
	m.pending = loads
}

// finishLoad records that one of a stage's loads has come back, and clears the
// stage once they all have.
//
// Results are not tagged with the stage that started them, so a result arriving
// for a stage that has already ended is counted against whatever is running now.
// The cost is that the header can stop saying "running" slightly early; nothing
// else is lost, because a message still reports its own outcome either way. That
// is worth the alternative, threading a stage token through every message, which
// would be more machinery than an indicator deserves.
func (m *Model) finishLoad() {
	if m.busy == "" || m.pending <= 0 {
		return
	}
	m.pending--
	if m.pending > 0 {
		return
	}

	stage := m.busy
	m.busy = ""

	// Only claim success when the stage actually succeeded. setStatus clears any
	// error, so announcing "refreshed" after a failed check would wipe the
	// reason it failed and leave a service that is down looking fine.
	if stage == "refreshing" && m.err == nil {
		m.setStatus("refreshed")
	}
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Typing into a setting takes the whole keyboard, exactly as a confirmation
	// does: "q" has to type a q rather than quit, and "1" has to type a 1.
	if m.editing {
		return m.handleEditKey(msg)
	}

	// A confirmation takes the whole keyboard, so a stray key cannot cause a
	// write.
	if m.confirmApply || m.confirmUndo {
		switch strings.ToLower(msg.String()) {
		case "y", "enter":
			if m.confirmApply {
				m.confirmApply = false
				m.startLoad("writing", 1)
				m.setStatus("writing markers...")
				return m, m.runApply()
			}
			if m.confirmUndo {
				m.confirmUndo = false
				m.startLoad("reverting", 1)
				m.setStatus("reverting...")
				return m, m.runUndo()
			}
			return m, nil
		default:
			m.confirmApply = false
			m.confirmUndo = false
			m.setStatus("cancelled")
			return m, nil
		}
	}

	// On the settings screen the arrow keys move the selection rather than
	// scrolling, because every row is something you can act on.
	if m.screen == screenSettings {
		switch msg.String() {
		case "up", "k":
			m.moveCursor(-1)
			return m, nil
		case "down", "j":
			m.moveCursor(1)
			return m, nil
		case "home", "g":
			m.cursor = 0
			return m, nil
		case "end":
			m.cursor = len(settingsRows()) - 1
			return m, nil
		case "enter", " ":
			return m.activateSetting()
		case "esc":
			m.setStatus("")
			return m, nil
		}
	}

	// On the preview screen the arrow keys move a cursor over the items and
	// space turns the one under it on or off. That is what turns a plan from a
	// report into a decision: only the items left on are written.
	if m.screen == screenPlan && m.result != nil && m.planLen() > 0 {
		switch msg.String() {
		case "up", "k":
			m.movePlanCursor(-1)
			return m, nil
		case "down", "j":
			m.movePlanCursor(1)
			return m, nil
		case "home", "g":
			m.setPlanCursor(0)
			return m, nil
		case "end", "G":
			m.setPlanCursor(m.planLen() - 1)
			return m, nil
		case " ", "enter":
			m.togglePlanItem()
			return m, nil
		case "A":
			m.selectPlanItems(true)
			return m, nil
		case "N":
			m.selectPlanItems(false)
			return m, nil
		case "R":
			m.togglePlanRescan()
			return m, nil
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.quit = true
		return m, tea.Quit
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Derived from the digit rather than listed one by one, so that adding
		// a screen does not silently leave its number key selecting nothing --
		// or, worse, selecting a screen that is not the one printed beside it.
		if index := int(msg.String()[0] - '1'); index < int(screenCount) {
			m.screen = screen(index)
			m.listOffset = 0
		}
		return m, nil
	case "tab":
		m.screen = (m.screen + 1) % screenCount
		m.listOffset = 0
		return m, nil
	case "shift+tab":
		m.screen = (m.screen + screenCount - 1) % screenCount
		m.listOffset = 0
		return m, nil
	case "r":
		// Three loads: the ledger, the services and the scheduler status.
		m.startLoad("refreshing", 3)
		return m, tea.Batch(m.loadStatus(), m.loadReadiness(), m.checkScheduler())
	case "l":
		m.startLoad("reading the library", 1)
		m.screen = screenLibrary
		m.setStatus("reading the library...")
		return m, m.loadInventory()
	case "p":
		// The screen switches now, not when the run finishes. Building the
		// change set makes one lookup per item, which on a large library is
		// minutes, and until this moved the only feedback was a line of status
		// text -- reported as "pressing P to plan ain't shit happening".
		m.screen = screenPlan
		m.startLoad("planning", 1)
		m.setStatus("planning: this makes one lookup per item, so it can take a while")
		return m, m.runPlan()
	case "a":
		if m.result == nil || len(m.result.Plan.SelectedWork()) == 0 {
			m.setError(fmt.Errorf("nothing to apply: press p to plan first"))
			return m, nil
		}
		m.confirmApply = true
		return m, nil
	case "u":
		m.confirmUndo = true
		return m, nil
	case "down", "j":
		m.listOffset++
		return m, nil
	case "up", "k":
		if m.listOffset > 0 {
			m.listOffset--
		}
		return m, nil
	case "pgdown":
		m.listOffset += m.pageSize()
		return m, nil
	case "pgup":
		m.listOffset -= m.pageSize()
		if m.listOffset < 0 {
			m.listOffset = 0
		}
		return m, nil
	case "home", "g":
		m.listOffset = 0
		return m, nil
	case "?":
		m.setStatus("1-5 screens, tab next, r refresh, l library, p plan, a apply, u undo; " +
			"on Preview: up/down move, space select, A all, N none, R re-scan; " +
			"on Settings: up/down move, enter change, q quit")
		return m, nil
	}
	return m, nil
}

// --- the preview screen's selection ----------------------------------------

// planLen is how many rows the preview screen has to move over.
func (m *Model) planLen() int {
	if m.result == nil {
		return 0
	}
	return len(m.result.Plan.Work())
}

// setPlanCursor moves the preview cursor to a row and keeps it on screen.
func (m *Model) setPlanCursor(index int) {
	n := m.planLen()
	if n == 0 {
		m.planCursor = 0
		return
	}
	if index < 0 {
		index = 0
	}
	if index >= n {
		index = n - 1
	}
	m.planCursor = index
	m.keepPlanCursorVisible(n)
}

// movePlanCursor steps the preview cursor by delta, stopping at either end
// rather than wrapping, so a held key cannot run away from the row in view.
func (m *Model) movePlanCursor(delta int) {
	m.setPlanCursor(m.planCursor + delta)
}

// keepPlanCursorVisible scrolls the list so the selected row is on the page.
func (m *Model) keepPlanCursorVisible(n int) {
	size := m.pageSize()
	if m.planCursor < m.listOffset {
		m.listOffset = m.planCursor
	}
	if m.planCursor >= m.listOffset+size {
		m.listOffset = m.planCursor - size + 1
	}
	m.clampOffset(n)
}

// togglePlanItem turns the item under the cursor on or off. An item that is off
// stays in the preview and is simply not written, so nothing is hidden.
func (m *Model) togglePlanItem() {
	work := m.result.Plan.Work()
	if m.planCursor < 0 || m.planCursor >= len(work) {
		return
	}
	ratingKey := work[m.planCursor].Item.RatingKey
	selection := m.result.Plan.EnsureSelection()
	selection.Select(ratingKey, !selection.Selected(ratingKey))
	m.recountPlan()
}

// selectPlanItems turns every item in the plan on or off.
func (m *Model) selectPlanItems(selected bool) {
	if m.result == nil {
		return
	}
	selection := m.result.Plan.EnsureSelection()
	for _, item := range m.result.Plan.Work() {
		selection.Select(item.Item.RatingKey, selected)
	}
	m.recountPlan()
}

// togglePlanRescan marks the item under the cursor to be asked about again.
//
// The mark takes effect on the next plan, because that is when lookups happen:
// the status line says so rather than implying the answer has already changed.
func (m *Model) togglePlanRescan() {
	work := m.result.Plan.Work()
	if m.planCursor < 0 || m.planCursor >= len(work) {
		return
	}
	item := work[m.planCursor]
	key, ok := item.Item.LookupKey()
	if !ok {
		m.setError(fmt.Errorf("%s has no TMDb, IMDb or TVDb id, so it cannot be looked up",
			item.Item.Label()))
		return
	}
	selection := m.result.Plan.EnsureSelection()
	marked := !selection.RescanKeys()[key]
	selection.MarkRescan(key, marked)
	if marked {
		m.setStatus(fmt.Sprintf("marked %s for a re-scan: press p to plan again and ask for it",
			item.Item.Label()))
		return
	}
	m.setStatus(fmt.Sprintf("%s will not be re-scanned", item.Item.Label()))
}

// recountPlan reports how much of the plan is still selected.
func (m *Model) recountPlan() {
	work := m.result.Plan.Work()
	selected := len(m.result.Plan.SelectedWork())
	if leftOut := len(work) - selected; leftOut > 0 {
		m.setStatus(fmt.Sprintf("%d of %d item(s) selected, %d left out",
			selected, len(work), leftOut))
		return
	}
	m.setStatus(fmt.Sprintf("all %d item(s) selected", len(work)))
}

// --- small helpers ---------------------------------------------------------

func (m *Model) setStatus(text string) {
	m.status = text
	m.statusT = time.Now()
	m.err = nil
}

func (m *Model) setError(err error) {
	if err == nil {
		return
	}
	m.err = err
	m.status = err.Error()
	m.statusT = time.Now()
}

// pageSize is how many rows fit on the current screen.
func (m *Model) pageSize() int {
	if m.height <= 0 {
		return 15
	}
	size := m.height - 8
	if size < 3 {
		return 3
	}
	return size
}

// clampOffset keeps the scroll position valid for a list of n rows.
func (m *Model) clampOffset(n int) {
	max := n - m.pageSize()
	if max < 0 {
		max = 0
	}
	if m.listOffset > max {
		m.listOffset = max
	}
	if m.listOffset < 0 {
		m.listOffset = 0
	}
}

// visible slices a list to the current page.
func (m *Model) visible(n int) (start, end int) {
	m.clampOffset(n)
	start = m.listOffset
	end = start + m.pageSize()
	if end > n {
		end = n
	}
	return start, end
}
