package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/schedule"
	"github.com/TheIntroDB/plex-sync/internal/sync"
)

// schedulerPID is the name of the PID file inside the state directory.
const schedulerPID = "scheduler.pid"

// schedulerPIDPath returns the full path to the scheduler PID file.
func schedulerPIDPath(stateDir string) string {
	return filepath.Join(stateDir, schedulerPID)
}

// schedulerRunning checks whether the scheduler process is alive by reading the
// PID file and sending signal 0.
func schedulerRunning(stateDir string) (bool, int) {
	raw, err := os.ReadFile(schedulerPIDPath(stateDir))
	if err != nil {
		return false, 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false, 0
	}
	// signal 0 tests whether the process exists without sending anything.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, 0
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false, 0
	}
	return true, pid
}

// writePID writes the current PID to the scheduler PID file.
func writePID(stateDir string) error {
	dir := filepath.Dir(schedulerPIDPath(stateDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(schedulerPIDPath(stateDir), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

// removePID removes the scheduler PID file when it belongs to this process.
func removePID(stateDir string) {
	pid := os.Getpid()
	if running, existingPID := schedulerRunning(stateDir); running && existingPID == pid {
		_ = os.Remove(schedulerPIDPath(stateDir))
	}
}

// newStartCmd runs the scheduler loop, writes a PID file, and cleans up on
// exit. It is the command to put in a README quickstart or a systemd unit.
func newStartCmd(g *globals) *cobra.Command {
	var (
		cronExpr     string
		plexStopped  bool
		skipSessions bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the scheduler for daily marker syncs",
		Long: strings.TrimSpace(`
Runs the sync on the schedule set in the configuration (default 07:30 daily).
The process holds its own schedule and writes a PID file to the state directory
so that "plex-sync stop" can signal it.

Equivalent to: plex-sync schedule --yes --run-on-start`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}

			// Check if already running.
			if running, pid := schedulerRunning(cfg.StateDir); running {
				return fmt.Errorf("scheduler is already running (PID %d); use 'plex-sync stop' first", pid)
			}

			// Write PID before starting, clean up on exit.
			if err := writePID(cfg.StateDir); err != nil {
				return fmt.Errorf("write PID file: %w", err)
			}
			defer removePID(cfg.StateDir)

			expr, err := scheduleExpression(cronExpr, cfg)
			if err != nil {
				return err
			}
			parsed, err := schedule.ParseCron(expr)
			if err != nil {
				return err
			}

			application, err := openApp(g, app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			log := application.Log
			log.Info("scheduler started",
				"every", parsed.Describe(),
				"timezone", time.Now().Location().String(),
				"pid", os.Getpid())

			runner := sync.New(application)
			options := sync.Options{
				Confirm:          true,
				PlexStopped:      plexStopped,
				SkipSessionCheck: skipSessions,
			}

			runOnce := func() {
				started := time.Now()
				result, err := runner.Run(cmd.Context(), options)
				if err != nil {
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

			// Run once on start.
			log.Info("running once on start")
			runOnce()

			// Then loop on the cron schedule.
			for {
				next := parsed.Next(time.Now())
				if next.IsZero() {
					return fmt.Errorf("the schedule %q never fires", expr)
				}
				log.Info("next run scheduled",
					"at", next.Format(time.RFC3339),
					"in", time.Until(next).Round(time.Second).String())

				timer := time.NewTimer(time.Until(next))
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					log.Info("scheduler stopped")
					return nil
				case <-timer.C:
					runOnce()
				}
			}
		},
	}

	cmd.Flags().StringVar(&cronExpr, "cron", "", "cron expression, five fields, in local time")
	cmd.Flags().BoolVar(&plexStopped, "plex-stopped", false, "assert that Plex is stopped")
	cmd.Flags().BoolVar(&skipSessions, "skip-session-check", false, "do not ask Plex whether anything is being watched")
	return cmd
}

// newStopCmd signals the running scheduler to stop.
func newStopCmd(g *globals) *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running scheduler",
		Long: strings.TrimSpace(`
Reads the PID file in the state directory and sends SIGTERM to the scheduler.
With --wait, it blocks until the process has exited and the PID file is gone.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}

			running, pid := schedulerRunning(cfg.StateDir)
			if !running {
				// Clean up a stale PID file if it exists.
				_ = os.Remove(schedulerPIDPath(cfg.StateDir))
				return fmt.Errorf("scheduler is not running")
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				_ = os.Remove(schedulerPIDPath(cfg.StateDir))
				return fmt.Errorf("scheduler not found at PID %d: %w", pid, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "stopping scheduler (PID %d)...\n", pid)
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("signal PID %d: %w", pid, err)
			}

			if wait {
				// Poll until the PID file is gone, up to 30 seconds.
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				for {
					if _, err := os.Stat(schedulerPIDPath(cfg.StateDir)); os.IsNotExist(err) {
						fmt.Fprintln(cmd.OutOrStdout(), "scheduler stopped.")
						return nil
					}
					select {
					case <-ctx.Done():
						return fmt.Errorf("timed out waiting for scheduler to stop (PID %d)", pid)
					case <-time.After(200 * time.Millisecond):
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the scheduler has exited")
	return cmd
}

// schedulerInfo returns a human-readable status line about the scheduler.
// Not a method so it works outside the TUI too.
func schedulerInfo(stateDir string) (running bool, pid int, desc string) {
	running, pid = schedulerRunning(stateDir)
	if running {
		return true, pid, fmt.Sprintf("running (PID %d)", pid)
	}
	return false, 0, "stopped"
}