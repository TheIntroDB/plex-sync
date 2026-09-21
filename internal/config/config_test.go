package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreUsable(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the built-in defaults must validate: %v", err)
	}
	if cfg.Apply.Policy != "fill" {
		t.Errorf("policy = %q, want fill: replacing Plex's own markers must be opt-in", cfg.Apply.Policy)
	}
	if cfg.Apply.Backup != true {
		t.Error("backups must default on: the first write should be reversible")
	}
	if cfg.Apply.AllowLive {
		t.Error("writing while Plex runs must not be the default")
	}
	if !cfg.Segments.Intro || !cfg.Segments.Credits {
		t.Error("intro and credits markers are the point of the tool and must default on")
	}
	if cfg.Segments.Preview {
		t.Error("previews fold into credits and must default off")
	}
	if cfg.Sources.Chapters || cfg.Sources.Detection {
		t.Error("the alternate sources must default off")
	}
}

// The pacing floor is not cosmetic: the server ceiling is 30 requests per 10
// seconds, and pacing at exactly the ceiling produces 429s as soon as there is
// any jitter.
func TestPacingStaysUnderTheServerCeiling(t *testing.T) {
	cfg := Default()
	if cfg.TheIntroDB.MaxPerWindow > 30 {
		t.Fatalf("max_per_window = %d, above the server ceiling of 30", cfg.TheIntroDB.MaxPerWindow)
	}
	if delay := cfg.TheIntroDB.MinDelay(); delay < 0.3 {
		t.Errorf("min delay = %.2fs, too close to the ceiling", delay)
	}
}

func TestAnonymousClientBudgetsForTheAnonymousAllowance(t *testing.T) {
	anonymous := TheIntroDB{DailyBudget: 1000}
	if got := anonymous.EffectiveDailyBudget(); got != AnonymousDailyBudget {
		t.Errorf("without a key the budget is %d, want %d: budgeting for more means "+
			"discovering the limit by being rate-limited", got, AnonymousDailyBudget)
	}

	withKey := TheIntroDB{DailyBudget: 1000, APIKey: "abc"}
	if got := withKey.EffectiveDailyBudget(); got != 1000 {
		t.Errorf("with a key the budget is %d, want the configured 1000", got)
	}

	// A non-positive budget means "do not count", and must stay that way.
	uncounted := TheIntroDB{DailyBudget: 0}
	if got := uncounted.EffectiveDailyBudget(); got != 0 {
		t.Errorf("an uncounted budget became %d", got)
	}
}

func TestLoadReadsFileEnvironmentAndDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[plex]
url = "http://plex.local:32400"
token = "from-file"
database = "/media/plex/com.plexapp.plugins.library.db"

[segments]
preview = true

[apply]
policy = "prefer-theintrodb"

[sources]
chapters = true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// The environment wins over the file.
	t.Setenv("PLEX_TOKEN", "from-env")
	t.Setenv("TIDB_DAILY_BUDGET", "250")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Plex.URL != "http://plex.local:32400" {
		t.Errorf("url = %q, want the file value", cfg.Plex.URL)
	}
	if cfg.Plex.Token != "from-env" {
		t.Errorf("token = %q, want the environment to win over the file", cfg.Plex.Token)
	}
	if cfg.TheIntroDB.DailyBudget != 250 {
		t.Errorf("daily budget = %d, want the environment value 250", cfg.TheIntroDB.DailyBudget)
	}
	if !cfg.Segments.Preview {
		t.Error("preview must be settable from the file")
	}
	if cfg.Apply.Policy != "prefer-theintrodb" {
		t.Errorf("policy = %q, want the file value", cfg.Apply.Policy)
	}
	if !cfg.Sources.Chapters {
		t.Error("chapters must be settable from the file")
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[plex]\nur1 = \"typo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a misspelled key must be reported, not silently ignored")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error = %v, want it to name the unknown key", err)
	}
}

func TestLoadRejectsInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is not toml = = =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid TOML must be reported")
	}
}

// isolateConfigSearch points every config location CandidatePaths consults at
// empty temporary directories.
//
// Without this the test depends on the machine it runs on: anyone who has
// actually installed a config file gets a spurious failure. Set HOME, the
// platform config variable and the working directory, because between them they
// are what discovery is built from.
func isolateConfigSearch(t *testing.T) {
	t.Helper()
	t.Setenv(EnvConfig, "")

	home := t.TempDir()
	t.Setenv("HOME", home)                                  // macOS and Linux
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg")) // Linux
	t.Setenv("AppData", filepath.Join(home, "AppData"))     // Windows

	// The working directory is a candidate too, so run from somewhere empty.
	t.Chdir(t.TempDir())
}

