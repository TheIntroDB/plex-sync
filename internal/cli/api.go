package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-integration/internal/api"
	"github.com/TheIntroDB/plex-integration/internal/app"
)

func newAPICmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Run the local JSON control API",
		Long: `The API is JSON only: no web interface, no HTML, nothing for a browser.
   It exists so scripts and other machines can drive the same runs the terminal
   interface does, and it is described by the OpenAPI schema at /openapi.json.

   It has no authentication, so keep it on localhost.`,
	}
	cmd.AddCommand(apiServeCmd(g))
	return cmd
}

func apiServeCmd(g *globals) *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the control API until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			if addr == "" {
				addr = cfg.API.Addr
			}
			application, err := app.Open(cfg, loggingFor(cfg, g), app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			resolved, err := api.Address(addr)
			if err != nil {
				return err
			}
			if !isLoopback(resolved) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: the API has no authentication and is listening on %s\n", resolved)
			}
			return api.New(application).Run(cmd.Context(), resolved)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "host:port to listen on (default from the configuration)")
	return cmd
}

// isLoopback reports whether an address only accepts local connections.
func isLoopback(addr string) bool {
	host := addr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			host = addr[:i]
			break
		}
	}
	switch host {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}
