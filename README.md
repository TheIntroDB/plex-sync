# TheIntroDB Plex Sync

<p align="center">
  <img src="https://raw.githubusercontent.com/TheIntroDB/theintrodb-assets/main/logo-banner.png">
</p>

`plex-sync` fills in Plex's **intro** and **credits** markers from
[TheIntroDB](https://theintrodb.org), so Plex does not have to audio-fingerprint
every file in your library. It is a single binary with a terminal interface and
a scriptable command line. There is no web interface and no browser involved.

Plex Pass is still required. The markers are never read to show skip buttons unless the server owner has a valid license and the feature enabled. See other requirements below.

---

## TOC
- [What it does](#what-it-does)
- [Requirements](#requirements)
- [Installation](#installation)
- [Quickstart](#quickstart)
- [Usage](#usage)
- [Scheduling](#scheduling)
- [Configuration](#configuration)
- [Sources](#sources)
- [Safety](#safety)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [License](#license)

---

## What it does

Plex detects intros by fingerprinting episodes, which is slow, and it can only
do it for episodes that have siblings to compare against. TheIntroDB already has
community-verified, cut-aware timings for a large number of shows and movies.
This tool fetches those and writes them into Plex as native markers, so the Skip
Intro and Skip Credits buttons appear without any local analysis.

- **Cut-aware.** TheIntroDB is given the real file length, so the timings it
  returns match the release you actually have, not an arbitrary other one.
- **Nothing is replaced silently.** By default Plex's own markers are kept and
  the tool only adds what is missing. A second policy lets TheIntroDB replace
  them.
- **Every write is reversible.** Each run writes an undo journal, and the Plex
  database is backed up before the first change.
- **Alternate sources fill the gaps.** Chapter names Plex already extracted, and
  optional local detection, cover items TheIntroDB does not have yet. They can
  never override TheIntroDB for a segment type it answered.
- **A large library finishes, a day at a time.** An item that has been looked up
  is remembered, so it is never asked about twice. A run spends the day's
  request allowance on the items it has never seen, stops when that is used up,
  and the next run carries on from the same place. A library of tens of thousands
  of items converges over a few days instead of re-asking forever.

---

## Requirements

- A Plex Media Server you can reach over HTTP, with a token.
- Read and write access to Plex's database file
  (`com.plexapp.plugins.library.db`). Writing markers requires it, because Plex's
  own marker API needs Plex Pass.
- **An active Plex Pass, or nothing will show.** Plex only offers skipping with a
  Pass, so both accounts need one: the **server owner** who administers the
  library, and **the account the player app is signed in as** — their own
  subscription, or membership of a Plex Home whose admin has one, which covers
  Managed Users. This holds even when the markers are already in the database,
  which is the confusing part: the rows are there, Plex serves them, and the client
  still shows no button. See [docs/troubleshooting.md](docs/troubleshooting.md) if
  you have a Pass and the button is still missing.
- **TMDb metadata is recommended** for accuracy. IMDb and Tvdb ids work as a
  fallback but are less exact for TV episodes.

The database is found automatically: the tool searches the platform's own
locations (macOS, Windows, Linux packages, snaps, containers and FreeBSD) and
uses the first one that really holds a database. `plex.database` or
`plex.config_dir` overrides it, and so do `PLEX_DB` and `PLEX_CONFIG_DIR`.

A token is required, and is read from the machine when you do not supply one:
`.LocalAdminToken` on a modern Plex install, `Preferences.xml` on the Linux and
Windows distributions, and the preferences plist on older macOS installs.

**One flag worth knowing if a first run refuses.** Markers are written into a
table whose rows hang off a tag Plex only creates when it writes a marker of its
own, so a library that has never held one has nowhere for them to go. When that is
the case the run says so and stops; `--force-create-initial-tag` makes the row,
behind the usual backup and undo journal, and it is a one-time debug step rather
than part of a normal run. It is not a setting, and it is not in the interface,
because almost nobody needs it: any library that has held a single marker already
has the tag.

**If you do not have Plex Pass**, this tool can still put markers in the database
and Plex will serve them, but no client will offer to skip anything, so there is
little point running it for that.

An API key is optional. With one, the daily allowance is higher and your own
pending submissions are included in what you get back.

---

## Installation

Pick whichever suits your machine. All of them install the same program; they
differ in how it arrives and how it is kept up to date.

- [Homebrew](#homebrew) — one command, and `brew upgrade` keeps it current
- [Prebuilt binaries](#prebuilt-binaries) — download the archive for your platform
- [Docker](#docker) — the container, which holds its own schedule
- [Unraid](#unraid) — import the template into Community Applications
- [Go](#go) — build it yourself with the Go toolchain

### Homebrew

```bash
brew tap Pasithea0/tap
brew trust --formula pasithea0/tap/plex-sync   # one-time, needed for a third-party tap
brew install plex-sync
```

`brew upgrade` keeps it current, and installing this way also sidesteps the
macOS quarantine problem described under prebuilt binaries: Homebrew downloads
the archive itself, so Gatekeeper is not involved.

The formula lives in [Pasithea0/tap](https://github.com/Pasithea0/tap), which
holds my other tools too.

### Prebuilt binaries

Download the archive for your platform from
[Releases](https://github.com/TheIntroDB/plex-sync/releases), check it
against the published checksums, and put the binary on your `PATH`:

```bash
tar xzf plex-sync_0.1.0_darwin_arm64.tar.gz
shasum -a 256 -c checksums.txt        # or sha256sum on Linux
sudo install -m 755 plex-sync /usr/local/bin/
```

The archive also contains the README, the licence, the documentation and the
Unraid template, so you have the matching docs for the version you installed.

**macOS:** the binaries are not signed or notarised, because that needs a paid
Apple Developer account. A binary downloaded through a browser carries macOS's
quarantine flag, and Gatekeeper will kill it on launch — silently, with no
output and exit code 137, which looks like a crash for no reason. Clear the flag
once:

```bash
xattr -d com.apple.quarantine $(which plex-sync)
```

Downloading with `curl` or `git` does not set the flag, so only the browser route
is affected, and installing with [Homebrew](#homebrew) avoids it entirely.
`plex-sync version` printing anything at all, rather than exiting 137, means it
is fine.

Targets: Linux (amd64, arm64, armv7), macOS (amd64, arm64), Windows (amd64,
arm64) and FreeBSD (amd64, armv7). Linux armv7 is the 32-bit Raspberry Pi build.

### Docker

```bash
docker run -d --restart=unless-stopped \
  --name plex-sync \
  -e PLEX_URL=http://plex:32400 \
  -e PLEX_TOKEN=xxxxxxxxxxxx \
  -v "/mnt/cache/appdata/plex/Library/Application Support/Plex Media Server:/plex:ro" \
  -v "/mnt/cache/appdata/plex-sync:/state" \
  theintrodb/plex-sync:latest schedule --yes
```

The Plex database must be mounted at its real, non-FUSE path. On Unraid that
means the `/mnt/cache/...` path, never `/mnt/user/...`: SQLite locking through
the shfs layer is not reliable and a write can corrupt the database. The tool
refuses a `/mnt/user` path unless you explicitly allow it.

The container holds its own schedule, so there is nothing else to install. It
needs the Plex URL, the token and the database mounted read-write, because that
is where the markers go.

**A container does not have to reach Plex at all.** Planning needs the Plex API;
writing needs only the database. Split the two if the container cannot see the
server, or if you would rather decide what changes before it happens:

```bash
# Wherever Plex is reachable: decide, and record the decision.
plex-sync preview --save /mnt/cache/appdata/plex-sync/preview.json

# The container: write exactly that, and nothing else.
docker run --rm \
  -v "/mnt/cache/appdata/plex-sync:/state" \
  -v "/mnt/cache/appdata/plex/Library/Application Support/Plex Media Server/Plug-in Support/Databases:/db" \
  -e PLEX_URL=http://unreachable \
  -e PLEX_DB=/db/com.plexapp.plugins.library.db \
  theintrodb/plex-sync:latest apply --preview /state/preview.json --yes --plex-stopped
```

Neither half is trusted on its own: applying a saved plan checks every change
against the rows actually in the database, and skips anything that no longer
matches rather than guessing. Applying the same plan twice does nothing the
second time.

### Unraid

Import the template from `unraid/plex-sync.xml`, or add this repository's
template URL to Community Applications. The template asks for the Plex URL, the
token and the Plex database path, and defaults to a daily run.

### Go

```bash
go install github.com/TheIntroDB/plex-sync@latest
```

The binary has no runtime dependencies and needs no CGO, so it cross-compiles to
Linux, macOS and Windows on both amd64 and arm64.

---

## Quickstart

Installed it? Two commands, with Plex stopped:

```bash
plex-sync setup            # one-time: says what it found, and prints the schedule
plex-sync sync --yes       # fetch what TheIntroDB has and write it into Plex
```

`setup` reports what it found and prints the crontab line to keep running
afterwards. It writes nothing unless you ask it to. It writes
nothing without the same backup and undo journal as any other run, and
`--dry-run` reports without writing anything at all.

If Plex is running, set `apply.allow_live = true` in the configuration (or toggle it in the settings screen) to write while it does, as long as nothing is
being streamed. Every write is reversible:

To run the sync process automatically, open the tui, navigate to settings, and `start cron` by pressing the enter key on the setting.

```bash
plex-sync preview --limit 20     # see what a run would change, touching nothing
plex-sync undo latest --yes      # put back what the last run wrote
plex-sync tui                    # or do all of this in the terminal interface
```

---

## Usage

Run `plex-sync` with no arguments in a terminal and you get the interface:

| key | screen |
| --- | --- |
| `1` | Status: what is in the library, what the ledger recorded, API quota |
| `2` | Library: every matched item with its ids, sources and marker state |
| `3` | Plan: exactly what a run would change. Up/down moves, space turns an item on or off, `A`/`N` select all or none, `R` marks one for a re-scan |
| `4` | Runs: history, and the undo journals from previous applies |
| `5` | Settings: every setting, editable in place with the arrow keys and enter |
| `?` | Help |
| `q` | Quit |

Settings are changed in the interface: the arrow keys move between rows, enter
toggles a boolean or cycles a choice, and enter on anything else opens a line you
type into. Escape abandons an edit. The status line says whether a change needs a
restart or applies to the next run, and the file is written as you go.

The same work is available as commands, for cron and scripts:

```bash
plex-sync config check          # validate configuration and reach both services
plex-sync library               # list items and the names Plex uses, for --show
plex-sync preview --show "the last of us"   # what a run would change, for one show
plex-sync preview --save preview.json  # ...and save it, to write later without Plex
plex-sync preview --deselect 1234      # leave a rating key out of the write
plex-sync preview --rescan tmdb:1399:1:1   # ask about one item again
plex-sync apply --yes           # write the markers (--dry-run to see them first)
plex-sync apply --preview preview.json --yes   # write a saved preview, without Plex
plex-sync undo latest --yes     # revert the most recent run
plex-sync status                # ledger, quota and recent runs
plex-sync sync --yes            # inventory, fetch and write, once
plex-sync schedule --yes        # the same, on a schedule the process holds itself
plex-sync api serve             # local JSON API, OpenAPI schema at /openapi.json
```

Add `--json` to any command for machine-readable output.

### Scheduling

```bash
plex-sync schedule --yes                       # daily at 07:30, by default
plex-sync schedule --cron '0 5 * * *' --yes    # or whenever you want
plex-sync schedule --print-next                # check an expression first
```

The process holds its own schedule, so nothing extra is needed: no cron daemon
and no shell, which is also why the container image can run this way. Run it
after Plex's own maintenance window so the two are not fighting over the
database. If you would rather your own scheduler owned it, `sync --yes` and
`schedule --once --yes` are both single passes:

```cron
30 7 * * * /usr/local/bin/plex-sync schedule --once --yes >>/var/log/plex-sync.log 2>&1
```

See [docs/scheduling.md](docs/scheduling.md) for systemd, launchd, Task
Scheduler, cron and container arrangements.

### Large libraries

TheIntroDB allows 1000 requests per UTC day with an API key, and 500 without one.
A library of tens of thousands of items cannot be scanned in a day, so the tool
is built to be left running rather than to be finished in one pass:

- **An item is scanned once.** Every lookup is recorded, and a recorded item is
  answered from the ledger afterwards without a request, however long ago it was
  scanned. Expiring that record is what would make a large library never finish,
  so nothing expires it.
- **A run uses the whole day's allowance.** It scans the items it has never seen
  until the allowance is spent, then stops cleanly and logs how many are left.
  That is not an error, and the nightly timer keeps its schedule.
- **The next run continues.** Because every scan is recorded, it picks up exactly
  where the last one stopped. A 40,000-item library at 1000 requests a day
  converges in about 40 days, and each run writes the markers it has data for.
- **Re-scanning is deliberate.** Nothing is asked about twice on a timer. When a
  submission lands and you want fresh answers, name the items:

```bash
# One item, by its lookup key (the same key the Library screen shows):
plex-sync sync --yes --rescan tmdb:1399:1:1

# Everything, at the cost of a full library of requests:
plex-sync sync --yes --rescan-all
```

In the interface, press `p` for a plan, move with the arrow keys, and press `R`
on the item you want re-scanned; planning again is what asks for it. `space`
turns an item off, which leaves it in the plan but out of the write.

`plex-sync status` reports how many items have been scanned and how they split,
which is the number that says how far through the library the tool has got.

---

## Configuration

Configuration is a TOML file, overridden by environment variables and then by
flags. Run `plex-sync config init` to write a commented example.

Lookup order: `$PLEX_SYNC_CONFIG`, `./plex-sync.toml`,
`$XDG_CONFIG_HOME/plex-sync/config.toml`, `/etc/plex-sync/config.toml`.

| key | default | meaning |
| --- | --- | --- |
| `plex.url` | `http://127.0.0.1:32400` | Plex Media Server base URL |
| `plex.token` | | Plex token, for the HTTP API |
| `plex.database` | | path to `com.plexapp.plugins.library.db` |
| `plex.config_dir` | | Plex application-support directory, if the database path is not given |
| `theintrodb.api_key` | | optional TheIntroDB API key |
| `theintrodb.daily_budget` | `1000` | requests per UTC day |
| `sources.chapters` | `false` | use chapter names as a source |
| `sources.detection` | `false` | run local fingerprint detection |
| `segments.intro` | `true` | write intro markers |
| `segments.recap` | `true` | write recap markers |
| `segments.credits` | `true` | write credits markers |
| `segments.preview` | `false` | write preview markers |
| `apply.policy` | `fill` | `fill` keeps Plex's markers; `prefer-theintrodb` replaces them |
| `apply.allow_live` | `false` | write while Plex is running, if nobody is streaming |
| `apply.backup` | `true` | back up the database before the first write |
| `state_dir` | `~/.config/plex-sync` | ledger, backups and undo journals |

Environment variables: `PLEX_URL`, `PLEX_TOKEN`, `PLEX_DB`, `PLEX_CONFIG_DIR`,
`TIDB_API_KEY`, `TIDB_API_URL`, `PLEX_SYNC_STATE_DIR`, `PLEX_SYNC_LOG_LEVEL`,
`PLEX_SYNC_CHAPTERS`, `PLEX_SYNC_DETECTION`, `PLEX_SYNC_ALLOW_LIVE`.

---

## Sources

TheIntroDB is authoritative: it is community-verified and cut-aware, because the
real file length is sent with every lookup. It always wins a segment type it
answered.

The alternate sources only fill the segment types TheIntroDB did not cover:

- **chapters** uses chapter names Plex already extracted from the file ("Intro",
  "Opening Credits", "End Credits", "Recap"). Free: no lookup, no media read,
  and exact for the file on disk. Chapter names are whatever the release group
  chose to write, so it stays a fallback. Off by default.
- **detection** is local audio fingerprinting: the intro of one episode is
  matched against another episode of the same season whose intro is already
  known. It only runs for episodes no other source covered, and only writes a
  marker when the match is unambiguous. Needs `ffmpeg` and `fpcalc` on `PATH`.
  Off by default.

Every marker records which source produced it, so you can always tell where a
timing came from.

### PAL speed-ups

Some releases are PAL speed-ups: 25 or 50 fps versions of film-rate content,
playing 4.3% fast. Timings measured on the normal-speed release drift on those,
by about five seconds at two minutes and two minutes at fifty. When an episode
at 25 or 50 fps sits in a season that is mostly film rate, or a movie's file is
2.5% to 6% shorter than its listed runtime, community timings are ignored for
that file and only its own chapters are used. Seasons that are entirely PAL
cannot be told apart from a speed-up, so they are left alone.

---

## Safety

- `apply` and `undo` require `--yes`. Without it they print the plan and stop.
- Before writing, the tool must positively confirm whether Plex is running. If
  it cannot tell, it refuses and says so, rather than guessing.
- Writing while Plex runs is refused unless you pass `--live`, and it is then
  refused again if anything is streaming.
- The database is backed up before the first write of a run, pruned to
  `apply.keep_backups` copies.
- Writes happen in transactions of `apply.chunk_size` items. Before each item is
  touched, its markers are re-read inside the transaction and compared with the
  plan; an item that changed in the meantime is skipped instead of overwritten.
- Every operation is journalled before it is applied, so `undo` can put the
  exact previous bytes back.

---

## Troubleshooting

**No markers appear.** Markers are written by a run, not on playback. Check
`plex-sync preview` to see what a run intends to do, and that your items have a
TMDb or IMDb id (`plex-sync library`).

**"cannot confirm whether Plex is running".** Plex's process was not visible and
its HTTP endpoint did not answer. Fix `plex.url`, or stop Plex and pass
`--plex-stopped`.

**"Plex database ... is under /mnt/user".** Point `plex.database` at the pool
path (`/mnt/cache/...` on Unraid). SQLite locking through FUSE is not reliable
enough to write safely.

**Everything is skipped as a PAL speed-up.** Your season is genuinely at 25 or
50 fps. Set `apply.pal_guard = false` to use community timings anyway.

**Markers disappeared after Plex re-analysed a season.** That is expected: Plex
clears custom markers when it re-analyses. The next run sees it and puts them
back.

See [docs/troubleshooting.md](docs/troubleshooting.md) for more.

---

## Development

```bash
make build     # build ./bin/plex-sync
make test      # go test ./...
make test-live # the tests that need a real Plex database copy
make lint      # gofumpt + go vet, pinned in the Makefile
make fmt       # format the source
make run       # build and open the terminal interface
```

Run `make tools` once to install the pinned formatter. `make lint` fails when
anything is not formatted.

See [docs/architecture.md](docs/architecture.md) for how the pieces fit together
and [docs/development.md](docs/development.md) for the interfaces between
packages.

---

## License

GPL-3.0. See [LICENSE](LICENSE).
