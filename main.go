// Command plex-sync fills in Plex's intro and credits markers from TheIntroDB.
package main

import (
	"os"

	"github.com/TheIntroDB/plex-sync/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