// A container with no configuration file must still run. But naming a path that
// does not exist is a typo, and falling back to the defaults silently would hide
// it.
func TestMissingConfigFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("an explicitly named file must exist", func(t *testing.T) {
		_, err := Load(filepath.Join(dir, "does-not-exist.toml"))
		if err == nil {
			t.Fatal("a named config file that is missing must be reported")
		}
	})

	t.Run("no file at all is fine", func(t *testing.T) {
		isolateConfigSearch(t)

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("a container with no config file must still run: %v", err)
		}
		if cfg.Path != "" {
			t.Errorf("Path = %q, want empty when nothing was found", cfg.Path)
		}
		if cfg.Plex.URL != Default().Plex.URL {
			t.Error("defaults must apply when there is no file")
		}
	})
}

// Writing the Plex database through Unraid's shfs layer is a documented way to
// corrupt it, so the path is refused unless the user says otherwise.
func TestFusePathIsRefused(t *testing.T) {
	cfg := Default()
	cfg.Plex.Database = "/mnt/user/appdata/plex/Library/Application Support/Plex Media Server/com.plexapp.plugins.library.db"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a /mnt/user database path must be refused")
	}
	if !strings.Contains(err.Error(), "allow_fuse_path") {
		t.Errorf("error = %v, want it to name the override", err)
	}

	cfg.Plex.AllowFusePath = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("the override must permit it: %v", err)
	}
}

func TestValidationCatchesContradictions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"unknown policy", func(c *Config) { c.Apply.Policy = "overwrite" }, "policy"},
		{"pacing above the ceiling", func(c *Config) { c.TheIntroDB.MaxPerWindow = 40 }, "max_per_window"},
		{"zero budget", func(c *Config) { c.TheIntroDB.DailyBudget = 0 }, "daily_budget"},
		{"nonsense log level", func(c *Config) { c.LogLevel = "chatty" }, "log_level"},
		{"empty api address", func(c *Config) { c.API.Addr = "  " }, "api.addr"},
		{"zero chunk size", func(c *Config) { c.Apply.ChunkSize = 0 }, "chunk_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestSourceOrderPinsTheIntroDBFirst(t *testing.T) {
	cfg := Default()
	cfg.Sources.Order = []string{"detection", "chapters", "theintrodb"}
	order := cfg.Sources.Ordered()
	if len(order) != 3 || order[0] != "theintrodb" {
		t.Fatalf("order = %v, want theintrodb first: it is authoritative wherever it has data", order)
	}
	if !cfg.Sources.Enable("theintrodb") {
		t.Error("theintrodb is the primary source and cannot be disabled")
	}
	if cfg.Sources.Enable("chapters") {
		t.Error("chapters must be off unless asked for")
	}
}

func TestEnabledTypesFollowTheToggles(t *testing.T) {
	cfg := Default()
	types := strings.Join(cfg.Segments.EnabledTypes(), ",")
	if types != "intro,recap,credits" {
		t.Errorf("enabled types = %q, want intro,recap,credits", types)
	}
	cfg.Segments.Recap = false
	if strings.Contains(strings.Join(cfg.Segments.EnabledTypes(), ","), "recap") {
		t.Error("disabling a segment type must remove it")
	}
}

// The example written by `config init` must be a configuration the tool accepts.
func TestExampleConfigIsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(Example()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the example configuration must load: %v", err)
	}
	if cfg.StateDir == "" {
		t.Error("the example must set a state directory")
	}
}

func TestStatePathsLiveUnderTheStateDirectory(t *testing.T) {
	cfg := Default()
	cfg.StateDir = "/var/lib/plex-sync"
	for name, got := range map[string]string{
		"ledger":      cfg.LedgerPath(),
		"backups":     cfg.BackupDir(),
		"undo":        cfg.UndoDir(),
		"fingerprint": cfg.FingerprintDir(),
	} {
		if !strings.HasPrefix(got, "/var/lib/plex-sync") {
			t.Errorf("%s path %q escaped the state directory", name, got)
		}
	}
}

func TestCandidatePathsAreOrderedMostSpecificFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfig, filepath.Join(dir, "explicit.toml"))

	paths := CandidatePaths()
	if len(paths) == 0 {
		t.Fatal("expected candidate paths")
	}
	if paths[0] != filepath.Join(dir, "explicit.toml") {
		t.Errorf("first candidate = %q, want the explicit PLEX_SYNC_CONFIG value", paths[0])
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, "plex-sync.toml") {
		t.Error("the working directory must be considered")
	}

	// The per-user location follows the platform: XDG_CONFIG_HOME on Linux,
	// Application Support on macOS, and the equivalent on Windows.
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this platform: %v", err)
	}
	want := filepath.Join(configDir, "plex-sync", "config.toml")
	if !strings.Contains(joined, want) {
		t.Errorf("candidate paths %v do not include %q", paths, want)
	}
	if !strings.Contains(joined, filepath.Join(string(filepath.Separator), "etc", "plex-sync", "config.toml")) {
		t.Error("a system-wide location must be considered for service installs")
	}
}
