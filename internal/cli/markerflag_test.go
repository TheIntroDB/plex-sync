package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-sync/internal/config"
)

// Creating the initial marker tag is a flag and nothing else.
//
// It writes a row into Plex's schema and it is needed to write marker rows into a
// library that has never held one, which makes it a first-run and debug step
// rather than something a normal run does or a setting anybody reads their way
// to. These tests hold that line: the flag is on the commands that write, and the
// setting it used to be is inert even when a file or the environment still asks
// for it.

// TestTheFlagIsOnTheCommandsThatWrite. A preview writes nothing, so it does not
// need it; sync, apply and setup all can.
func TestTheFlagIsOnTheCommandsThatWrite(t *testing.T) {
	g := &globals{}
	for name, cmd := range map[string]func(*globals) *cobra.Command{
		"sync":    newSyncCmd,
		"apply":   newApplyCmd,
		"setup":   newSetupCmd,
		"preview": newPreviewCmd,
	} {
		if cmd(g).Flags().Lookup("force-create-initial-tag") == nil {
			t.Errorf("%s does not offer --force-create-initial-tag", name)
		}
	}
}

// TestTheOldSettingDoesNothing, so that a configuration file or an environment
// variable written when this was a setting cannot quietly start writing Plex's
// schema again. The key is still parsed -- the loader rejects unknown keys, and
// refusing to load a file over a removed option would be a worse failure than
// ignoring it.
func TestTheOldSettingDoesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[apply]
create_missing_marker_tag = true
`), 0o600); err != nil {
		t.Fatalf("write a configuration file: %v", err)
	}
	t.Setenv("PLEX_SYNC_CREATE_MARKER_TAG", "1")
	t.Setenv("PLEX_SYNC_STATE_DIR", t.TempDir())

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("a file carrying the old key must still load: %v", err)
	}
	if cfg.Apply.CreateMissingMarkerTag {
		t.Error("the old setting is still live, so a configuration file can still write Plex's schema")
	}
}
