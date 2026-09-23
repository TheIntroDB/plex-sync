# Running it on a schedule

The point of this tool is to run it overnight and forget about it. There are two
ways: let the tool hold its own schedule, or hand it to your operating system's
scheduler. Both end up running the same work.

Whichever you choose, the run has to be **confirmed** to write. Nothing here
writes without `--yes`, and nothing writes while someone is watching something.

## What a run does

| command | what happens |
| --- | --- |
| `plex-sync schedule` | reads the library, reports what is missing, changes nothing |
| `plex-sync schedule --yes` | the same, then writes the markers |
| `plex-sync schedule --once --yes` | one pass, then exit: for systemd, launchd or Task Scheduler |
| `plex-sync schedule --print-next` | the next five run times, then exit |

Every run writes a line per item to the ledger and a log line to standard error,
so `journalctl`, the Docker log or a file all work as a record of what happened.

## Large libraries, and the daily allowance

TheIntroDB allows 1000 requests per UTC day with an API key and 500 without one,
so a library of tens of thousands of items is not scanned in one night. That is
expected, and the schedule is how it is meant to be worked through:

- An item that has been looked up is recorded, and a recorded item is answered
  from the ledger afterwards without a request. Nothing expires that record, so
  an item is asked about once rather than once every couple of weeks.
- A run spends what is left of the day's allowance on items it has never seen,
  then stops. It logs how many items are still to scan and exits **zero**: the
  allowance being spent is a stopping point, not a failure, and a nightly timer
  that reported an error every night would be a timer nobody reads.
- The next run continues from the same place, because every scan is recorded.

A schedule therefore converges: a 40,000-item library at 1000 requests a day
gets through in about 40 days, writing the markers it has data for each night.
Raising `theintrodb.daily_budget` beyond the allowance does not help — the
server refuses the request either way; the budget is what stops the client
before the refusal.

To ask about something again, name it. Nothing is re-scanned on a timer:

```bash
plex-sync sync --yes --rescan tmdb:1399:1:1    # one item, by lookup key
plex-sync sync --yes --rescan-all              # everything; a full library of requests
```

`plex-sync status` reports how many items have been scanned, split by whether
TheIntroDB had data, which is the progress figure for a library this size.

## The built-in schedule

```bash
plex-sync schedule --yes
```

The process holds the schedule itself, so it needs no cron, no shell and no
second process. That is what the container image runs, since the image has none
of those. The expression is the usual five fields, evaluated in local time:

```
minute hour day-of-month month day-of-week
```

```bash
plex-sync schedule --yes --cron '30 7 * * *'    # daily at 07:30
plex-sync schedule --yes --cron '0 */6 * * *'   # every six hours
plex-sync schedule --yes --cron '0 3 * * 0'     # Sundays at 03:00
```

The default is `30 7 * * *`. Plex runs its own maintenance tasks at 07:00, and
two things writing to the same database at the same time is exactly what the
backup and the transaction handling exist to avoid. Being an hour behind it is
the cheap way to stay out of its way.

Check what any expression means before trusting it to a nightly job:

```bash
$ plex-sync schedule --cron '0 */6 * * *' --print-next
2026-09-21T00:00:00-06:00
2026-09-21T06:00:00-06:00
2026-09-21T12:00:00-06:00
2026-09-21T18:00:00-06:00
2026-09-22T00:00:00-06:00
```

Set it in configuration rather than on the command line, so the container or a
service file does not have to carry it:

```toml
[schedule]
cron = "30 7 * * *"
run_on_start = false
```

`run_on_start` is off by default, deliberately: recreating a container should
not, by itself, decide to write to your library.

The expression can also come from the environment, which is how the Unraid
template sets it:

```bash
PLEX_SYNC_SCHEDULE='0 5 * * *'
```

A malformed expression is rejected when the configuration is checked, not at
midnight:

```bash
$ plex-sync config check
plex-sync: configuration is not usable: schedule.cron: minute field: 60-60 is
outside the allowed range 0-59
```

