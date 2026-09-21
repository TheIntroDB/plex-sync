// Package config loads and validates the tool's configuration.
//
// Precedence, lowest to highest: built-in defaults, TOML file, environment
// variables, command-line flags. Every value has a usable default, so a first
// run needs nothing but a Plex URL and token, and often not even those.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/TheIntroDB/plex-sync/internal/schedule"
)

// EnvConfig names the variable that points at a config file.
const EnvConfig = "PLEX_SYNC_CONFIG"

// PlexDBName is the Plex database file name.
const PlexDBName = "com.plexapp.plugins.library.db"

// PlexDBSubpath is where the database actually lives inside Plex's application
// support directory. Plex has never put it at the top level: it is under
// "Plug-in Support/Databases", whatever the platform or the container image.
const PlexDBSubpath = "Plug-in Support/Databases/" + PlexDBName

// LocalAdminTokenFile is the per-install token newer Plex versions write. It is
// read before Preferences.xml because it always exists on a modern install and
// grants local API access without any cloud account.
const LocalAdminTokenFile = ".LocalAdminToken"

// fusePrefixes are mount points whose SQLite locking cannot be trusted.
// Writing the Plex database through Unraid's shfs/FUSE layer can corrupt it.
var fusePrefixes = []string{"/mnt/user/", "/mnt/user0/"}

// Config is the whole configuration tree.
type Config struct {
	Plex       Plex       `toml:"plex"`
	TheIntroDB TheIntroDB `toml:"theintrodb"`
	Sources    Sources    `toml:"sources"`
	Segments   Segments   `toml:"segments"`
	Apply      Apply      `toml:"apply"`
	API        API        `toml:"api"`
	Schedule   Schedule   `toml:"schedule"`
	StateDir   string     `toml:"state_dir"`
	LogLevel   string     `toml:"log_level"`

	// Path is where the config was loaded from, empty for defaults.
	Path string `toml:"-"`
}

// Plex configures how to reach Plex and where its database lives.
type Plex struct {
	URL   string `toml:"url"`
	Token string `toml:"token"`
	// Database is the full path to com.plexapp.plugins.library.db.
	Database string `toml:"database"`
	// ConfigDir is the application-support directory; Database is derived from
	// it when Database is empty.
	ConfigDir string  `toml:"config_dir"`
	TimeoutS  float64 `toml:"timeout_s"`
	// AllowFusePath permits a database under /mnt/user, which is refused by
	// default because SQLite locking there is unreliable.
	AllowFusePath bool `toml:"allow_fuse_path"`
	// InsecureSkipVerify is only for a self-signed local Plex.
	InsecureSkipVerify bool `toml:"insecure_skip_verify"`
}

// TheIntroDB configures the API client.
type TheIntroDB struct {
	BaseURL     string  `toml:"base_url"`
	APIKey      string  `toml:"api_key"`
	TimeoutS    float64 `toml:"timeout_s"`
	DailyBudget int     `toml:"daily_budget"`
	// MaxPerWindow is requests per WindowS. The server ceiling is 30 per 10s;
	// pacing at 25 leaves room for the request that lands while one is in flight.
	MaxPerWindow int     `toml:"max_per_window"`
	WindowS      float64 `toml:"window_s"`
	// MissTTLDays is how long a cached "no data" answer is trusted.
	MissTTLDays int `toml:"miss_ttl_days"`
	// HitTTLDays is how long a cached answer is trusted before a refresh.
	HitTTLDays int `toml:"hit_ttl_days"`
}

// Sources selects the alternate marker sources and their tuning.
type Sources struct {
	// Order is the priority order; theintrodb is always first.
	Order []string `toml:"order"`
	// Chapters uses chapter names Plex already extracted. Off by default.
	Chapters bool `toml:"chapters"`
	// Detection runs local fingerprint detection. Off by default.
	Detection bool `toml:"detection"`
	// FpcalcPath is the chromaprint binary; empty means look on PATH.
	FpcalcPath string `toml:"fpcalc_path"`
	// FFmpegPath is the ffmpeg binary; empty means look on PATH.
	FFmpegPath string `toml:"ffmpeg_path"`
	// DetectionMinSiblings is how many season siblings must agree.
	DetectionMinSiblings int `toml:"detection_min_siblings"`
	// DetectionSampleS is the audio window sampled per file, in seconds.
	DetectionSampleS int `toml:"detection_sample_s"`
	// DetectionToleranceS is how far a detection may sit from the reference.
	DetectionToleranceS float64 `toml:"detection_tolerance_s"`
	// DetectionWorkers bounds parallel file reads.
	DetectionWorkers int `toml:"detection_workers"`
}

