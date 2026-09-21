// Package tidb is the TheIntroDB API client: lookups, the response cache, the
// request pacing and the daily budget.
//
// It goes through internal/httpclient (Fiber's client) for every outbound call,
// and through internal/ledger for every piece of durable state, so this package
// itself owns no sockets and no files by default.
//
// The behaviour is shaped by four facts about the API that are easy to get
// wrong:
//
//   - A 429 is either the rate window (30 requests per 10 seconds) or the daily
//     allowance (1000 per account, 500 per public IP, reset at UTC midnight).
//     A rate-limit 429 carries only a Retry-After in seconds. A usage-limit 429
//     carries X-UsageLimit-Reset in SECONDS UNTIL UTC MIDNIGHT, which can be up
//     to about 86400, plus a JSON body with a code of usage_limit_exceeded or
//     specific_media_usage_limit_exceeded. Clamping that reset to the five
//     minutes that are right for a rate limit turns an exhausted budget into a
//     loop that waits five minutes, sends one request, gets a 429 and waits
//     again, so a whole scan makes two requests and skips the library. The two
//     are told apart and waited for separately here.
//   - TheIntroDB counts from UTC midnight, so the budget is counted from
//     ledger.StartOfUTCDay.
//   - A null start_ms means "begins at 0:00" and a null end_ms means "runs to
//     the end of the media". Neither is a zero and neither is "no data", so
//     both survive parsing as nil and are resolved later against the real file
//     length.
//   - 401 and 403 are terminal: the key is missing or rejected, and retrying
//     cannot fix either.
package tidb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheIntroDB/plex-integration/internal/config"
	"github.com/TheIntroDB/plex-integration/internal/httpclient"
	"github.com/TheIntroDB/plex-integration/internal/ledger"
)

// API paths, relative to the configured base URL.
const (
	mediaPath = "/media"
	statsPath = "/user/stats"
)

// DefaultUserAgent identifies the tool to the API.
const DefaultUserAgent = "tidb-plex"

// Ceilings and thresholds that encode the API's behaviour.
const (
	// maxRateWait caps a rate-limit wait. A rate-limit reset is a window, so a
	// few minutes is always enough.
	maxRateWait = 5 * time.Minute
	// maxUsageWait caps a daily-allowance wait at one day; the server's own
	// value never exceeds about 86400 seconds.
	maxUsageWait = 24 * time.Hour
	// usageResetFloorMS ... a reset above 300 seconds cannot be a rate window.
	usageResetFloor = 300
	// backoffCap is the largest multiplier applied to consecutive rate-limit
	// waits: 1, 2, 4, 8.
	backoffCap = 8
	// usageWaitDefault is used when a usage-limit 429 carries no usable reset.
	usageWaitDefault = time.Hour
)

// Reasons reported by Lookup.
const (
	// ReasonHit: the answer came out of the ledger, with no network call.
	ReasonHit = "hit"
	// ReasonMiss: the cache had no fresh entry, so the network answered. Check
	// Status for what it answered; ReasonNoData is reported for a 404 either
	// way.
	ReasonMiss = "miss"
	// ReasonNoData: TheIntroDB has nothing for this item (HTTP 404).
	ReasonNoData = "no-data"
	// ReasonBudget: the daily request budget is spent, so no request was made.
	ReasonBudget = "budget"
	// ReasonRateLimit: the server rate-limited the request (HTTP 429).
	ReasonRateLimit = "rate-limited"
	// ReasonUsageLimit: the server's daily allowance is spent (HTTP 429).
	ReasonUsageLimit = "usage-limited"
	// ReasonError: the lookup could not be completed.
	ReasonError = "error"
)

// Error kinds, for callers that branch on the failure.
const (
	KindNoIDs      = "no-ids"
	KindBudget     = "budget"
	KindAuth       = "auth"
	KindRateLimit  = "rate-limited"
	KindUsageLimit = "usage-limited"
	KindStatus     = "status"
	KindParse      = "parse"
	KindNetwork    = "network"
)

