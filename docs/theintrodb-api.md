# TheIntroDB API

What this tool relies on, and what it does when the API pushes back. The
behaviour below is not incidental: it is the difference between a run that
finishes and one that spends its daily allowance in the first two minutes.

## The lookup

```
GET {base_url}/media?tmdb_id=<id>[&season=<n>&episode=<n>][&duration_ms=<ms>]
Authorization: Bearer <api_key>          (optional)
```

`imdb_id` and `tvdb_id` are accepted instead of `tmdb_id`. TMDb is preferred
because the data is keyed on it and it is the most accurate for television.

**`duration_ms` is what makes the answer cut-aware.** It is the real file
length, not the runtime Plex reports. Sending it lets the API return the timings
that belong to the release you actually have, which matters because episodic
releases are routinely a few seconds different from each other, and a credits
marker placed against the wrong cut lands inside the credits or past the end of
the file.

The tool sends `media_items.duration` from the Plex database rather than the
metadata agent's rounded runtime, because the two disagree by more than thirty
seconds for a large fraction of a typical library.

## The answer

A 200 body carries the keys `intro`, `recap`, `credits` and `preview`. Each is
either an object or `null`, and a key may also arrive as a list of objects, so
both shapes are accepted.

Each segment has `start_ms` and `end_ms`, and **either may be null**:

- a null `start_ms` means the segment begins at 0:00;
- a null `end_ms` means it runs to the end of the media.

Neither is "no data". The tool resolves a null start against zero and a null end
against the file length, and drops the segment only when both are missing, when
it starts past the end of the file (a different cut), or when the resulting
range is shorter than `segments.min_marker_ms`.

A `credits` segment that reaches the end of the file becomes Plex's **final**
credits marker, the one that raises the Up Next prompt.

## Statuses

| status | meaning | what the tool does |
| --- | --- | --- |
| 200 | data | uses it |
| 404 | TheIntroDB holds nothing for this item | records the miss and moves on; never invents a segment |
| 401, 403 | the key is missing or rejected | terminal: the run stops rather than hammering the API |
| 429 | rate or allowance limit | waits, as described below |

## Limits

Two independent limits apply, and the order matters: the rate limiter runs
first, then the daily allowance.

**Rate limit: 30 requests per 10 seconds**, keyed by account when authenticated
and otherwise by client IP. This tool paces at **25 per 10 seconds** (400 ms
apart). Pacing at the ceiling produces 429s as soon as there is any jitter,
because the request that lands while another is in flight counts too.

A rate-limit 429 carries only a `Retry-After` header and a non-JSON body. There
is no error code to inspect.

**Daily allowance: 1000 requests per UTC day** per account, and 500 per public
IP without a key. A usage-limit 429 is distinguishable: it carries
`X-UsageLimit-Limit`, `X-UsageLimit-Remaining` and `X-UsageLimit-Reset`, where
the reset is seconds until UTC midnight and can be nearly a day, plus a JSON body
with a `code` of `usage_limit_exceeded` or
`specific_media_usage_limit_exceeded`.

### The trap this tool is built to avoid

A usage-limit reset is measured in hours, so it must not be clamped to a short
wait. If a five-minute ceiling is applied to it, an exhausted allowance turns
into: wait five minutes, send one request, receive a 429, wait five minutes. A
full scan then sends two requests and skips the rest of the library, which looks
like a broken tool rather than an exhausted budget.

So the two kinds of 429 are handled differently:

- a **rate-limit** reset is clamped to five minutes, because it is a window;
- a **usage-limit** reset is trusted up to twenty-four hours.

Consecutive 429s grow the wait (doubling, capped at eight times and at five
minutes), and the next send is held when `X-RateLimit-Remaining` reports one or
less.

## Budgeting

A full pass over a large library cannot finish in one day: a library of thirty
thousand items needs thirty thousand requests against an allowance of one
thousand. So progress has to accumulate rather than complete:

- every answer, including a 404, is cached in the ledger;
- a cached 200 is trusted for `theintrodb.hit_ttl_days`;
- a cached 404 is trusted for `theintrodb.miss_ttl_days`, because new
  submissions appear over time and a permanent "no" would hide them;
- the run stops cleanly when the day's allowance is spent, and the next day
  resumes where it left off.

Requests are counted in the ledger as they are made, so a run that is killed
still shows what it spent.

## Key validation

```
GET {base_url}/user/stats
Authorization: Bearer <api_key>
```

Returns contribution statistics for a valid key, and a 401 for a missing or
rejected one. That 401 is expected when no key is configured: it means the API
is reachable, not that anything is wrong, and the tool reports it that way.
