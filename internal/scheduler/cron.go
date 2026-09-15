// Package scheduler runs cron-style recurring jobs against an agent. Jobs
// persist to a SQLite store so they survive restarts. The cron parser
// supports a five-field expression plus a handful of named aliases
// (@hourly, @daily, @weekday, etc.).
package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParsedCron is the materialised form of a 5-field expression.
type ParsedCron struct {
	Expression string
	Minutes    map[int]struct{}
	Hours      map[int]struct{}
	DOM        map[int]struct{}
	Months     map[int]struct{}
	DOW        map[int]struct{}
}

var aliases = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@weekday":  "0 9 * * 1-5",
	"@minutely": "* * * * *",
}

// ranges defines the inclusive [min,max] for each of the 5 fields, in order:
// minute, hour, day-of-month, month, day-of-week.
var ranges = [5][2]int{
	{0, 59},
	{0, 23},
	{1, 31},
	{1, 12},
	{0, 6},
}

// ParseCron accepts both the 5-field syntax and the @aliases. Day-of-week
// uses 0 = Sunday to match Vixie cron.
func ParseCron(input string) (ParsedCron, error) {
	expr := strings.TrimSpace(input)
	if a, ok := aliases[expr]; ok {
		expr = a
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return ParsedCron{}, fmt.Errorf("cron expression must have 5 fields, got %d: %q", len(fields), input)
	}
	pc := ParsedCron{Expression: expr}
	sets := []*map[int]struct{}{&pc.Minutes, &pc.Hours, &pc.DOM, &pc.Months, &pc.DOW}
	for i, f := range fields {
		s, err := expandField(f, ranges[i][0], ranges[i][1])
		if err != nil {
			return ParsedCron{}, err
		}
		*sets[i] = s
	}
	return pc, nil
}

func expandField(field string, lo, hi int) (map[int]struct{}, error) {
	set := make(map[int]struct{})
	for _, part := range strings.Split(field, ",") {
		step := 1
		rng := part
		if i := strings.LastIndex(part, "/"); i != -1 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return nil, fmt.Errorf("invalid step in cron field: %q", part)
			}
			step = s
			rng = part[:i]
		}
		var from, to int
		switch {
		case rng == "*" || rng == "":
			from, to = lo, hi
		case strings.Contains(rng, "-"):
			parts := strings.SplitN(rng, "-", 2)
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[1])
			if errA != nil || errB != nil {
				return nil, fmt.Errorf("invalid range in cron field: %q", part)
			}
			from, to = a, b
		default:
			v, err := strconv.Atoi(rng)
			if err != nil {
				return nil, fmt.Errorf("invalid value in cron field: %q", part)
			}
			from, to = v, v
		}
		if from < lo || to > hi || from > to {
			return nil, fmt.Errorf("cron field %q out of range [%d,%d]", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set[v] = struct{}{}
		}
	}
	return set, nil
}

// Matches reports whether dateUTC falls in the cron schedule. UTC throughout
// to avoid DST surprises.
func Matches(p ParsedCron, t time.Time) bool {
	t = t.UTC()
	_, mins := p.Minutes[t.Minute()]
	_, hrs := p.Hours[t.Hour()]
	_, dom := p.DOM[t.Day()]
	_, mon := p.Months[int(t.Month())]
	_, dow := p.DOW[int(t.Weekday())]
	return mins && hrs && dom && mon && dow
}

// NextRun returns the next UTC time after `after` that matches p, walking
// minute by minute. Bounded at 4 years to protect against impossible
// expressions like "0 0 30 2 *" (Feb 30).
func NextRun(p ParsedCron, after time.Time) (time.Time, error) {
	candidate := after.UTC().Add(time.Minute).Truncate(time.Minute)
	stopAt := candidate.Year() + 4
	for candidate.Year() < stopAt {
		if Matches(p, candidate) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, errors.New("no future occurrence within 4 years")
}
