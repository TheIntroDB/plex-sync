// Package sync orchestrates a run: inventory, fetch, plan, apply.
//
// It is the only package that decides when something is written. The terminal
// interface, the command line and the local API all call into it, so every
// interface gets the same safety checks and the same plan.
package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/ledger"
	"github.com/TheIntroDB/plex-sync/internal/model"
	"github.com/TheIntroDB/plex-sync/internal/planfile"
	"github.com/TheIntroDB/plex-sync/internal/planner"
	"github.com/TheIntroDB/plex-sync/internal/plexdb"
	"github.com/TheIntroDB/plex-sync/internal/source"
	"github.com/TheIntroDB/plex-sync/internal/tidb"
)

// Options controls one run.
type Options struct {
	// Sections limits the run to these Plex library sections. Empty means all.
	Sections []int
	// Filter keeps only items whose label contains this text, case-insensitively.
	// This is how you pilot a run on one show before committing to a library.
	Filter string
	// Limit caps how many items are planned. Zero means no cap.
	Limit int
	// DryRun plans and reports but never opens the database for writing.
	DryRun bool
	// Confirm is the --yes flag. A write without it is refused.
	Confirm bool
	// Live allows a write while Plex is running, if nothing is streaming.
	Live bool
	// PlexStopped asserts that Plex is stopped, for when the tool cannot tell.
	PlexStopped bool
	// SkipSessionCheck allows a live write without checking for playback.
	SkipSessionCheck bool
	// NoBackup skips the pre-write backup. It is deliberately awkward to reach.
	NoBackup bool
	// Sources overrides the enabled alternate sources for this run.
	Sources []string
	// Progress receives stage and item updates. It may be nil.
	Progress func(Event)

	now func() time.Time
}

// Event is a progress update, used by the terminal interface.
type Event struct {
	Stage   string
	Label   string
	Done    int
	Total   int
	Message string
}

// Survey is what the inventory and fetch stages found.
type Survey struct {
	Sections     int
	Items        int
	Planned      int
	WithData     int
	NoData       int
	Unavailable  int
	Cached       int
	Lookups      int
	Requests     int
	SkipReasons  map[string]int
	Detected     int
	BudgetLeft   int
	BudgetKnown  bool
	Errors       []string
	ChapterItems int
}

// Result is the outcome of a plan or a run.
type Result struct {
	Plan       model.Plan
	Survey     Survey
	Stats      plexdb.WriteStats
	BackupPath string
	UndoPath   string
	// Applied reports whether the database was actually written.
	Applied bool
	// RunID is the ledger row for this run.
	RunID int64
}

// ErrNeedsConfirmation is returned when a write was asked for without --yes.
var ErrNeedsConfirmation = errors.New(
	"this would write to your Plex database; re-run with --yes to confirm")

// ErrPlexRunning is returned when Plex is up and a live write was not allowed.
var ErrPlexRunning = errors.New(
	"Plex is running; stop it first, or pass --live to write while nothing is playing")

// Runner performs runs against one application.
type Runner struct {
	app *app.App
	now func() time.Time
}

// New builds a runner.
func New(a *app.App) *Runner {
	return &Runner{app: a, now: time.Now}
}

// Now returns the runner's clock.
func (r *Runner) Now() time.Time { return r.now() }

// Inventory lists the library items a run would consider, without looking
// anything up. The library screen and `plex-sync library` use it.
func (r *Runner) Inventory(ctx context.Context, opts Options) ([]model.LibraryItem, error) {
	sections, err := r.app.Plex.Sections(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Plex libraries: %w", err)
	}
	sectionKeys := opts.Sections
	if len(sectionKeys) == 0 {
		for _, s := range sections {
			switch s.Type {
			case "movie", "show":
				sectionKeys = append(sectionKeys, s.Key)
			}
		}
	}
	items, err := r.app.Plex.Items(ctx, sectionKeys)
	if err != nil {
		return nil, fmt.Errorf("read the Plex library: %w", err)
	}
	return filterItems(items, opts), nil
}

