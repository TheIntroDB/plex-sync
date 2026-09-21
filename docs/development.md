# Development

## Requirements

- Go 1.24 or newer (the module is built and released with Go 1.27).
- `ffmpeg` and `fpcalc` only if you enable the local detection source.

## Layout

```
main.go                     binary entry point
internal/buildinfo/         version, commit, build date (set with -ldflags)
internal/config/            TOML + environment + flags
internal/model/             shared types, no I/O
internal/plex/              Plex over HTTP (fiber client), Plex over SQLite
internal/tidb/              TheIntroDB API client
internal/ledger/            our own SQLite state
internal/planner/           source merge, type mapping, change decisions
internal/source/            the alternate sources: chapters, detection
internal/sync/              run orchestration: inventory, fetch, plan, apply
internal/api/               local JSON control API (fiber + huma, OpenAPI)
internal/tui/               the Bubble Tea terminal interface
internal/cli/               cobra commands
```

## Commands

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .
go run . --help
go run . tui            # the interactive interface
```

## Interfaces between packages

Keep these signatures stable; the tests and the TUI depend on them.

### `internal/plex` (HTTP)

```go
type Client struct{ ... }
func NewClient(cfg config.Plex, http *httpclient.Client) *Client

func (c *Client) Identity(ctx context.Context) (map[string]any, error)
func (c *Client) Sections(ctx context.Context) ([]Section, error)
func (c *Client) Items(ctx context.Context, sectionKeys []int) ([]model.LibraryItem, error)
func (c *Client) Chapters(ctx context.Context, ratingKey int) ([]model.Chapter, error)
func (c *Client) Markers(ctx context.Context, ratingKey int) ([]model.ExistingMarker, error)
func (c *Client) ActiveSessions(ctx context.Context) (int, error)
func (c *Client) Running(ctx context.Context) (bool, error)

func TokenFromPrefs(configDir string) string
```

`Running` returns an error when it genuinely cannot tell; callers fail closed.

### `internal/plex` (SQLite)

```go
type DB struct{ ... }
func OpenDB(path string, readOnly bool) (*DB, error)
func (d *DB) Close() error

func (d *DB) MarkerTagID() (int64, error)
func (d *DB) ReadMarkers(ratingKey, tagID int64) ([]model.ExistingMarker, error)
func (d *DB) Parts(ratingKey int64) ([]Part, error)

func EncodeExtra(core map[string]string) string
func RewriteExtra(raw string, types []string, intros, credits []model.Marker) (string, bool)

func Backup(dbPath, destDir string, keep int) (string, error)
func IntegrityCheck(dbPath string) (string, error)

type Journal struct{ ... }
func NewJournal(path string) (*Journal, error)
func (j *Journal) Record(op map[string]any) error
func (j *Journal) Close() error
func ReadJournal(path string) ([]map[string]any, error)
func Undo(dbPath, journalPath string) (int, error)

func (d *DB) ApplyPlans(plans []model.ItemPlan, tagID int64, chunk int, j *Journal) (WriteStats, error)
```

### `internal/tidb`

```go
type Client struct{ ... }
func NewClient(cfg config.TheIntroDB, ledger *ledger.Ledger, http *httpclient.Client) *Client

func (c *Client) Lookup(ctx context.Context, item model.LibraryItem) (model.SegmentSet, LookupResult, error)
func (c *Client) UserStats(ctx context.Context) (map[string]any, error)
func (c *Client) Usage() Usage
```

`LookupResult` carries whether the answer came from cache, the HTTP status, and
the request accounting the status screen shows.

### `internal/ledger`

```go
type Ledger struct{ ... }
func Open(path string) (*Ledger, error)
func (l *Ledger) Close() error

func (l *Ledger) Lookup(key string) (CachedLookup, bool)
func (l *Ledger) PutLookup(key string, status int, body string, ttlSeconds int64, kind string) error

func (l *Ledger) Applied(ratingKey int64) ([]model.Marker, error)
func (l *Ledger) ReplaceApplied(ratingKey int64, markers []model.Marker) error
func (l *Ledger) ForgetApplied(ratingKey int64) error

func (l *Ledger) RecordRequest(source string) error
func (l *Ledger) RequestsSince(ts int64, source string) (int, error)
func (l *Ledger) RecordRun(run Run) (int64, error)
func (l *Ledger) Runs(limit int) ([]Run, error)
func (l *Ledger) Stats() (Stats, error)
```

### `internal/planner`

```go
func Merge(sets map[model.SourceName]model.SegmentSet, cfg config.Config) ([]model.Segment, map[string]string)
func Build(items []model.LibraryItem, in Inputs, cfg config.Config) model.Plan
```

### `internal/source`

```go
// chapters
func Chapters(item model.LibraryItem, chapters []model.Chapter, cfg config.Config) (model.SegmentSet, bool)

// detection
func Detect(ctx context.Context, item model.LibraryItem, refs []Reference, cfg config.Config) (model.SegmentSet, bool, error)
func Compare(reference, target []uint32) (offsetFrames int, distance float64, ok bool)
```

`Compare` is pure: it is unit-tested on synthetic chromaprint frames, so the
matching maths is covered even where no media is available.

## Testing

- Unit tests never touch the network. Both HTTP clients take an injectable
  fiber client, and the tests point them at an `httptest` server or a stub.
- There are no database files in the repository. `internal/plexdb`'s fixture
  builds a real Plex schema in a temporary directory from the statements Plex
  itself uses, and the write tests copy that first, so nothing is shared between
  tests and nothing is left behind.

### Tests against real data

Some things cannot be tested synthetically, because the point of them is what a
real Plex database contains. Those tests are skipped unless an environment
variable points at a copy:

```bash
scripts/live-e2e.sh                       # finds Plex's database itself
scripts/live-e2e.sh /path/to/library.db   # or names one
```

The script copies the database, removes the schema objects that only Plex's own
SQLite can parse, creates the marker tag Plex would have created, and runs the
live tests in `internal/plexdb`. The source database is never opened for
writing. See the comments at the top of the script for why each step is needed.

Live TheIntroDB checks work the same way:

```bash
TIDB_LIVE=1 go test ./internal/tidb/ -run Live -v
```

Both kinds are worth running before a release. Every defect they guard against
was found by running against the real thing, and none of them was reproducible
against the synthetic fixtures.
