# Troubleshooting

Start with `plex-sync config check`. It validates the configuration and probes
both services, and its output usually names the problem.

## Nothing appears in Plex

**Markers are written by a run, not on playback.** Plex is not asked for
anything when you press play, so a library that has never been synced has no
markers.

1. `plex-sync preview` prints what a run intends to do. If it says nothing to do,
   the next two checks explain why.
2. `plex-sync library` lists every item with the id a lookup would use. Items
   showing `no id` cannot be looked up at all.
3. Run `plex-sync apply --yes`, or `plex-sync sync --yes` from a scheduled job.

If the plan says items have no data, TheIntroDB does not have those segments yet.

This integration only ever requests segments and writes them into Plex. It does
not submit anything, so there is no `submit` command. If you want to contribute a
timing you know, do it on [theintrodb.org](https://theintrodb.org), which is
where contributions are made.

## "read the Plex library: ... timeout or cancel"

The request that reads a library section did not come back inside
`plex.timeout_s` (20 seconds by default).

The library read is paged, the first request included, so no single request asks
Plex to build a response proportional to the size of the section. On a build
older than that fix the first request was unpaged, and Plex answered it by
assembling the whole section at once — which on a section of tens of thousands
of episodes outlives any sensible timeout. Raising `plex.timeout_s` was the
workaround then:

```toml
[plex]
timeout_s = 120
```

It still works as a workaround, but it is no longer the fix, and it is worth
checking that the failing request is the section read before reaching for it: a
timeout on `/library/metadata/<key>` is a different problem.

`/identity` answering — which is what `setup` and `config check` report as `plex
server ok` — proves only that the URL reaches a Plex server. It is
unauthenticated, so it says nothing about the token, and it is a small answer, so
it says nothing about whether a library-sized one can be served. A rejected token
answers 401 immediately rather than timing out.

## "could not read the shows behind the episodes; they keep their own ids"

The run could not read the show behind each episode out of the Plex database, so
every episode keeps the provider id in its own row. That id is the episode's, and
TheIntroDB is asked for an episode by series id, so those lookups answer "media
not found" — the run spends its allowance on questions that cannot succeed, and
no TV markers are written. It is one WARN line and no other symptom, which is why
it is worth reading.

The known cause was a library larger than SQLite's bound-parameter limit: the
walk bound one parameter per episode in a single statement, and above 32,766
(`SQLITE_MAX_VARIABLE_NUMBER`) the statement failed outright with `too many SQL
variables`. The walk is chunked now, so a library of any size reads.

The other causes are ordinary: no read access to the database, or episodes whose
show rows carry no provider ids. Check what `plex-sync library` prints for the
ids an item would be looked up with.

## A "new device used your server" notification every run

Expected once, not nightly. Plex identifies a caller by the identifier it sends,
and one it has not seen before is a new device, which is what it notifies about.

The identifier is stored in `<state_dir>/client-id` and reused, so a fresh
install registers one device and keeps it. Every run looking like a new device
means that file is not surviving: most often the state directory is inside a
container that is recreated without a volume, or it is somewhere that is wiped.
Keep `state_dir` — the same directory the ledger lives in — on something
persistent.

If the notification has empty brackets where the device name should be, the name
is not set. `plex.device_name`, or `PLEX_SYNC_DEVICE_NAME` for a container, is
what Plex shows; it defaults to `plex-sync`.

The device list in Plex is not cleaned up by any of this. Runs from before the
identifier was stored each left an entry of their own, and those stay until they
are removed by hand.

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

## "no marker tag"

The Plex database has no marker tag (`tags` row with `tag_type = 12`), which means
Plex has never created a marker of its own in this library. Marker rows hang off
that tag, so there is nowhere to write them until it exists.

```bash
plex-sync sync --yes --force-create-initial-tag
```

A run that needs the tag stops and names that flag rather than writing half of
what it meant to. The flag is a first-run and diagnostic step: it writes one row,
after a backup, and `undo latest --yes` removes it again. It is deliberately not a
setting and not in the interface, because a library that has held a single marker
already has the tag and nobody else will ever see this.

Creating it is a one-time step, and this tool does it on request:

```bash
plex-sync setup          # checks everything and makes the tag
```

on the run that needs it, with the database backed up first and `undo latest
--yes` to remove it again.

The reason it is not automatic is that it touches Plex's schema rather than data:
the `tags` table carries four FTS4 triggers whose table uses Plex's own ICU
tokenizer, so the triggers are dropped for the duration of the write and put back
exactly as they were, all inside one transaction. An earlier release refused to do
this at all and told you to get Plex to create the row, which needs Plex Pass and
so never happened on most servers. The full explanation, with the measurements
behind it, is in [plex-database.md](plex-database.md).

Nothing else is affected. If your server already has the tag, this never comes up:
`taggings` and `media_parts`, the tables this tool does write to, carry no
triggers.

## The skip button does not appear

Check what Plex itself says first, because that is the whole question:

```bash
curl -s -H 'Accept: application/json' \
  "http://127.0.0.1:32400/library/metadata/<ratingKey>?includeMarkers=1&X-Plex-Token=<token>" \
  | python3 -m json.tool | grep -A4 Marker
```

If Plex reports the marker, the write is fine and the problem is the client or the
player, not the database. If it reports none, work down this list:

1. **Is there an active Plex Pass?** This is the answer more often than anything
   else, and it is the one that is easy to miss because the markers are plainly
   there. Both sides need it: the account that administers the server, and the
   account the player app is signed in as — their own subscription, or a Plex Home
   where the admin has one, which includes Managed Users. Plex's clients will not
   offer to skip without it, even though the rows exist and Plex's own API serves
   them. For a server without a Pass, see the note on PlexAutoSkip in the README.
2. **Is the marker in `metadata_item_setting_markers`?** On Plex 1.43 and later
   that is the table it reads, and a marker written only to `taggings` is invisible
   to it. There is no `metadata_item_setting_markers` on older servers, and there
   the `taggings` row is the one that counts.
3. **Does the item have a `metadata_item_settings` row?** Markers hang off one by
   foreign key. Plex creates these as people watch things, and the tool creates one
   when it is missing, so this is only a problem if a run was interrupted.
4. **Is "Generate video intro marker" on** for that library, under Settings →
   Library? That governs Plex's own detection rather than reading markers, but it
   is worth checking on a server that has never had one.
5. **Was anything written at all?** `plex-sync status` shows the last run, and
   `plex-sync preview --show "<title>"` says whether the item has data to write.
   An episode that TheIntroDB has no timings for is skipped by design.
6. **A marker Plex detected is not replaced** unless `apply.policy` is
   `prefer-theintrodb`. That is deliberate: Plex's own detection beats a guess.
7. **Are the server and the player up to date?** Markers are old, but the plumbing
   around them has changed more than once.

A restart is not needed, and neither is a metadata reimport: both were tried while
this was being worked out, and neither made a marker appear that was in the wrong
table.

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
  made with the file length (`plex-sync preview` reports the source of each
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
day works on new content.

## Undo

Every apply writes a journal to `state/undo/`. `plex-sync undo latest --yes`
reverts the most recent run by restoring the exact previous rows.

Only the most recent journal is offered by default. Reverting an older one after
a newer run would overwrite the newer changes, so doing that takes an explicit
path.
