package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-integration/internal/app"
	"github.com/TheIntroDB/plex-integration/internal/buildinfo"
	"github.com/TheIntroDB/plex-integration/internal/model"
	"github.com/TheIntroDB/plex-integration/internal/planfile"
	"github.com/TheIntroDB/plex-integration/internal/sync"
)

// runOptions builds the run options every command shares.
func runOptions(g *globals) sync.Options {
	return sync.Options{}
}

// addRunFlags registers the flags shared by plan, apply and sync.
func addRunFlags(cmd *cobra.Command, opts *sync.Options) {
	flags := cmd.Flags()
	flags.IntSliceVar(&opts.Sections, "section", nil, "limit to these Plex library section keys")
	flags.StringVar(&opts.Filter, "show", "", "only items whose title contains this text")
	flags.IntVar(&opts.Limit, "limit", 0, "plan at most this many items")
	flags.BoolVar(&opts.DryRun, "dry-run", false, "report what would happen without writing")
	flags.BoolVar(&opts.PlexStopped, "plex-stopped", false, "assert that Plex is stopped")
	flags.BoolVar(&opts.Live, "live", false, "allow writing while Plex runs and nothing is playing")
	flags.BoolVar(&opts.SkipSessionCheck, "skip-session-check", false, "skip the active session check")
	flags.BoolVar(&opts.NoBackup, "no-backup", false, "skip the pre-write database backup")
}

// --- library ---------------------------------------------------------------