// Plan surveys the library and computes what a run would change.
func (r *Runner) Plan(ctx context.Context, opts Options) (*Result, error) {
	started := r.now()
	cfg := r.app.Cfg

	sections, err := r.app.Plex.Sections(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Plex libraries: %w", err)
	}
	sectionKeys := opts.Sections
	if len(sectionKeys) == 0 {
		for _, s := range sections {
			switch s.Type {
			case "movie", "show":
				sectionKeys = append(sectionKeys, s.Key)
			}
		}
	}

	items, err := r.app.Plex.Items(ctx, sectionKeys)
	if err != nil {
		return nil, fmt.Errorf("read the Plex library: %w", err)
	}
	items = filterItems(items, opts)

	res := &Result{}
	res.Survey.Sections = len(sectionKeys)
	res.Survey.Items = len(items)
	res.Survey.SkipReasons = map[string]int{}

	// The existing markers are read from the database, which is the only place
	// they exist in full. Without database access the Plex API answers for one
	// item at a time, which is worth doing for a small pilot and not for a
	// library, so the fallback is used only when the database is unreadable.
	db, dbErr := r.app.PlexDB(true)
	var tagID int64
	if dbErr == nil {
		tagID, err = db.MarkerTagID()
		if err != nil {
			// A library that has never held a marker has no marker tag. That is
			// not an error while planning: the survey says so, and the apply
			// either creates the row or explains how to have it created. The
			// read-only handle stays open, because reading markers for existing
			// items still works from it.
			r.app.Log.Warn("no marker tag in the Plex database yet, so nothing is preserved as already marked",
				"error", err)
			db = nil
		}
	} else {
		r.app.Log.Warn("reading markers from the Plex API instead of the database",
			"error", dbErr)
	}

	seen := make([]model.LibraryItem, 0, len(items))
	existing := map[int][]model.ExistingMarker{}
	written := map[int][]model.Marker{}
	sets := map[int]map[model.SourceName]model.SegmentSet{}
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		r.emit(opts, Event{Stage: "survey", Label: item.Label(), Done: index, Total: len(items)})

		itemExisting, err := r.existingMarkers(ctx, db, tagID, item)
		if err != nil {
			res.Survey.Errors = append(res.Survey.Errors, item.Label()+": "+err.Error())
			res.Survey.Unavailable++
			continue
		}
		seen = append(seen, item)
		existing[item.RatingKey] = itemExisting

		if markers, err := r.app.Ledger.Applied(int64(item.RatingKey)); err == nil {
			written[item.RatingKey] = markers
		}

		itemSets := map[model.SourceName]model.SegmentSet{}

		// TheIntroDB first. Its answer is cached in the ledger, so repeat runs
		// cost nothing until the cache entry expires.
		if _, ok := item.LookupKey(); ok {
			body, look, err := r.app.TIDB.Lookup(ctx, item)
			res.Survey.Lookups++
			if err != nil {
				if tidb.IsTerminal(err) {
					// A rejected key or an item with no usable id: retrying in
					// this run cannot help, so stop rather than hammering it.
					return res, fmt.Errorf("%s: %w", item.Label(), err)
				}
				res.Survey.Errors = append(res.Survey.Errors, item.Label()+": "+err.Error())
			} else {
				if look.Cached {
					res.Survey.Cached++
				}
				res.Survey.BudgetLeft = look.Remaining
				res.Survey.BudgetKnown = look.RemainingKnown
				if look.Status == 200 && len(body.Segments) > 0 {
					itemSets[model.SourceTheIntroDB] = body
					res.Survey.WithData++
				} else if look.Status == 404 {
					res.Survey.NoData++
				}
			}
		}

		// Chapters, when enabled. Free and exact for this file, so it also
		// covers the item when TheIntroDB had nothing.
		if cfg.Sources.Enable("chapters") {
			if chapters, err := r.app.Plex.Chapters(ctx, item.RatingKey); err == nil {
				if set, ok := source.Chapters(item, chapters, *cfg); ok {
					itemSets[model.SourceChapters] = set
					res.Survey.ChapterItems++
				}
			} else {
				r.app.Log.Debug("chapters unavailable", "item", item.Label(), "error", err)
			}
		}
		if len(itemSets) > 0 {
			sets[item.RatingKey] = itemSets
		}
	}

	// Local detection runs last, only for episodes nothing else covered.
	if cfg.Sources.Enable("detection") {
		detected, err := r.detect(ctx, seen, sets, db, opts)
		if err != nil {
			res.Survey.Errors = append(res.Survey.Errors, "detection: "+err.Error())
		}
		res.Survey.Detected = detected
	}

	inputs := planner.Inputs{
		Sources:   sets,
		Existing:  existing,
		Written:   written,
		PALSpedUp: planner.PALSpedUp(seen),
	}
	res.Plan = planner.Build(seen, inputs, *cfg)
	res.Survey.Planned = len(res.Plan.Work())
	for _, item := range res.Plan.Items {
		if item.Skipped() {
			res.Survey.SkipReasons[item.Reason]++
		}
	}
	if usage := r.app.TIDB.Usage(); usage.Requests > 0 {
		res.Survey.Requests = usage.Requests
	}

	r.recordRun(started, "plan", res, nil)
	r.emit(opts, Event{Stage: "survey", Done: len(items), Total: len(items), Message: "planned"})
	return res, nil
}

