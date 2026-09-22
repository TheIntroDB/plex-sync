package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TheIntroDB/plex-sync/internal/plexdb"
)

// EnsureMarkerTag makes the marker tag in the Plex database when the library has
// never held a marker, and reports the tag to use.
//
// Markers attach to a row in Plex's tags table, and Plex creates that row the
// first time it writes a marker itself. Writing markers is a Plex Pass feature,
// so on a server without it the row is never there and every marker this tool
// would write has nothing to attach to. It was believed for the first release
// that the row could not be created from outside Plex at all -- true of a plain
// INSERT, which cannot even be prepared outside Plex, and not true of the
// sequence plexdb uses, which drops the table's FTS4 triggers for the duration
// of the write and puts them back. See plexdb.withoutTagTriggers.
//
// It goes through the same gates as any other write: the confirmation, the
// preflight that refuses when Plex is running unannounced or when something is
// being watched, a backup, and a journal that undo understands. The setup
// command and the interface both call this rather than writing the row
// themselves, because a one-time path exercised by nobody is a one-time path
// that breaks.
//
// The returned path is the undo journal, empty when there was nothing to do.
func (r *Runner) EnsureMarkerTag(ctx context.Context, opts Options) (int64, string, error) {
	cfg := r.app.Cfg

	if err := r.Preflight(ctx, opts); err != nil {
		return 0, "", err
	}

	db, err := r.app.PlexDB(false)
	if err != nil {
		return 0, "", err
	}

	if id, err := db.MarkerTagID(); err == nil {
		// Plex made one, or an earlier run of this tool did. Nothing to do.
		return id, "", nil
	}

	if cfg.Apply.Backup && !opts.NoBackup {
		if _, err := plexdb.Backup(r.app.PlexDBPath(), cfg.BackupDir(), cfg.Apply.KeepBackups); err != nil {
			return 0, "", fmt.Errorf("back up the Plex database before writing: %w", err)
		}
	}

	if err := os.MkdirAll(cfg.UndoDir(), 0o755); err != nil {
		return 0, "", err
	}
	journalPath := filepath.Join(cfg.UndoDir(),
		"undo-"+r.now().UTC().Format("20060102T150405Z")+".jsonl")
	journal, err := plexdb.NewJournal(journalPath)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = journal.Close() }()

	id, err := db.MarkerTagIDOrCreate(ctx, journal)
	if err != nil {
		return 0, journalPath, err
	}
	return id, journalPath, nil
}

// MarkerTagID reports the marker tag without writing anything, so that the
// interface can describe the state before offering to change it.
func (r *Runner) MarkerTagID(ctx context.Context) (int64, error) {
	db, err := r.app.PlexDB(true)
	if err != nil {
		return 0, err
	}
	return db.MarkerTagID()
}
