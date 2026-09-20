package plexdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupPrefix names the copies Backup writes.
const BackupPrefix = "library.db.pre-tidb-"

// Backup makes a copy of a Plex database and prunes old copies.
//
// The copy is a single step, not a paged incremental one: an incremental backup
// restarts every time Plex writes to the database, so against a live server it
// can loop forever and never finish. VACUUM INTO writes the whole file in one
// pass and is safe to run while Plex is running.
//
// The destination is destDir/<BackupPrefix><UTC stamp> with the colons and dots
// taken out of the stamp, so the copies sort in the order they were made. The
// directory is created when it is missing. keep is how many copies to leave
// behind, counting the new one; zero or less leaves them all.
func Backup(dbPath, destDir string, keep int) (string, error) {
	if strings.TrimSpace(dbPath) == "" || strings.TrimSpace(destDir) == "" {
		return "", fmt.Errorf("plexdb: backup needs a database path and a destination directory")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return "", fmt.Errorf("plexdb: backup source %s: %w", dbPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("plexdb: backup source %s is a directory", dbPath)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("plexdb: create backup directory %s: %w", destDir, err)
	}

	dest, err := uniqueBackupPath(destDir, time.Now().UTC())
	if err != nil {
		return "", err
	}

	src, err := OpenDB(dbPath, false)
	if err != nil {
		return "", err
	}
	defer src.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// VACUUM INTO refuses to overwrite, hence the unique name above. It cannot
	// run inside a transaction, and none has been opened on this connection.
	if _, err := src.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		os.Remove(dest)
		return "", fmt.Errorf("plexdb: backup %s to %s: %w", dbPath, dest, err)
	}

	if err := PruneBackups(destDir, keep); err != nil {
		return dest, err
	}
	return dest, nil
}

// backupStamp renders a UTC time the way backup names carry it: RFC3339 with
// the colons and dots stripped.
func backupStamp(t time.Time) string {
	stamp := t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return strings.NewReplacer(":", "", ".", "").Replace(stamp)
}

// uniqueBackupPath returns a free destination path. Two backups inside the same
// second would otherwise collide, and VACUUM INTO refuses to overwrite.
func uniqueBackupPath(destDir string, now time.Time) (string, error) {
	stamp := backupStamp(now)
	dest := filepath.Join(destDir, BackupPrefix+stamp)
	for n := 1; ; n++ {
		_, err := os.Stat(dest)
		if os.IsNotExist(err) {
			return dest, nil
		}
		if err != nil {
			return "", fmt.Errorf("plexdb: inspect backup %s: %w", dest, err)
		}
		dest = filepath.Join(destDir, fmt.Sprintf("%s%s-%d", BackupPrefix, stamp, n))
		if n > 1000 {
			return "", fmt.Errorf("plexdb: cannot find a free backup name in %s", destDir)
		}
	}
}

// Backups lists the backups in a directory, newest first.
func Backups(destDir string) ([]string, error) {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return nil, fmt.Errorf("plexdb: list backups in %s: %w", destDir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), BackupPrefix) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, filepath.Join(destDir, name))
	}
	return out, nil
}

// PruneBackups deletes all but the newest keep backups in a directory.
func PruneBackups(destDir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	all, err := Backups(destDir)
	if err != nil {
		return err
	}
	for i := keep; i < len(all); i++ {
		if err := os.Remove(all[i]); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("plexdb: prune backup %s: %w", all[i], err)
		}
	}
	return nil
}
