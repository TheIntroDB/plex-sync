// Package cli is the command line and the entry point.
//
// It has two jobs: give scripts and cron everything the terminal interface can
// do, and own the process-level concerns (configuration, logging, exit codes)
// so every command behaves the same way.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/logging"
)

// Exit codes. They are stable because scripts branch on them.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitNeedsReview = 3 // a write was requested but not confirmed
)

// globals holds the flags that apply to every command.
type globals struct {
	configPath string
	stateDir   string
	logLevel   string
	logFormat  string
	jsonOut    bool
}

// Execute runs the command line and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewRoot()
	if err := root.ExecuteContext(ctx); err != nil {
		var silent *silentError
		if errors.As(err, &silent) {
			return silent.code
		}
		fmt.Fprintf(os.Stderr, "plex-sync: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// silentError carries an exit code for a condition that has already been
// explained to the user, so the message is not printed twice.
type silentError struct {
	code int
	err  error
}

func (e *silentError) Error() string {
	if e.err == nil {
		return "exit"
	}
	return e.err.Error()
}
func (e *silentError) Unwrap() error { return e.err }

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	g := &globals{}

	root := &cobra.Command{
		Use:   "plex-sync",
		Short: "Skip intros, recaps and credits in Plex with TheIntroDB",
		Long: strings.TrimSpace(`
plex-sync fills in Plex's intro and credits markers from TheIntroDB, so Plex
does not have to fingerprint every file in your library.

Run it with no arguments in a terminal to get the interactive interface, or use
the commands below to script it. Nothing is written to your Plex database
without --yes.`),
		SilenceUsage:  true,
		SilenceErrors: true,
		// The interactive interface is the default when nothing else is asked
		// for, but only when there is a terminal to draw on.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !hasTerminal(cmd) {
				return cmd.Help()
			}
			return runTUI(cmd, g)
		},
	}

	flags := root.PersistentFlags()
	flags.StringVar(&g.configPath, "config", "", "path to the configuration file")
	flags.StringVar(&g.stateDir, "state-dir", "", "directory for the ledger, backups and undo journals")
	flags.StringVar(&g.logLevel, "log-level", "", "debug, info, warn or error")
	flags.StringVar(&g.logFormat, "log-format", "text", "log output format: text or json")
	flags.BoolVar(&g.jsonOut, "json", false, "print machine-readable output where supported")

	root.AddCommand(
		newVersionCmd(),
		newConfigCmd(g),
		newSetupCmd(g),
		newLibraryCmd(g),
		newPreviewCmd(g),
		newApplyCmd(g),
		newSyncCmd(g),
		newUndoCmd(g),
		newStatusCmd(g),
		newScheduleCmd(g),
		newTUICmd(g),
		newAPICmd(g),
	)
	return root
}

// loadConfig resolves the configuration from the file, the environment and the
// flags, and builds the logger.
func loadConfig(g *globals) (*config.Config, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil, err
	}
	if g.stateDir != "" {
		cfg.StateDir = g.stateDir
	}
	if g.logLevel != "" {
		cfg.LogLevel = strings.ToLower(g.logLevel)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// openApp resolves configuration and opens the shared runtime.
func openApp(g *globals, opts app.Options) (*app.App, error) {
	cfg, err := loadConfig(g)
	if err != nil {
		return nil, err
	}
	log := logging.New(cfg.LogLevel, g.logFormat, os.Stderr)
	return app.Open(cfg, log, opts)
}

// hasTerminal reports whether the command was started from a terminal. A
// container run or a cron job must never try to draw an interface.
func hasTerminal(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	out := cmd.OutOrStdout()
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// stdout is the writer command results go to.
func stdout(cmd *cobra.Command) io.Writer { return cmd.OutOrStdout() }

// loggingFor builds the logger for a resolved configuration.
func loggingFor(cfg *config.Config, g *globals) *slog.Logger {
	format := "text"
	if g != nil && g.logFormat != "" {
		format = g.logFormat
	}
	return logging.New(cfg.LogLevel, format, os.Stderr)
}
