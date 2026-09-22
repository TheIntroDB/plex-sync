package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/schedule"
)

// settingKind is how a row is edited.
type settingKind int

const (
	// settingBool is toggled with enter: no typing involved.
	settingBool settingKind = iota
	// settingText takes free text.
	settingText
	// settingSecret takes free text but never shows what is already stored.
	settingSecret
	// settingInt takes a whole number.
	settingInt
	// settingChoice cycles through a fixed set with enter.
	settingChoice
	// settingAction is a button. Enter runs it rather than opening an editor,
	// and it is the only row whose value does not come from the configuration
	// file: what a button shows is the state of something out in the world.
	settingAction
)

// setting is one editable row on the Settings screen.
type setting struct {
	section string
	key     string
	label   string
	kind    settingKind
	choices []string
	// help is what the row means, shown for the selected row.
	help string

	// get returns the value to show for the row.
	get func(*config.Config) string
	// set applies a value typed or cycled by the user. It may reject one.
	set func(*config.Config, string) error
	// start returns what the input line begins with. Secrets start empty, so
	// an existing key is never put back on the screen.
	start func(*config.Config) string

	// fallback is what the row is in fact using when the setting itself is
	// empty, said in words. Two settings are resolved on the machine rather
	// than stored: the database path and the token. Both were showing "(empty)"
	// and "not set" while the tool was using a value it had found, which reads
	// as a fault in a screen whose whole job is to say what is going on.
	fallback func(*config.Config) string

	// state is what a button shows, read from the program rather than from the
	// configuration, and run is what pressing enter does after confirmation.
	state func(*Model) string
	run   func(*Model) tea.Cmd
}

// display renders a row's value, masking anything secret.
func (s setting) display(m *Model) string {
	if s.kind == settingAction {
		if s.state == nil {
			return ""
		}
		// A button says what pressing it would do, or what has already been
		// done, because that is the whole of what it reports.
		return s.state(m)
	}
	cfg := m.app.Cfg
	value := s.get(cfg)
	switch s.kind {
	case settingBool:
		return yesNo(value == "true")
	case settingSecret:
		if value == "" {
			// Never the value itself: whether one is in use is the question
			// this row answers, and a secret on a settings screen is a secret
			// in a screenshot.
			if s.fallback != nil {
				return s.fallback(cfg)
			}
			return "not set"
		}
		return "set, hidden"
	case settingChoice:
		return value
	default:
		if value == "" {
			if s.fallback != nil {
				return s.fallback(cfg)
			}
			return "(empty)"
		}
		return value
	}
}

// text is what goes on the input line when the row is opened for editing.
func (s setting) text(cfg *config.Config) string {
	if s.start != nil {
		return s.start(cfg)
	}
	switch s.kind {
	case settingBool, settingChoice:
		return s.get(cfg)
	case settingSecret:
		return ""
	default:
		return s.get(cfg)
	}
}

// restartKeys are the settings the process has already read and will not read
// again: the Plex and TheIntroDB clients and the logger are built once, at
// startup. Everything else is consulted as a run happens, so it applies to the
// next run without restarting.
//
// Saying which is which matters more than it sounds: "saved" on its own invites
// someone to change the API key, see nothing happen, and conclude it is broken.
var restartKeys = map[string]bool{
	"plex.url":                true,
	"plex.token":              true,
	"theintrodb.api_key":      true,
	"theintrodb.daily_budget": true,
	"log_level":               true,
}

func needsRestart(key string) bool { return restartKeys[key] }

