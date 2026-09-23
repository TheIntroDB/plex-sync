# Architecture

`plex-sync` is a batch tool. It reads your Plex library, asks TheIntroDB for the
segments it knows, and writes the result into the Plex database as native intro
and credits markers. There is no daemon, no web interface and no agent that runs
while you watch something: a run is started by you, by cron, by a systemd timer
or by the container's scheduler, and it exits when it is done.

```
             Plex HTTP API                       TheIntroDB API
   library, ids, chapters, markers          intro / recap / credits / preview
                    |                                  |
                    v                                  v
              +-----------+                    +---------------+
              | plex_api  |                    |  theintrodb   |
              +-----------+                    +---------------+
                    |                                  |
                    |            +----------+          |
                    +----------->|  ledger  |<---------+     responses cached,
                                 +----------+               misses remembered
                                       |
                                       v
                                 +-----------+
                                 |  planner  |   sources merged, ranges resolved,
                                 +-----------+   policy applied, PAL guard
                                       |
                                       v
   Plex SQLite  <------------------+---------+
   taggings + media_parts.extra_data|  writer |
                                    +---------+
```

## Why writes go through the database

Plex does document marker endpoints: `POST /library/metadata/{ids}/marker`
creates one, and there are edit and delete operations beside it. They are not
usable here, for a reason worth recording.

Marker creation is a Plex Pass feature. On a server without Plex Pass the
endpoint answers **400 for every marker type**, including the values that are
documented, so there is no type to fall back to and no error message that says
why. That was measured on a real server rather than inferred. Its account
reports `subscription.active: false`, and its database has never held a marker.

None of that applies to writing markers into the database, which is what this
tool does. Measured on a server with `subscriptionActive="0"`: a row written into
`metadata_item_setting_markers` comes straight back from
`GET /library/metadata/{id}?includeMarkers=1` as an `intro`, and a row written into
`taggings` never does. Plex Pass gates Plex's own detection and its marker API, not
what Plex will read out of its own database.

That distinction is real and it is also not the one that decides whether the
feature works, which is worth being clear about because it cost this project a
week of looking in the wrong place. Plex's *clients* only offer skipping with an
active Plex Pass, on the server owner's account and on the account the player is
signed in as. So a marker can be in the database, be served by the API, and still
produce no button — which is what a server without a Pass looks like, and what
this tool looked like when it was broken in a different way entirely.

Everything that can be done over HTTP is done over HTTP. Only the marker write,
the backup and the undo touch the file.

See [plex-database.md](plex-database.md) for the schema, the exact bytes Plex
expects, and the safety rules that come with editing a live database.

## Modules

| package | responsibility | touches |
| --- | --- | --- |
| `internal/config` | load and validate TOML, environment and flags; find Plex's files per platform | disk (read) |
| `internal/model` | the shared data types | nothing |
| `internal/plexapi` | enumerate libraries, read ids, chapters and existing markers | Plex HTTP |
| `internal/plexdb` | read and write markers, back up, undo | Plex SQLite |
| `internal/tidb` | TheIntroDB client: pacing, budget, scan records, null times | TheIntroDB HTTP |
| `internal/ledger` | durable state: scan records, lookup cache, what we wrote, run history | our SQLite |
| `internal/planner` | merge sources, map types, resolve ranges, decide the change set | nothing |
| `internal/source` | markers from chapter names Plex extracted, and local detection for the rest | `plexapi` output, ffmpeg + fpcalc |
| `internal/schedule` | the cron subset the process holds its own timer with | nothing |
| `internal/planfile` | read and write plans on disk, with provenance and a format version | disk (read/write) |
| `internal/sync` | orchestrate a run: inventory, fetch, plan, apply, undo | all of the above |
| `internal/cli` | argument parsing and human output | everything |
| `internal/tui` | the terminal interface | `internal/app` |
| `internal/api` | the local JSON control API | `internal/app` |

Data flows one way: sources produce segments, the planner turns them into
`ItemPlan`s, the writer turns a plan into rows. No package below the planner
performs a lookup, and no source package writes anything.

`internal/planfile` is what lets the two halves run in different places: a plan
can be decided where Plex is reachable and written where the database is, and
`internal/sync` reconciles each change against the database before it is applied,
so a plan that is no longer current is skipped rather than trusted.

## Sources

TheIntroDB is authoritative. It is cut-aware (it is given the real file length,
so it can hand back the timing that matches the file you actually have), and
every row is community-verified.

The alternate sources exist to cover what TheIntroDB does not have yet. They can
never override it for a segment type it already answered:

1. **theintrodb** - the API, keyed by TMDb (IMDb and Tvdb as fallback).
2. **chapters** - chapter names Plex already extracted into the file's
   metadata ("Intro", "Opening Credits", "End Credits", "Recap"). Free: no
   lookups, no media reads, exact for the file on disk. Off by default.
3. **detection** - local audio fingerprinting, run only for episodes no other
   source covered. Off by default, requires `ffmpeg` and `fpcalc`.

A segment type is won by the first enabled source that has it, and each written
marker records which source produced it, so the status page and the plan output
can always answer "where did this timing come from".

## Scanning a large library

Two different questions get answered by two different pieces of ledger state,
and keeping them apart is what makes a library of tens of thousands of items
finish:

- **"Do we still trust this body?"** — the lookup cache (`lookups`), with a TTL.
  A 200 is cached for `theintrodb.hit_ttl_days` and a 404 for
  `theintrodb.miss_ttl_days`, because a 404 becomes a 200 the moment someone
  submits the timing.
- **"Have we spent a request on this item at all?"** — the scan record
  (`scans`), with no expiry. A record means the item has been looked up, and a
  lookup that finds one is answered from the cache without a request.

The second is what a large library runs on. Time alone never causes a request:
`LookupForced` is the only thing that asks about a scanned item again, and that
is reached by naming an item for a re-scan (`--rescan`, `--rescan-all`, or `R`
on the Preview screen). Expiring the scan record would put the library back to
re-asking it forever and never finishing.

When the day's allowance is spent the client refuses the next request, and
`internal/sync` treats that as a stopping point rather than an error: the run
ends, records the number of items still to scan, and the next run begins where
it left off. The nightly timer therefore makes progress every night instead of
failing every night.

A plan also carries a `Selection`: the items turned off, and the items named for
a re-scan. It is recorded in the plan file so a plan made on a host and applied
in a container writes the same subset, and so the interface's selection is a
decision the writer honours rather than a note the writer ignores.

## Idempotence and provenance

The ledger records every marker the tool wrote. On the next run, a marker that
is still present and still matches the plan is left alone; one that Plex wiped
(which happens whenever Plex re-analyses a season) is re-added. A marker Plex
detected itself is never touched under `apply.policy = "fill"`.

## Failure behaviour

Nothing partial is written. A run plans first, applies in transactions of
`apply.chunk_size` items, and re-checks each item against the live database
inside its transaction, so an item that changed between planning and writing is
skipped rather than overwritten. Every operation is journalled to an undo log
before it is applied, and `undo` replays that log in reverse.
