// Command tidb-plex fills in Plex's intro and credits markers from TheIntroDB.
package main

import (
	"os"

	"github.com/TheIntroDB/plex-integration/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