## systemd (Linux)

Run it once per firing and let systemd own the timer. Give it a user so the
state directory belongs to that user:

```ini
# /etc/systemd/system/plex-sync.service
[Unit]
Description=Fill in Plex intro and credits markers from TheIntroDB
After=network-online.target plexmediaserver.service

[Service]
Type=oneshot
User=plex
Environment=PLEX_SYNC_STATE_DIR=/var/lib/plex-sync
ExecStart=/usr/local/bin/plex-sync schedule --once --yes
# A run that finds nothing to do exits 0, so the timer keeps its schedule.
```

```ini
# /etc/systemd/system/plex-sync.timer
[Unit]
Description=Nightly plex-sync run

[Timer]
# Do not stack up missed runs after downtime: one catch-up pass is enough.
OnCalendar=daily
Persistent=true
RandomizedDelaySec=30m
AccuracySec=1m

[Install]
WantedBy=timers.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now plex-sync.timer
systemctl list-timers plex-sync.timer      # when it will run next
journalctl -u plex-sync.service -n 50      # what the last run did
```

`RandomizedDelaySec` matters if you run this on more than one server: without
it, every instance asks TheIntroDB at the same second.

If Plex and this tool run under different users, the tool needs to be able to
write the database and Plex needs to be able to see the result. Running as the
same user as Plex, as above, avoids the question.

## launchd (macOS)

```xml
<!-- ~/Library/LaunchAgents/org.theintrodb.plex.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>org.theintrodb.plex</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/plex-sync</string>
    <string>schedule</string>
    <string>--once</string>
    <string>--yes</string>
    <string>--plex-stopped</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key><integer>7</integer>
    <key>Minute</key><integer>30</integer>
  </dict>
  <key>RunAtLoad</key>
  <false/>
  <key>StandardOutPath</key>
  <string>/tmp/plex-sync.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/plex-sync.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/org.theintrodb.plex.plist
launchctl start org.theintrodb.plex          # run it now, to test
tail -f /tmp/plex-sync.log
```

`--plex-stopped` is there because Plex on a desktop is usually running. It
asserts that Plex is stopped, which is a claim the tool cannot verify, and it
only takes effect when the running check cannot be made at all. See
"Writing while Plex is running" below.

## Task Scheduler (Windows)

```powershell
$action  = New-ScheduledTaskAction -Execute "$env:ProgramFiles\plex-sync\plex-sync.exe" `
             -Argument 'schedule --once --yes --plex-stopped'
$trigger = New-ScheduledTaskTrigger -Daily -At 7:30am
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable `
             -DontStopOnIdleEnd -ExecutionTimeLimit (New-TimeSpan -Hours 2)
Register-ScheduledTask -TaskName 'plex-sync' -Action $action -Trigger $trigger `
  -Settings $settings -Description 'Fill in Plex markers from TheIntroDB'
```

`-StartWhenAvailable` gives the same catch-up as `Persistent=true` above, and
the execution limit stops a run that hangs on a wedged database from holding the
task forever.

## cron

```cron
# Nightly at 07:30, logging to a file. cron has a minimal environment, so the
# binary is named in full and the state directory is set explicitly.
30 7 * * *  PLEX_SYNC_STATE_DIR=/var/lib/plex-sync /usr/local/bin/plex-sync schedule --once --yes >>/var/log/plex-sync.log 2>&1
```

A run that finds nothing to do is not an error. One that has simply used up the
day's request allowance exits zero as well — see "Large libraries" above — while
one that genuinely fails exits non-zero and logs why; cron will mail that to you
if the machine can send mail.

## Docker

The image holds its own schedule, so the container is the whole setup:

```bash
docker run -d --name plex-sync --restart=unless-stopped \
  -v "/mnt/cache/appdata/plex/Library/Application Support/Plex Media Server:/plex:ro" \
  -v /mnt/cache/appdata/plex-sync:/state \
  -e PLEX_URL=http://172.17.0.1:32400 \
  -e PLEX_TOKEN=xxxxxxxxxxxx \
  -e PLEX_DB="/plex/Plug-in Support/Databases/com.plexapp.plugins.library.db" \
  -e PLEX_SYNC_SCHEDULE='30 7 * * *' \
  ghcr.io/theintrodb/plex-sync:latest schedule --yes
