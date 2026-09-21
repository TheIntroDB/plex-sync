# Architecture

`tidb-plex` is a batch tool. It reads your Plex library, asks TheIntroDB for the
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

Everything that can be done over HTTP is done over HTTP. Only the marker write,
the backup and the undo touch the file.

See [plex-database.md](plex-database.md) for the schema, the exact bytes Plex
expects, and the safety rules that come with editing a live database.

## Modules

| module | responsibility | touches |
| --- | --- | --- |
| `config.py` | load and validate TOML, environment and flags | disk (read) |
| `models.py` | the shared data types | nothing |
| `plex_api.py` | enumerate libraries, read ids, chapters and existing markers | Plex HTTP |
| `plex_db.py` | read and write markers, back up, undo | Plex SQLite |
| `theintrodb.py` | TheIntroDB client: pacing, budget, cache, null times | TheIntroDB HTTP |
| `ledger.py` | durable state: lookup cache, what we wrote, run history | our SQLite |
| `planner.py` | merge sources, map types, resolve ranges, decide the change set | nothing |
| `sources/chapters.py` | markers from chapter names Plex already extracted | `plex_api` output |
| `sources/detection.py` | local fingerprint detection for what nothing else covers | ffmpeg + fpcalc |
| `sync.py` | orchestrate a run: inventory, fetch, plan, apply, undo | all of the above |
| `cli.py` | argument parsing and human output | everything |

Data flows one way: sources produce `SegmentSet`s, the planner turns them into
`ItemPlan`s, the writer turns a plan into rows. No module below the planner
performs a lookup, and no source module writes anything.

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