// Segments selects which segment types become markers and how they map.
type Segments struct {
	Intro   bool `toml:"intro"`
	Recap   bool `toml:"recap"`
	Credits bool `toml:"credits"`
	Preview bool `toml:"preview"`
	// MapRecap folds a recap into the intro marker.
	MapRecap bool `toml:"map_recap"`
	// MapPreview folds a preview into the credits marker.
	MapPreview bool `toml:"map_preview"`
	// MinMarkerMS is the shortest marker worth writing.
	MinMarkerMS int64 `toml:"min_marker_ms"`
	// FinalSlackMS: a credits marker ending this close to the file's end
	// becomes the marker that raises Up Next.
	FinalSlackMS int64 `toml:"final_slack_ms"`
}

// Apply configures writes into the Plex database.
type Apply struct {
	// Policy is "fill" (keep what Plex detected) or "prefer-theintrodb".
	Policy string `toml:"policy"`
	// AllowLive writes while Plex runs. Refused unless no one is streaming.
	AllowLive bool `toml:"allow_live"`
	// RequireStoppedConfirmation refuses to write when the tool cannot tell
	// whether Plex is running.
	RequireStoppedConfirmation bool `toml:"require_stopped_confirmation"`
	Backup                     bool `toml:"backup"`
	KeepBackups                int  `toml:"keep_backups"`
	ChunkSize                  int  `toml:"chunk_size"`
	// PALGuard ignores community timings for PAL speed-up files.
	PALGuard bool `toml:"pal_guard"`
	// CreateMissingMarkerTag creates the marker tag when Plex has not made one.
	//
	// Off by default: inventing a schema row is a bigger step than writing a
	// marker, so it is taken deliberately. It is needed on a server without Plex
	// Pass, where Plex will never create the row itself because writing markers
	// is a Plex Pass feature.
	CreateMissingMarkerTag bool `toml:"create_missing_marker_tag"`
}

// API configures the local control API (JSON only, no web interface).
type API struct {
	Enabled bool   `toml:"enabled"`
	Addr    string `toml:"addr"`
}

// DefaultSchedule is when the container runs itself when nothing else is set.
//
// 07:30, because Plex runs its own maintenance at 07:00 and two things writing
// to the same database is what the backup and the transaction handling exist to
// avoid.
const DefaultSchedule = "30 7 * * *"

// Schedule configures the built-in timer.
type Schedule struct {
	// Cron is a five-field cron expression, evaluated in local time.
	Cron string `toml:"cron"`
	// RunOnStart runs once immediately, before waiting for the first firing.
	// Off by default: a container being recreated must not decide to write to
	// the library on its own.
	RunOnStart bool `toml:"run_on_start"`
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Plex: Plex{
			URL:      "http://127.0.0.1:32400",
			TimeoutS: 20,
		},
		TheIntroDB: TheIntroDB{
			BaseURL:      "https://api.theintrodb.org/v3",
			TimeoutS:     20,
			DailyBudget:  1000,
			MaxPerWindow: 25,
			WindowS:      10,
			MissTTLDays:  14,
			HitTTLDays:   30,
		},
		Sources: Sources{
			Order:                []string{"theintrodb", "chapters", "detection"},
			DetectionMinSiblings: 3,
			DetectionSampleS:     16,
			DetectionToleranceS:  5,
			DetectionWorkers:     2,
		},
		Segments: Segments{
			Intro:        true,
			Recap:        true,
			Credits:      true,
			Preview:      false,
			MapRecap:     true,
			MapPreview:   false,
			MinMarkerMS:  3000,
			FinalSlackMS: 2000,
		},
		Apply: Apply{
			Policy:                     "fill",
			AllowLive:                  false,
			RequireStoppedConfirmation: true,
			Backup:                     true,
			KeepBackups:                3,
			ChunkSize:                  200,
			PALGuard:                   true,
		},
		API: API{
			Enabled: true,
			Addr:    "127.0.0.1:8765",
		},
		Schedule: Schedule{
			Cron:       DefaultSchedule,
			RunOnStart: false,
		},
		StateDir: defaultStateDir(),
		LogLevel: "info",
	}
}

