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

## Not done yet

These are known gaps, not surprises.

- [ ] **Submit** - send Plex's own detected markers back to TheIntroDB. The API
      has `POST /v3/submit` and `PUT /v3/submissions/{id}`; this needs a client
      method, a dry run that shows candidates and their agreement with accepted
      data, and the same read-only-by-default discipline as everything else.
- [ ] **`changes?since=` delta refresh** - a TTL re-check over the cached 404s
      does not scale, because it costs hundreds of requests a day on a large
      library. One endpoint listing accepted media keys since a timestamp would
      replace that with a single request. It needs an indexed `AcceptedAtMs` on
      submissions, which is an API-side change.
- [ ] **Container image publishing**, and the daily scheduler inside the image.
      The Unraid template offers a `TIDB_PLEX_SCHEDULE` variable that the
      entrypoint does not implement yet, so scheduling is done by cron on the
      host for now.
- [ ] **End-to-end run against a real Plex server.** Everything here is tested
      against a synthetic database built from Plex's real schema, and the write
      path round-trips through it, but it has not yet run against a production
      library. That is the next thing to do before calling this beta.
- [ ] **Detection against real media.** The matching maths is unit-tested on
      synthetic fingerprints and the orchestration is tested with a stubbed
      runner, but no real file has been fingerprint-matched yet.
- [ ] **Scheduler examples** for systemd, launchd and Windows Task Scheduler.
- [ ] **Translations** - TheIntroDB/translations scans integration repos for
      locale directories. This one has none yet.
