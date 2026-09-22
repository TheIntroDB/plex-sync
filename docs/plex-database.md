# The Plex database

This is the part of the project that is not guessable from documentation,
because Plex does not document it. Everything here was established by reading
Plex's own behaviour and cross-checking against the community tools that do the
same thing (notably MarkerEditorForPlex, which this project's write path matches
byte for byte).

## Why the database is involved at all

Plex documents marker endpoints, and creating a marker through them is a Plex
Pass feature. On a server without Plex Pass, `POST /library/metadata/{id}/marker`
returns HTTP 400 for every marker type, so writing markers means writing the
database. Every tool in this space does, and this one is no exception.

Everything that can be done over HTTP is done over HTTP. Only the marker write,
the backup and the undo touch the file.

## Where markers live

Plex has changed where it keeps markers, and this tool writes both places:

| storage | read by |
| --- | --- |
| `metadata_item_setting_markers` | Plex from schema revision `202309200911` onwards, which is what its API serves |
| `taggings` and `media_parts.extra_data` | older versions, and the other tools in this space |

A run writes the marker table first, then the older pair. Either one working means
the skip button appears, which is what keeps a single writer serving every version.

### `metadata_item_setting_markers`

The table Plex reads today. One row per marker:

| column | meaning |
| --- | --- |
| `marker_type` | the kind, as a number — see below |
| `metadata_item_setting_id` | foreign key into `metadata_item_settings`, `ON DELETE CASCADE` |
| `start_time_offset` | start, in milliseconds |
| `end_time_offset` | end, in milliseconds |
| `title` | what Plex shows beside the marker, `Intro` or `Credits` |
| `extra_data` | this tool stamps it, so its own rows can be told from Plex's |

The numbers are not guessable and are not documented anywhere, so they were
measured by writing a row and asking Plex what it called the result:

| `marker_type` | Plex reports |
| --- | --- |
| 1 | `intro` |
| 2 | `commercial` |
| 3 | `bookmark` |
| 4 | `resume` |
| 5 | `credits` |
| 0, and 6 upwards | no type at all |

`metadata_item_settings` is **per account** — it is where playback state lives,
including `skip_count` and `last_skipped_at` — so a marker has to belong to
somebody and this tool writes the lowest account id, which is the one Plex used
for the library owner. Plex only creates a settings row when somebody has actually
watched the item, so the writer creates one when it is missing; without it there is
nothing for the foreign key to point at.

Because a settings row can exist before this tool has ever run on an item, a
marker row with no provenance stamp is treated as Plex's own: it is left alone
unless `apply.policy` is `prefer-theintrodb`, since Plex's own detection is a
better answer than a guess. The same reasoning applies to `taggings`, where the
ledger records what this tool wrote.

### `taggings` (older Plex, and the other tools)

One row per marker.

| column | meaning |
| --- | --- |
| `metadata_item_id` | the `metadata_items.id` of the movie or episode |
| `tag_id` | the marker tag: a `tags` row with `tag_type = 12` |
| `index` | the row's position ordered by start time **across all marker types** for that item |
| `text` | `intro`, `credits`, `commercial` or `bookmark` |
| `time_offset` | start, in milliseconds |
| `end_time_offset` | end, in milliseconds |
| `extra_data` | a small JSON blob, see below |

`index` is why inserting one marker can require rewriting the others: it is a
single sequence shared by intro, credits and commercial markers for the item,
ordered by start time, and Plex sorts by it. The sequence is **0-based**, which
was established rather than assumed: a tool that has run against production
libraries builds its own zero-based position list from the same ordering and
finds its values identical to what Plex had stored, byte for byte, across tens
of thousands of rows.

Every tag must already exist in `tags` with `tag_type = 12`. That applies to this
`taggings` copy, not to the marker table above: Plex 1.43 ignores these rows
entirely, so on such a server the tag is not needed for markers to appear. A
database that has never had a marker does not have the tag row, and a marker row
written there has nothing to hang off until it does. `--force-create-initial-tag`
makes it, once, on the run that needs it. See "Why the marker tag can be created
after all" below.

### `media_parts.extra_data`

A JSON object whose members describe the part, with one member, `url`, encoding
all the others as a query string. Two members matter:

```json
{
  "pv:intros": "{\"MediaPartMarkersArray\":{\"attributeName\":\"intros\",\"version\":5,\"MediaPartMarker\":[{\"startTimeOffset\":61000,\"endTimeOffset\":90000}]}}",
  "pv:credits": "{\"MediaPartMarkersArray\":{\"attributeName\":\"credits\",\"version\":4,\"MediaPartMarker\":[{\"startTimeOffset\":2300000,\"endTimeOffset\":2400000,\"final\":true}]}}",
  "url": "pv%3Acredits=...&pv%3Aintros=..."
}
```

