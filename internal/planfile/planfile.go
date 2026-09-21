// Package planfile reads and writes plans on disk.
//
// A plan file exists so the work can be split in two: deciding what should
// change, which needs Plex, and changing it, which needs only the database. That
// is how the container applies a plan made on the host, without reaching Plex at
// all.
//
// A plan file is a snapshot, and the library does not stand still while it sits
// there. Nothing here trusts it: the writer reconciles every change against the
// rows that are actually in the database before it touches anything, and skips
// any item that no longer looks the way the plan assumed. The format version is
// checked on the way in, so a file this build cannot interpret is refused rather
// than half-understood.
package planfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// Format is the version of the file layout. A plan written by a different
// version is refused rather than guessed at.
const Format = 1

// Meta describes where a plan came from.
type Meta struct {
	// CreatedAt is when the plan was made, not when it was applied.
	CreatedAt time.Time `json:"created_at"`
	// Tool and Version are the build that made it.
	Tool    string `json:"tool,omitempty"`
	Version string `json:"version,omitempty"`
	// Database names the database the plan was made against. Writing a plan
	// against a different library than it was built for is almost certainly a
	// mistake, so it is checked.
	Database string `json:"database,omitempty"`
	// Plex is the server the library was read from.
	Plex string `json:"plex,omitempty"`
	// Items is how many items the plan wanted to change, kept so a file can be
	// described without being parsed in full.
	Items int `json:"items"`
}

// File is what is actually written.
type File struct {
	Format int        `json:"format"`
	Meta   Meta       `json:"meta"`
	Plan   model.Plan `json:"plan"`
}

// Save writes a plan to path, replacing any file already there.
//
// The write goes to a temporary file in the same directory and is then renamed,
// so an interrupted save cannot leave a half-written plan behind for something
// else to pick up and apply.
func Save(path string, plan *model.Plan, meta Meta) error {
	if plan == nil {
		return fmt.Errorf("planfile: no plan to save")
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now()
	}
	meta.Items = len(plan.Work())

	body, err := json.MarshalIndent(File{Format: Format, Meta: meta, Plan: *plan}, "", "  ")
	if err != nil {
		return fmt.Errorf("planfile: encode: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("planfile: %w", err)
	}
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("planfile: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed

	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return fmt.Errorf("planfile: write: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("planfile: flush: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("planfile: close: %w", err)
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return fmt.Errorf("planfile: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("planfile: replace %s: %w", path, err)
	}
	return nil
}

// Load reads a plan file.
func Load(path string) (*model.Plan, Meta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("planfile: %w", err)
	}

	var file File
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, Meta{}, fmt.Errorf("planfile: %s is not a plan file: %w", path, err)
	}
	// A missing format means something else entirely: every plan this tool
	// writes declares one. Checked first, because the mismatch message below
	// would be nonsense for a file that never claimed a format.
	if file.Format == 0 {
		return nil, Meta{}, fmt.Errorf("planfile: %s has no format marker, so it is not a plan file", path)
	}
	if file.Format != Format {
		return nil, Meta{}, fmt.Errorf(
			"planfile: %s is format %d, and this build reads format %d; make a new plan",
			path, file.Format, Format)
	}
	return &file.Plan, file.Meta, nil
}

// Age reports how long ago a plan was made. A zero time means it did not say.
func (m Meta) Age(now time.Time) time.Duration {
	if m.CreatedAt.IsZero() {
		return 0
	}
	return now.Sub(m.CreatedAt)
}

// Describe renders the provenance for a log line.
func (m Meta) Describe() string {
	if m.CreatedAt.IsZero() {
		return "no timestamp"
	}
	out := "made " + m.CreatedAt.Format(time.RFC3339)
	if m.Version != "" {
		out += " by " + m.Tool + " " + m.Version
	}
	return out
}
