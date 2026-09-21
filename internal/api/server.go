// Package api is the local control API.
//
// It is JSON only: there is no web interface and no HTML anywhere in this
// project. The API exists so that the terminal interface, scripts and other
// machines can drive the same runs, and it is documented by the OpenAPI schema
// Huma generates at /openapi.json.
//
// It binds to localhost by default and has no authentication, which is why it
// binds to localhost by default. Exposing it means exposing the ability to
// write to your Plex database.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humafiber"
	"github.com/gofiber/fiber/v3"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/buildinfo"
	"github.com/TheIntroDB/plex-sync/internal/model"
	"github.com/TheIntroDB/plex-sync/internal/sync"
)

// Server wraps the router and the operations.
type Server struct {
	app    *app.App
	runner *sync.Runner
	router *fiber.App
	api    huma.API
}

// New builds the server and registers every operation.
func New(a *app.App) *Server {
	s := &Server{
		app:    a,
		runner: sync.New(a),
		router: fiber.New(fiber.Config{AppName: "plex-sync"}),
	}
	config := huma.DefaultConfig("plex-sync", buildinfo.Version)
	config.Info.Description = strings.TrimSpace(`
Local control API for plex-sync. It reads the Plex library, plans marker
changes, applies them and reverts them.

Planning asks TheIntroDB about every item, so it can take minutes on a large
library. Applying writes to your Plex database and requires confirm: true.
`)
	s.api = humafiber.New(s.router, config)
	s.register()
	return s
}

// Router exposes the underlying Fiber application.
func (s *Server) Router() *fiber.App { return s.router }

// Address resolves the address to listen on, filling in the port when the
// configured address has none.
func Address(addr string) (string, error) {
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:8765"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// A bare port or host without one: accept both.
		if _, err := fmt.Sscanf(addr, "%d", new(int)); err == nil {
			addr = "127.0.0.1:" + addr
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("api address %q is not host:port", addr)
		}
	}
	return addr, nil
}

// Run serves until the context is cancelled, then shuts down.
func (s *Server) Run(ctx context.Context, addr string) error {
	resolved, err := Address(addr)
	if err != nil {
		return err
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.router.ShutdownWithContext(shutdownCtx); err != nil {
			s.app.Log.Warn("api shutdown", "error", err)
		}
	}()

	s.app.Log.Info("local API listening", "addr", resolved, "schema", resolved+"/openapi.json")
	err = s.router.Listen(resolved)
	<-shutdownDone
	if err != nil && !errors.Is(err, fiber.ErrServiceUnavailable) {
		return err
	}
	return nil
}

// --- inputs and outputs ----------------------------------------------------

type emptyInput struct{}

type healthOutput struct {
	Body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
}

type readinessOutput struct {
	Body struct {
		PlexOK      bool   `json:"plex_ok"`
		PlexVersion string `json:"plex_version,omitempty"`
		PlexError   string `json:"plex_error,omitempty"`
		TIDBOK      bool   `json:"theintrodb_ok"`
		TIDBError   string `json:"theintrodb_error,omitempty"`
		Database    string `json:"plex_database,omitempty"`
	}
}

type statusOutput struct {
	Body struct {
		Stats       any      `json:"ledger"`
		RequestsDay int      `json:"requests_today"`
		Budget      int      `json:"daily_budget"`
		Runs        any      `json:"recent_runs"`
		Sources     []string `json:"sources"`
		Policy      string   `json:"policy"`
	}
}

type libraryInput struct {
	Filter string `query:"filter" doc:"only items whose title contains this text"`
	Limit  int    `query:"limit" doc:"list at most this many items"`
}

type libraryOutput struct {
	Body struct {
		Items []model.LibraryItem `json:"items"`
		Count int                 `json:"count"`
	}
}

type runsInput struct {
	Limit int `query:"limit" default:"25" doc:"how many runs to return"`
}

type runsOutput struct {
	Body struct {
		Runs any `json:"runs"`
	}
}

type planInput struct {
	Body struct {
		Filter   string `json:"filter,omitempty"`
		Limit    int    `json:"limit,omitempty"`
		Sections []int  `json:"sections,omitempty"`
	}
}

type planOutput struct {
	Body any
}

type applyInput struct {
	Body struct {
		// Confirm must be true. Without it nothing is written, so an
		// accidental POST cannot modify a library.
		Confirm bool   `json:"confirm" doc:"must be true to write to the Plex database"`
		Filter  string `json:"filter,omitempty"`
		Limit   int    `json:"limit,omitempty"`
		Live    bool   `json:"live,omitempty" doc:"allow writing while Plex runs and nothing is playing"`
	}
}

type applyOutput struct {
	Body struct {
		Applied     bool   `json:"applied"`
		Added       int    `json:"added"`
		Removed     int    `json:"removed"`
		Items       int    `json:"items"`
		Skipped     int    `json:"skipped"`
		UndoJournal string `json:"undo_journal,omitempty"`
		Backup      string `json:"backup,omitempty"`
	}
}

type undoInput struct {
	Body struct {
		Confirm bool   `json:"confirm" doc:"must be true to revert"`
		Journal string `json:"journal,omitempty" doc:"path to a journal; defaults to the most recent"`
	}
}

type undoOutput struct {
	Body struct {
		Reverted int `json:"reverted"`
	}
}

// --- operations ------------------------------------------------------------

