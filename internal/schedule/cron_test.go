package schedule

import (
	"testing"
	"time"
)

// utc builds a time in a fixed zone so the tests do not depend on where the
// machine is.
func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func TestParseCronAcceptsTheUsualForms(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"30 7 * * *",
		"*/15 * * * *",
		"0 3 * * 0",
		"0 3 * * 7", // 7 is Sunday too
		"0 0 1 * *",
		"0 0 1 1 *",
		"5,35 * * * *",
		"0 9-17 * * 1-5",
		"0 0-23/6 * * *",
		"  30   7  *  *  *  ", // surrounding and repeated spaces
	} {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q) = %v, want it accepted", expr, err)
		}
	}
}

func TestParseCronRejectsWhatItCannotHonour(t *testing.T) {
	cases := map[string]string{
		"four fields":        "30 7 * *",
		"six fields":         "30 7 * * * *",
		"empty":              "",
		"minute too large":   "60 * * * *",
		"hour too large":     "* 24 * * *",
		"day zero":           "* * 0 * *",
		"day too large":      "* * 32 * *",
		"month too large":    "* * * 13 *",
		"weekday too large":  "* * * * 8",
		"backwards range":    "30-10 * * * *",
		"zero step":          "*/0 * * * *",
		"negative step":      "*/-1 * * * *",
		"not a number":       "five * * * *",
		"empty list entry":   "1,,2 * * * *",
		"range out of range": "0 0 * * 5-9",
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCron(expr); err == nil {
				t.Errorf("ParseCron(%q) was accepted, want an error", expr)
			}
		})
	}
}

func TestNextFindsTheNamedMinute(t *testing.T) {
	cron, err := ParseCron("30 7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// Plex's maintenance window ends at 07:00, which is why the default is 07:30.
	got := cron.Next(utc(2026, time.September, 20, 6, 0))
	want := utc(2026, time.September, 20, 7, 30)
	if !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}

	// A minute that has already passed belongs to tomorrow.
	got = cron.Next(utc(2026, time.September, 20, 8, 0))
	want = utc(2026, time.September, 21, 7, 30)
	if !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	cron, err := ParseCron("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	// Standing exactly on a firing minute must give the following one, or a
	// timer that wakes up on time would fire the same minute twice.
	at := utc(2026, time.September, 20, 6, 0)
	got := cron.Next(at)
	if !got.Equal(at.Add(time.Minute)) {
		t.Errorf("next from %s = %s, want %s", at, got, at.Add(time.Minute))
	}

	// Part-way through a minute rounds up to the next whole one.
	got = cron.Next(at.Add(30 * time.Second))
	if !got.Equal(at.Add(time.Minute)) {
		t.Errorf("next from half a minute past = %s, want %s", got, at.Add(time.Minute))
	}
}

func TestNextHandlesStepsAndLists(t *testing.T) {
	cron, err := ParseCron("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	got := cron.Next(utc(2026, time.September, 20, 6, 1))
	if want := utc(2026, time.September, 20, 6, 15); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}

	list, err := ParseCron("5,35 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	got = list.Next(utc(2026, time.September, 20, 6, 6))
	if want := utc(2026, time.September, 20, 6, 35); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
}

func TestNextHandlesDayOfWeek(t *testing.T) {
	cron, err := ParseCron("0 3 * * 0") // Sundays at 03:00
	if err != nil {
		t.Fatal(err)
	}
	// 20 September 2026 is a Sunday.
	got := cron.Next(utc(2026, time.September, 20, 0, 0))
	if want := utc(2026, time.September, 20, 3, 0); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
	got = cron.Next(utc(2026, time.September, 20, 4, 0))
	if want := utc(2026, time.September, 27, 3, 0); !got.Equal(want) {
		t.Errorf("next Sunday = %s, want %s", got, want)
	}
	// 7 means Sunday as well, which is worth pinning: cron accepts both and a
	// schedule written by hand may use either.
	seven, err := ParseCron("0 3 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := cron.Next(utc(2026, time.September, 21, 0, 0)), seven.Next(utc(2026, time.September, 21, 0, 0)); !a.Equal(b) {
		t.Errorf("weekday 0 and 7 differ: %s vs %s", a, b)
	}
}

// Both day fields restricted means either may match. It surprises people, but it
// is what cron does, and matching cron matters more than being sensible.
func TestBothDayFieldsRestrictedMeansEither(t *testing.T) {
	cron, err := ParseCron("0 0 1 * 1") // the 1st, or a Monday
	if err != nil {
		t.Fatal(err)
	}
	// 21 September 2026 is a Monday.
	if got := cron.Next(utc(2026, time.September, 20, 12, 0)); !got.Equal(utc(2026, time.September, 21, 0, 0)) {
		t.Errorf("next = %s, want the Monday", got)
	}
	// A day of month with no other match in the week still fires on the 1st.
	if got := cron.Next(utc(2026, time.September, 28, 0, 0)); !got.Equal(utc(2026, time.October, 1, 0, 0)) {
		t.Errorf("next = %s, want the first of the month", got)
	}
}

// Only one day field restricted means only that field applies.
func TestOneDayFieldRestrictedAppliesAlone(t *testing.T) {
	cron, err := ParseCron("0 0 15 * *")
	if err != nil {
		t.Fatal(err)
	}
	got := cron.Next(utc(2026, time.September, 16, 0, 0))
	if want := utc(2026, time.October, 15, 0, 0); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
}

func TestNextSkipsImpossibleSchedules(t *testing.T) {
	// The 31st of February never happens.
	cron, err := ParseCron("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := cron.Next(utc(2026, time.January, 1, 0, 0)); !got.IsZero() {
		t.Errorf("an impossible schedule returned %s, want the zero time", got)
	}
}

func TestLeapDay(t *testing.T) {
	cron, err := ParseCron("0 0 29 2 *")
	if err != nil {
		t.Fatal(err)
	}
	got := cron.Next(utc(2026, time.March, 1, 0, 0))
	if want := utc(2028, time.February, 29, 0, 0); !got.Equal(want) {
		t.Errorf("next leap day = %s, want %s", got, want)
	}
}

func TestDescribe(t *testing.T) {
	cron, err := ParseCron("30 7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if got := cron.Describe(); got != "30 7 * * *" {
		t.Errorf("Describe = %q, want the expression", got)
	}
	every, err := ParseCron("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if got := every.Describe(); got != "every minute" {
		t.Errorf("Describe = %q, want prose for the every-minute case", got)
	}
}

// A schedule a day out has to be found without walking every minute of the day,
// which is what the hour and month jumps are for. This is a correctness check as
// much as a speed one.
func TestNextDoesNotMissFarAwayMatches(t *testing.T) {
	cron, err := ParseCron("0 3 1 1 *") // once a year
	if err != nil {
		t.Fatal(err)
	}
	got := cron.Next(utc(2026, time.January, 2, 0, 0))
	if want := utc(2027, time.January, 1, 3, 0); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
}

func TestNextKeepsTheLocation(t *testing.T) {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	cron, err := ParseCron("30 7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	got := cron.Next(time.Date(2026, time.September, 20, 6, 0, 0, 0, loc))
	if got.Location() != loc {
		t.Errorf("location = %s, want %s", got.Location(), loc)
	}
	// The wall clock reads 07:30 local, whatever the offset is.
	if got.Hour() != 7 || got.Minute() != 30 {
		t.Errorf("next = %s, want 07:30 local", got.Format(time.RFC3339))
	}
}
