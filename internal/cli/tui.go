package cli

import (
	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/logging"
	"github.com/TheIntroDB/plex-sync/internal/tui"
)

func newTUICmd(g *globals) *cobra.Command {
	var (
		filter string
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Start the interactive terminal interface",
		Long: `Starts the interactive interface: status, library, plan, runs and settings,
   driven entirely from the keyboard. ` + "`plex-sync`" + ` with no arguments does the
same thing when it is run from a terminal.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return startTUI(cmd, g, filter, limit)
		},
	}
	cmd.Flags().StringVar(&filter, "show", "", "only items whose title contains this text")
	cmd.Flags().IntVar(&limit, "limit", 0, "plan at most this many items")
	return cmd
}

// runTUI is the default action of the bare command.
func runTUI(cmd *cobra.Command, g *globals) error {
	return startTUI(cmd, g, "", 0)
}

func startTUI(cmd *cobra.Command, g *globals, filter string, limit int) error {
	cfg, err := loadConfig(g)
	if err != nil {
		return err
	}
	// The interface owns the screen, so log output would corrupt it. Errors are
	// shown in the interface's own status line instead.
	application, err := app.Open(cfg, logging.Discard(), app.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = application.Close() }()

	return tui.Run(cmd.Context(), tui.Options{
		App:    application,
		Filter: filter,
		Limit:  limit,
	})
}
