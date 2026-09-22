package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/schedule"
	"github.com/TheIntroDB/plex-sync/internal/sync"
)

// newSetupCmd is the first-run walkthrough: what this machine looks like, the
// one change a library that has never held a marker needs, and what to do next.
//
// It exists because the first attempt at this tool left people to work out the
// order themselves, and the honest summary from a user was "installed it, booted
// up my server, and loaded an episode while monitoring the traffic. It never
// even called your api". Nothing was wrong with the tool's answers; it was
// waiting for a library that had never held a marker, and said so with a message
// about tag_type 12.
//
// So: one command that checks the configuration, makes the marker tag if the
// library needs one, and prints the schedule and the next command to run. It
// writes nothing without the same confirmation, backup and journal as any other
// run, and --dry-run reports without touching anything.
func newSetupCmd(g *globals) *cobra.Command {
	opts := sync.Options{}
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "One-time setup: check the configuration, then make the library writable",
		Long: strings.TrimSpace(`
Run this once after installing, with Plex stopped.

It checks the configuration and both services, then looks for the marker tag in
the Plex database. Plex creates that row the first time it writes a marker
itself, which needs Plex Pass, so on a server without it the row is never there
and markers have nothing to attach to. This makes the row, with the same backup
and undo journal as any other write, and prints the schedule to use afterwards.

Nothing is written unless the safety checks a normal run uses pass. Use
--dry-run to see the report without the write, and ` + "`undo latest`" + ` to reverse it.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := stdout(cmd)

			application, err := openApp(g, app.Options{})
			if err != nil {
				return fmt.Errorf("configuration is not usable: %w", err)
			}
			defer func() { _ = application.Close() }()
			cfg := application.Cfg

			// --- what this machine looks like ---------------------------------
			fmt.Fprintln(out, "configuration")
			if cfg.Path != "" {
				fmt.Fprintf(out, "  file              %s\n", cfg.Path)
			} else {
				fmt.Fprintln(out, "  file              none; the defaults are in use")
			}

			dbPath := cfg.Plex.ResolvedDatabase()
			switch {
			case dbPath == "":
				fmt.Fprintln(out, "  plex database     not found, and writing markers needs it")
			default:
				if _, err := os.Stat(dbPath); err != nil {
					fmt.Fprintf(out, "  plex database     MISSING (%s)\n", dbPath)
				} else {
					fmt.Fprintf(out, "  plex database     %s\n", dbPath)
				}
			}

			readiness := application.Ready(ctx)
			if readiness.PlexOK {
				fmt.Fprintf(out, "  plex server       ok (%s)\n", orNone(readiness.PlexVersion))
			} else {
				fmt.Fprintf(out, "  plex server       FAILED (%s)\n", readiness.PlexError)
			}
			if readiness.TIDBOK {
				detail := "no API key, which is allowed: requesting needs none"
				if cfg.TheIntroDB.APIKey != "" {
					detail = "API key accepted"
				}
				fmt.Fprintf(out, "  theintrodb        ok (%s)\n", detail)
			} else {
				fmt.Fprintf(out, "  theintrodb        FAILED (%s)\n", readiness.TIDBError)
			}
			fmt.Fprintf(out, "  state directory   %s\n", cfg.StateDir)

			// --- the marker tag ----------------------------------------------
			//
			// Only older Plex versions need it: their marker rows hang off this
			// tag, and current versions read a table of their own. So this is
			// reported rather than acted on, and made only when asked for.
			fmt.Fprintln(out)
			db, err := application.PlexDB(false)
			if err != nil {
				fmt.Fprintf(out, "marker tag          cannot be checked (%v)\n", err)
				return &silentError{code: ExitError}
			}

			if tagID, err := db.MarkerTagID(); err == nil {
				fmt.Fprintf(out, "marker tag          present (tag %d)\n", tagID)
			} else {
				fmt.Fprintln(out, "marker tag          absent")
				fmt.Fprintln(out, "                    Plex creates one when it writes a marker of its own, and")
				fmt.Fprintln(out, "                    only its older marker table needs it. Plex 1.43 and later")
				fmt.Fprintln(out, "                    read markers from a different table, so nothing is missing")
				fmt.Fprintln(out, "                    for those. For an older server, add the flag:")
				fmt.Fprintln(out, "                      plex-sync sync --yes --force-create-initial-tag")

				switch {
				case dryRun:
					fmt.Fprintln(out, "                    --dry-run, so nothing was created")
				case opts.ForceCreateInitialTag:
					if err := createMarkerTagForSetup(cmd, application, opts); err != nil {
						fmt.Fprintf(out, "                    FAILED: %v\n", err)
						return &silentError{code: ExitError}
					}
					fmt.Fprintln(out, "                    created, and reversible with `plex-sync undo latest --yes`")
				}
			}

			// --- the schedule -------------------------------------------------
			fmt.Fprintln(out)
			expr, err := scheduleExpression("", cfg)
			if err != nil {
				fmt.Fprintf(out, "schedule            not usable: %v\n", err)
				return &silentError{code: ExitError}
			}
			parsed, err := schedule.ParseCron(expr)
			if err != nil {
				fmt.Fprintf(out, "schedule            not usable: %v\n", err)
				return &silentError{code: ExitError}
			}
			fmt.Fprintf(out, "schedule            %s (in the configuration)\n", expr)
			when := time.Now()
			for i := 0; i < 3; i++ {
				when = parsed.Next(when)
				if when.IsZero() {
					break
				}
				fmt.Fprintf(out, "  next run          %s\n", when.Format(time.RFC3339))
			}
			fmt.Fprintln(out, "  to install        add this line to a crontab on the Plex host:")
			fmt.Fprintf(out, "                      %s %s schedule --yes\n", expr, selfPath())

			// --- what to do now -----------------------------------------------
			fmt.Fprintln(out)
			fmt.Fprintln(out, "next")
			fmt.Fprintln(out, "  write once now    plex-sync sync --yes")
			fmt.Fprintln(out, "                    (with Plex stopped, or --live while it runs)")
			fmt.Fprintln(out, "  see first         plex-sync preview --limit 20")
			if hasTerminal(cmd) {
				fmt.Fprintln(out, "  or the interface  plex-sync tui")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen without writing")
	// The one thing here that touches Plex's schema, and only older versions
	// need it: a debug and migration flag rather than a step to walk through.
	cmd.Flags().BoolVar(&opts.ForceCreateInitialTag, "force-create-initial-tag", false,
		"make the marker tag older Plex versions need, when the database has none (debug)")
	cmd.Flags().BoolVar(&opts.Live, "live", false, "allow writing while Plex runs and nothing is playing")
	cmd.Flags().BoolVar(&opts.PlexStopped, "plex-stopped", false, "assert that Plex is stopped")
	cmd.Flags().BoolVar(&opts.SkipSessionCheck, "skip-session-check", false, "skip the active session check")
	return cmd
}

// createMarkerTagForSetup makes the marker tag through the runner, so the setup
// command and the interface cannot drift apart in what they check. The gates --
// confirmation, preflight, backup, journal -- are the runner's, not this
// function's.
func createMarkerTagForSetup(cmd *cobra.Command, application *app.App, opts sync.Options) error {
	opts.Confirm = true
	runner := sync.New(application)

	id, journalPath, err := runner.EnsureMarkerTag(cmd.Context(), opts)
	if err != nil {
		return err
	}
	if journalPath == "" {
		fmt.Fprintf(stdout(cmd), "                    already present as tag %d\n", id)
		return nil
	}
	fmt.Fprintf(stdout(cmd), "                    created as tag %d\n", id)
	fmt.Fprintf(stdout(cmd), "                    undo journal %s\n", journalPath)
	return nil
}

// selfPath is the command to name in a crontab, which is this executable when it
// can be found and the bare program name when it cannot.
func selfPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "plex-sync"
}
