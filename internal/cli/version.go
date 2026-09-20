package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-integration/internal/buildinfo"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(stdout(cmd), buildinfo.String())
			return nil
		},
	}
}