func newLibraryCmd(g *globals) *cobra.Command {
	var (
		sectionFlag int
		filter      string
		limit       int
		withData    bool
	)
	cmd := &cobra.Command{
		Use:   "library",
		Short: "List library items and the ids used for lookups",
		Long: strings.TrimSpace(`
Lists what the tool sees: every movie and episode with a provider id, the ids
themselves, and the file length that lookups are made cut-aware with.

Items without an id are listed too, because a missing id is the usual reason an
item never gets markers.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := openApp(g, app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			opts := sync.Options{Filter: filter, Limit: limit}
			if sectionFlag > 0 {
				opts.Sections = []int{sectionFlag}
			}
			runner := sync.New(application)
			items, err := runner.Inventory(cmd.Context(), opts)
			if err != nil {
				return err
			}

			if g.jsonOut {
				encoder := json.NewEncoder(stdout(cmd))
				encoder.SetIndent("", "  ")
				return encoder.Encode(items)
			}
			out := stdout(cmd)
			matched, unmatched := 0, 0
			for _, item := range items {
				key, ok := item.LookupKey()
				if !ok {
					unmatched++
				} else {
					matched++
				}
				if withData && !ok {
					continue
				}
				fmt.Fprintf(out, "%-52s %-22s %s\n",
					truncate(item.Label(), 52), orDash(key), formatDuration(item.BestDuration()))
			}
			fmt.Fprintf(out, "\n%d item(s): %d with a provider id, %d without\n", len(items), matched, unmatched)
			if unmatched > 0 {
				fmt.Fprintln(out, "Items without a TMDb, IMDb or Tvdb id cannot be looked up. Set your agent to TMDb.")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&sectionFlag, "section", 0, "limit to this Plex library section key")
	cmd.Flags().StringVar(&filter, "show", "", "only items whose title contains this text")
	cmd.Flags().IntVar(&limit, "limit", 0, "list at most this many items")
	cmd.Flags().BoolVar(&withData, "matched-only", false, "only items that have a provider id")
	return cmd
}

// --- plan ------------------------------------------------------------------

func newPlanCmd(g *globals) *cobra.Command {
	opts := sync.Options{}
	var show int
	var savePath string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what a run would change, without changing anything",
		Long: strings.TrimSpace(`
Builds the change set and prints it. Nothing is written and no database lock is
taken. Every lookup it makes is recorded and cached, so a plan followed by a
sync does not pay for its requests twice.

With --save, the plan is also written to a file, which ` + "`apply --plan`" + ` can
write later without contacting Plex. That is how a container applies a plan made
on the host, where the library is reachable. The file records what made it and
against which database, and applying it checks both.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := openApp(g, app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			opts.DryRun = true
			runner := sync.New(application)
			res, err := runner.Plan(cmd.Context(), opts)
			if err != nil {
				return err
			}

			if savePath != "" {
				meta := planfile.Meta{
					CreatedAt: time.Now(),
					Tool:      "tidb-plex",
					Version:   buildinfo.Version,
					Database:  application.PlexDBPath(),
					Plex:      application.Cfg.Plex.URL,
				}
				if err := planfile.Save(savePath, &res.Plan, meta); err != nil {
					return err
				}
				if !g.jsonOut {
					fmt.Fprintf(stdout(cmd), "Saved %d item(s) to %s\n",
						len(res.Plan.Work()), savePath)
					fmt.Fprintf(stdout(cmd),
						"Write it later with: tidb-plex apply --plan %s --yes\n", savePath)
				}
			}

			if g.jsonOut {
				encoder := json.NewEncoder(stdout(cmd))
				encoder.SetIndent("", "  ")
				return encoder.Encode(res)
			}
			printPlan(cmd, res, show)
			return nil
		},
	}
	addRunFlags(cmd, &opts)
	cmd.Flags().IntVar(&show, "show-items", 25, "how many items to print")
	cmd.Flags().StringVar(&savePath, "save", "", "also write the plan to this file, for `apply --plan`")
	return cmd
}

// --- apply -----------------------------------------------------------------

func newApplyCmd(g *globals) *cobra.Command {
	opts := sync.Options{}
	var yes bool
	var planPath string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Write the planned markers into the Plex database",
		Long: strings.TrimSpace(`
Plans and then writes. Requires --yes: without it the plan is printed and
nothing is written.

Before writing, the tool must confirm whether Plex is running. If Plex is up it
refuses unless --live is given, and then refuses again if anything is playing.
The database is backed up first, and every change is journalled so it can be
reverted with ` + "`tidb-plex undo`" + `.

With --plan, it writes the plan saved earlier by ` + "`plan --save`" + ` instead of
making a fresh one, and never contacts Plex at all. That is how a container
applies a plan made on the host. The saved plan is not trusted on its own: every
item is checked against the rows actually in the database first, and anything
that no longer matches is skipped rather than guessed at.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := openApp(g, app.Options{NeedPlexDB: !opts.DryRun})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			opts.Confirm = yes
			runner := sync.New(application)

			var res *sync.Result
			if planPath != "" {
				res, err = runner.ApplyPlanFile(cmd.Context(), planPath, opts)
			} else {
				if res, err = runner.Plan(cmd.Context(), opts); err == nil {
					if !g.jsonOut {
						printPlan(cmd, res, 10)
					}
					err = runner.Apply(cmd.Context(), res, opts)
				}
			}
			if err != nil {
				if err == sync.ErrNeedsConfirmation && !g.jsonOut {
					fmt.Fprintln(stdout(cmd))
					if planPath != "" {
						fmt.Fprintf(stdout(cmd),
							"Nothing was written. Re-run with --yes to apply %s.\n", planPath)
					} else {
						fmt.Fprintln(stdout(cmd),
							"Nothing was written. Re-run with --yes to apply the plan above.")
					}
					return &silentError{code: ExitNeedsReview}
				}
				return err
			}
			if g.jsonOut {
				encoder := json.NewEncoder(stdout(cmd))
				encoder.SetIndent("", "  ")
				return encoder.Encode(res)
			}
			if res.Applied {
				fmt.Fprintf(stdout(cmd),
					"\nWrote %d marker(s) across %d item(s), removed %d, skipped %d.\n",
					res.Stats.Added, res.Stats.Written, res.Stats.Removed, res.Stats.Skipped)
				if res.BackupPath != "" {
					fmt.Fprintf(stdout(cmd), "Backup:     %s\n", res.BackupPath)
				}
				fmt.Fprintf(stdout(cmd), "Undo with:  tidb-plex undo %s --yes\n", res.UndoPath)
			} else {
				fmt.Fprintln(stdout(cmd), "\nNothing to do.")
			}
			return nil
		},
	}
	addRunFlags(cmd, &opts)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm writing to the Plex database")
	cmd.Flags().StringVar(&planPath, "plan", "", "apply a plan saved by `plan --save` instead of making one (needs no Plex)")
	return cmd
}

// --- sync ------------------------------------------------------------------

func newSyncCmd(g *globals) *cobra.Command {
	opts := sync.Options{}
	var yes bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Inventory, fetch, plan and apply: the scheduled entry point",
		Long: strings.TrimSpace(`
The command to put in cron. It plans and, with --yes, applies in the same run.

Because it can be scheduled, it fails closed: if Plex is running without --live,
if a playback session is active, or if the database path cannot be trusted, it
writes nothing and exits non-zero rather than doing something surprising.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := openApp(g, app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			opts.Confirm = yes
			runner := sync.New(application)
			res, err := runner.Run(cmd.Context(), opts)
			if err != nil {
				if err == sync.ErrNeedsConfirmation {
					fmt.Fprintln(os.Stderr,
						"tidb-plex sync: nothing was written. Pass --yes to allow writes.")
					return &silentError{code: ExitNeedsReview}
				}
				return err
			}
			if g.jsonOut {
				encoder := json.NewEncoder(stdout(cmd))
				encoder.SetIndent("", "  ")
				return encoder.Encode(res)
			}
			out := stdout(cmd)
			fmt.Fprintf(out, "examined %d item(s), %d with data, %d without, %d lookup(s), %d cached\n",
				res.Survey.Items, res.Survey.WithData, res.Survey.NoData,
				res.Survey.Lookups, res.Survey.Cached)
			if res.Applied {
				fmt.Fprintf(out, "wrote %d marker(s) across %d item(s), removed %d, skipped %d\n",
					res.Stats.Added, res.Stats.Written, res.Stats.Removed, res.Stats.Skipped)
			} else {
				fmt.Fprintln(out, "nothing to do")
			}
			for _, problem := range res.Survey.Errors {
				fmt.Fprintf(out, "  ! %s\n", problem)
			}
			return nil
		},
	}
	addRunFlags(cmd, &opts)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm writing to the Plex database")
	return cmd
}

// --- undo ------------------------------------------------------------------

func newUndoCmd(g *globals) *cobra.Command {
	var yes, dryRun, live, plexStopped, skipSessionCheck bool
	cmd := &cobra.Command{
		Use:   "undo <journal>",
		Short: "Revert the changes recorded in an undo journal",
		Long: strings.TrimSpace(`
Replays a journal in reverse, restoring the exact rows that were there before.
Pass the path printed by an apply, or "latest" for the most recent journal.

Undoing an older journal after a newer run would overwrite the newer changes, so
only the most recent journal is accepted by name; anything else must be given
explicitly and knowingly.`),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := openApp(g, app.Options{NeedPlexDB: !dryRun})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			path := args[0]
			if path == "latest" {
				path, err = latestJournal(application.UndoDir())
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout(cmd), "journal: %s\n", path)
			}
			runner := sync.New(application)
			count, err := runner.Undo(cmd.Context(), path, sync.Options{
				Confirm:          yes,
				DryRun:           dryRun,
				Live:             live,
				PlexStopped:      plexStopped,
				SkipSessionCheck: skipSessionCheck,
			})
			if err != nil {
				if err == sync.ErrNeedsConfirmation && !dryRun {
					fmt.Fprintln(stdout(cmd), "Nothing was reverted. Re-run with --yes to confirm.")
					return &silentError{code: ExitNeedsReview}
				}
				return err
			}
			if dryRun {
				fmt.Fprintf(stdout(cmd), "would revert %d operation(s)\n", count)
			} else {
				fmt.Fprintf(stdout(cmd), "reverted %d operation(s)\n", count)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm the revert")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be reverted")
	cmd.Flags().BoolVar(&live, "live", false, "allow reverting while Plex runs and nothing is playing")
	cmd.Flags().BoolVar(&plexStopped, "plex-stopped", false, "assert that Plex is stopped")
	cmd.Flags().BoolVar(&skipSessionCheck, "skip-session-check", false, "skip the active session check")
	return cmd
}

// --- status ----------------------------------------------------------------

func newStatusCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the ledger, the request budget and recent runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := openApp(g, app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			stats, err := application.Ledger.Stats()
			if err != nil {
				return err
			}
			runs, err := application.Ledger.Runs(10)
			if err != nil {
				return err
			}
			if g.jsonOut {
				payload := map[string]any{
					"ledger":  stats,
					"runs":    runs,
					"usage":   application.TIDB.Usage(),
					"budget":  application.Cfg.TheIntroDB.EffectiveDailyBudget(),
					"sources": application.Cfg.Sources.Ordered(),
				}
				encoder := json.NewEncoder(stdout(cmd))
				encoder.SetIndent("", "  ")
				return encoder.Encode(payload)
			}

			out := stdout(cmd)
			usage := application.TIDB.Usage()
			fmt.Fprintf(out, "ledger            %s\n", stats.DatabasePath)
			fmt.Fprintf(out, "lookups           %d (%d hits, %d misses)\n",
				stats.Lookups, stats.LookupHits, stats.LookupMisses)
			fmt.Fprintf(out, "markers recorded  %d across %d item(s)\n",
				stats.AppliedMarkers, stats.AppliedItems)
			fmt.Fprintf(out, "requests today    %d of %d",
				stats.RequestsToday, application.Cfg.TheIntroDB.EffectiveDailyBudget())
			if usage.RemainingKnown {
				fmt.Fprintf(out, " (%d left according to the API)", usage.Remaining)
			}
			fmt.Fprintln(out)
			if stats.LastRun != nil {
				fmt.Fprintf(out, "last run          %s (%s, %s)\n",
					stats.LastRun.FinishedAt.Local().Format("2006-01-02 15:04"),
					stats.LastRun.Note, stats.LastRun.Status)
			} else {
				fmt.Fprintln(out, "last run          never")
			}

			if len(runs) > 0 {
				fmt.Fprintln(out, "\nrecent runs")
				for _, run := range runs {
					fmt.Fprintf(out, "  %s  %-12s %-6s items=%-5d add=%-4d rm=%-4d skip=%-4d err=%d\n",
						run.FinishedAt.Local().Format("01-02 15:04"), orDash(run.Note), run.Status,
						run.Items, run.Added, run.Removed, run.Skipped, run.Errors)
				}
			}
			return nil
		},
	}
	return cmd
}