Rules that are easy to get wrong:

- The members are serialised with their **keys sorted**, and nested values are
  JSON strings, not nested objects.
- `url` is rebuilt from the members: each `key=value` pair joined with `&`, with
  both sides percent-encoded so that **only `A-Z a-z 0-9 _ -` remain literal**.
  `net/url.QueryEscape` leaves `.`, `!`, `~`, `*`, `'`, `(`, `)` unescaped, and
  `net/url.Values.Encode` additionally turns a space into `+`. Both are wrong
  here; the encoder applies the missing escapes explicitly.
- Rows in the pre-1.40 format, or which do not parse as an object with a `url`
  member, are left exactly as they are. Guessing at an older format is how a
  media database gets damaged.
- The `final` flag on a credits marker is what makes Plex treat it as the end of
  the item, which is what raises the Up Next prompt. It belongs on the credits
  marker that reaches the end of the file, and nowhere else.

The per-row `extra_data` on the `taggings` entry uses the same encoding and one
of three exact shapes:

| marker | value |
| --- | --- |
| intro | `{"pv:version":"5","url":"pv%3Aversion=5"}` |
| credits | `{"pv:version":"4","url":"pv%3Aversion=4"}` |
| credits, final | `{"pv:final":"1","pv:version":"4","url":"pv%3Afinal=1&pv%3Aversion=4"}` |

## Durations

`metadata_items.duration` is the metadata agent's rounded runtime, typically a
whole minute. `media_items.duration` is the real file length. They differ by
more than thirty seconds for a large fraction of a typical library.

Markers must be placed against `media_items.duration`. Using the metadata
duration puts a credits marker past the end of the file on a shorter release,
and inside the credits on a longer one.

## Ids

Provider ids live in `tags` with `tag_type = 314`, joined through `taggings`,
with values like `tmdb://1396`, `imdb://tt0944947`, `tvdb://121361`. The prefix
is eight characters including `://`.

An item carrying more than one TMDb id is ambiguous (it happens with shows Plex
has matched twice). Those items are skipped rather than guessed at, both for
lookups and for anything else.

`metadata_type` is 1 for a movie and 4 for an episode. An episode's `parent_id`
points at its season, and the season's `parent_id` points at the show, which is
where the season and episode numbers live.

**Id levels are not interchangeable.** Every level of the tree carries ids, and
they mean different things:

```
show     Bones                 imdb://tt0460627  tmdb://1911   tvdb://75682
season                         tmdb://5520       tvdb://9191
episode  The Man in the Bear   imdb://tt0529895  tmdb://125526 tvdb://298563
```

TheIntroDB is asked for an episode by **series** id plus season and episode, so an
episode lookup has to use the show's ids. Building it from the episode's own row
asks about an id that does not exist — `tmdb_id=125526&season=1&episode=4` answers
"media not found" every time, while the show's `tmdb_id=1911`, `tvdb_id=75682` and
`imdb_id=tt0460627` all answer with the same timings. This is worth stating because
the failure is silent: the tool simply finds nothing for every episode in the
library and looks like it is working.

The one exception is a library matched by the legacy agents, where an episode's
guid was `com.plexapp.agents.thetvdb://<series>/<season>/<episode>` and therefore
does hold the series id. So the show's ids are used when the show has any, and what
the episode carried is left alone when it does not.

## Safety rules

The database is a live SQLite file that Plex writes to constantly, including
while people are streaming. The rules below are not optional.

- **Never write through a FUSE path.** On Unraid, `/mnt/user/...` goes through
  shfs, whose locking is not reliable enough for SQLite. Use the pool path
  (`/mnt/cache/...`). The tool refuses a FUSE path unless explicitly allowed.
- **Set `PRAGMA busy_timeout`.** Thirty seconds. Without it a concurrent Plex
  write fails the transaction immediately.
- **Prefer Plex stopped.** MarkerEditorForPlex recommends it, and it is right.
  Writing live is allowed only with `--live` and only when no session is active,
  because a write during playback is a write during Plex's own reads.
- **Back up first.** One copy, not a paged incremental one: an incremental
  backup restarts whenever Plex commits and may never finish. `VACUUM INTO` is a
  single step and is what this tool uses.
- **Verify before you write.** Re-read the item's markers inside the transaction
  and compare them with the plan. If anything moved, skip the item. A plan is a
  snapshot; the library is not.
