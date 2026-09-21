package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// header is written at the top of every file this tool generates.
//
// It says what the file is and what overrides it, because a generated file
// looks authoritative and is not: the environment wins over it, and so does a
// command-line flag.
const header = `# plex-sync configuration.
#
# Written by the tool. Values here are the effective ones at the time of
# writing, so anything set in the environment or on the command line is now
# recorded here too.
#
# Precedence, lowest to highest: these values, the environment, then flags.
# Environment variables that override the file: PLEX_URL, PLEX_TOKEN, PLEX_DB,
# PLEX_CONFIG_DIR, TIDB_API_KEY, TIDB_API_URL, TIDB_DAILY_BUDGET,
# PLEX_SYNC_STATE_DIR, PLEX_SYNC_LOG_LEVEL, PLEX_SYNC_SCHEDULE,
# PLEX_SYNC_CHAPTERS, PLEX_SYNC_DETECTION, PLEX_SYNC_ALLOW_LIVE,
# PLEX_SYNC_API_ADDR, PLEX_SYNC_API_ENABLED.

`

// forWriting returns a copy to encode, with the values that were discovered
// rather than chosen left blank.
//
// Two of them are found by looking at the machine: the Plex token and the Plex
// database. Writing either one down is wrong twice over. It stores a secret
// nobody asked to store, and it freezes a value that is only true right now: a
// token Plex later rotates would sit in the file looking authoritative, and the
// discovery that would have found the new one never runs, because a value in the
// file wins.
//
// A value that came from the file, the environment or a flag is kept, because
// then it was chosen, and clearing it would throw away what somebody typed.
func (c *Config) forWriting() Config {
	out := *c

	if token := out.Plex.Token; token != "" {
		probe := out.Plex
		probe.Token = ""
		if probe.ResolvedToken() == token {
			out.Plex.Token = ""
		}
	}
	if database := out.Plex.Database; database != "" {
		probe := out.Plex
		probe.Database = ""
		if probe.ResolvedDatabase() == database {
			out.Plex.Database = ""
		}
	}
	return out
}

// WritePath is where the configuration should be written: the file it was read
// from, or the platform's own location for a first run.
func (c *Config) WritePath() string {
	if c.Path != "" {
		return c.Path
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "plex-sync", "config.toml")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "plex-sync", "config.toml")
	}
	return "plex-sync.toml"
}

// Save writes the configuration to path, or to WritePath when path is empty,
// and records the path on the configuration.
//
// The file is written to a temporary name and renamed into place, so an
// interrupted save cannot leave a half-written config that the next run would
// fail to parse.
//
// It writes the values in memory, which are the effective ones: anything that
// came from the environment is stored as well. That is deliberate, because it is
// what the interface showed the user when they changed something.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.WritePath()
	}
	if path == "" {
		return fmt.Errorf("config: nowhere to write the configuration")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed

	if _, err := temp.WriteString(header); err != nil {
		_ = temp.Close()
		return fmt.Errorf("config: write: %w", err)
	}
	encoder := toml.NewEncoder(temp)
	// The struct is nested and every section is written, which is verbose but
	// means the file shows every setting rather than only the changed ones.
	if err := encoder.Encode(c.forWriting()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("config: encode: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("config: flush: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("config: close: %w", err)
	}
	// The file can hold an API key and a Plex token.
	if err := os.Chmod(tempName, 0o600); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	c.Path = path
	return nil
}
