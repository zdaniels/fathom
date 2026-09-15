package scheduler

import (
	"fmt"
	"strconv"
	"strings"
)

// FriendlyToCron converts human-friendly --every / --at inputs into a 5-field
// cron expression that ParseCron understands. It's a thin convenience layer so
// users don't have to know cron syntax; the result is an ordinary cron string
// the rest of the scheduler stores, parses, and prints like any other.
//
// every (case-insensitive):
//
//	minute / minutely        every minute
//	hour / hourly            top of each hour (or :MM when at is a minute)
//	day / daily / ""         once a day
//	weekday / weekdays       Mon–Fri
//	weekend / weekends       Sat + Sun
//	week / weekly            once a week (Sunday)
//	mon|tue|…|sunday         that weekday
//	<N>m / <N>h / <N>d       every N minutes / hours / days
//
// at applies to the day/week/weekday/weekend/named-day forms (and sets the
// minute for hourly). It accepts "9am", "9:30am", "9pm", "09:00", "17:30",
// "9", and ":15" (minute-only). An empty at means midnight (00:00) for the
// daily/weekly forms.
func FriendlyToCron(every, at string) (string, error) {
	every = strings.ToLower(strings.TrimSpace(every))
	at = strings.ToLower(strings.TrimSpace(at))

	if every == "" && at == "" {
		return "", fmt.Errorf("nothing to schedule: pass --every (e.g. day, weekday, 30m) and/or --at")
	}

	// Interval forms (<N>m / <N>h / <N>d) come first; they ignore --at.
	if cron, ok, err := intervalCron(every); ok || err != nil {
		if err != nil {
			return "", err
		}
		if at != "" {
			return "", fmt.Errorf("--at doesn't apply to an interval like %q — use e.g. --every day --at 9am instead", every)
		}
		return cron, nil
	}

	switch every {
	case "minute", "minutely":
		if at != "" {
			return "", fmt.Errorf("--at doesn't apply to --every minute")
		}
		return "* * * * *", nil
	case "hour", "hourly":
		minute := 0
		if at != "" {
			m, err := parseMinuteOnly(at)
			if err != nil {
				return "", fmt.Errorf("--at for an hourly schedule must be a minute like ':15' or '15': %w", err)
			}
			minute = m
		}
		return fmt.Sprintf("%d * * * *", minute), nil
	}

	// Remaining forms run once on a given day at a given clock time.
	hour, minute, err := parseClock(at) // at=="" → 00:00
	if err != nil {
		return "", err
	}

	switch every {
	case "", "day", "daily":
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	case "weekday", "weekdays":
		return fmt.Sprintf("%d %d * * 1-5", minute, hour), nil
	case "weekend", "weekends":
		return fmt.Sprintf("%d %d * * 0,6", minute, hour), nil
	case "week", "weekly":
		return fmt.Sprintf("%d %d * * 0", minute, hour), nil // Sunday
	}

	if dow, ok := weekdayNum(every); ok {
		return fmt.Sprintf("%d %d * * %d", minute, hour, dow), nil
	}

	return "", fmt.Errorf("don't understand --every %q (try: day, weekday, weekend, hour, week, a weekday name like monday, or an interval like 30m / 2h / 1d)", every)
}

// intervalCron handles "<N>m", "<N>h", "<N>d". ok is false when every is not
// an interval at all (no leading digit), so the caller falls through to the
// named forms; ok is true with a non-nil err when it looks like an interval
// but the number is out of range.
func intervalCron(every string) (cron string, ok bool, err error) {
	i := 0
	for i < len(every) && every[i] >= '0' && every[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", false, nil // doesn't start with a number
	}
	n, convErr := strconv.Atoi(every[:i])
	if convErr != nil {
		return "", false, nil
	}
	switch every[i:] {
	case "m", "min", "mins", "minute", "minutes":
		if n < 1 || n > 59 {
			return "", true, fmt.Errorf("a minute interval must be 1–59, got %d", n)
		}
		return fmt.Sprintf("*/%d * * * *", n), true, nil
	case "h", "hr", "hrs", "hour", "hours":
		if n < 1 || n > 23 {
			return "", true, fmt.Errorf("an hour interval must be 1–23, got %d", n)
		}
		return fmt.Sprintf("0 */%d * * *", n), true, nil
	case "d", "day", "days":
		if n < 1 || n > 31 {
			return "", true, fmt.Errorf("a day interval must be 1–31, got %d", n)
		}
		return fmt.Sprintf("0 0 */%d * *", n), true, nil
	default:
		return "", false, nil
	}
}

// parseClock parses a clock time into 24-hour hour+minute. Accepts 12-hour
// ("9am", "9:30am", "9pm"), 24-hour ("09:00", "17:30"), and bare hours
// ("9", "14"). An empty string is midnight.
func parseClock(at string) (hour, minute int, err error) {
	if at == "" {
		return 0, 0, nil
	}
	s := strings.TrimSpace(at)
	half := "" // "am" / "pm"
	switch {
	case strings.HasSuffix(s, "am"):
		half, s = "am", strings.TrimSpace(strings.TrimSuffix(s, "am"))
	case strings.HasSuffix(s, "pm"):
		half, s = "pm", strings.TrimSpace(strings.TrimSuffix(s, "pm"))
	}

	hStr, mStr := s, "0"
	if i := strings.IndexAny(s, ":."); i != -1 {
		hStr, mStr = s[:i], s[i+1:]
	}
	h, e1 := strconv.Atoi(strings.TrimSpace(hStr))
	m, e2 := strconv.Atoi(strings.TrimSpace(mStr))
	if e1 != nil || e2 != nil {
		return 0, 0, fmt.Errorf("can't read the time %q (try 9am, 9:30am, or 17:30)", at)
	}

	if half != "" {
		if h < 1 || h > 12 {
			return 0, 0, fmt.Errorf("a 12-hour time like %q must use hour 1–12", at)
		}
		if half == "pm" && h != 12 {
			h += 12
		}
		if half == "am" && h == 12 {
			h = 0
		}
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("the time %q is out of range", at)
	}
	return h, m, nil
}

// parseMinuteOnly reads a bare or colon-prefixed minute (":15" or "15") for
// hourly schedules.
func parseMinuteOnly(at string) (int, error) {
	m, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(at), ":"))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("minute must be 0–59")
	}
	return m, nil
}

// weekdayNum maps a weekday name (full or common abbreviation) to its cron
// day-of-week number (0 = Sunday, matching ParseCron).
func weekdayNum(s string) (int, bool) {
	switch s {
	case "sun", "sunday":
		return 0, true
	case "mon", "monday":
		return 1, true
	case "tue", "tues", "tuesday":
		return 2, true
	case "wed", "weds", "wednesday":
		return 3, true
	case "thu", "thur", "thurs", "thursday":
		return 4, true
	case "fri", "friday":
		return 5, true
	case "sat", "saturday":
		return 6, true
	}
	return 0, false
}
