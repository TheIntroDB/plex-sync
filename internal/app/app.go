// Package app wires the pieces together.
//
// It owns the shared resources every interface needs, so the terminal
// interface, the command line and the local API all drive the same objects
// instead of each constructing their own. Anything that opens a file or a
// socket lives here and is closed once, in Close.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/buildinfo"
	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/httpclient"
	"github.com/TheIntroDB/plex-sync/internal/ledger"
	"github.com/TheIntroDB/plex-sync/internal/plexapi"
	"github.com/TheIntroDB/plex-sync/internal/plexdb"
	"github.com/TheIntroDB/plex-sync/internal/tidb"
)

// App is the shared runtime state.
type App struct {
	Cfg    *config.Config
	Log    *slog.Logger
	HTTP   *httpclient.Client
	Ledger *ledger.Ledger
	Plex   *plexapi.Client
	TIDB   *tidb.Client

	// plexDB is opened lazily. Reading the library and looking segments up
	// never needs the database, and on some installs it cannot be read at all.
	plexDB     *plexdb.DB
	plexDBPath string
}

// Options controls how Open behaves.
type Options struct {
	// NeedPlexDB opens the Plex database immediately, and fails when it cannot
	// be opened. Write commands set this; read-only commands do not.
	NeedPlexDB bool
	// ReadOnly opens the Plex database read-only.
	ReadOnly bool
	// UserAgent overrides the reported client version.
	UserAgent string
}

// Open loads nothing itself; it takes a ready configuration and connects
// everything it can.
func Open(cfg *config.Config, log *slog.Logger, opts Options) (*App, error) {
	if cfg == nil {
		return nil, errors.New("app: no configuration")
	}
	if log == nil {
		return nil, errors.New("app: no logger")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", cfg.StateDir, err)
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = buildinfo.UserAgent()
	}

	// A Plex server requires a token for everything but /identity. When none was
	// configured, use the one Plex keeps on this machine so that a local install
	// works without the user having to go and find it.
	if cfg.Plex.Token == "" {
		if token := cfg.Plex.ResolvedToken(); token != "" {
			cfg.Plex.Token = token
			log.Debug("using the Plex token found on this machine")
		}
	}

	application := &App{
		Cfg: cfg,
		Log: log,
		HTTP: httpclient.New(
			time.Duration(cfg.Plex.TimeoutS*float64(time.Second)),
			cfg.Plex.InsecureSkipVerify,
			ua,
		),
	}

	led, err := ledger.Open(cfg.LedgerPath())
	if err != nil {
		application.HTTP.Close()
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	application.Ledger = led

	// TheIntroDB has its own timeout, which may differ from Plex's, so it gets
	// its own connection pool rather than sharing Plex's.
	application.Plex = plexapi.NewClient(cfg.Plex, application.HTTP)
	application.TIDB = tidb.NewClient(cfg.TheIntroDB, led, application.HTTP)

	if opts.NeedPlexDB {
		if _, err := application.PlexDB(opts.ReadOnly); err != nil {
			_ = application.Close()
			return nil, err
		}
	}
	return application, nil
}

// PlexDB opens the Plex database on first use and remembers the result, so a
// failed open is reported once rather than on every call.
func (a *App) PlexDB(readOnly bool) (*plexdb.DB, error) {
	if a.plexDB != nil {
		return a.plexDB, nil
	}
	path, err := a.Cfg.CheckDatabase()
	if err != nil {
		return nil, err
	}
	db, err := plexdb.OpenDB(path, readOnly)
	if err != nil {
		return nil, fmt.Errorf("open Plex database %s: %w", path, err)
	}
	a.plexDB = db
	a.plexDBPath = path
	return db, nil
}

// PlexDBPath returns the Plex database path, once one has been resolved.
func (a *App) PlexDBPath() string {
	if a.plexDBPath != "" {
		return a.plexDBPath
	}
	return a.Cfg.Plex.ResolvedDatabase()
}

// UndoDir is where undo journals are written.
func (a *App) UndoDir() string { return a.Cfg.UndoDir() }

// Close releases everything the app holds. Errors are joined rather than
// dropped, but a failure to close is never fatal.
func (a *App) Close() error {
	var errs []error
	if a.plexDB != nil {
		if err := a.plexDB.Close(); err != nil {
			errs = append(errs, err)
		}
		a.plexDB = nil
	}
	if a.Ledger != nil {
		if err := a.Ledger.Close(); err != nil {
			errs = append(errs, err)
		}
		a.Ledger = nil
	}
	if a.HTTP != nil {
		a.HTTP.Close()
		a.HTTP = nil
	}
	return errors.Join(errs...)
}

// Ready probes both services and reports what answered.
//
// It never fails the caller: a Plex server that is down is an ordinary state
// for this tool to be run in, and the interface still needs to open.
type Readiness struct {
	PlexOK      bool
	PlexVersion string
	PlexError   string
	TIDBOK      bool
	TIDBKeyOK   bool
	TIDBError   string
}

// Ready implements the probe described above.
func (a *App) Ready(ctx context.Context) Readiness {
	var out Readiness

	if identity, err := a.Plex.Identity(ctx); err != nil {
		out.PlexError = err.Error()
	} else {
		out.PlexOK = true
		// Identity returns the MediaContainer's own contents, not the envelope.
		if version, ok := identity["version"].(string); ok {
			out.PlexVersion = version
		}
	}

	// The key check and the reachability check are not the same thing. Without
	// a key configured, /user/stats answers 401 by design, which says the API
	// is up and working, not that anything is wrong.
	stats, err := a.TIDB.UserStats(ctx)
	switch {
	case err == nil:
		out.TIDBOK = true
		out.TIDBKeyOK = stats != nil
	case isAuthError(err):
		out.TIDBOK = true
		if a.Cfg.TheIntroDB.APIKey != "" {
			out.TIDBError = "the API key was rejected: " + err.Error()
		}
	default:
		out.TIDBError = err.Error()
	}
	return out
}

// isAuthError reports whether an error is the API rejecting the credentials.
func isAuthError(err error) bool {
	var apiErr *tidb.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == 401 || apiErr.Status == 403 || apiErr.Kind == tidb.KindAuth
}