// defaultStateDir puts state next to the user's other application data.
func defaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "plex-sync")
	}
	return ".plex-sync"
}

// MinDelay is the minimum spacing between API requests.
func (t TheIntroDB) MinDelay() float64 {
	if t.MaxPerWindow <= 0 {
		return 0.4
	}
	return t.WindowS / float64(t.MaxPerWindow)
}

// AnonymousDailyBudget is the allowance without an API key: 500 per public IP
// per UTC day, against 1000 for an account.
const AnonymousDailyBudget = 500

// AccountDailyBudget is the allowance with an API key, and the highest any
// caller can have.
const AccountDailyBudget = 1000

// EffectiveDailyBudget is the allowance that actually applies.
//
// Budgeting for 1000 requests when only 500 are allowed means discovering the
// limit by being rate-limited for the rest of the day, so an anonymous client
// budgets for the anonymous allowance. A non-positive budget still means
// "uncounted".
func (t TheIntroDB) EffectiveDailyBudget() int {
	if t.DailyBudget <= 0 {
		return t.DailyBudget
	}
	if t.APIKey == "" && t.DailyBudget > AnonymousDailyBudget {
		return AnonymousDailyBudget
	}
	return t.DailyBudget
}

// ResolvedDatabase returns the Plex database path, or "" when none can be found.
//
// The database is not at the top of the application support directory: it lives
// under "Plug-in Support/Databases", whatever the platform or container image. A
// path given explicitly is used as-is, because a user who names a file means
// that file. With nothing configured, the machine is searched, so a local
// install works without any configuration at all.
func (p Plex) ResolvedDatabase() string {
	if p.Database != "" {
		return p.Database
	}
	if p.ConfigDir != "" {
		return filepath.Join(p.ConfigDir, PlexDBSubpath)
	}
	if dir := DiscoverPlexDir(); dir != "" {
		return filepath.Join(dir, PlexDBSubpath)
	}
	return ""
}

// ResolvedToken returns the configured token, or the one Plex keeps on this
// machine when none was configured.
func (p Plex) ResolvedToken() string {
	if p.Token != "" {
		return p.Token
	}
	dir := p.ConfigDir
	if dir == "" {
		dir = DiscoverPlexDir()
	}
	if token := ReadPlexToken(dir); token != "" {
		return token
	}
	// The last resort is the platform's own preferences store, which is outside
	// the application support directory.
	if runtime.GOOS == "darwin" {
		return tokenFromPlist()
	}
	return ""
}

// ReadPlexToken reads the token that belongs to one Plex application-support
// directory. It never looks outside it, so calling it with a directory can only
// ever return that installation's token.
//
// Two shapes are in the wild:
//
//   - ".LocalAdminToken", written by modern Plex versions. Preferred: it is
//     per-install, needs no cloud account, and is the only one present on a
//     fresh macOS install.
//   - "Preferences.xml", used by the Linux and Windows distributions and by the
//     container images.
func ReadPlexToken(configDir string) string {
	dir := strings.TrimSpace(configDir)
	if dir == "" {
		return ""
	}
	if raw, err := os.ReadFile(filepath.Join(dir, LocalAdminTokenFile)); err == nil {
		if token := strings.TrimSpace(string(raw)); token != "" {
			return token
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Preferences.xml"))
	if err != nil {
		return ""
	}
	for _, re := range plexTokenPatterns {
		if m := re.FindSubmatch(raw); len(m) > 1 && len(m[1]) > 0 {
			return string(m[1])
		}
	}
	return ""
}

// plexTokenPatterns match the shapes Plex has used for the server token in
// Preferences.xml, most likely first.
var plexTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`<Setting[^>]*id="PlexOnlineToken"[^>]*value="([^"]*)"`),
	regexp.MustCompile(`<Setting[^>]*value="([^"]*)"[^>]*id="PlexOnlineToken"`),
	regexp.MustCompile(`<PlexOnlineToken[^>]*value="([^"]*)"`),
	regexp.MustCompile(`PlexOnlineToken="([^"]*)"`),
}

