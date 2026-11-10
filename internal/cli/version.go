// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 TheIntroDB

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-sync/internal/buildinfo"
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
