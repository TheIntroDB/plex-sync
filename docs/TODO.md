# Batches

Work is tracked in batches so each one can be reviewed and committed on its own.
A batch is done when its boxes are ticked and `go build ./... && go test ./...`
is clean.

Status: **batches 1 to 5 complete**.

## Batch 1 - foundation

- [x] Repository, licence (GPL-3.0), module, pinned dependencies
- [x] `internal/model` shared types
- [x] `internal/config` TOML, environment and validation
- [x] `internal/httpclient` one outbound HTTP path over Fiber's client
- [x] `docs/architecture.md`, `docs/plex-database.md`

## Batch 2 - the three core packages

- [x] `internal/plexapi` Plex over HTTP
- [x] `internal/plexdb` Plex over SQLite, including the write path and undo
- [x] `internal/ledger` and `internal/tidb` API client

## Batch 3 - decisions

- [x] `internal/planner` merge, map, decide, PAL guard
- [x] `internal/source` chapters and local detection
- [x] `internal/sync` orchestration: inventory, fetch, plan, apply, undo
- [x] `internal/app` shared runtime wiring

## Batch 4 - the interface

- [x] `internal/tui` Bubble Tea interface: status, library, plan, runs, settings
- [x] `internal/cli` cobra commands, including the non-interactive paths
- [x] `internal/api` local JSON control API (Fiber + Huma, OpenAPI 3.1)

## Batch 5 - shipping

- [x] `Dockerfile`
- [x] `unraid/theintrodb-plex.xml`
- [x] GitHub Actions: build, vet, test on three platforms, plus cross-compile
- [x] GoReleaser configuration
- [x] `docs/troubleshooting.md`, `docs/theintrodb-api.md`

## Batch 6 - live verification

Run against a real Plex server (1.43.4), the real TheIntroDB API, a real key and
a real database copy. It found five defects that no synthetic test would have.

- [x] Plex database path: real Plex keeps it under `Plug-in Support/Databases`,
      not at the top of the application support directory
- [x] Per-platform discovery (macOS, Windows, Linux package, snap, container,
      FreeBSD) plus `plex.database`, `plex.config_dir`, `PLEX_DB`,
      `PLEX_CONFIG_DIR`
- [x] Token discovery: `.LocalAdminToken` on modern installs, `Preferences.xml`
      on Linux and Windows, the preferences plist on older macOS
- [x] Backups: `VACUUM INTO` fails against every real Plex database
      (`no such collation sequence: icu_root`); now uses SQLite's page-level
      online backup API
- [x] `extra_data`: nested members are JSON strings, as Plex writes them, not
      nested objects
- [x] `taggings.extra_data`: payload values are quoted, as Plex and the
      reference implementation write them
- [x] The writer no longer invents a `final` credits marker, which made the
      taggings payload and the media_parts payload disagree
- [x] `extra_data` encoder verified byte-for-byte against three rows copied out
      of a real database
- [x] Apply and undo verified end to end against a copy of a real database,
      including a byte-identical restore
- [x] `undo` gained the same escapes as `apply` (`--live`, `--plex-stopped`,
      `--skip-session-check`); it could not be run at all while Plex ran

## Batch 7 - the verification, made repeatable

- [x] `internal/plexdb/live_test.go` and `scripts/live-e2e.sh`: apply, check every
      byte that lands, undo, check the row is back exactly as it was
- [x] The assumption the design rests on is now a test: `taggings` and
      `media_parts` carry no triggers, because `tags` does and cannot be written
      from outside Plex

## Batch 8 - the container

- [x] `schedule`: the process holds its own cron, so the image needs no cron
      daemon and no shell. Cron subset tested, including leap days and the rule
      that two restricted day fields mean either may match
- [x] `TIDB_PLEX_SCHEDULE`, `TIDB_PLEX_RUN_ON_START`, `[schedule]` in the config,
      and `config check` rejecting a malformed expression
- [x] Two bugs found while testing: `--print-next` ignored the configured
      schedule, and a read-only scheduled run returned an error instead of a
      report
- [x] `docs/scheduling.md`: systemd, launchd, Task Scheduler, cron, container
- [x] Unraid template: `PostArgs` was `sync --yes`, and its default database path
      has never existed in any Plex install