func (s *Server) register() {
	huma.Register(s.api, huma.Operation{
		OperationID: "health",
		Method:      "GET",
		Path:        "/health",
		Summary:     "Liveness probe",
		Tags:        []string{"meta"},
	}, func(_ context.Context, _ *emptyInput) (*healthOutput, error) {
		out := &healthOutput{}
		out.Body.Status = "ok"
		out.Body.Version = buildinfo.Version
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "readiness",
		Method:      "GET",
		Path:        "/ready",
		Summary:     "Check that Plex and TheIntroDB answer",
		Tags:        []string{"meta"},
	}, func(ctx context.Context, _ *emptyInput) (*readinessOutput, error) {
		out := &readinessOutput{}
		readiness := s.app.Ready(ctx)
		out.Body.PlexOK = readiness.PlexOK
		out.Body.PlexVersion = readiness.PlexVersion
		out.Body.PlexError = readiness.PlexError
		out.Body.TIDBOK = readiness.TIDBOK
		out.Body.TIDBError = readiness.TIDBError
		out.Body.Database = s.app.PlexDBPath()
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "status",
		Method:      "GET",
		Path:        "/status",
		Summary:     "Ledger, request budget and recent runs",
		Tags:        []string{"status"},
	}, func(_ context.Context, _ *emptyInput) (*statusOutput, error) {
		out := &statusOutput{}
		if stats, err := s.app.Ledger.Stats(); err == nil {
			out.Body.Stats = stats
			out.Body.RequestsDay = stats.RequestsToday
		}
		if runs, err := s.app.Ledger.Runs(25); err == nil {
			out.Body.Runs = runs
		}
		out.Body.Budget = s.app.Cfg.TheIntroDB.EffectiveDailyBudget()
		out.Body.Sources = s.app.Cfg.Sources.Ordered()
		out.Body.Policy = s.app.Cfg.Apply.Policy
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "library",
		Method:      "GET",
		Path:        "/library",
		Summary:     "List library items and their lookup ids",
		Tags:        []string{"library"},
	}, func(ctx context.Context, in *libraryInput) (*libraryOutput, error) {
		items, err := s.runner.Inventory(ctx, sync.Options{Filter: in.Filter, Limit: in.Limit})
		if err != nil {
			return nil, huma.Error502BadGateway("could not read the Plex library", err)
		}
		out := &libraryOutput{}
		out.Body.Items = items
		out.Body.Count = len(items)
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "runs",
		Method:      "GET",
		Path:        "/runs",
		Summary:     "Run history",
		Tags:        []string{"status"},
	}, func(_ context.Context, in *runsInput) (*runsOutput, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 25
		}
		runs, err := s.app.Ledger.Runs(limit)
		if err != nil {
			return nil, huma.Error500InternalServerError("could not read the ledger", err)
		}
		out := &runsOutput{}
		out.Body.Runs = runs
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "plan",
		Method:      "POST",
		Path:        "/plan",
		Summary:     "Compute the change set without writing anything",
		Description: "Asks TheIntroDB about every item, which can take minutes on a large " +
			"library. Every answer is cached in the ledger, so a following apply does not " +
			"pay for the requests twice.",
		Tags: []string{"run"},
	}, func(ctx context.Context, in *planInput) (*planOutput, error) {
		res, err := s.runner.Plan(ctx, sync.Options{
			Filter:   in.Body.Filter,
			Limit:    in.Body.Limit,
			Sections: in.Body.Sections,
			DryRun:   true,
		})
		if err != nil {
			return nil, huma.Error502BadGateway("could not plan", err)
		}
		out := &planOutput{}
		out.Body = res
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "apply",
		Method:      "POST",
		Path:        "/apply",
		Summary:     "Plan and write markers into the Plex database",
		Description: "Writes only when confirm is true. It fails closed when Plex is running " +
			"without live, when something is streaming, or when the database path cannot be " +
			"trusted.",
		Tags: []string{"run"},
	}, func(ctx context.Context, in *applyInput) (*applyOutput, error) {
		if !in.Body.Confirm {
			return nil, huma.Error400BadRequest(
				"set confirm to true: this writes to your Plex database")
		}
		opts := sync.Options{
			Confirm: true,
			Filter:  in.Body.Filter,
			Limit:   in.Body.Limit,
			Live:    in.Body.Live,
		}
		res, err := s.runner.Run(ctx, opts)
		if err != nil {
			switch {
			case errors.Is(err, sync.ErrNeedsConfirmation):
				return nil, huma.Error403Forbidden(err.Error())
			case errors.Is(err, sync.ErrPlexRunning):
				return nil, huma.Error409Conflict(err.Error())
			default:
				return nil, huma.Error500InternalServerError("the run failed", err)
			}
		}
		out := &applyOutput{}
		out.Body.Applied = res.Applied
		out.Body.Added = res.Stats.Added
		out.Body.Removed = res.Stats.Removed
		out.Body.Items = res.Stats.Written
		out.Body.Skipped = res.Stats.Skipped
		out.Body.UndoJournal = res.UndoPath
		out.Body.Backup = res.BackupPath
		return out, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "undo",
		Method:      "POST",
		Path:        "/undo",
		Summary:     "Revert a run from its undo journal",
		Description: "Defaults to the most recent journal. Reverting an older journal after a " +
			"newer run would overwrite the newer changes, so name one only when you mean it.",
		Tags: []string{"run"},
	}, func(ctx context.Context, in *undoInput) (*undoOutput, error) {
		if !in.Body.Confirm {
			return nil, huma.Error400BadRequest("set confirm to true: this rewrites marker rows")
		}
		journal := in.Body.Journal
		if journal == "" {
			journals, err := Journals(s.app.UndoDir())
			if err != nil || len(journals) == 0 {
				return nil, huma.Error404NotFound("there are no undo journals yet")
			}
			journal = journals[len(journals)-1]
		}
		count, err := s.runner.Undo(ctx, journal, sync.Options{Confirm: true})
		if err != nil {
			return nil, huma.Error500InternalServerError("could not revert the journal", err)
		}
		out := &undoOutput{}
		out.Body.Reverted = count
		return out, nil
	})
}
