# Troubleshooting

Start with `tidb-plex config check`. It validates the configuration and probes
both services, and its output usually names the problem.

## Nothing appears in Plex

**Markers are written by a run, not on playback.** Plex is not asked for
anything when you press play, so a library that has never been synced has no
markers.

1. `tidb-plex plan` prints what a run intends to do. If it says nothing to do,
   the next two checks explain why.
2. `tidb-plex library` lists every item with the id a lookup would use. Items
   showing `no id` cannot be looked up at all.
3. Run `tidb-plex apply --yes`, or `tidb-plex sync --yes` from a scheduled job.

If the plan says items have no data, TheIntroDB does not have those segments yet.
Contributing them is done on [theintrodb.org](https://theintrodb.org) for now; a
`submit` command that sends Plex's own detections back to the API is planned, and
is tracked in [TODO.md](TODO.md).

## "no-provider-id"

The item has no TMDb, IMDb or Tvdb id, so there is nothing to look it up by.
Set your Plex library's metadata agent to TMDb and refresh the item. TMDb is
also the most accurate source for episodes, which is why it is preferred.

## "cannot confirm whether Plex is running"

The tool looks for Plex's process and then asks its HTTP endpoint. If neither
answers and it cannot tell, it refuses to write, because writing to the database
while Plex is running without knowing is how databases get damaged.

Fix `plex.url` so it reaches your server, or stop Plex and pass `--plex-stopped`
to state it explicitly.

## "Plex is running; stop it first"

Stop Plex, or pass `--live`. A live write is then refused again if any playback
session is active, because a write during playback competes with Plex's own
reads. `--skip-session-check` exists but should not be needed.

## "Plex database ... is under /mnt/user"

Point `plex.database` at the pool path, `/mnt/cache/...` on Unraid, rather than
the `/mnt/user` FUSE view. SQLite's locking through shfs is not reliable enough
to write safely, and a write that goes wrong can corrupt the database.

If you genuinely accept that risk, `plex.allow_fuse_path = true` bypasses the
check. Doing so is not recommended.

## "no marker tag yet"

The Plex database has no marker tag (`tags` row with `tag_type = 12`), which
means Plex has never created a marker of its own. New markers have to hang off
that tag, and creating it ourselves would be inventing schema.

Let Plex detect one intro or credits marker, which it does during analysis of an
episode that has siblings, then run the tool again.

## Everything is skipped as a PAL speed-up

Your season really is at 25 or 50 fps while most shows are at film rate, and
timings measured on a film-rate release drift on it. The tool therefore ignores
community timings for that file and uses only its own chapters.

If you would rather have the community timings anyway, set
`apply.pal_guard = false`.

## Markers disappeared after Plex re-analysed a season

Expected. Plex clears custom markers when it re-analyses an item, and there is no
event for it. The ledger notices on the next run and the plan reports `reapply`,
which puts them back.

## Markers moved or look wrong

- **A few seconds out**: mostly a cut difference. Check that lookups are being
  made with the file length (`tidb-plex plan` reports the source of each
  marker), and that the file has not been replaced since.
- **Minutes out**: usually a PAL speed-up, or an episode-numbering mismatch. A
  show that Plex numbers differently from TMDb (common with anime and
  absolute-order shows) can receive another episode's timings. Those items are
  not detectable automatically; exclude them with `--show` filters.
- **Warning about more than one TMDb id**: the item is ambiguous, so it is
  skipped rather than guessed at. Fix the metadata in Plex.

## "budget" or "usage-limited" in a plan

The day's allowance is spent. The run stops cleanly and resumes tomorrow. Without
an API key the allowance is 500 requests a day instead of 1000; a key on your
TheIntroDB account raises it.

## Running out of allowance every day

Expected on a large library: a full pass needs more requests than a day allows.
Progress accumulates, because every answer including a 404 is cached, so each
day works on new content. See [theintrodb-api.md](theintrodb-api.md).

## Undo

Every apply writes a journal to `state/undo/`. `tidb-plex undo latest --yes`
reverts the most recent run by restoring the exact previous rows.

Only the most recent journal is offered by default. Reverting an older one after
a newer run would overwrite the newer changes, so doing that takes an explicit
path.