// --- shared output helpers -------------------------------------------------

func printPlan(cmd *cobra.Command, res *sync.Result, show int) {
	out := stdout(cmd)
	fmt.Fprintf(out, "planned %d item(s) from %d examined: %d with data, %d without\n",
		res.Survey.Planned, res.Survey.Items, res.Survey.WithData, res.Survey.NoData)

	reasons := make([]string, 0, len(res.Survey.SkipReasons))
	for reason := range res.Survey.SkipReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(out, "  %-24s %d\n", reason, res.Survey.SkipReasons[reason])
	}

	work := res.Plan.Work()
	if len(work) == 0 {
		fmt.Fprintln(out, "\nNothing to do.")
		return
	}

	fmt.Fprintln(out)
	for i, item := range work {
		if show > 0 && i >= show {
			fmt.Fprintf(out, "  ... and %d more\n", len(work)-show)
			break
		}
		fmt.Fprintf(out, "%-46s %-8s %s\n",
			truncate(item.Item.Label(), 46), item.Reason, describeMarkers(item))
	}
	if stats := res.Plan.Stats; len(stats) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "totals: %d item(s) to change, %d marker(s) to add, %d to remove\n",
			res.Survey.Planned, countStats(stats, "markers:"), countRemovals(work))
	}
}

