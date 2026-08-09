package views

import (
	"testing"
	"time"
)

// TestParseBucketTimeWeekKeys covers the hand-rolled %Y-W%W decoder: strftime
// %W weeks start at the year's first Monday (2026: Jan 5), with earlier days
// in week 00. The old "2006-W01" layout could never parse these keys.
func TestParseBucketTimeWeekKeys(t *testing.T) {
	cases := []struct {
		key  string
		want string // local date of the week's first day; "" = must not parse
	}{
		{"2026-W01", "2026-01-05"},
		{"2026-W31", "2026-08-03"},
		{"2026-W00", "2026-01-01"},
		{"2026-05-18", ""}, // date string is not a week key
		{"2026-W54", ""},
		{"garbage", ""},
	}
	for _, c := range cases {
		got, ok := parseBucketTime(c.key, "week")
		if c.want == "" {
			if ok {
				t.Errorf("parseBucketTime(%q, week) = %v, want no parse", c.key, got)
			}
			continue
		}
		if !ok {
			t.Errorf("parseBucketTime(%q, week) failed to parse", c.key)
			continue
		}
		if got.Format("2006-01-02") != c.want || got.Location() != time.Local {
			t.Errorf("parseBucketTime(%q, week) = %v, want %s local", c.key, got, c.want)
		}
	}
}
