package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// A Windows path in the generated file must survive being read back.
//
// This is the bug a Windows CI runner found: the example config put the state
// directory into a TOML basic string unescaped, so C:\Users\... was read as a
// unicode escape and `config init` produced a file the tool then refused to load.
// macOS and Linux passed because their paths have no backslashes.
//
// The path is passed in rather than discovered, so this runs the same way
// wherever it is run. Waiting for Windows to tell us was the mistake.
func TestAWindowsStateDirectorySurvivesTheFile(t *testing.T) {
	windowsPaths := []string{
		`C:\Users\someone\AppData\Local\plex-sync`,
		`D:\state\plex-sync`,
		`C:\Users\a"quoted"\plex-sync`,
		`C:\Users\someone\AppData\Local\plex-sync\`,
	}

	for _, stateDir := range windowsPaths {
		t.Run(stateDir, func(t *testing.T) {
			body := exampleConfig(stateDir)
			if !strings.Contains(body, "state_dir = ") {
				t.Fatal("the example does not set a state directory")
			}

			// The real check: the file has to parse.
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("the example does not load: %v", err)
			}
			if cfg.StateDir != stateDir {
				t.Errorf("state directory came back as %q, want %q", cfg.StateDir, stateDir)
			}
		})
	}
}

// The same for the values the encoder writes, since Save and the example are two
// different paths to the same file format.
func TestAWindowsPathSurvivesSave(t *testing.T) {
	cfg := Default()
	cfg.StateDir = `C:\Users\someone\AppData\Local\plex-sync`
	cfg.Plex.Database = `C:\Program Files\Plex\library.db`
	cfg.TheIntroDB.APIKey = `key\with\backslashes`

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("the saved file does not load: %v", err)
	}
	if loaded.StateDir != cfg.StateDir {
		t.Errorf("state dir = %q, want %q", loaded.StateDir, cfg.StateDir)
	}
	if loaded.Plex.Database != cfg.Plex.Database {
		t.Errorf("database = %q, want %q", loaded.Plex.Database, cfg.Plex.Database)
	}
	if loaded.TheIntroDB.APIKey != cfg.TheIntroDB.APIKey {
		t.Errorf("api key = %q, want %q", loaded.TheIntroDB.APIKey, cfg.TheIntroDB.APIKey)
	}
}

// tomlString must produce something the TOML parser accepts and returns
// unchanged, for anything a path can contain.
func TestTomlStringRoundTrips(t *testing.T) {
	for _, value := range []string{
		``,
		`plain`,
		`C:\Users\someone`,
		`with "quotes" inside`,
		"tab\there",
		"newline\nhere",
		`back\slash and "quote"`,
		`unicode: unicorn 🦄`,
		"control:\x01",
	} {
		encoded := "value = " + tomlString(value) + "\n"

		var decoded struct {
			Value string `toml:"value"`
		}
		if _, err := toml.Decode(encoded, &decoded); err != nil {
			t.Errorf("tomlString(%q) produced %q, which does not parse: %v", value, encoded, err)
			continue
		}
		if decoded.Value != value {
			t.Errorf("tomlString(%q) came back as %q", value, decoded.Value)
		}
	}
}

// The example is the file `config init` writes, so it has to load on the machine
// it was written on, whatever that machine is.
func TestTheExampleLoadsOnThisMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(Example()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the file config init writes does not load: %v", err)
	}
	if cfg.StateDir != defaultStateDir() {
		t.Errorf("state directory = %q, want the platform default %q",
			cfg.StateDir, defaultStateDir())
	}
}