// Apply writes a plan to the Plex database.
func (r *Runner) Apply(ctx context.Context, res *Result, opts Options) error {
	if res == nil {
		return errors.New("nothing to apply")
	}
	work := res.Plan.Work()
	if len(work) == 0 {
		r.app.Log.Info("nothing to do")
		return nil
	}
	if opts.DryRun {
		r.app.Log.Info("dry run: nothing written", "items", len(work))
		return nil
	}
	if err := r.Preflight(ctx, opts); err != nil {
		return err
	}

	cfg := r.app.Cfg
	db, err := r.app.PlexDB(false)
	if err != nil {
		return err
	}

	if cfg.Apply.Backup && !opts.NoBackup {
		path, err := plexdb.Backup(r.app.PlexDBPath(), cfg.BackupDir(), cfg.Apply.KeepBackups)
		if err != nil {
			return fmt.Errorf("back up the Plex database before writing: %w", err)
		}
		res.BackupPath = path
		r.app.Log.Info("backed up the Plex database", "path", path)
	}

	if err := os.MkdirAll(cfg.UndoDir(), 0o755); err != nil {
		return err
	}
	journalPath := filepath.Join(cfg.UndoDir(), "undo-"+r.now().UTC().Format("20060102T150405Z")+".jsonl")
	journal, err := plexdb.NewJournal(journalPath)
	if err != nil {
		return err
	}
	res.UndoPath = journalPath

	// The marker tag is resolved after the backup and after the journal, because
	// creating it is a write like any other and has to be as reversible as the
	// rest.
	tagID, err := db.MarkerTagID()
	if err != nil {
		if !cfg.Apply.CreateMissingMarkerTag {
			_ = journal.Close()
			return fmt.Errorf(
				"the Plex database has no marker tag, so a marker has nothing to attach to. "+
					"Plex makes that row the first time it writes a marker itself, which needs "+
					"Plex Pass, so on a server without it this tool makes the row instead: run "+
					"'plex-sync setup' to do it as a one-time step, or set "+
					"apply.create_missing_marker_tag = true (PLEX_SYNC_CREATE_MARKER_TAG=1) to "+
					"let every run do it. The row is created with the same backup and undo "+
					"journal as any other write: %w", err)
		}
		tagID, err = db.MarkerTagIDOrCreate(ctx, journal)
		if err != nil {
			_ = journal.Close()
			return err
		}
		r.app.Log.Warn("created the marker tag Plex had not made", "tag_id", tagID)
	}

	stats, err := db.ApplyPlans(work, tagID, cfg.Apply.ChunkSize, journal)
	if closeErr := journal.Close(); err == nil {
		err = closeErr
	}
	res.Stats = stats
	if err != nil {
		return fmt.Errorf(
			"writing to the Plex database failed after %d item(s); undo the earlier ones with "+
				"`plex-sync undo %s --yes`: %w", stats.Written, journalPath, err)
	}
	res.Applied = true

	// Remember what we wrote, so the next run can tell our markers from Plex's
	// own and can notice when Plex wipes them.
	for _, item := range work {
		if len(item.Desired) == 0 {
			if err := r.app.Ledger.ForgetApplied(int64(item.Item.RatingKey)); err != nil {
				r.app.Log.Warn("could not update the ledger", "item", item.Item.Label(), "error", err)
			}
			continue
		}
		if err := r.app.Ledger.ReplaceApplied(int64(item.Item.RatingKey), item.Desired); err != nil {
			r.app.Log.Warn("could not update the ledger", "item", item.Item.Label(), "error", err)
		}
	}

	r.app.Log.Info("wrote markers",
		"items", stats.Written, "added", stats.Added, "removed", stats.Removed,
		"skipped", stats.Skipped, "undo", journalPath)
	for _, reason := range stats.SkipReasons {
		r.app.Log.Warn("item skipped during the write", "reason", reason)
	}
	return nil
}