// settingsRows is the whole editable surface, in display order.
//
// This is the list the cursor walks. Rows are grouped by the section they belong
// to in the config file, which is also the order they are written in.
func settingsRows() []setting {
	boolSet := func(dst func(*config.Config, bool)) func(*config.Config, string) error {
		return func(cfg *config.Config, value string) error {
			on, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("expected true or false")
			}
			dst(cfg, on)
			return nil
		}
	}
	boolGet := func(src func(*config.Config) bool) func(*config.Config) string {
		return func(cfg *config.Config) string { return strconv.FormatBool(src(cfg)) }
	}
	intSet := func(dst func(*config.Config, int)) func(*config.Config, string) error {
		return func(cfg *config.Config, value string) error {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%q is not a whole number", value)
			}
			dst(cfg, n)
			return nil
		}
	}
	intGet := func(src func(*config.Config) int) func(*config.Config) string {
		return func(cfg *config.Config) string { return strconv.Itoa(src(cfg)) }
	}

	return []setting{
		{
			section: "Plex", key: "plex.url", label: "address", kind: settingText,
			help: "Where Plex answers. A local server is usually http://127.0.0.1:32400.",
			get:  func(c *config.Config) string { return c.Plex.URL },
			set:  func(c *config.Config, v string) error { c.Plex.URL = strings.TrimSpace(v); return nil },
		},
		{
			section: "Plex", key: "plex.token", label: "token", kind: settingSecret,
			help: "Found automatically on this machine when it is empty. Set it for a remote server.",
			get:  func(c *config.Config) string { return c.Plex.Token },
			set:  func(c *config.Config, v string) error { c.Plex.Token = strings.TrimSpace(v); return nil },
			fallback: func(c *config.Config) string {
				// One is read out of Plex's own files when this is empty, so
				// "not set" was wrong whenever it was found. The token itself
				// is never shown; that it is in use is the useful part.
				if c.Plex.ResolvedToken() != "" {
					return "found on this machine, hidden"
				}
				return "not set"
			},
		},
		{
			section: "Plex", key: "plex.database", label: "database", kind: settingText,
			help: "The library database. Empty means find it in Plex's own directories, which is what happens by default.",
			get:  func(c *config.Config) string { return c.Plex.Database },
			set:  func(c *config.Config, v string) error { c.Plex.Database = strings.TrimSpace(v); return nil },
			fallback: func(c *config.Config) string {
				// The path is searched for on this machine, so an empty setting
				// is not an empty answer: showing "(empty)" here while the tool
				// writes to a database it found is what made this inconsistent
				// with `config check` and the status screen, which both print
				// the path in use.
				if found := c.Plex.ResolvedDatabase(); found != "" {
					return "found automatically: " + found
				}
				return "not found, and writing needs it"
			},
		},

		{
			// The one row here that writes to Plex's database rather than to
			// the configuration file. It is a button because there is no value
			// to set: either the library has a marker tag or it does not.
			section: "Plex", key: "plex.marker_tag", label: "marker tag",
			kind: settingAction,
			help: "Markers hang off one row in Plex's tags table, and Plex only creates that row " +
				"when it writes a marker of its own, which needs Plex Pass. On a server without it, " +
				"this is the one thing to press before anything can be written: it adds that row, " +
				"after a backup, and `undo latest` removes it again.",
			state: func(m *Model) string {
				if !m.setup.checked {
					return "checking..."
				}
				if m.setup.tagError != "" {
					return "missing, enter to create it"
				}
				return fmt.Sprintf("present, tag %d", m.setup.tagID)
			},
			run: func(m *Model) tea.Cmd { return m.runSetup() },
		},
		{
			section: "TheIntroDB", key: "theintrodb.api_key", label: "API key", kind: settingSecret,
			help: "Optional. A key raises the daily allowance and is required to submit timings.",
			get:  func(c *config.Config) string { return c.TheIntroDB.APIKey },
			set:  func(c *config.Config, v string) error { c.TheIntroDB.APIKey = strings.TrimSpace(v); return nil },
		},
		{
			section: "TheIntroDB", key: "theintrodb.daily_budget", label: "daily budget", kind: settingInt,
			help: "Requests per UTC day. The ceiling is 500 without a key and 1000 with one.",
			get:  intGet(func(c *config.Config) int { return c.TheIntroDB.DailyBudget }),
			set:  intSet(func(c *config.Config, n int) { c.TheIntroDB.DailyBudget = n }),
		},

		{
			section: "Segments", key: "segments.intro", label: "intro", kind: settingBool,
			help: "Write intro markers.",
			get:  boolGet(func(c *config.Config) bool { return c.Segments.Intro }),
			set:  boolSet(func(c *config.Config, on bool) { c.Segments.Intro = on }),
		},
		{
			section: "Segments", key: "segments.recap", label: "recap", kind: settingBool,
			help: "Write recap markers. These fold into the intro unless mapped elsewhere.",
			get:  boolGet(func(c *config.Config) bool { return c.Segments.Recap }),
			set:  boolSet(func(c *config.Config, on bool) { c.Segments.Recap = on }),
		},
		{
			section: "Segments", key: "segments.credits", label: "credits", kind: settingBool,
			help: "Write credits markers.",
			get:  boolGet(func(c *config.Config) bool { return c.Segments.Credits }),
			set:  boolSet(func(c *config.Config, on bool) { c.Segments.Credits = on }),
		},
		{
			section: "Segments", key: "segments.preview", label: "preview", kind: settingBool,
			help: "Write next-episode preview markers. These fold into the credits.",
			get:  boolGet(func(c *config.Config) bool { return c.Segments.Preview }),
			set:  boolSet(func(c *config.Config, on bool) { c.Segments.Preview = on }),
		},

		{
			section: "Writing", key: "apply.policy", label: "policy", kind: settingChoice,
			choices: []string{"fill", "prefer-theintrodb"},
			help:    "fill keeps Plex's own markers and adds what is missing. prefer-theintrodb lets TheIntroDB replace them.",
			get:     func(c *config.Config) string { return c.Apply.Policy },
			set:     func(c *config.Config, v string) error { c.Apply.Policy = v; return nil },
		},
		{
			section: "Writing", key: "apply.backup", label: "back up first", kind: settingBool,
			help: "Copy the database before the first write of a run.",
			get:  boolGet(func(c *config.Config) bool { return c.Apply.Backup }),
			set:  boolSet(func(c *config.Config, on bool) { c.Apply.Backup = on }),
		},
		{
			section: "Writing", key: "apply.keep_backups", label: "backups kept", kind: settingInt,
			help: "How many copies to keep. The oldest are removed.",
			get:  intGet(func(c *config.Config) int { return c.Apply.KeepBackups }),
			set:  intSet(func(c *config.Config, n int) { c.Apply.KeepBackups = n }),
		},
		{
			section: "Writing", key: "apply.allow_live", label: "write while Plex runs", kind: settingBool,
			help: "Off by default. Even when on, a run refuses while anything is playing.",
			get:  boolGet(func(c *config.Config) bool { return c.Apply.AllowLive }),
			set:  boolSet(func(c *config.Config, on bool) { c.Apply.AllowLive = on }),
		},
		{
			section: "Writing", key: "apply.pal_guard", label: "PAL guard", kind: settingBool,
			help: "Ignore community timings for files that are playing faster than they were measured.",
			get:  boolGet(func(c *config.Config) bool { return c.Apply.PALGuard }),
			set:  boolSet(func(c *config.Config, on bool) { c.Apply.PALGuard = on }),
		},
		{
			section: "Writing", key: "apply.chunk_size", label: "items per transaction", kind: settingInt,
			help: "Smaller means more, shorter write transactions.",
			get:  intGet(func(c *config.Config) int { return c.Apply.ChunkSize }),
			set:  intSet(func(c *config.Config, n int) { c.Apply.ChunkSize = n }),
		},

		{
			section: "Sources", key: "sources.chapters", label: "chapters", kind: settingBool,
			help: "Fill gaps from chapter names Plex already read. No lookups, no media reads.",
			get:  boolGet(func(c *config.Config) bool { return c.Sources.Chapters }),
			set:  boolSet(func(c *config.Config, on bool) { c.Sources.Chapters = on }),
		},
		{
			section: "Sources", key: "sources.detection", label: "local detection", kind: settingBool,
			help: "Fingerprint episodes nothing else covers. Needs ffmpeg and fpcalc on PATH.",
			get:  boolGet(func(c *config.Config) bool { return c.Sources.Detection }),
			set:  boolSet(func(c *config.Config, on bool) { c.Sources.Detection = on }),
		},

		{
			section: "Schedule", key: "schedule.cron", label: "when to run", kind: settingText,
			help: "Five fields: minute hour day month weekday, in local time.",
			get:  func(c *config.Config) string { return c.Schedule.Cron },
			set:  func(c *config.Config, v string) error { c.Schedule.Cron = strings.TrimSpace(v); return nil },
		},
		{
			section: "Schedule", key: "schedule.run_on_start", label: "run at startup", kind: settingBool,
			help: "Run once as soon as the process starts, before the first scheduled run.",
			get:  boolGet(func(c *config.Config) bool { return c.Schedule.RunOnStart }),
			set:  boolSet(func(c *config.Config, on bool) { c.Schedule.RunOnStart = on }),
		},

		{
			section: "Logging", key: "log_level", label: "log level", kind: settingChoice,
			choices: []string{"debug", "info", "warn", "error"},
			help:    "How much the logs say.",
			get:     func(c *config.Config) string { return c.LogLevel },
			set:     func(c *config.Config, v string) error { c.LogLevel = v; return nil },
		},
	}
}

