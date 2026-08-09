package views

import (
	"strings"
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

// TestTrendChartSurvivesUnparseableBucketKey pins the length guard in
// buildTrendChart (issue #32). bucketTimes drops a bucket whose key does not
// parse, so times can be shorter than buckets — and the push loop indexes
// times[i] over buckets. Unguarded that is an index-out-of-range inside View(),
// which takes the whole TUI down, so the trend path falls back exactly as the
// frame path already does.
func TestTrendChartSurvivesUnparseableBucketKey(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	const w, h = 60, 8

	buckets := heroTestBuckets(6)
	if _, _, ok := buildTrendChart(c, buckets, "day", w, h); !ok {
		t.Fatal("a fully parseable timeline must still build a trend chart")
	}

	// One unparseable key is enough to desynchronise the two slices.
	buckets[2].Keys["day"] = "not-a-date"
	if _, _, ok := buildTrendChart(c, buckets, "day", w, h); ok {
		t.Fatal("buildTrendChart accepted a timeline whose keys do not all parse")
	}
	got := heroBody(c, buckets, "day", lay, w, h, -1)
	if n := len(strings.Split(got, "\n")); n != h {
		t.Fatalf("heroBody over a partly unparseable timeline = %d lines, want %d", n, h)
	}
}
