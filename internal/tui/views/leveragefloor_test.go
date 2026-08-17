package views

import (
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/store"
)

// hourInputBuckets builds n consecutive hour buckets whose input clears the
// HOUR default floor comfortably but sits far under the 200K a day bucket asks
// for — the shape of a normal working hour on the "today" range.
func hourInputBuckets(n int) []store.Bucket {
	base := time.Date(2026, 7, 8, 9, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 0, n)
	for i := 0; i < n; i++ {
		in := int64(50_000 + 1_000*(i%7))
		out = append(out, store.Bucket{
			Keys:      map[string]string{"hour": base.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15")},
			Input:     in,
			Output:    in / 3,
			CacheRead: in * 40,
			Total:     in * 44,
		})
	}
	return out
}

// TestDefaultLeverageFloorScalesWithBucketSpan pins the derivation: the floor is
// leverageFloorPerDay prorated over the bucket's own width, so every grouping is
// judged against an input it could plausibly carry.
func TestDefaultLeverageFloorScalesWithBucketSpan(t *testing.T) {
	cases := []struct {
		dim  string
		want int64
	}{
		{"hour", 8_333},
		{"day", 200_000},
		{"week", 1_400_000},
		{"month", 6_000_000},
	}
	for _, c := range cases {
		if got := defaultLeverageFloor(c.dim); got != c.want {
			t.Errorf("defaultLeverageFloor(%q) = %d, want %d", c.dim, got, c.want)
		}
	}
	// An unknown dim is the day default (bucketStep's own fallback), never zero:
	// a zero floor would plot every idle bucket as leverage.
	if got := defaultLeverageFloor("fortnight"); got != 200_000 {
		t.Errorf("defaultLeverageFloor(unknown) = %d, want the day default", got)
	}
}

// TestLeverageFloorFollowsBucketSpan is the same fact read through the rendered
// pivot: a 50K-input HOUR bucket is a busy hour and must plot, while the same
// 50K in a DAY bucket is a nearly idle day and must stay suppressed. One flat
// constant cannot express both, and the one that shipped silently blanked the
// pivot for the whole "today" range (issue #39).
func TestLeverageFloorFollowsBucketSpan(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)

	hourly := OverviewData{Timeline: hourInputBuckets(20), TimelineDim: "hour", Mode: HeroLeverage}
	out := ansiHero.ReplaceAllString(heroBodyMemo(c, hourly, lay, 77, 16, -1), "")
	if strings.Contains(out, "leverage skipped") {
		t.Errorf("hour buckets at 50K input were suppressed by a day-sized floor:\n%s", out)
	}
	if !hasBraille(out) {
		t.Errorf("hour-bucket pivot plotted no ratio line:\n%s", out)
	}

	daily := OverviewData{Timeline: lowInputBuckets(20), TimelineDim: "day", Mode: HeroLeverage}
	dout := ansiHero.ReplaceAllString(heroBodyMemo(c, daily, lay, 77, 16, -1), "")
	if !strings.Contains(dout, "leverage skipped") {
		t.Errorf("day buckets at 50K input plotted; the floor stopped suppressing noise:\n%s", dout)
	}
}

// TestLeverageFloorOverrideDrivesTheMessage: a configured floor beats the
// derived default everywhere, including in the below-floor notice. A message
// quoting a number the segments were not filtered against is worse than no
// message at all.
func TestLeverageFloorOverrideDrivesTheMessage(t *testing.T) {
	lay := ComputeLayout(120, 30)
	// heroTestBuckets carry 2.0M-3.2M input: plotted under the day default,
	// suppressed under a 5M override.
	d := OverviewData{Timeline: heroTestBuckets(20), TimelineDim: "day", Mode: HeroLeverage}

	c := heroTestCtx()
	c.LeverageFloor = 5_000_000
	out := ansiHero.ReplaceAllString(heroBodyMemo(c, d, lay, 77, 16, -1), "")
	if !strings.Contains(out, "leverage skipped") {
		t.Fatalf("the configured 5M floor did not suppress 3.2M-input buckets:\n%s", out)
	}
	if !strings.Contains(out, "over 5.0M input") {
		t.Errorf("the notice does not name the configured floor:\n%s", out)
	}

	if def := ansiHero.ReplaceAllString(heroBodyMemo(heroTestCtx(), d, lay, 77, 16, -1), ""); strings.Contains(def, "leverage skipped") {
		t.Errorf("the derived day floor suppressed 2M+ input buckets:\n%s", def)
	}
}

// TestLeverageFloorOverrideReachesTheFallbackStrip: the sub-chart pivot filters
// its sparkline values against the same resolved floor, so the two treatments
// can never disagree about which buckets counted.
func TestLeverageFloorOverrideReachesTheFallbackStrip(t *testing.T) {
	lay := ComputeLayout(120, 30)
	d := OverviewData{Timeline: heroTestBuckets(20), TimelineDim: "day", Mode: HeroLeverage}

	c := heroTestCtx()
	c.LeverageFloor = 5_000_000
	out := ansiHero.ReplaceAllString(heroBodyMemo(c, d, lay, 77, 6, -1), "")
	if !strings.Contains(out, "over 5.0M input") {
		t.Errorf("the fallback strip ignored the configured floor:\n%s", out)
	}
}
