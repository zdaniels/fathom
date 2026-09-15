package scheduler

import (
	"testing"
	"time"
)

func TestParseCronAliases(t *testing.T) {
	cases := map[string]string{
		"@hourly":   "0 * * * *",
		"@daily":    "0 0 * * *",
		"@weekly":   "0 0 * * 0",
		"@weekday":  "0 9 * * 1-5",
		"@minutely": "* * * * *",
	}
	for alias, want := range cases {
		p, err := ParseCron(alias)
		if err != nil {
			t.Errorf("ParseCron(%q): %v", alias, err)
			continue
		}
		if p.Expression != want {
			t.Errorf("ParseCron(%q).Expression = %q, want %q", alias, p.Expression, want)
		}
	}
}

func TestParseCronValidExpressions(t *testing.T) {
	cases := []string{
		"* * * * *",
		"0 9 * * 1-5",
		"*/15 * * * *",
		"0,30 * * * *",
		"0 0 1,15 * *",
		"0 0 * * 0",
	}
	for _, c := range cases {
		if _, err := ParseCron(c); err != nil {
			t.Errorf("ParseCron(%q) unexpectedly errored: %v", c, err)
		}
	}
}

func TestParseCronRejectsBadInput(t *testing.T) {
	cases := []string{
		"",
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 32 * *",  // dom out of range
		"* * * 13 *",  // month out of range
		"* * * * 7",   // dow out of range
		"foo * * * *", // non-numeric
		"*/0 * * * *", // zero step
	}
	for _, c := range cases {
		if _, err := ParseCron(c); err == nil {
			t.Errorf("ParseCron(%q) should have errored", c)
		}
	}
}

func TestMatchesUTC(t *testing.T) {
	p, _ := ParseCron("0 9 * * 1-5")                     // 9am UTC weekdays
	mon9 := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC) // 2026-05-25 is a Monday
	sat9 := time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC) // Saturday
	mon10 := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	if !Matches(p, mon9) {
		t.Error("Mon 9:00 UTC should match @weekday")
	}
	if Matches(p, sat9) {
		t.Error("Sat 9:00 UTC must not match @weekday")
	}
	if Matches(p, mon10) {
		t.Error("Mon 10:00 UTC must not match minute=0")
	}
}

func TestNextRunWalksMinuteByMinute(t *testing.T) {
	p, _ := ParseCron("*/15 * * * *")
	after := time.Date(2026, 5, 22, 18, 7, 30, 0, time.UTC)
	next, err := NextRun(p, after)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 5, 22, 18, 15, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("NextRun = %v, want %v", next, want)
	}
}

func TestNextRunWeekdayRollover(t *testing.T) {
	// @weekday = 0 9 * * 1-5. Friday 2026-05-22 18:00 UTC → next is Mon 2026-05-25 09:00.
	p, _ := ParseCron("@weekday")
	friEve := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)
	next, err := NextRun(p, friEve)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("@weekday Friday 6pm → next = %v, want %v", next, want)
	}
}

func TestNextRunImpossibleExpression(t *testing.T) {
	// Feb 30 — month 2 has at most 29 days. NextRun should bail within
	// the 4-year search bound.
	p, _ := ParseCron("0 0 30 2 *")
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := NextRun(p, after); err == nil {
		t.Error("NextRun should error on impossible cron expression")
	}
}
