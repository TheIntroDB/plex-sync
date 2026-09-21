// Package schedule implements the cron subset the tool needs to run itself on a
// timer.
//
// It exists so the container does not need a cron daemon, a shell or a second
// process: the image is a single static binary, and that binary can hold its own
// schedule. Ten lines of a cron expression that nobody uses are not supported,
// and every expression that is supported is tested.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxLookahead bounds the search for the next time an expression fires. A
// schedule that matches nothing at all (31 February, say) gives up rather than
// spinning forever.
const maxLookahead = 5 * 366 * 24 * time.Hour

// Cron is a parsed five-field cron expression.
type Cron struct {
	expr string

	minute  [60]bool
	hour    [24]bool
	day     [32]bool // 1..31
	month   [13]bool // 1..12
	weekday [7]bool  // 0..6, Sunday is 0

	// dayRestricted and weekdayRestricted record whether the day fields were
	// anything other than "*", which decides how they combine.
	dayRestricted     bool
	weekdayRestricted bool
}

// ParseCron reads a standard five-field expression:
//
//	minute hour day-of-month month day-of-week
//
// Each field accepts "*", a number, a range ("1-5"), a step ("*/15", "1-5/2")
// and a comma-separated list of those. Day-of-week is 0 to 6 with Sunday as 0,
// and 7 is accepted as Sunday as well.
func ParseCron(expr string) (*Cron, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf(
			"a cron expression needs five fields (minute hour day month weekday), got %d in %q",
			len(fields), expr)
	}

	c := &Cron{expr: strings.Join(fields, " ")}

	if err := parseField(fields[0], 0, 59, func(v int) { c.minute[v] = true }); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if err := parseField(fields[1], 0, 23, func(v int) { c.hour[v] = true }); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	if err := parseField(fields[2], 1, 31, func(v int) { c.day[v] = true }); err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	if err := parseField(fields[3], 1, 12, func(v int) { c.month[v] = true }); err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	if err := parseField(fields[4], 0, 7, func(v int) {
		if v == 7 {
			v = 0 // Sunday is both 0 and 7
		}
		c.weekday[v] = true
	}); err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}

	c.dayRestricted = fields[2] != "*"
	c.weekdayRestricted = fields[4] != "*"
	return c, nil
}

// parseField fills one field's slots from its text.
func parseField(text string, low, high int, set func(int)) error {
	if text == "" {
		return errors.New("empty")
	}
	for _, part := range strings.Split(text, ",") {
		if err := parsePart(part, low, high, set); err != nil {
			return err
		}
	}
	return nil
}

func parsePart(part string, low, high int, set func(int)) error {
	if part == "" {
		return errors.New("empty entry")
	}

	step := 1
	if slash := strings.IndexByte(part, '/'); slash >= 0 {
		raw := part[slash+1:]
		part = part[:slash]
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("step %q must be a positive number", raw)
		}
		step = parsed
	}

	start, end := low, high
	switch {
	case part == "*":
		// every value
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		from, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if err != nil {
			return fmt.Errorf("%q is not a number", bounds[0])
		}
		to, err := strconv.Atoi(strings.TrimSpace(bounds[1]))
		if err != nil {
			return fmt.Errorf("%q is not a number", bounds[1])
		}
		if from > to {
			return fmt.Errorf("range %d-%d counts backwards", from, to)
		}
		start, end = from, to
	default:
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return fmt.Errorf("%q is not a number, a range or *", part)
		}
		start, end = value, value
	}

	if start < low || end > high {
		return fmt.Errorf("%d-%d is outside the allowed range %d-%d", start, end, low, high)
	}
	for v := start; v <= end; v += step {
		set(v)
	}
	return nil
}

// String returns the expression as it was written.
func (c *Cron) String() string { return c.expr }

// Next returns the first time strictly after the given moment that the
// expression fires, in that moment's location. The zero time is returned when
// nothing matches within the lookahead.
func (c *Cron) Next(after time.Time) time.Time {
	// Start at the next whole minute, since a schedule fires on minute
	// boundaries and a partial minute would otherwise be matched twice.
	candidate := after.Truncate(time.Minute).Add(time.Minute)

	limit := candidate.Add(maxLookahead)
	for candidate.Before(limit) {
		if c.matches(candidate) {
			return candidate
		}
		// Jump whole hours and days that cannot match, so a schedule that fires
		// once a day is not found by walking 1440 minutes.
		candidate = c.advance(candidate)
	}
	return time.Time{}
}

// matches reports whether a minute is one the expression selects.
func (c *Cron) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}

	dayMatch := c.day[t.Day()]
	weekdayMatch := c.weekday[int(t.Weekday())]

	switch {
	case c.dayRestricted && c.weekdayRestricted:
		// The traditional rule: when both are restricted, either one matching
		// is enough. It surprises people, but it is what cron does and a
		// schedule that behaves differently from cron is worse than one that
		// behaves oddly.
		return dayMatch || weekdayMatch
	case c.dayRestricted:
		return dayMatch
	case c.weekdayRestricted:
		return weekdayMatch
	default:
		return true
	}
}

// advance returns the next candidate minute, skipping ahead where it can.
func (c *Cron) advance(t time.Time) time.Time {
	// A month that cannot match: jump to the first minute of the next month.
	if !c.month[int(t.Month())] {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).
			AddDate(0, 1, 0)
	}
	// An hour that cannot match: jump to the next hour.
	if !c.hour[t.Hour()] {
		return t.Truncate(time.Hour).Add(time.Hour)
	}
	// Otherwise step a minute. The day fields can only rule out whole days, and
	// checking that here would mean duplicating the day logic, so a day that
	// cannot match is walked a minute at a time. At most 1440 steps, and real
	// schedules match within a few.
	return t.Add(time.Minute)
}

// Describe renders a schedule in words, for log output and the interface.
func (c *Cron) Describe() string {
	if c.expr == "* * * * *" {
		return "every minute"
	}
	if len(c.expr) > 0 {
		return c.expr
	}
	return "unscheduled"
}
