// Package logging configures the program's logger.
//
// Output goes to standard error, because standard output belongs to command
// results: `tidb-plex plan --json` must stay parseable while progress messages
// are printed.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds a logger for the given level and format.
//
// format is "text" for a terminal or "json" for a log collector.
func New(level, format string, out io.Writer) *slog.Logger {
	if out == nil {
		out = os.Stderr
	}
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Discard is a logger that writes nothing, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
