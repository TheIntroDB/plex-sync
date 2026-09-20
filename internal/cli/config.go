package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TheIntroDB/plex-integration/internal/app"
	"github.com/TheIntroDB/plex-integration/internal/config"
)

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the configuration",
	}
	cmd.AddCommand(
		configCheckCmd(g),
		configPathCmd(g),
		configInitCmd(g),
		configShowCmd(g),
	)
	return cmd
}

// configPathCmd prints the file that would be loaded, if any.
func configPathCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file in use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := g.configPath
			if path == "" {
				path = config.FindFile()
			}
			if path == "" {
				fmt.Fprintln(stdout(cmd), "(none: using defaults, the environment and flags)")
				return nil
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				abs = path
			}
			fmt.Fprintln(stdout(cmd), abs)
			return nil
		},
	}
}

// configInitCmd writes a commented example configuration.
func configInitCmd(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented example configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := g.configPath
			if path == "" {
				dir, err := os.UserConfigDir()
				if err != nil {
					return err
				}
				path = filepath.Join(dir, "tidb-plex", "config.toml")
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(config.Example()), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(stdout(cmd), "wrote %s\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return cmd
}

// configShowCmd prints the effective configuration, secrets masked.
func configShowCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			if g.jsonOut {
				encoder := json.NewEncoder(stdout(cmd))
				encoder.SetIndent("", "  ")
				return encoder.Encode(viewOf(cfg))
			}
			path := cfg.Path
			if path == "" {
				path = "(none: defaults, environment and flags)"
			}
			out := stdout(cmd)
			fmt.Fprintf(out, "configuration file : %s\n", path)
			fmt.Fprintf(out, "state directory    : %s\n", cfg.StateDir)
			fmt.Fprintf(out, "log level          : %s\n\n", cfg.LogLevel)
			fmt.Fprintf(out, "[plex]\n  url                : %s\n", cfg.Plex.URL)
			fmt.Fprintf(out, "  token              : %s\n", mask(cfg.Plex.Token))
			fmt.Fprintf(out, "  database           : %s\n", orNone(cfg.Plex.ResolvedDatabase()))
			fmt.Fprintf(out, "\n[theintrodb]\n  url                : %s\n", cfg.TheIntroDB.BaseURL)
			fmt.Fprintf(out, "  api key            : %s\n", mask(cfg.TheIntroDB.APIKey))
			fmt.Fprintf(out, "  daily budget       : %d requests\n", cfg.TheIntroDB.DailyBudget)
			fmt.Fprintf(out, "  pacing             : %.2f s between requests\n", cfg.TheIntroDB.MinDelay())
			fmt.Fprintf(out, "\n[sources]\n  order              : %s\n", strings.Join(cfg.Sources.Ordered(), ", "))
			fmt.Fprintf(out, "  chapters           : %t\n", cfg.Sources.Chapters)
			fmt.Fprintf(out, "  detection          : %t\n", cfg.Sources.Detection)
			fmt.Fprintf(out, "\n[segments]\n  enabled            : %s\n", strings.Join(cfg.Segments.EnabledTypes(), ", "))
			fmt.Fprintf(out, "  recap folds into   : %s\n", foldTarget(cfg.Segments.MapRecap, "intro"))
			fmt.Fprintf(out, "  preview folds into : %s\n", foldTarget(cfg.Segments.MapPreview, "credits"))
			fmt.Fprintf(out, "\n[apply]\n  policy             : %s\n", cfg.Apply.Policy)
			fmt.Fprintf(out, "  backup             : %t (keeping %d)\n", cfg.Apply.Backup, cfg.Apply.KeepBackups)
			fmt.Fprintf(out, "  allow live writes  : %t\n", cfg.Apply.AllowLive)
			fmt.Fprintf(out, "  PAL guard          : %t\n", cfg.Apply.PALGuard)
			return nil
		},
	}
}

// configCheckCmd validates the configuration and probes both services.
func configCheckCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate the configuration and check that Plex and TheIntroDB answer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return fmt.Errorf("configuration is not usable: %w", err)
			}
			out := stdout(cmd)
			fmt.Fprintln(out, "configuration ok")
			if cfg.Path != "" {
				fmt.Fprintf(out, "  loaded from       %s\n", cfg.Path)
			}
			if db := cfg.Plex.ResolvedDatabase(); db != "" {
				if _, err := os.Stat(db); err != nil {
					fmt.Fprintf(out, "  plex database     MISSING (%s)\n", db)
				} else {
					fmt.Fprintf(out, "  plex database     %s\n", db)
				}
			} else {
				fmt.Fprintln(out, "  plex database     not configured (writing markers will not be possible)")
			}

			application, err := app.Open(cfg, loggingFor(cfg, g), app.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			readiness := application.Ready(cmd.Context())
			if readiness.PlexOK {
				fmt.Fprintf(out, "  plex server       ok (%s)\n", orNone(readiness.PlexVersion))
			} else {
				fmt.Fprintf(out, "  plex server       FAILED (%s)\n", readiness.PlexError)
			}
			if readiness.TIDBOK {
				detail := "no API key, which is allowed"
				if cfg.TheIntroDB.APIKey != "" {
					detail = "API key accepted"
				}
				if readiness.TIDBError != "" {
					detail = readiness.TIDBError
				}
				fmt.Fprintf(out, "  theintrodb        ok (%s)\n", detail)
			} else {
				fmt.Fprintf(out, "  theintrodb        FAILED (%s)\n", readiness.TIDBError)
			}

			if !readiness.PlexOK && !readiness.TIDBOK {
				return &silentError{code: ExitError}
			}
			return nil
		},
	}
}