// Run plans and applies in one go.
func (r *Runner) Run(ctx context.Context, opts Options) (*Result, error) {
	started := r.now()
	res, err := r.Plan(ctx, opts)
	if err != nil {
		r.recordRun(started, "sync", res, err)
		return res, err
	}
	if err := r.Apply(ctx, res, opts); err != nil {
		r.recordRun(started, "sync", res, err)
		return res, err
	}
	r.recordRun(started, "sync", res, nil)
	return res, nil
}

// ApplyPlanFile applies a plan made earlier, from a file.
//
// This is the half of the work that needs no Plex: the plan already says what
// should change, so all that is left is the database. That is what lets a
// container apply a plan made elsewhere.
//
// The plan is a snapshot and is not trusted. Every item is reconciled against
// the rows actually in the database first, and anything that no longer looks the
// way the plan assumed is skipped rather than guessed at.
func (r *Runner) ApplyPlanFile(ctx context.Context, path string, opts Options) (*Result, error) {
	started := r.now()

	plan, meta, err := planfile.Load(path)
	if err != nil {
		r.recordRun(started, "apply-plan", nil, err)
		return nil, err
	}

	// The database it was made against is worth reporting, and not worth refusing
	// over: making a plan on the host and applying it in a container, where the same
	// database is mounted at a different path, is the point of plan files.
	if meta.Database != "" {
		if current := r.app.PlexDBPath(); current != "" && meta.Database != current {
			r.app.Log.Warn("this plan was made against a database at a different path",
				"made_against", meta.Database,
				"applying_to", current,
				"note", "every change is checked against the database before it is written")
		}
	}

	if age := meta.Age(r.now()); age > planMaxAge {
		r.app.Log.Warn("this plan is old",
			"made", meta.CreatedAt.Format(time.RFC3339),
			"age", age.Round(time.Hour).String(),
			"note", "each change is checked against the database before it is written")
	}
	r.app.Log.Info("applying a saved plan",
		"path", path,
		"items", len(plan.Work()),
		"made", meta.Describe())

	res := &Result{Plan: *plan}
	if err := r.Apply(ctx, res, opts); err != nil {
		r.recordRun(started, "apply-plan", res, err)
		return res, err
	}
	r.recordRun(started, "apply-plan", res, nil)
	return res, nil
}

// planMaxAge is when a plan is old enough to be worth mentioning. It is not a
// limit: the reconciliation is what makes an old plan safe, not the date.
const planMaxAge = 24 * time.Hour

// Undo reverts a journal.
func (r *Runner) Undo(ctx context.Context, journalPath string, opts Options) (int, error) {
	if _, err := os.Stat(journalPath); err != nil {
		return 0, fmt.Errorf("undo journal not readable: %w", err)
	}
	if opts.DryRun {
		ops, err := plexdb.ReadJournal(journalPath)
		if err != nil {
			return 0, err
		}
		r.app.Log.Info("dry run: nothing reverted", "operations", len(ops))
		return len(ops), nil
	}
	if err := r.Preflight(ctx, opts); err != nil {
		return 0, err
	}
	if _, err := r.app.PlexDB(false); err != nil {
		return 0, err
	}

	reverted, err := plexdb.Undo(r.app.PlexDBPath(), journalPath)
	if err != nil {
		return reverted, err
	}

	// Forget these items entirely, so the next plan does not mistake the
	// restored state for a wipe and re-apply immediately.
	ops, err := plexdb.ReadJournal(journalPath)
	if err == nil {
		touched := map[int64]bool{}
		for _, op := range ops {
			if key, ok := op["rating_key"].(float64); ok {
				touched[int64(key)] = true
			}
		}
		for ratingKey := range touched {
			if err := r.app.Ledger.ForgetApplied(ratingKey); err != nil {
				r.app.Log.Warn("could not update the ledger", "rating_key", ratingKey, "error", err)
			}
		}
	}
	r.app.Log.Info("reverted the journal", "operations", reverted, "journal", journalPath)
	return reverted, nil
}