- [x] Image built and run (distroless, no CGO); verified it finds the database in
      the read-only mount, holds the schedule, runs read-only against a live
      server, and refuses to write while a session is playing

## Batch 9 - plan files

- [x] `plan --save` and `apply --plan`, so deciding what changes and changing it
      can happen in different places
- [x] A plan is reconciled against the database before anything is written;
      applying the same plan twice writes nothing the second time
- [x] Format version, atomic save, and provenance (what made it, when, against
      which database)
- [x] Verified in the container with Plex unreachable: applied, wrote both
      markers, undid them again
- [x] `docs/architecture.md` corrected: its module table still listed the
      abandoned Python files

## Not done yet

These are known gaps, not surprises.

### Verification gaps

Things that are implemented and unit-tested but have never met the real world.

- [x] ~~Byte-exactness of `media_parts.extra_data`.~~ Closed: the encoder
      reproduces three rows copied out of a real database, byte for byte,
      including the url it builds from them.
- [x] ~~Plex over HTTP against a real server.~~ Closed for identity, sections,
      items with GUIDs, and session counting, all exercised against Plex 1.43.4.
- [x] ~~A real TheIntroDB response.~~ Closed for `/media` answers and for the
      401 an absent key produces. Still unverified: the exact 429 headers, since
      no limit was hit.
- [ ] **Chapters against a real file.** Both test files have no chapters
      (`pv:chapters` is `{"Chapters":{}}`), so the chapter source has never had
      anything to match. The parser is unit-tested on captured field-name
      variants only.
- [ ] **Concurrency with Plex itself.** New rows are inserted with an explicit
      id taken while holding `BEGIN IMMEDIATE`, which is correct under this
      tool's own write lock and untested against a concurrent Plex writer.
- [ ] **Fingerprint detection against real media.** The matching maths is
      unit-tested on synthetic fingerprints and the orchestration is tested with
      a stubbed runner, but no real file has been fingerprinted.
- [ ] **Plex serving markers this tool wrote.** The rows are written correctly
      into a real database, but no client has been shown the resulting skip
      button, because the test server cannot hold markers at all (no Plex Pass).
- [ ] **`Retry-After` as an HTTP date.** Only integer seconds are parsed; a date
      would be ignored and fall back to the backoff multiplier.

### Features

- [x] ~~**Submit** - send Plex's own detected markers back to TheIntroDB.~~
      Out of scope by decision: this integration requests segments and writes
      them into Plex, and does not submit. The client therefore has no submit
      method and the tool has no submit command.
- [ ] **`changes?since=` delta refresh** - a TTL re-check over the cached 404s
      does not scale, because it costs hundreds of requests a day on a large
      library. One endpoint listing accepted media keys since a timestamp would
      replace that with a single request. It needs an indexed `AcceptedAtMs` on
      submissions, which is an API-side change.
- [x] ~~**The daily scheduler**~~ Built, and the container uses it. `schedule`
      holds its own cron in-process, so the image needs no cron daemon and no
      shell. `TIDB_PLEX_SCHEDULE` and `TIDB_PLEX_RUN_ON_START` are honoured, and
      a malformed expression is caught by `config check`. Verified in the
      container: it finds the database inside the read-only mount, holds the
      schedule, runs read-only against a live server, opens the database
      read-write as the non-root user, and refuses to write while a session is
      playing.
- [x] ~~**Scheduler examples**~~ `docs/scheduling.md` covers systemd, launchd,
      Task Scheduler, cron and the container.
- [x] ~~**A plan file**~~ `plan --save` and `apply --plan`. Planning needs the
      Plex API; writing needs only the database, so the two halves can run in
      different places. Verified: the container applied a plan made on the host
      with Plex unreachable, wrote both markers, and undid them again. Applying
      the same plan twice wrote nothing the second time (skipped=1), which is
      what makes it safe on a timer. This also closed the container gap below.
- [ ] **Container image publishing.** The image builds and runs (`docker build`,
      distroless, no CGO), and the write path has now been watched happen inside
      it, so all that is left is pushing it to a registry and wiring the release
      workflow to do so.
- [ ] **Translations** - TheIntroDB/translations scans integration repos for
      locale directories. This one has none yet.
