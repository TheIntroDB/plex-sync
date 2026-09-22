package tidb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// LookupResult describes how one lookup was answered.
//
// It is what the status screen and the plan output quote, so it carries both
// the protocol facts and the request accounting.
type LookupResult struct {
	// Status is the HTTP status of the answer: 200, 404, 429, 401/403, or 0
	// when no request was made (a cache hit, or the budget refusing one).
	Status int `json:"status"`
	// Cached reports that the answer came from the ledger without a network
	// call.
	Cached bool `json:"cached"`
	// Reason is one of ReasonHit, ReasonMiss, ReasonNoData, ReasonBudget,
	// ReasonRateLimit, ReasonUsageLimit or ReasonError.
	//
	// It describes the ledger decision first: ReasonHit when a fresh cache
	// entry answered, ReasonMiss when the network had to. ReasonNoData means
	// TheIntroDB holds nothing for the item, whether that came from the cache
	// or from a fresh 404; ReasonBudget, ReasonRateLimit and ReasonUsageLimit
	// mean no answer was obtained, and ReasonError means the lookup failed.
	Reason string `json:"reason"`
	// Remaining is how many requests are left in the day, and RemainingKnown
	// whether that figure is meaningful.
	Remaining      int  `json:"remaining"`
	RemainingKnown bool `json:"remaining_known"`
	// RetryAfter is how long the client will hold its next send, after a 429.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// Elapsed is how long the lookup took end to end.
	Elapsed time.Duration `json:"elapsed"`
	// Key is the cache key the lookup used.
	Key string `json:"key,omitempty"`
}

// Data reports whether the item has segments coming from a 200 answer.
func (r LookupResult) Data() bool { return r.Status == 200 }

// OK reports whether the lookup completed and produced an answer (data or a
// clean no-data), as opposed to being refused or failing.
func (r LookupResult) OK() bool { return r.Status == 200 || r.Status == 404 }