func describeMarkers(item model.ItemPlan) string {
	var parts []string
	for _, m := range item.Add {
		parts = append(parts, fmt.Sprintf("%s %s-%s[%s]",
			m.Text, model.FormatMS(m.StartMS), model.FormatMS(m.EndMS), m.Source))
	}
	if len(item.Remove) > 0 {
		parts = append(parts, fmt.Sprintf("remove %d", len(item.Remove)))
	}
	return strings.Join(parts, " ")
}

func countStats(stats map[string]int, prefix string) int {
	total := 0
	for key, value := range stats {
		if strings.HasPrefix(key, prefix) {
			total += value
		}
	}
	return total
}

func countRemovals(work []model.ItemPlan) int {
	total := 0
	for _, item := range work {
		total += len(item.Remove)
	}
	return total
}

func latestJournal(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no undo journals found in %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "undo-") && strings.HasSuffix(entry.Name(), ".jsonl") {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no undo journals found in %s", dir)
	}
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1]), nil
}

func truncate(text string, width int) string {
	if len(text) <= width {
		return text
	}
	if width <= 1 {
		return text[:width]
	}
	return text[:width-1] + "…"
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatDuration(ms *int64) string {
	if ms == nil || *ms <= 0 {
		return "unknown"
	}
	total := *ms / 1000
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total/60)%60, total%60)
}
