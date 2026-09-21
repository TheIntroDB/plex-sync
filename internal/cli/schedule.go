package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-integration/internal/app"
	"github.com/TheIntroDB/plex-integration/internal/config"
	"github.com/TheIntroDB/plex-integration/internal/schedule"
	"github.com/TheIntroDB/plex-integration/internal/sync"
)

// newScheduleCmd runs sync on a timer inside this process.
//
// It exists so the container needs no cron daemon, no shell and no second
// process: the image is one static binary, and that binary holds its own
// schedule.
func newScheduleCmd(g *globals) *cobra.Command {
	var (
		cronExpr     string
		printNext    bool
		planPath     string
		runOnStart   bool
		once         bool
		yes          bool
		dryRun       bool
		live         bool
		plexStopped  bool
		skipSessions bool
	)

	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Run sync on a cron schedule, in this process",
		Long: `Run sync on a cron schedule, in this process.

The expression has the usual five fields, in local time:

    minute hour day-of-month month day-of-week

This is what a container runs. There is no cron daemon and no shell inside the
image: the process that does the work holds its own schedule.

A scheduled run confirms nothing by itself. Repeating a write on a timer is
exactly where a mistake compounds, so a schedule reads unless you have asked for
writes with --yes.

    tidb-plex schedule                 report what is missing, once a day
    tidb-plex schedule --yes            write it, once a day
    tidb-plex schedule --yes --live     write even while Plex is streaming
    tidb-plex schedule --once --yes     a single run, for systemd or launchd
    tidb-plex schedule --print-next     the next five run times, then exit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The config alone, without opening the library, the ledger or the
			// Plex database: --print-next must answer from the schedule and
			// nothing else, and TIDB_PLEX_SCHEDULE has to be reflected.
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			expr, err := scheduleExpression(cronExpr, cfg)
			if err != nil {
				return err
			}
			parsed, err := schedule.ParseCron(expr)
			if err != nil {
				return err
			}

			// Asking when it would run should not open anything.
			if printNext {
				when := time.Now()
				for i := 0; i < 5; i++ {
					when = parsed.Next(when)
					if when.IsZero() {
						return fmt.Errorf("the schedule %q never fires", expr)
					}
					if _, err := io.WriteString(cmd.OutOrStdout(),
						when.Format(time.RFC3339)+"\n"); err != nil {
						return err
					}
				}
				return nil
			}

			application, err := openApp(g, app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			log := application.Log
			log.Info("schedule started",
				"every", parsed.Describe(),
				"timezone", time.Now().Location().String())

			// Without --yes a scheduled run reports rather than writes. It
			// would otherwise be an error every night, and a timer that fails
			// nightly is a timer that gets ignored.
			if !yes && !dryRun {
				dryRun = true
				log.Info("no --yes given, so this run only reports what is missing")
			}

			runner := sync.New(application)
			options := sync.Options{
				Confirm:          yes,
				DryRun:           dryRun,
				Live:             live,
				PlexStopped:      plexStopped,
				SkipSessionCheck: skipSessions,
			}

			runOnce := func() {
				started := time.Now()
				var (
					result *sync.Result
					err    error
				)
				if planPath != "" {
					result, err = runner.ApplyPlanFile(cmd.Context(), planPath, options)
				} else {
					result, err = runner.Run(cmd.Context(), options)
				}
				if err != nil {
					// A failed run must not end the schedule. A library that is
					// mid-scan, a server that is restarting or a rate limit that
					// has been reached are all reasons to wait for the next
					// firing, not to stop trying.
					log.Error("scheduled run failed",
						"error", err,
						"took", time.Since(started).Round(time.Second).String())
					return
				}
				log.Info("scheduled run finished",
					"planned", len(result.Plan.Items),
					"changed", result.Stats.Written,
					"markers_added", result.Stats.Added,
					"markers_removed", result.Stats.Removed,
					"applied", result.Applied,
					"took", time.Since(started).Round(time.Second).String())
			}

			if once {
				runOnce()
				return nil
			}

			if runOnStart || application.Cfg.Schedule.RunOnStart {
				log.Info("running once on start, as configured")
				runOnce()
			}

			for {
				next := parsed.Next(time.Now())
				if next.IsZero() {
					return fmt.Errorf("the schedule %q never fires, so there is nothing to wait for", expr)
				}
				log.Info("next run scheduled",
					"at", next.Format(time.RFC3339),
					"in", time.Until(next).Round(time.Second).String())

				timer := time.NewTimer(time.Until(next))
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					log.Info("schedule stopped")
					return nil
				case <-timer.C:
					runOnce()
				}
			}
		},
	}

	cmd.Flags().StringVar(&cronExpr, "cron", "", "cron expression, five fields, in local time")
	cmd.Flags().StringVar(&planPath, "plan", "", "apply a plan saved by `plan --save` instead of planning, so no Plex is needed")
	cmd.Flags().BoolVar(&printNext, "print-next", false, "print the next five run times and exit")
	cmd.Flags().BoolVar(&runOnStart, "run-on-start", false, "run once immediately, then wait for the first firing")
	cmd.Flags().BoolVar(&once, "once", false, "run a single time and exit, for a systemd or launchd timer")
	cmd.Flags().BoolVar(&yes, "yes", false, "write the markers that are missing")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	cmd.Flags().BoolVar(&live, "live", false, "allow writing while Plex is running")
	cmd.Flags().BoolVar(&plexStopped, "plex-stopped", false, "confirm Plex is stopped")
	cmd.Flags().BoolVar(&skipSessions, "skip-session-check", false, "do not ask Plex whether anything is being watched")

	return cmd
}

// scheduleExpression resolves the expression from the flag, then the config,
// then the default. Config may be nil when only the flag can apply.
func scheduleExpression(flag string, cfg *config.Config) (string, error) {
	if trimmed := strings.TrimSpace(flag); trimmed != "" {
		return trimmed, nil
	}
	if cfg != nil {
		if trimmed := strings.TrimSpace(cfg.Schedule.Cron); trimmed != "" {
			return trimmed, nil
		}
	}
	return config.DefaultSchedule, nil
}