// effectiveView is the JSON shape of `config show`.
type effectiveView struct {
	ConfigPath string `json:"config_path,omitempty"`
	StateDir   string `json:"state_dir"`
	LogLevel   string `json:"log_level"`
	Plex       struct {
		URL      string `json:"url"`
		Database string `json:"database,omitempty"`
		HasToken bool   `json:"has_token"`
	} `json:"plex"`
	TheIntroDB struct {
		BaseURL       string  `json:"base_url"`
		HasAPIKey     bool    `json:"has_api_key"`
		DailyBudget   int     `json:"daily_budget"`
		MinDelay      float64 `json:"min_delay_seconds"`
		MaxPerWindow  int     `json:"max_per_window"`
		WindowSeconds float64 `json:"window_seconds"`
	} `json:"theintrodb"`
	Sources struct {
		Order     []string `json:"order"`
		Chapters  bool     `json:"chapters"`
		Detection bool     `json:"detection"`
	} `json:"sources"`
	Segments struct {
		Enabled    []string `json:"enabled"`
		MapRecap   bool     `json:"map_recap"`
		MapPreview bool     `json:"map_preview"`
	} `json:"segments"`
	Apply struct {
		Policy                     string `json:"policy"`
		Backup                     bool   `json:"backup"`
		KeepBackups                int    `json:"keep_backups"`
		AllowLive                  bool   `json:"allow_live"`
		RequireStoppedConfirmation bool   `json:"require_stopped_confirmation"`
		PALGuard                   bool   `json:"pal_guard"`
		ChunkSize                  int    `json:"chunk_size"`
	} `json:"apply"`
}

func viewOf(cfg *config.Config) effectiveView {
	var v effectiveView
	v.ConfigPath = cfg.Path
	v.StateDir = cfg.StateDir
	v.LogLevel = cfg.LogLevel
	v.Plex.URL = cfg.Plex.URL
	v.Plex.Database = cfg.Plex.ResolvedDatabase()
	v.Plex.HasToken = cfg.Plex.Token != ""
	v.TheIntroDB.BaseURL = cfg.TheIntroDB.BaseURL
	v.TheIntroDB.HasAPIKey = cfg.TheIntroDB.APIKey != ""
	v.TheIntroDB.DailyBudget = cfg.TheIntroDB.DailyBudget
	v.TheIntroDB.MinDelay = cfg.TheIntroDB.MinDelay()
	v.TheIntroDB.MaxPerWindow = cfg.TheIntroDB.MaxPerWindow
	v.TheIntroDB.WindowSeconds = cfg.TheIntroDB.WindowS
	v.Sources.Order = cfg.Sources.Ordered()
	v.Sources.Chapters = cfg.Sources.Chapters
	v.Sources.Detection = cfg.Sources.Detection
	v.Segments.Enabled = cfg.Segments.EnabledTypes()
	v.Segments.MapRecap = cfg.Segments.MapRecap
	v.Segments.MapPreview = cfg.Segments.MapPreview
	v.Apply.Policy = cfg.Apply.Policy
	v.Apply.Backup = cfg.Apply.Backup
	v.Apply.KeepBackups = cfg.Apply.KeepBackups
	v.Apply.AllowLive = cfg.Apply.AllowLive
	v.Apply.RequireStoppedConfirmation = cfg.Apply.RequireStoppedConfirmation
	v.Apply.PALGuard = cfg.Apply.PALGuard
	v.Apply.ChunkSize = cfg.Apply.ChunkSize
	return v
}

func mask(secret string) string {
	if secret == "" {
		return "(not set)"
	}
	return "(set, hidden)"
}

func orNone(value string) string {
	if value == "" {
		return "(not set)"
	}
	return value
}

func presentAbsent(present bool) string {
	if present {
		return "present"
	}
	return "absent"
}

func foldTarget(enabled bool, target string) string {
	if !enabled {
		return "not folded"
	}
	return target
}

// contextWithoutCancel is a convenience for commands that must finish cleanup
// after the user pressed Ctrl-C.
func contextWithoutCancel(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}