- **Journal before you write.** Every insert, delete, index change and
  `extra_data` replacement is recorded before it happens, so undo can restore
  the previous bytes. Undo replays the journal in reverse inside one transaction.

## Why the marker tag can be created after all

This is about the `taggings` copy, which older Plex versions read. Rows there hang
off a `tags` row with `tag_type = 12`, and Plex only creates that row when it
writes a marker of its own, which needs Plex Pass. On a server without Plex Pass
it never appears, so this tool is the only thing that would ever create it. The
first release concluded that it could not, and refused instead:

```
CREATE VIRTUAL TABLE fts4_tag_titles_icu USING fts4(
  tag, tokenize=collating 'root@colStrength=primary;colAlternate=shifted')
```

`tags` carries four FTS4 triggers, and one of them inserts into that table. SQLite
has to prepare a trigger's body before it can run a write against the table the
trigger is on, and preparing that body means opening the FTS4 table, which means
resolving the `collating` tokenizer. That tokenizer is Plex's own ICU tokenizer
and exists only inside the SQLite that Plex ships, so **every** write to `tags`
fails, before the trigger's `WHEN` clause is ever considered, no matter that a
`tag_type` of 12 is not in that clause.

Measured, not theorised:

| client | result on `INSERT INTO tags` |
| --- | --- |
| this program, pure-Go SQLite | `no such module: fts4` |
| the system `sqlite3` command | `unknown tokenizer: collating` |

A CGO build with FTS4 compiled in would fail the same way, because FTS4 is not
the missing part: the tokenizer is.

**What was wrong was the conclusion, not the measurement.** A plain `INSERT` is
impossible. `DROP TRIGGER` is not, because dropping a trigger does not require its
body to be prepared. So the row is written by taking the triggers off the table,
inserting, and putting them back:

1. read each trigger's `sql` out of `sqlite_master`, so that what goes back is
   exactly what Plex put there rather than a copy of it kept in this program
2. `DROP TRIGGER` for each of the four
3. `INSERT INTO tags (id, tag_type, tag, created_at, updated_at) VALUES (...)`
4. recreate each trigger from the SQL read in step 1
5. count the triggers on `tags`, and refuse to commit when the count is not what
   it was

All five steps happen in one transaction, and SQLite's DDL is transactional, so an
interruption anywhere rolls the schema back with the data: there is no window in
which the database exists without its triggers. This is `plexdb.withoutTagTriggers`.
The journal's `tag_insert` operation uses the same sequence in reverse when undo
removes the row, because a `DELETE` fires the before-delete and after-delete
triggers and meets the same problem from the other side.

The one visible cost is that the FTS index does not gain the new row, since
filling it needs the same tokenizer, so Plex's full-text search will not match the
new tag's name. Markers are resolved by tag id and not by search, so nothing this
tool does depends on it. `taggings` and `media_parts`, the tables markers are
written to, carry no triggers at all.

An earlier version of this section said the row had to come from Plex and that no
other tool could make one. That was wrong: it is what stopped this tool writing
anything into a library that has never held a marker.

The row is made by `--force-create-initial-tag`, after the same backup and with the
same undo journal as any other write. It is a flag rather than a setting or a
button because it is a first-run and diagnostic step: a library that has held one
marker already has the tag, and nothing after that needs it.

## Where the files live

The database is never at the top of Plex's application-support directory: it is
under `Plug-in Support/Databases`. The directory itself is platform-dependent, so
the tool searches the platform's own locations and accepts the first that really
holds a database.

| platform | application-support directory |
| --- | --- |
| macOS | `~/Library/Application Support/Plex Media Server` |
| Windows | `%LOCALAPPDATA%\Plex Media Server` |
| Linux, container | `/config/Library/Application Support/Plex Media Server` |
| Linux, package | `/var/lib/plexmediaserver/Library/Application Support/Plex Media Server` |
| Linux, snap | `/var/snap/plexmediaserver/common/Library/Application Support/Plex Media Server` |
| FreeBSD | `/usr/local/plexdata/Plex Media Server` |

Three ways to point at it yourself, in increasing precedence:

1. `plex.config_dir` in the configuration file, for an unusual layout.
2. `plex.database` in the configuration file, which names the file itself and is
   used exactly as written.
3. `PLEX_CONFIG_DIR` or `PLEX_DB` in the environment, which is what a container
   and a scheduled job should use.

## What Plex does to markers on its own

When Plex re-analyses a season it clears custom markers. There is no event for
this and nothing to subscribe to, so the tool detects it after the fact: the
ledger knows what it wrote, and a run that finds those markers missing treats it
as a re-apply rather than a first add. That is the `reapply` reason in a preview.