// Error is a failure from a lookup or an API call.
type Error struct {
	// Kind is one of the Kind* constants.
	Kind string
	// Status is the HTTP status when a response was received, else 0.
	Status int
	// RetryAfter is how long the client will hold its next send.
	RetryAfter time.Duration
	// Message is the human-readable explanation.
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsTerminal reports whether retrying later in this run cannot help.
//
// A rejected key stays rejected, and an item with no ids never gains one.
func (e *Error) IsTerminal() bool {
	switch e.Kind {
	case KindAuth, KindNoIDs:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether err is a failure that retrying cannot fix.
func IsTerminal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsTerminal()
}

// Usage is the request accounting the status screen shows.
type Usage struct {
	// Requests is every API request this process made.
	Requests int `json:"requests"`
	// Lookups is every Lookup call, cached or not.
	Lookups int `json:"lookups"`
	// CacheHits is the lookups answered from the ledger with no network call.
	CacheHits int `json:"cache_hits"`
	// Data is the lookups TheIntroDB answered with segments.
	Data int `json:"data"`
	// NoData is the lookups TheIntroDB answered 404.
	NoData int `json:"no_data"`
	// RateLimited and UsageLimited count 429s by kind.
	RateLimited  int `json:"rate_limited"`
	UsageLimited int `json:"usage_limited"`
	// Errors counts failed lookups.
	Errors int `json:"errors"`
	// Budget is the configured requests-per-UTC-day allowance.
	Budget int `json:"budget"`
	// Today is the requests already recorded in the ledger for today.
	Today int `json:"today"`
	// Remaining is the requests left today, from the API's own header when it
	// sent one and otherwise from the ledger's count against Budget.
	Remaining      int  `json:"remaining"`
	RemainingKnown bool `json:"remaining_known"`
	// HoldFor is how long the client is holding its next send, after a 429.
	HoldFor time.Duration `json:"hold_for"`
	// LastStatus is the status of the most recent request.
	LastStatus int `json:"last_status"`
	// Consecutive429 is the run of rate-limit 429s that is growing the wait.
	Consecutive429 int `json:"consecutive_429"`
}

// Client talks to TheIntroDB.
type Client struct {
	cfg    config.TheIntroDB
	ledger *ledger.Ledger
	http   *httpclient.Client

	// clock is the source of time. Tests replace it so pacing, backoff and TTLs
	// are deterministic and instant.
	clock func() time.Time
	// sleep waits for a duration. Tests replace it to advance the fake clock
	// instead of sleeping.
	sleep func(context.Context, time.Duration) error

	mu          sync.Mutex
	nextAllowed time.Time
	holdUntil   time.Time
	consecutive int

	requests     int
	lookups      int
	cacheHits    int
	data         int
	noData       int
	rateLimited  int
	usageLimited int
	errs         int
	lastStatus   int

	remaining      int
	remainingKnown bool
}

// NewClient builds a client.
//
// cfg may carry zero values for the optional knobs: MinDelay falls back to its
// own default, and a non-positive DailyBudget means "uncounted". A nil ledger
// disables caching and budget counting; a nil httpclient gets a default one.
func NewClient(cfg config.TheIntroDB, led *ledger.Ledger, hc *httpclient.Client) *Client {
	// Without a key the public allowance is 500 requests a day, not 1000.
	// Budget for the allowance that actually applies rather than discovering it
	// by being rate-limited for the rest of the day.
	cfg.DailyBudget = cfg.EffectiveDailyBudget()
	if hc == nil {
		timeout := time.Duration(cfg.TimeoutS * float64(time.Second))
		if timeout <= 0 {
			timeout = httpclient.DefaultTimeout
		}
		hc = httpclient.New(timeout, false, DefaultUserAgent)
	}
	return &Client{
		cfg:    cfg,
		ledger: led,
		http:   hc,
		clock:  time.Now,
		sleep:  SleepContext,
	}
}

// SetClock replaces the client's clock, and points the ledger at the same one
// so request timestamps and the UTC day boundary agree with the client's idea
// of the time.
//
// Tests use it to make pacing, backoff and cache TTLs deterministic and fast.
// It must not be called with lookups in flight.
func (c *Client) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	c.clock = now
	if c.ledger != nil {
		c.ledger.SetClock(now)
	}
}

// SetSleeper replaces the function used to wait between requests. Tests point
// it at a fake clock; production leaves the default, which respects the context.
func (c *Client) SetSleeper(fn func(context.Context, time.Duration) error) {
	if fn == nil {
		fn = SleepContext
	}
	c.sleep = fn
}

// SleepContext waits for d, returning early when the context is cancelled. It is
// the client's default sleeper.
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Ledger is the state store this client writes to, or nil.
func (c *Client) Ledger() *ledger.Ledger { return c.ledger }

// Config is the configuration the client was built with.
func (c *Client) Config() config.TheIntroDB { return c.cfg }

// minDelay is the configured minimum spacing between requests.
func (c *Client) minDelay() time.Duration {
	d := c.cfg.MinDelay()
	if d <= 0 {
		return 0
	}
	return time.Duration(d * float64(time.Second))
}

// url joins the base URL and a path.
func (c *Client) url(path string) string {
	return strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/") + path
}

// checkBudget refuses a request once the day's allowance is spent.
//
// It counts from UTC midnight, because that is when the API's allowance resets,
// and it counts the ledger rather than this process, so requests made by an
// earlier run today still count.
func (c *Client) checkBudget() error {
	if c.ledger == nil {
		return nil
	}
	budget := c.cfg.DailyBudget
	if budget <= 0 {
		return nil
	}
	now := c.clock()
	used, err := c.ledger.RequestsSince(ledger.StartOfUTCDay(now), ledger.SourceTheIntroDB)
	if err != nil {
		return fmt.Errorf("theintrodb: read request history: %w", err)
	}
	if used >= budget {
		return &Error{
			Kind: KindBudget,
			Message: fmt.Sprintf(
				"TheIntroDB daily request budget reached: %d of %d used for %s UTC; "+
					"the allowance resets at 00:00 UTC",
				used, budget, now.UTC().Format("2006-01-02")),
		}
	}
	return nil
}

// pace reserves the next send slot and waits for it.
//
// The slot is reserved under the lock, so concurrent callers queue up at least
// MinDelay apart instead of bursting. Both the pacing floor and any hold left by
// a 429 are respected; the wait itself happens outside the lock.
func (c *Client) pace(ctx context.Context) error {
	now := c.clock()
	delay := c.minDelay()

	c.mu.Lock()
	send := now
	if c.nextAllowed.After(send) {
		send = c.nextAllowed
	}
	if c.holdUntil.After(send) {
		send = c.holdUntil
	}
	c.nextAllowed = send.Add(delay)
	c.mu.Unlock()

	if wait := send.Sub(now); wait > 0 {
		return c.sleep(ctx, wait)
	}
	return nil
}

// send performs one authenticated GET, enforcing the budget, the pacing floor
// and any hold, and records the request in the ledger BEFORE the call goes out
// so that a run killed mid-flight still counts against the budget.
func (c *Client) send(ctx context.Context, path string, query url.Values) (*httpclient.Response, error) {
	if err := c.checkBudget(); err != nil {
		return nil, err
	}
	if err := c.pace(ctx); err != nil {
		return nil, err
	}
	if c.ledger != nil {
		if err := c.ledger.RecordRequest(ledger.SourceTheIntroDB); err != nil {
			return nil, fmt.Errorf("theintrodb: record request: %w", err)
		}
	}

	headers := map[string]string{"Accept": "application/json"}
	if key := strings.TrimSpace(c.cfg.APIKey); key != "" {
		headers["Authorization"] = "Bearer " + key
	}

	resp, err := c.http.Do(ctx, "GET", c.url(path), headers, query)
	if err != nil {
		c.mu.Lock()
		c.errs++
		c.mu.Unlock()
		return nil, &Error{
			Kind:    KindNetwork,
			Message: fmt.Sprintf("theintrodb: GET %s: %v", path, err),
		}
	}
	c.observe(resp)
	return resp, nil
}

// observe records what a response says about pacing and quota.
func (c *Client) observe(resp *httpclient.Response) {
	now := c.clock()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.requests++
	c.lastStatus = resp.Status

	if remaining, ok := resp.IntHeader("X-UsageLimit-Remaining"); ok {
		c.remaining, c.remainingKnown = remaining, true
	}

	// Down to one request left in the window: hold the next send one slot.
	if windowLeft, ok := resp.IntHeader("X-RateLimit-Remaining"); ok && windowLeft <= 1 {
		if until := now.Add(c.minDelay()); until.After(c.holdUntil) {
			c.holdUntil = until
		}
	}

	if resp.Status != 429 {
		return
	}

	if usageLimited(resp) {
		wait := usageReset(resp)
		wait = clamp(wait, 0, maxUsageWait)
		c.consecutive = 0
		c.holdUntil = now.Add(wait)
		return
	}

	c.consecutive++
	factor := 1
	for i := 0; i < c.consecutive-1 && factor < backoffCap; i++ {
		factor *= 2
	}
	base := retryAfter(resp)
	if base <= 0 {
		base = c.minDelay()
	}
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	wait := clamp(time.Duration(factor)*base, 0, maxRateWait)
	c.holdUntil = now.Add(wait)
}

// currentRemaining reports how many requests are left today: the API's own
// figure when it has sent one, otherwise the ledger's count against the budget.
func (c *Client) currentRemaining() (int, bool) {
	c.mu.Lock()
	if c.remainingKnown {
		value := c.remaining
		c.mu.Unlock()
		return value, true
	}
	c.mu.Unlock()

	if c.ledger == nil || c.cfg.DailyBudget <= 0 {
		return 0, false
	}
	used, err := c.ledger.RequestsSince(ledger.StartOfUTCDay(c.clock()), ledger.SourceTheIntroDB)
	if err != nil {
		return 0, false
	}
	remaining := c.cfg.DailyBudget - used
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

// holdFor is how long the client is still holding its next send.
func (c *Client) holdFor() time.Duration {
	now := c.clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := c.holdUntil.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Usage reports the request accounting.
func (c *Client) Usage() Usage {
	now := c.clock()

	c.mu.Lock()
	u := Usage{
		Requests:       c.requests,
		Lookups:        c.lookups,
		CacheHits:      c.cacheHits,
		Data:           c.data,
		NoData:         c.noData,
		RateLimited:    c.rateLimited,
		UsageLimited:   c.usageLimited,
		Errors:         c.errs,
		Budget:         c.cfg.DailyBudget,
		LastStatus:     c.lastStatus,
		Consecutive429: c.consecutive,
	}
	if c.remainingKnown {
		u.Remaining, u.RemainingKnown = c.remaining, true
	}
	if d := c.holdUntil.Sub(now); d > 0 {
		u.HoldFor = d
	}
	c.mu.Unlock()

	if c.ledger != nil {
		if n, err := c.ledger.RequestsSince(ledger.StartOfUTCDay(now), ledger.SourceTheIntroDB); err == nil {
			u.Today = n
		}
	}
	if !u.RemainingKnown && c.cfg.DailyBudget > 0 {
		u.Remaining = clampInt(c.cfg.DailyBudget-u.Today, 0, c.cfg.DailyBudget)
		u.RemainingKnown = true
	}
	return u
}

// UserStats reads the account's submission statistics.
//
// It doubles as key validation: a 200 means the key is good, and a 401 or 403
// comes back as a terminal *Error of kind auth, never as a silent retry.
func (c *Client) UserStats(ctx context.Context) (map[string]any, error) {
	resp, err := c.send(ctx, statsPath, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Status >= 200 && resp.Status < 300:
		var out map[string]any
		if err := resp.JSON(&out); err != nil {
			return nil, &Error{
				Kind: KindParse, Status: resp.Status,
				Message: fmt.Sprintf("theintrodb: decode user stats: %v", err),
			}
		}
		return out, nil
	case resp.Status == 401 || resp.Status == 403:
		return nil, &Error{
			Kind:   KindAuth,
			Status: resp.Status,
			Message: fmt.Sprintf(
				"TheIntroDB rejected the API key (HTTP %d): %s", resp.Status, detail(resp.Body)),
		}
	default:
		return nil, &Error{
			Kind:   KindStatus,
			Status: resp.Status,
			Message: fmt.Sprintf("theintrodb: user stats returned HTTP %d: %s",
				resp.Status, detail(resp.Body)),
		}
	}
}

// ValidateKey checks the configured API key against /user/stats, returning a
// terminal error when it is missing or rejected.
func (c *Client) ValidateKey(ctx context.Context) error {
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return &Error{
			Kind:    KindAuth,
			Message: "no TheIntroDB API key configured, so there is nothing to validate",
		}
	}
	if _, err := c.UserStats(ctx); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Response classification
// ---------------------------------------------------------------------------

// usageLimited reports whether a 429 spent the day's allowance rather than the
// rate window.
//
// Two independent signals, because either can be the one that is present: the
// JSON body's code, and an X-UsageLimit-Reset above 300 seconds. A rate-limit
// 429 carries only Retry-After and a non-JSON body, so a reset above five
// minutes can only be a daily reset. Clamping one of those to five minutes is
// the bug that makes a scan stop after two requests.
func usageLimited(resp *httpclient.Response) bool {
	if len(resp.Body) > 0 {
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(resp.Body, &body); err == nil {
			switch body.Code {
			case "usage_limit_exceeded", "specific_media_usage_limit_exceeded":
				return true
			}
		}
	}
	if reset, ok := resp.IntHeader("X-UsageLimit-Reset"); ok && reset > usageResetFloor {
		return true
	}
	return false
}

// usageReset is how long a usage-limit 429 says to wait.
//
// The reset header is seconds until UTC midnight and is trusted up to a day.
// When it is absent the body's retry_after string ("3 hours") is used instead.
func usageReset(resp *httpclient.Response) time.Duration {
	if reset, ok := resp.IntHeader("X-UsageLimit-Reset"); ok && reset > 0 {
		return time.Duration(reset) * time.Second
	}
	if len(resp.Body) > 0 {
		var body struct {
			RetryAfter string `json:"retry_after"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(resp.Body, &body); err == nil {
			if d, ok := parseDelay(body.RetryAfter); ok {
				return d
			}
			if d, ok := parseDelay(body.Error); ok {
				return d
			}
		}
	}
	return usageWaitDefault
}

// retryAfter is the seconds a rate-limit 429 asks for, or zero when the header
// is absent or unusable.
func retryAfter(resp *httpclient.Response) time.Duration {
	raw := strings.TrimSpace(resp.Header("Retry-After"))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// delayRe matches the "3 hours", "45 minutes", "120 seconds" style the API uses
// in its retry_after field.
var delayRe = regexp.MustCompile(`(\d+)\s*(hours|hour|hrs|hr|h|minutes|minute|mins|min|m|seconds|second|secs|sec|s)\b`)

// parseDelay parses a human delay string. A bare number means seconds.
func parseDelay(s string) (time.Duration, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	var total time.Duration
	matched := false
	for _, m := range delayRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch m[2][0] {
		case 'h':
			total += time.Duration(n) * time.Hour
		case 'm':
			total += time.Duration(n) * time.Minute
		case 's':
			total += time.Duration(n) * time.Second
		}
		matched = true
	}
	return total, matched && total > 0
}

// detail trims a body down to something worth putting in an error message.
func detail(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func clamp(d, low, high time.Duration) time.Duration {
	if d < low {
		return low
	}
	if d > high {
		return high
	}
	return d
}

func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