// Lookup answers "what does TheIntroDB know about this item".
//
// The order is deliberate: the ledger is consulted first and, while the answer
// is fresh, it is returned without any network call at all. Only then is the
// budget checked, the pacing floor and any hold applied, the request recorded
// and the API asked, with the real file length, which is what makes the answer
// cut-aware.
//
// A miss is a miss: an empty SegmentSet comes back with ReasonNoData. No
// segment is ever invented.
func (c *Client) Lookup(ctx context.Context, item model.LibraryItem) (model.SegmentSet, LookupResult, error) {
	started := c.clock()
	empty := model.SegmentSet{Source: model.SourceTheIntroDB}

	c.mu.Lock()
	c.lookups++
	c.mu.Unlock()

	key, ok := item.LookupKey()
	if !ok {
		remaining, known := c.currentRemaining()
		return empty, LookupResult{
			Reason:         ReasonError,
			Remaining:      remaining,
			RemainingKnown: known,
			Elapsed:        c.clock().Sub(started),
		}, &Error{
			Kind: KindNoIDs,
			Message: fmt.Sprintf(
				"theintrodb: %s has no TMDb, IMDb or TVDb id, so it cannot be looked up",
				item.Label()),
		}
	}

	now := c.clock()

	if c.ledger != nil {
		if cached, found := c.ledger.Lookup(key); found && cached.Fresh(now) {
			switch cached.Status {
			case 200:
				set, err := ParseSegments(cached.Body)
				if err == nil {
					c.note(CachedData)
					remaining, known := c.currentRemaining()
					return set, LookupResult{
						Status:         200,
						Cached:         true,
						Reason:         ReasonHit,
						Remaining:      remaining,
						RemainingKnown: known,
						Elapsed:        c.clock().Sub(started),
						Key:            key,
					}, nil
				}
				// A body we can no longer parse is not a reason to fail the
				// run: fall through and ask again.
			case 404:
				c.note(CachedNoData)
				remaining, known := c.currentRemaining()
				return empty, LookupResult{
					Status:         404,
					Cached:         true,
					Reason:         ReasonNoData,
					Remaining:      remaining,
					RemainingKnown: known,
					Elapsed:        c.clock().Sub(started),
					Key:            key,
				}, nil
			}
		}
	}

	resp, err := c.send(ctx, mediaPath, c.buildQuery(item))
	if err != nil {
		return empty, c.failure(key, err, started), err
	}

	remaining, known := c.currentRemaining()
	result := LookupResult{
		Status:         resp.Status,
		Reason:         ReasonMiss,
		Remaining:      remaining,
		RemainingKnown: known,
		Elapsed:        c.clock().Sub(started),
		Key:            key,
	}

	switch resp.Status {
	case 200:
		set, perr := ParseSegments(string(resp.Body))
		if perr != nil {
			c.note(Failed)
			result.Reason = ReasonError
			return empty, result, perr
		}
		c.store(key, 200, string(resp.Body), item)
		c.note(Data)
		return set, result, nil

	case 404:
		// Cached for MissTTLDays only: a 404 becomes a 200 the moment someone
		// submits the timing, so it must expire.
		c.store(key, 404, string(resp.Body), item)
		c.note(NoData)
		result.Reason = ReasonNoData
		return empty, result, nil

	case 401, 403:
		// Terminal: the key is missing or rejected.
		c.note(Failed)
		result.Reason = ReasonError
		return empty, result, &Error{
			Kind:   KindAuth,
			Status: resp.Status,
			Message: fmt.Sprintf(
				"TheIntroDB rejected the API key (HTTP %d): %s", resp.Status, detail(resp.Body)),
		}

	case 429:
		hold := c.holdFor()
		result.RetryAfter = hold
		if usageLimited(resp) {
			c.note(UsageLimited)
			result.Reason = ReasonUsageLimit
			return empty, result, &Error{
				Kind:       KindUsageLimit,
				Status:     429,
				RetryAfter: hold,
				Message: fmt.Sprintf(
					"TheIntroDB daily allowance is spent; the client is holding requests for %s, "+
						"until the allowance resets: %s", hold.Round(time.Second), detail(resp.Body)),
			}
		}
		c.note(RateLimited)
		result.Reason = ReasonRateLimit
		return empty, result, &Error{
			Kind:       KindRateLimit,
			Status:     429,
			RetryAfter: hold,
			Message: fmt.Sprintf(
				"TheIntroDB rate-limited the request (HTTP 429); the client is holding requests for %s",
				hold.Round(time.Second)),
		}

	default:
		c.note(Failed)
		result.Reason = ReasonError
		return empty, result, &Error{
			Kind:   KindStatus,
			Status: resp.Status,
			Message: fmt.Sprintf("theintrodb: lookup returned HTTP %d: %s",
				resp.Status, detail(resp.Body)),
		}
	}
}

// failure turns a transport-level error from send into a LookupResult.
func (c *Client) failure(key string, err error, started time.Time) LookupResult {
	remaining, known := c.currentRemaining()
	out := LookupResult{
		Reason:         ReasonError,
		Remaining:      remaining,
		RemainingKnown: known,
		Elapsed:        c.clock().Sub(started),
		Key:            key,
	}
	var e *Error
	if errors.As(err, &e) {
		out.Status = e.Status
		out.RetryAfter = e.RetryAfter
		switch e.Kind {
		case KindBudget:
			out.Reason = ReasonBudget
		case KindRateLimit:
			out.Reason = ReasonRateLimit
			out.RetryAfter = c.holdFor()
		case KindUsageLimit:
			out.Reason = ReasonUsageLimit
			out.RetryAfter = c.holdFor()
		}
		if out.RetryAfter == 0 {
			out.RetryAfter = e.RetryAfter
		}
	}
	return out
}

// buildQuery is the request TheIntroDB is asked with.
//
// The duration is the real file length when it is known, which is what makes
// the answer cut-aware; without it the timings may belong to a different cut.
// Season and episode are sent only for episodes. The provider order matches
// model.ExternalIDs.LookupKey, so the cache key and the query always agree.
func (c *Client) buildQuery(item model.LibraryItem) url.Values {
	q := url.Values{}
	switch {
	case item.IDs.TMDB != nil:
		q.Set("tmdb_id", strconv.Itoa(*item.IDs.TMDB))
	case item.IDs.IMDb != nil && *item.IDs.IMDb != "":
		q.Set("imdb_id", *item.IDs.IMDb)
	case item.IDs.TVDB != nil:
		q.Set("tvdb_id", strconv.Itoa(*item.IDs.TVDB))
	}
	if !item.IsMovie() && item.Season != nil && item.Episode != nil {
		q.Set("season", strconv.Itoa(*item.Season))
		q.Set("episode", strconv.Itoa(*item.Episode))
	}
	if duration := item.BestDuration(); duration != nil && *duration > 0 {
		q.Set("duration_ms", strconv.FormatInt(*duration, 10))
	}
	return q
}

