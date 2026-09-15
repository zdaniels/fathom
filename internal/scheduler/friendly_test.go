package scheduler

import "testing"

func TestFriendlyToCron(t *testing.T) {
	cases := []struct {
		every, at string
		want      string
	}{
		// daily / time-of-day
		{"day", "9am", "0 9 * * *"},
		{"daily", "9:30am", "30 9 * * *"},
		{"day", "17:30", "30 17 * * *"},
		{"day", "12pm", "0 12 * * *"},
		{"day", "12am", "0 0 * * *"},
		{"day", "", "0 0 * * *"},
		{"", "8am", "0 8 * * *"}, // empty every + at → daily
		// weekday / weekend / week
		{"weekday", "9am", "0 9 * * 1-5"},
		{"weekend", "10am", "0 10 * * 0,6"},
		{"week", "", "0 0 * * 0"},
		// named days
		{"monday", "8am", "0 8 * * 1"},
		{"fri", "5pm", "0 17 * * 5"},
		{"sunday", "", "0 0 * * 0"},
		// hour / minute
		{"hour", "", "0 * * * *"},
		{"hourly", ":15", "15 * * * *"},
		{"minute", "", "* * * * *"},
		// intervals
		{"30m", "", "*/30 * * * *"},
		{"5min", "", "*/5 * * * *"},
		{"2h", "", "0 */2 * * *"},
		{"1d", "", "0 0 */1 * *"},
	}
	for _, c := range cases {
		got, err := FriendlyToCron(c.every, c.at)
		if err != nil {
			t.Errorf("FriendlyToCron(%q,%q) errored: %v", c.every, c.at, err)
			continue
		}
		if got != c.want {
			t.Errorf("FriendlyToCron(%q,%q) = %q, want %q", c.every, c.at, got, c.want)
		}
		// Every generated expression must be a valid cron the engine accepts.
		if _, perr := ParseCron(got); perr != nil {
			t.Errorf("FriendlyToCron(%q,%q) produced unparseable cron %q: %v", c.every, c.at, got, perr)
		}
	}
}

func TestFriendlyToCronErrors(t *testing.T) {
	bad := []struct{ every, at string }{
		{"", ""},             // nothing
		{"30m", "9am"},       // --at on an interval
		{"minute", "9am"},    // --at on minutely
		{"day", "25:00"},     // bad hour
		{"day", "9:70"},      // bad minute
		{"day", "13pm"},      // 12-hour out of range
		{"fortnight", "9am"}, // unknown cadence
		{"0m", ""},           // interval out of range
		{"99m", ""},          // interval out of range
	}
	for _, c := range bad {
		if got, err := FriendlyToCron(c.every, c.at); err == nil {
			t.Errorf("FriendlyToCron(%q,%q) = %q, want error", c.every, c.at, got)
		}
	}
}