// applySettingValue puts a value into the configuration, rejecting anything that
// would leave it unusable.
//
// The whole configuration is validated afterwards rather than each field on its
// own, so a value that is fine by itself but breaks the file as a whole, such as
// a daily budget above the server's ceiling, is refused here too. Whatever the
// user was told in the interface matches what the next run will accept.
func applySettingValue(cfg *config.Config, row setting, value string) error {
	if row.kind == settingChoice {
		found := false
		for _, choice := range row.choices {
			if value == choice {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("expected one of %s", strings.Join(row.choices, ", "))
		}
	}
	if row.kind == settingText && row.key == "schedule.cron" {
		if _, err := schedule.ParseCron(value); err != nil {
			return err
		}
	}

	// Apply to a copy, so a rejected value does not half-change the running
	// configuration.
	probe := *cfg
	if err := row.set(&probe, value); err != nil {
		return err
	}
	if err := probe.Validate(); err != nil {
		return err
	}
	return row.set(cfg, value)
}

// nextChoice returns the value enter should move a choice row to.
func nextChoice(row setting, current string) string {
	if len(row.choices) == 0 {
		return current
	}
	for i, choice := range row.choices {
		if choice == current {
			return row.choices[(i+1)%len(row.choices)]
		}
	}
	return row.choices[0]
}