```

### When the container cannot see Plex

Planning reads the library over Plex's API. Writing needs only the database. If
the container cannot reach Plex, or you would rather see what is going to change
before it does, split the work with a plan file:

```bash
# On the host, where Plex is reachable:
plex-sync preview --save /mnt/cache/appdata/plex-sync/preview.json

# In the container, with no route to Plex at all:
docker run --rm \
  -v /mnt/cache/appdata/plex-sync:/state \
  -v "/mnt/cache/appdata/plex/Library/Application Support/Plex Media Server/Plug-in Support/Databases:/db" \
  -e PLEX_DB=/db/com.plexapp.plugins.library.db \
  -e PLEX_URL=http://unreachable \
  ghcr.io/theintrodb/plex-sync:latest apply --preview /state/preview.json --yes --plex-stopped
```

`--plex-stopped` is honest here in a way it is not elsewhere: nothing else is
using that database, so the check is skipped rather than guessed. It matters
because there is no Plex to ask.

The plan file is not trusted on its own. Every change in it is checked against
the rows actually in the database before anything is written, and any item that
no longer looks the way the plan assumed is skipped. Applying the same plan
twice writes the second time nothing, which is what makes this safe on a timer:

```
$ docker run ... apply --preview /state/preview.json --yes --plex-stopped
Wrote 2 marker(s) across 1 item(s), removed 0, skipped 0.
$ docker run ... apply --preview /state/preview.json --yes --plex-stopped
Wrote 0 marker(s) across 0 item(s), removed 0, skipped 1.
```

The plan records what made it, when, and against which database. Applying it
elsewhere logs that the path differs. That is expected, not wrong: the point is
that the same database is mounted at a different place.

The Plex folder is mounted **read only** except for the database. SQLite creates
its journal and shared-memory files next to the database it opens, so a directory
that cannot be written to cannot be written through: mounting the whole folder
read-only means the tool cannot write markers at all. Mount the folder writable,
or mount just `Plug-in Support/Databases` writable on top of a read-only parent.

There is no shell in the image. To look inside:

```bash
docker exec -it plex-sync /plex-sync status
docker logs -f plex-sync
```

The `-v /state` volume holds the ledger, the database backups, undo journals and
fingerprints. Back it up. Without the ledger the tool cannot tell its own
markers from Plex's, and undo needs it.

### Unraid

The template in `unraid/plex-sync.xml` sets all of this up, including
`PLEX_SYNC_SCHEDULE` and an option to run once at container start.

## Writing while Plex is running

SQLite handles concurrent writers, but Plex holding a long read can make a write
wait, and the tool would rather not find out what happens if it gives up
mid-transaction. So a run that wants to write checks three things:

1. **Is Plex running?** If yes, and `apply.allow_live` is off and `--live` was not
   passed, it stops and says so.
2. **Is anything playing?** If a session is active, it stops. This is not
   about the database: rewriting markers for something someone is watching can
   make their client jump.
3. **Only then** does it write, and it warns that it is doing so.

If Plex is genuinely stopped, pass `--plex-stopped` so the check is skipped
rather than attempted. It only applies when the check cannot be made at all: if
the tool can see that Plex is up, it will say so and refuse, whatever the flag
claims.

For an unattended nightly run the simplest arrangement is to let Plex's own
maintenance window end first and schedule this an hour later. The default
schedule does exactly that.

## Afterwards

`plex-sync status` shows what the last runs did, `plex-sync runs` lists them, and

```bash
plex-sync undo latest --yes
```

reverts the most recent write, restoring every touched row byte for byte from the
journal it wrote at the time. That works even if Plex has been running since.