// store caches an answer: 200s for HitTTLDays, 404s for MissTTLDays.
func (c *Client) store(key string, status int, body string, item model.LibraryItem) {
	if c.ledger == nil {
		return
	}
	var ttl time.Duration
	switch status {
	case 200:
		ttl = time.Duration(c.cfg.HitTTLDays) * 24 * time.Hour
	case 404:
		ttl = time.Duration(c.cfg.MissTTLDays) * 24 * time.Hour
	default:
		return
	}
	_ = c.ledger.PutLookup(key, status, body, int64(ttl/time.Second), string(item.Kind))
}

// note updates the process counters.
type noteKind int

const (
	Data noteKind = iota
	NoData
	CachedData
	CachedNoData
	RateLimited
	UsageLimited
	Failed
)

func (c *Client) note(kind noteKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case Data:
		c.data++
	case NoData:
		c.noData++
	case CachedData:
		c.cacheHits++
		c.data++
	case CachedNoData:
		c.cacheHits++
		c.noData++
	case RateLimited:
		c.rateLimited++
		c.errs++
	case UsageLimited:
		c.usageLimited++
		c.errs++
	case Failed:
		c.errs++
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// apiSegment is one segment as the API writes it. Both bounds are pointers so
// that a JSON null stays nil: a null start means the segment begins at 0:00 and
// a null end means it runs to the end of the media, and neither is a zero.
type apiSegment struct {
	StartMS    *int64   `json:"start_ms"`
	EndMS      *int64   `json:"end_ms"`
	Confidence *float64 `json:"confidence"`
}

// ParseSegments turns a TheIntroDB response body into a SegmentSet.
//
// Each of the four keys may be absent, null, a single object or an array of
// objects, and either bound of a segment may be null. Nulls are preserved as
// nil here and resolved later against the real file length by
// model.Segment.Resolve, so nothing is invented and nothing is dropped. An
// empty body and a body of "null" both mean "no segments".
//
// It is exported so a cached body can be re-parsed without another request.
func ParseSegments(body string) (model.SegmentSet, error) {
	set := model.SegmentSet{Source: model.SourceTheIntroDB}
	trimmed := bytes.TrimSpace([]byte(body))
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return set, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return set, &Error{
			Kind:    KindParse,
			Message: fmt.Sprintf("theintrodb: decode response: %v", err),
		}
	}

	for _, t := range model.SegmentTypes {
		value, ok := raw[string(t)]
		if !ok {
			continue
		}
		segments, err := parseSegmentValue(value, t)
		if err != nil {
			return set, err
		}
		set.Segments = append(set.Segments, segments...)
	}
	return set, nil
}

// parseSegmentValue accepts one object, an array of objects, or null.
func parseSegmentValue(raw json.RawMessage, t model.SegmentType) ([]model.Segment, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	var entries []apiSegment
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, &Error{
				Kind:    KindParse,
				Message: fmt.Sprintf("theintrodb: decode %s segment list: %v", t, err),
			}
		}
	} else {
		var one apiSegment
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, &Error{
				Kind:    KindParse,
				Message: fmt.Sprintf("theintrodb: decode %s segment: %v", t, err),
			}
		}
		entries = append(entries, one)
	}

	out := make([]model.Segment, 0, len(entries))
	for _, entry := range entries {
		out = append(out, model.Segment{
			Type:       t,
			StartMS:    entry.StartMS,
			EndMS:      entry.EndMS,
			Source:     model.SourceTheIntroDB,
			Confidence: entry.Confidence,
		})
	}
	return out, nil
}

// IsNotFound reports whether err is a 404 from the API, which is a clean "no
// data" rather than a failure.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 404
}