// tokenFromPlist reads the token from the macOS preferences plist.
//
// There is no plist reader in the standard library and this is the last
// fallback on one platform, so it shells out rather than adding a dependency
// for a case that only applies to installs predating the local admin token.
func tokenFromPlist() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	plist := filepath.Join(home, "Library", "Preferences", "com.plexapp.plexmediaserver.plist")
	if _, err := os.Stat(plist); err != nil {
		return ""
	}
	for _, tool := range [][]string{
		{"/usr/libexec/PlistBuddy", "-c", "Print PlexOnlineToken", plist},
		{"defaults", "read", "com.plexapp.plexmediaserver", "PlexOnlineToken"},
	} {
		out, err := exec.Command(tool[0], tool[1:]...).Output()
		if err != nil {
			continue
		}
		// PlistBuddy quotes the value; defaults does not.
		token := strings.Trim(strings.TrimSpace(string(out)), `"`)
		if token != "" {
			return token
		}
	}
	return ""
}

// Enable reports whether a source is switched on.
func (s Sources) Enable(name string) bool {
	switch name {
	case "theintrodb":
		return true
	case "chapters":
		return s.Chapters
	case "detection":
		return s.Detection
	}
	return false
}

// Ordered returns the source names with theintrodb pinned first.
func (s Sources) Ordered() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range s.Order {
		switch n {
		case "theintrodb", "chapters", "detection":
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	for _, n := range []string{"theintrodb", "chapters", "detection"} {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	// TheIntroDB is authoritative wherever it has data, whatever the file says.
	for i, n := range out {
		if n == "theintrodb" && i != 0 {
			out = append([]string{"theintrodb"}, append(out[:i:i], out[i+1:]...)...)
			break
		}
	}
	return out
}

// Enabled reports whether a segment type is written.
func (s Segments) Enabled(t string) bool {
	switch t {
	case "intro":
		return s.Intro
	case "recap":
		return s.Recap
	case "credits":
		return s.Credits
	case "preview":
		return s.Preview
	}
	return false
}

// EnabledTypes lists the segment types that will be written.
func (s Segments) EnabledTypes() []string {
	var out []string
	for _, t := range []string{"intro", "recap", "credits", "preview"} {
		if s.Enabled(t) {
			out = append(out, t)
		}
	}
	return out
}

// LedgerPath is the SQLite file holding lookups, applied markers and runs.
func (c *Config) LedgerPath() string {
	return filepath.Join(c.StateDir, "plex-sync.db")
}

// BackupDir is where Plex database backups are written.
func (c *Config) BackupDir() string {
	return filepath.Join(c.StateDir, "backups")
}

// UndoDir is where the journals that make a write reversible are written.
func (c *Config) UndoDir() string {
	return filepath.Join(c.StateDir, "undo")
}

// FingerprintDir is where the local detection source keeps its fingerprints.
func (c *Config) FingerprintDir() string {
	return filepath.Join(c.StateDir, "fingerprints")
}

// TempDir is scratch space for decoding audio during detection.
func (c *Config) TempDir() string {
	return filepath.Join(c.StateDir, "tmp")
}

// CandidatePaths lists config file locations, most specific first.
func CandidatePaths() []string {
	var out []string
	if v := strings.TrimSpace(os.Getenv(EnvConfig)); v != "" {
		out = append(out, v)
	}
	if cwd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(cwd, "plex-sync.toml"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(dir, "plex-sync", "config.toml"))
	}
	out = append(out, "/etc/plex-sync/config.toml")
	return out
}

// FindFile returns the first config file that exists, or "".
func FindFile() string {
	for _, p := range CandidatePaths() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Load reads the configuration, applying environment overrides.
//
// A file that was discovered but is missing is fine: defaults plus the
// environment are a valid configuration, which is what a container run relies
// on. A file the caller named explicitly and that does not exist is an error,
// because naming a path means you meant it and a silent fallback would hide a
// typo.
func Load(path string) (*Config, error) {
	cfg := Default()
	explicit := path != ""
	if path == "" {
		path = FindFile()
	}
	cfg.Path = path

	if path != "" {
		if _, err := os.Stat(path); err != nil {
			if explicit {
				return nil, fmt.Errorf("config file: %w", err)
			}
			cfg.Path = ""
			path = ""
		}
	}

	if path != "" {
		md, err := toml.DecodeFile(path, cfg)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, 0, len(undecoded))
			for _, k := range undecoded {
				keys = append(keys, k.String())
			}
			return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv overlays the environment on top of file values.
func (c *Config) applyEnv() {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				*dst = b
			}
		}
	}
	integer := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*dst = n
			}
		}
	}

	str("PLEX_URL", &c.Plex.URL)
	str("PLEX_TOKEN", &c.Plex.Token)
	str("PLEX_DB", &c.Plex.Database)
	str("PLEX_CONFIG_DIR", &c.Plex.ConfigDir)
	boolean("PLEX_INSECURE", &c.Plex.InsecureSkipVerify)
	boolean("PLEX_ALLOW_FUSE_PATH", &c.Plex.AllowFusePath)

	str("TIDB_API_KEY", &c.TheIntroDB.APIKey)
	str("TIDB_API_URL", &c.TheIntroDB.BaseURL)
	integer("TIDB_DAILY_BUDGET", &c.TheIntroDB.DailyBudget)

	str("PLEX_SYNC_STATE_DIR", &c.StateDir)
	str("PLEX_SYNC_LOG_LEVEL", &c.LogLevel)
	str("PLEX_SYNC_API_ADDR", &c.API.Addr)
	boolean("PLEX_SYNC_API_ENABLED", &c.API.Enabled)

	boolean("PLEX_SYNC_CHAPTERS", &c.Sources.Chapters)
	boolean("PLEX_SYNC_DETECTION", &c.Sources.Detection)
	boolean("PLEX_SYNC_ALLOW_LIVE", &c.Apply.AllowLive)
	boolean("PLEX_SYNC_CREATE_MARKER_TAG", &c.Apply.CreateMissingMarkerTag)

	str("PLEX_SYNC_SCHEDULE", &c.Schedule.Cron)
	boolean("PLEX_SYNC_RUN_ON_START", &c.Schedule.RunOnStart)

	if v, ok := os.LookupEnv("PLEX_SYNC_SOURCES"); ok {
		c.Sources.Order = splitList(v)
	}
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks the configuration for contradictions and unsafe values.
func (c *Config) Validate() error {
	var problems []string

	if c.TheIntroDB.DailyBudget <= 0 {
		problems = append(problems, "theintrodb.daily_budget must be positive")
	}
	// An account allowance of 1000 is the highest anyone can have, so a bigger
	// number is not a bigger budget, it is a number that cannot be true. Left
	// unchecked it would sit in the file looking like an allowance nobody will
	// honour.
	if c.TheIntroDB.DailyBudget > AccountDailyBudget {
		problems = append(problems, fmt.Sprintf(
			"theintrodb.daily_budget must not exceed the ceiling of %d requests a day",
			AccountDailyBudget))
	}
	if c.TheIntroDB.MaxPerWindow > 30 {
		problems = append(problems,
			"theintrodb.max_per_window must stay at or below the server ceiling of 30")
	}
	if c.Apply.Policy != "fill" && c.Apply.Policy != "prefer-theintrodb" {
		problems = append(problems,
			`apply.policy must be "fill" or "prefer-theintrodb", got `+strconv.Quote(c.Apply.Policy))
	}
	if c.Apply.ChunkSize <= 0 {
		problems = append(problems, "apply.chunk_size must be positive")
	}
	if c.Segments.MinMarkerMS < 0 {
		problems = append(problems, "segments.min_marker_ms must not be negative")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems,
			`log_level must be one of debug, info, warn, error, got `+strconv.Quote(c.LogLevel))
	}
	if db := c.Plex.ResolvedDatabase(); db != "" && !c.Plex.AllowFusePath {
		for _, prefix := range fusePrefixes {
			if strings.HasPrefix(db, prefix) {
				problems = append(problems, fmt.Sprintf(
					"plex.database %q is under %s, where SQLite locking is unreliable and a "+
						"write can corrupt the database; point it at the pool path instead, or set "+
						"plex.allow_fuse_path to accept the risk", db, prefix))
			}
		}
	}
	if c.API.Enabled && strings.TrimSpace(c.API.Addr) == "" {
		problems = append(problems, "api.addr must not be empty when the API is enabled")
	}
	// A schedule that cannot be parsed would otherwise be discovered by the
	// container failing at some later point, or by never running at all.
	if strings.TrimSpace(c.Schedule.Cron) != "" {
		if _, err := schedule.ParseCron(c.Schedule.Cron); err != nil {
			problems = append(problems, "schedule.cron: "+err.Error())
		}
	} else {
		problems = append(problems, "schedule.cron must not be empty")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// CheckDatabase verifies that the Plex database exists and is usable for writes.
func (c *Config) CheckDatabase() (string, error) {
	path := c.Plex.ResolvedDatabase()
	if path == "" {
		return "", errors.New(
			"Plex database not found: set plex.database or plex.config_dir " +
				"(PLEX_DB / PLEX_CONFIG_DIR), or run `plex-sync config check`")
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Plex database not found at %s: %w", path, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("Plex database path %s is a directory", path)
	}
	if !c.Plex.AllowFusePath {
		for _, prefix := range fusePrefixes {
			if strings.HasPrefix(path, prefix) {
				return "", fmt.Errorf(
					"Plex database %s is under %s, where SQLite locking is unreliable; use the "+
						"pool path or set plex.allow_fuse_path", path, prefix)
			}
		}
	}
	return path, nil
}

// DiscoverPlexDir returns the first Plex application-support directory that
// actually holds a database, or "".
//
// The order is: the PLEX_CONFIG_DIR override, then the platform's own locations
// (see plexDirsForOS). A directory is only accepted when a database is really
// there, so a stale entry in the list costs nothing, and a user who keeps Plex
// somewhere unusual can point at it with the environment variable or with
// plex.config_dir / plex.database in the configuration file.
func DiscoverPlexDir() string {
	if v := strings.TrimSpace(os.Getenv("PLEX_CONFIG_DIR")); v != "" && plexDatabaseIn(v) {
		return v
	}
	return findPlexDir(PlatformPlexDirs())
}

// findPlexDir returns the first candidate that really holds a database.
//
// The candidate list is a parameter so the search can be tested against each
// platform's list on any machine. Discovery itself is the platform's list, but
// the part that decides whether a directory counts must not depend on the host
// it was compiled for: that is how a test that only ever ran on macOS came to
// pass while failing on the two platforms in CI.
func findPlexDir(candidates []string) string {
	for _, dir := range candidates {
		if plexDatabaseIn(dir) {
			return dir
		}
	}
	return ""
}

// Example returns a commented config file body, for `config init`.
func Example() string {
	return `# plex-sync configuration.
# Every value here is optional; the defaults shown are the built-in ones.
# Environment variables override this file (PLEX_URL, PLEX_TOKEN, PLEX_DB,
# PLEX_CONFIG_DIR, TIDB_API_KEY, TIDB_API_URL, PLEX_SYNC_STATE_DIR).

# Top-level keys must come before the first section header, or TOML reads them
# as part of that section.
state_dir = "` + defaultStateDir() + `"
log_level = "info"

[plex]
# Base URL of your Plex Media Server.
url = "http://127.0.0.1:32400"
# Your Plex token. Prefer the PLEX_TOKEN environment variable over this file.
token = ""
# Full path to the Plex database. Required to write markers.
# On LinuxServer.io/docker: /config/Library/Application Support/Plex Media Server/com.plexapp.plugins.library.db
# Never point this at an Unraid /mnt/user FUSE path: SQLite locking there is
# unreliable and a write can corrupt the database. Use the /mnt/cache path.
database = ""
# Alternatively, the Plex application-support directory.
config_dir = ""

[theintrodb]
# The TheIntroDB API key is optional. With a key the daily allowance is higher
# and your own pending submissions are included in what you get back.
api_key = ""
daily_budget = 1000

[sources]
# Alternate marker sources. TheIntroDB always wins a segment type it answered;
# these only fill the gaps. Both are off by default.
chapters = false
detection = false

[segments]
intro = true
recap = true
credits = true
preview = false

[apply]
# "fill" keeps the markers Plex detected itself and adds what is missing.
# "prefer-theintrodb" lets TheIntroDB replace Plex's own intro/credits markers.
policy = "fill"
# Writing while Plex is running is only allowed with no active playback session.
allow_live = false
backup = true

[api]
# Local JSON control API (no web interface). Used by the TUI and by scripts.
enabled = true
addr = "127.0.0.1:8765"

[schedule]
# Five fields: minute hour day-of-month month day-of-week, in local time.
# 07:30, after Plex's own maintenance window at 07:00.
cron = "30 7 * * *"
# Run once as soon as the process starts, before waiting for the first firing.
# Off by default, so recreating a container does not by itself write anything.
run_on_start = false
`
}