// Preflight performs the checks that must pass before anything is written.
func (r *Runner) Preflight(ctx context.Context, opts Options) error {
	cfg := r.app.Cfg
	if !opts.Confirm {
		return ErrNeedsConfirmation
	}
	if _, err := r.app.Cfg.CheckDatabase(); err != nil {
		return err
	}

	running, err := r.app.Plex.Running(ctx)
	switch {
	case err != nil && !opts.PlexStopped && cfg.Apply.RequireStoppedConfirmation:
		return fmt.Errorf(
			"cannot tell whether Plex is running, so nothing will be written. "+
				"Check plex.url, or stop Plex and pass --plex-stopped: %w", err)
	case err != nil:
		// The configuration says a confirmation is not required, so carry on
		// with the user's assertion.
		running = false
	}

	if !running {
		return nil
	}
	if !cfg.Apply.AllowLive && !opts.Live {
		return ErrPlexRunning
	}
	sessions, err := r.app.Plex.ActiveSessions(ctx)
	if err != nil {
		if opts.SkipSessionCheck {
			r.app.Log.Warn("could not check for active Plex sessions; writing anyway", "error", err)
			return nil
		}
		return fmt.Errorf("could not read Plex sessions, so nothing will be written: %w", err)
	}
	if sessions > 0 {
		return fmt.Errorf(
			"%d Plex session(s) are playing; nothing will be written while someone is watching",
			sessions)
	}
	r.app.Log.Warn("writing while Plex is running, with nothing playing")
	return nil
}

// existingMarkers reads an item's current markers from the database, falling
// back to the Plex API when the database is not readable.
func (r *Runner) existingMarkers(
	ctx context.Context, db *plexdb.DB, tagID int64, item model.LibraryItem,
) ([]model.ExistingMarker, error) {
	if db != nil && tagID > 0 {
		return db.ReadMarkers(int64(item.RatingKey), tagID)
	}
	return r.app.Plex.Markers(ctx, item.RatingKey)
}

// recordRun stores the run in the ledger, so the history screen has something
// to show even when the process is killed later.
func (r *Runner) recordRun(started time.Time, kind string, res *Result, runErr error) {
	if r.app.Ledger == nil {
		return
	}
	run := ledger.Run{
		StartedAt:  started,
		FinishedAt: r.now(),
		Source:     "theintrodb",
		Status:     "ok",
		Note:       kind,
	}
	if runErr != nil {
		run.Status = "error"
		run.Note = kind + ": " + runErr.Error()
	}
	if res != nil {
		run.Items = res.Survey.Items
		run.Lookups = res.Survey.Lookups
		run.Hits = res.Survey.WithData
		run.NoData = res.Survey.NoData
		run.Misses = res.Survey.Lookups - res.Survey.Cached
		run.Added = res.Stats.Added
		run.Removed = res.Stats.Removed
		run.Skipped = res.Stats.Skipped
		run.Errors = len(res.Survey.Errors)
	}
	id, err := r.app.Ledger.RecordRun(run)
	if err == nil && res != nil {
		res.RunID = id
	}
	if err != nil {
		r.app.Log.Warn("could not record the run", "error", err)
	}
}

func (r *Runner) emit(opts Options, event Event) {
	if opts.Progress != nil {
		opts.Progress(event)
	}
}

// filterItems applies the run's section, filter and limit options.
func filterItems(items []model.LibraryItem, opts Options) []model.LibraryItem {
	out := make([]model.LibraryItem, 0, len(items))
	filter := strings.ToLower(strings.TrimSpace(opts.Filter))
	for _, item := range items {
		if filter != "" && !strings.Contains(strings.ToLower(item.Label()), filter) {
			continue
		}
		out = append(out, item)
	}
	// Most recently watched first: a run is usually triggered because something
	// is being watched now, and those are the items whose markers matter.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastViewedAt != out[j].LastViewedAt {
			return out[i].LastViewedAt > out[j].LastViewedAt
		}
		return out[i].Label() < out[j].Label()
	})
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out
}
