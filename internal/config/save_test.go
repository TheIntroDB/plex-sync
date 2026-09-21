package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A saved configuration has to load back as the same configuration, or the
// interface is writing files that break the next run.
func TestSaveRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	original := Default()
	original.Plex.URL = "http://192.168.1.10:32400"
	original.Plex.Token = "token-value"
	original.Plex.Database = "/plex/library.db"
	original.TheIntroDB.APIKey = "theintrodb:user:x:y"
	original.TheIntroDB.BaseURL = "https://api.theintrodb.org/v3"
	original.Segments.Preview = true
	original.Apply.AllowLive = true
	original.Apply.Policy = "prefer-theintrodb"
	original.Schedule.Cron = "0 4 * * *"
	original.LogLevel = "debug"

	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("the file it wrote does not load: %v", err)
	}

	if loaded.Plex.URL != original.Plex.URL {
		t.Errorf("plex url = %q, want %q", loaded.Plex.URL, original.Plex.URL)
	}
	if loaded.Plex.Token != original.Plex.Token {
		t.Errorf("plex token = %q, want %q", loaded.Plex.Token, original.Plex.Token)
	}
	if loaded.Plex.Database != original.Plex.Database {
		t.Errorf("database = %q, want %q", loaded.Plex.Database, original.Plex.Database)
	}
	if loaded.TheIntroDB.APIKey != original.TheIntroDB.APIKey {
		t.Errorf("api key = %q, want %q", loaded.TheIntroDB.APIKey, original.TheIntroDB.APIKey)
	}
	if !loaded.Segments.Preview {
		t.Error("segments.preview did not survive")
	}
	if !loaded.Apply.AllowLive {
		t.Error("apply.allow_live did not survive")
	}
	if loaded.Apply.Policy != "prefer-theintrodb" {
		t.Errorf("policy = %q", loaded.Apply.Policy)
	}
	if loaded.Schedule.Cron != "0 4 * * *" {
		t.Errorf("schedule.cron = %q", loaded.Schedule.Cron)
	}
	if loaded.LogLevel != "debug" {
		t.Errorf("log level = %q", loaded.LogLevel)
	}
}

// The file can hold an API key and a Plex token, so it must not be readable by
// everyone on the machine.
func TestSaveIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.TheIntroDB.APIKey = "secret"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestSaveRecordsThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	if cfg.Path != "" {
		t.Fatalf("a default configuration has a path: %q", cfg.Path)
	}

	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
	// A second save with no argument goes to the same place, rather than
	// jumping back to the platform default.
	cfg.LogLevel = "warn"
	if err := cfg.Save(""); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LogLevel != "warn" {
		t.Errorf("log level = %q, want the second save", reloaded.LogLevel)
	}
}

// Saving over an existing file must replace it, not append and not corrupt it.
func TestSaveReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	first := Default()
	first.Plex.URL = "http://one:32400"
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}

	second := Default()
	second.Plex.URL = "http://two:32400"
	if err := second.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plex.URL != "http://two:32400" {
		t.Errorf("url = %q, want the second save", loaded.Plex.URL)
	}

	// One file, and no temporary files left next to it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only config.toml", names)
	}
}

func TestSaveCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deeply", "nested", "config.toml")
	if err := Default().Save(path); err != nil {
		t.Fatalf("Save into a missing directory: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
}

// The file says what overrides it, because it looks authoritative and is not.
func TestSaveWritesAHeaderThatNamesTheOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Default().Save(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	for _, want := range []string{"PLEX_URL", "TIDB_API_KEY", "PLEX_SYNC_STATE_DIR", "Precedence"} {
		if !strings.Contains(text, want) {
			t.Errorf("the header does not mention %q", want)
		}
	}
	if !strings.HasPrefix(text, "#") {
		t.Error("the file does not start with a comment")
	}
}

// A token that was discovered must not be written down: it stores a secret
// nobody asked to store, and it freezes a value Plex can rotate.
func TestSaveDoesNotWriteADiscoveredToken(t *testing.T) {
	// Pretend the machine has a Plex install with a token in it.
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, ".LocalAdminToken"),
		[]byte("local-abc-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLEX_CONFIG_DIR", fake)

	cfg := Default()
	cfg.Plex.ConfigDir = fake
	// This is what app.Open does: fill in the token it found, so the run works.
	cfg.Plex.Token = cfg.Plex.ResolvedToken()
	if cfg.Plex.Token == "" {
		t.Skip("discovery found nothing on this machine to test with")
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), cfg.Plex.Token) {
		t.Error("the discovered token was written to the config file")
	}

	// And it still works afterwards, because discovery finds it again.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Plex.ResolvedToken() != cfg.Plex.Token {
		t.Error("the token is no longer discoverable after a save")
	}
}

// A token somebody actually typed is theirs, and must be kept.
func TestSaveKeepsATokenThatWasChosen(t *testing.T) {
	cfg := Default()
	cfg.Plex.Token = "typed-by-hand-token"

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plex.Token != "typed-by-hand-token" {
		t.Errorf("token = %q, want the one that was set", loaded.Plex.Token)
	}
}

func TestSaveKeepsAChosenDatabasePath(t *testing.T) {
	cfg := Default()
	cfg.Plex.Database = "/somewhere/chosen/library.db"

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plex.Database != "/somewhere/chosen/library.db" {
		t.Errorf("database = %q, want the chosen path", loaded.Plex.Database)
	}
}

func TestWritePathPrefersTheLoadedFile(t *testing.T) {
	cfg := Default()
	cfg.Path = "/somewhere/else/config.toml"
	if got := cfg.WritePath(); got != cfg.Path {
		t.Errorf("WritePath = %q, want the loaded file", got)
	}

	// With no file loaded it falls back to the platform's own location, which
	// is a place a first run can create.
	fresh := Default()
	got := fresh.WritePath()
	if got == "" {
		t.Fatal("WritePath is empty")
	}
	if !strings.HasSuffix(got, filepath.Join("plex-sync", "config.toml")) {
		t.Errorf("WritePath = %q, want a plex-sync/config.toml", got)
	}
}
