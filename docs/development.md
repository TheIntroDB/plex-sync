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
make tools        # install the pinned formatter
make build        # build ./bin/plex-sync
make test         # go test ./...
make test-race    # ...with the race detector
make test-live    # the tests that need a real Plex database copy
make test-linux   # the whole suite on Linux, in a container
make vet-other    # every released platform compiles, test files included
make lint         # fmt-check + go vet
make fmt          # format the source
go run . --help
go run . tui      # the interactive interface
```

Formatting is `gofumpt`, pinned in the Makefile, and `make lint` fails when
something is not formatted: run `make fmt` before committing. Code that is only
gofmt'd is not enough, so an editor should be pointed at gofumpt rather than
gofmt or the two will disagree.

Import grouping is the usual one: standard library, then everything else. Order
inside a group is gofmt's business.

## Interfaces between packages

Keep these signatures stable; the tests and the TUI depend on them.

### `internal/plexapi` (HTTP)

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

### `internal/plexdb` (SQLite)

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

### The other platforms

CI runs the tests on Linux, macOS and Windows, and they are not interchangeable.
Twice now the suite has been green on the machine it was written on and red in
CI: first macOS passing while Linux and Windows failed, then both passing while
Windows failed. Waiting for CI to say so costs a cycle every time, so:

```bash
make test-linux    # the whole suite on Linux, in a container
make vet-other     # every released platform compiles, test files included
```

`test-linux` runs the tests for real. `vet-other` only compiles the others —
Windows cannot be run here — but that is not nothing: it catches a test that does
not build for a platform, which is how a Windows-only failure could have been
seen before pushing.

Neither replaces running CI. They are the checks worth doing before pushing, not
after.

A rule that came out of this: do not write a test whose result depends on the
machine it runs on. The specific mistakes were a test that built a macOS-shaped
directory and assumed the platform's candidate list would contain it, one that
used a Unix absolute path with a string prefix check, and one that asserted POSIX
file modes. Any of those can be written to pass everywhere by passing the
platform, the environment or a real temporary path in.

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

### Plex Pass, and the marker tag

Both matter when something looks wrong, and neither is obvious from the code.

**Plex Pass is required for the feature to be visible at all.** Markers can be
written to the database without one, and Plex's API serves them, which is why this
was believed to work and did not: Plex's *clients* only offer to skip when the
server owner and the player's account both have an active Pass. A report of
"nothing happens" on a server without a Pass is not a bug to chase.

**The marker tag is a first-run problem.** `taggings` rows hang off a `tags` row
with `tag_type = 12`, which Plex only creates when it writes a marker of its own,
so a library that has never held one has nowhere to put them and a run stops and
says so. `--force-create-initial-tag` on `sync`, `apply` or `setup` makes the row.
It is a flag and not a setting on purpose: it writes to Plex's schema, and a
library that has held one marker already has the tag. The same sequence is in
`plexdb.withoutTagTriggers`, which is what makes the write possible at all — the
table's FTS4 triggers cannot be prepared outside Plex, so they come off for the
duration and go back from the SQL read out of the database.

`scripts/live-e2e.sh` creates that tag so the live tests have somewhere to write,
which is why it mentions it.

### Skipping without Plex Pass

[PlexAutoSkip](https://github.com/mdhiggins/PlexAutoSkip) takes the other approach:
instead of relying on Plex's own skip button it watches playback and seeks past the
segment itself. Untested here, and its custom marker files are its own format, but
it is what to point someone at who does not have a Pass.

Live TheIntroDB checks work the same way:

```bash
TIDB_LIVE=1 go test ./internal/tidb/ -run Live -v
```

Both kinds are worth running before a release. Every defect they guard against
was found by running against the real thing, and none of them was reproducible
against the synthetic fixtures.
