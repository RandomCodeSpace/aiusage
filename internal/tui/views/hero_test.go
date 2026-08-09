package views

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// ansiHero strips SGR sequences: lipgloss v2 always emits them from Render, so
// every legibility assertion below reads the monochrome channel by construction.
var ansiHero = regexp.MustCompile("\x1b\\[[0-9;]*m")

// heroHumanize is a real humanizer (the memo tests use a constant one, which
// would make every ring label identical).
func heroHumanize(n int64) string {
	switch {
	case n < 1_000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "K"
	case n < 1_000_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	default:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
	}
}

// heroTestCtx is a chart-capable Ctx. The adaptive colors must be non-zero: a
// zero compat.AdaptiveColor panics inside lipgloss v2 Render.
func heroTestCtx() Ctx {
	ac := func(s string) compat.AdaptiveColor {
		return compat.AdaptiveColor{Light: lipgloss.Color(s), Dark: lipgloss.Color(s)}
	}
	return Ctx{
		Comp:     CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("1")),
		Humanize: heroHumanize,
		Truncate: func(s string, w int) string { return s },
		PadLeft:  padLeftLocal,
		PadRight: padRightLocal,
		NowColor: ac("3"), AccentColor: ac("6"), FaintColor: ac("8"),
		GoodColor: ac("2"), WarnColor: ac("1"), BorderColor: ac("8"),
	}
}

// heroTestBuckets builds n daily buckets with the real 150x cache dominance,
// dropping the days in skip so the series carry gaps.
func heroTestBuckets(n int, skip ...int) []store.Bucket {
	gone := map[int]bool{}
	for _, s := range skip {
		gone[s] = true
	}
	base := time.Date(2026, 7, 8, 0, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 0, n)
	for i := 0; i < n; i++ {
		if gone[i] {
			continue
		}
		in := int64(2_000_000 + 200_000*(i%7))
		out = append(out, store.Bucket{
			Keys:          map[string]string{"day": base.AddDate(0, 0, i).Format("2006-01-02")},
			Input:         in,
			Output:        in / 3,
			CacheRead:     in * 150,
			CacheCreation: in * 5,
			Total:         in * 156,
		})
	}
	return out
}

func heroTestData(mode HeroMode) OverviewData {
	return OverviewData{
		Timeline:    heroTestBuckets(30, 10, 11, 20),
		TimelineDim: "day",
		Mode:        mode,
		Cursor:      5,
	}
}

// TestHeroPaneSplitGeometry pins the pane budget: the two panes exactly fill the
// chart body, the cache pane keeps a usable plot area under its x axis, and the
// axis-less fresh pane gets an ODD height — ntcharts draws row i's label at
// origin.Y-i, so on a pane with no x axis an even height puts the top ring one
// row above the canvas, where it is clipped.
func TestHeroPaneSplitGeometry(t *testing.T) {
	for chartTotal := minHeroTwoPaneH - 2; chartTotal <= 40; chartTotal++ {
		freshH, cacheH := heroPaneSplit(chartTotal)
		if freshH+cacheH != chartTotal {
			t.Fatalf("chartTotal=%d: panes %d+%d do not fill the body", chartTotal, freshH, cacheH)
		}
		if freshH%2 == 0 {
			t.Fatalf("chartTotal=%d: fresh pane height %d is even — top ring clips", chartTotal, freshH)
		}
		if cacheH-2 < minPaneGraphH {
			t.Fatalf("chartTotal=%d: cache plot area %d rows, want >= %d", chartTotal, cacheH-2, minPaneGraphH)
		}
	}
}

// TestHeroModeBodyExactHeight: the hero body must be EXACTLY the rows it was
// handed in every mode and at every size, or a pane pushes its panel past its
// budget. (heroheight_test.go covers the same contract for the fallback body.)
func TestHeroModeBodyExactHeight(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 40)
	for _, mode := range []HeroMode{HeroTrend, HeroLeverage} {
		d := heroTestData(mode)
		for _, w := range []int{48, 66, 100, 116} {
			for _, h := range []int{5, 7, 9, 12, 13, 16, 20, 29} {
				got := len(strings.Split(heroBodyMemo(c, d, lay, w, h, 3), "\n"))
				if got != h {
					t.Errorf("mode=%d body(w=%d,h=%d) = %d lines, want exactly %d", mode, w, h, got, h)
				}
			}
		}
	}
}

// TestHeroTwoPaneMonoLegibility reads the hero through the monochrome channel:
// both pane names and both SCALE readouts must survive with every SGR sequence
// stripped. The declared scale is the only thing that makes two independently
// detented panes comparable, so it can never be carried by color alone.
func TestHeroTwoPaneMonoLegibility(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	out := ansiHero.ReplaceAllString(heroPanel(c, heroTestData(HeroTrend), 81, 19, lay, true), "")

	for _, want := range []string{"TREND", "fresh", "cache", "/div"} {
		if !strings.Contains(out, want) {
			t.Errorf("mono hero is missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "SCALE "); n != 2 {
		t.Errorf("mono hero carries %d SCALE readouts, want one per pane (2):\n%s", n, out)
	}
	// Both panes' plot areas must start in the same column — the shared gutter
	// is what lets the eye compare them. Take the axis column of each pane.
	axes := map[int]bool{}
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.IndexRune(ln, '│'); i >= 0 {
			axes[i] = true
		}
	}
	if len(axes) != 1 {
		t.Errorf("panes are not column-aligned: Y axis found in columns %v", axes)
	}
}

// TestHeroPivotMonoLegibility: the leverage pivot's header and magnitude footer
// must survive the same strip. The chart deliberately carries no magnitudes, so
// losing the footer would leave the ratio unanchored.
func TestHeroPivotMonoLegibility(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(160, 40)
	out := ansiHero.ReplaceAllString(heroPanel(c, heroTestData(HeroLeverage), 119, 29, lay, true), "")

	for _, want := range []string{"leverage", "cache-read / input", "SCALE ", "x/div", "range ", "input", "output", "cache"} {
		if !strings.Contains(out, want) {
			t.Errorf("mono pivot is missing %q:\n%s", want, out)
		}
	}
	// The dropped 1x break-even reference must not come back: at 40-310x it is
	// geometrically indistinguishable from the axis and reads as a gridline.
	if strings.Contains(out, "break-even") {
		t.Error("pivot re-introduced the break-even reference")
	}
}

// TestHeroModeChangesBody guards the toggle actually pivoting the render.
func TestHeroModeChangesBody(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	trend := heroBodyMemo(c, heroTestData(HeroTrend), lay, 77, 16, -1)
	pivot := heroBodyMemo(c, heroTestData(HeroLeverage), lay, 77, 16, -1)
	if trend == pivot {
		t.Fatal("the leverage pivot renders the same body as the trend hero")
	}
}

// TestHeroGapRunsBreakSeries: contiguous runs are what stop a braille line from
// drawing an interpolated diagonal across missing buckets, so a gap must split
// the series into separate datasets rather than a longer one.
func TestHeroGapRunsBreakSeries(t *testing.T) {
	buckets := heroTestBuckets(10, 4, 5)
	times := bucketTimes(buckets, "day")
	if len(times) != len(buckets) {
		t.Fatalf("bucket times = %d, want %d", len(times), len(buckets))
	}
	runs := gapRuns(times, "day")
	if len(runs) != 2 {
		t.Fatalf("gapRuns over a two-day gap = %d runs, want 2", len(runs))
	}
	if len(runs[0])+len(runs[1]) != len(times) {
		t.Fatalf("runs cover %d of %d buckets", len(runs[0])+len(runs[1]), len(times))
	}
	// No gap at all is one run; an hour dim uses its own step.
	if got := len(gapRuns(bucketTimes(heroTestBuckets(6), "day"), "day")); got != 1 {
		t.Fatalf("gapless timeline = %d runs, want 1", got)
	}
}

// TestHeroFallsBackBelowTwoPaneFloor: under the two-pane floor the hero renders
// the EXISTING treatment (log chart → strip → numbers) rather than a half-built
// pane pair. The quantized decade-ring log treatment for this band is undesigned
// (issue #8) — this test pins the seam so it is a deliberate handover.
func TestHeroFallsBackBelowTwoPaneFloor(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	d := heroTestData(HeroTrend)
	const w = 77
	for _, h := range []int{6, 9, minHeroTwoPaneH - 1} {
		got := heroBodyMemo(c, d, lay, w, h, -1)
		want := heroBody(c, d.Timeline, d.TimelineDim, lay, w, h, -1)
		if got != want {
			t.Fatalf("h=%d: body diverges from the existing hero fallback", h)
		}
		if strings.Contains(ansiHero.ReplaceAllString(got, ""), "SCALE ") {
			t.Fatalf("h=%d: a detented pane leaked into the fallback band", h)
		}
	}
	// At the floor the two panes take over.
	at := ansiHero.ReplaceAllString(heroBodyMemo(c, d, lay, w, minHeroTwoPaneH, -1), "")
	if strings.Count(at, "SCALE ") != 2 {
		t.Fatalf("two-pane hero did not engage at the floor (h=%d):\n%s", minHeroTwoPaneH, at)
	}
}

// hasBraille reports whether s carries any braille cell (U+2800..U+28FF) — the
// signature of a real ntcharts build, as opposed to a block sparkline strip.
func hasBraille(s string) bool {
	for _, r := range s {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	return false
}

// TestHeroMemoCoversDegradedBand: between the log-chart floor and the two-pane
// floor the hero still draws a FULL braille chart (just without pane headers),
// so that band must be memoized like the paned bodies. Gating the memo on the
// paned floor alone rebuilt the chart on every View and every scrub keypress,
// which is exactly what the issue #4 contract forbids.
func TestHeroMemoCoversDegradedBand(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	const w = 77
	braille := 0
	for h := minHeroLogH; h < minHeroTwoPaneH; h++ {
		d := heroTestData(HeroTrend)
		d.Gen, d.Memo = 1, NewHeroMemo()

		body := heroBodyMemo(c, d, lay, w, h, -1)
		if !hasBraille(body) {
			continue // narrower degradations (strip/numbers) are cheap by design
		}
		braille++
		if got := d.Memo.Builds(); got != 1 {
			t.Fatalf("h=%d: braille band built %d times through the memo, want 1", h, got)
		}
		if again := heroBodyMemo(c, d, lay, w, h, -1); again != body {
			t.Fatalf("h=%d: repeat render returned a different body", h)
		}
		if got := d.Memo.Builds(); got != 1 {
			t.Fatalf("h=%d: repeat render rebuilt the braille chart: builds = %d", h, got)
		}
		scrubbed := heroBodyMemo(c, d, lay, w, h, 5)
		if got := d.Memo.Builds(); got != 1 {
			t.Fatalf("h=%d: scrub step rebuilt the braille chart: builds = %d", h, got)
		}
		if scrubbed == body {
			t.Fatalf("h=%d: scrub crosshair did not reach the degraded band", h)
		}
	}
	if braille == 0 {
		t.Fatalf("no height in [%d,%d) rendered braille — the assertion never ran",
			minHeroLogH, minHeroTwoPaneH)
	}
}

// TestHeroDegradedBandMatchesDirectRender: routing the degraded band through the
// memo must not change a single cell of the treatment it inherited.
func TestHeroDegradedBandMatchesDirectRender(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	d := heroTestData(HeroTrend)
	d.Gen, d.Memo = 1, NewHeroMemo()
	const w = 77
	for h := minHeroLogH; h < minHeroTwoPaneH; h++ {
		for _, scrub := range []int{-1, 5} {
			got := heroBodyMemo(c, d, lay, w, h, scrub)
			want := heroBody(c, d.Timeline, d.TimelineDim, lay, w, h, scrub)
			if got != want {
				t.Fatalf("h=%d scrub=%d: memoized band diverges from the direct render", h, scrub)
			}
		}
	}
}

// lowInputBuckets builds n daily buckets that all sit UNDER leverageInputFloor —
// a light user, or any hour-bucketed range (RangeToday groups by hour, where a
// 200K per-bucket input floor excludes nearly everyone).
func lowInputBuckets(n int) []store.Bucket {
	base := time.Date(2026, 7, 8, 0, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 0, n)
	for i := 0; i < n; i++ {
		in := int64(50_000 + 1_000*(i%21))
		out = append(out, store.Bucket{
			Keys:          map[string]string{"day": base.AddDate(0, 0, i).Format("2006-01-02")},
			Input:         in,
			Output:        in / 3,
			CacheRead:     in * 40,
			CacheCreation: in * 2,
			Total:         in * 44,
		})
	}
	return out
}

// TestHeroPivotBelowInputFloor: a range whose every bucket sits under the
// leverage floor is NOT an empty range. The pivot must say the ratio was
// skipped and keep the magnitude footer — "no rows in range" plus "press t to
// change range" is false on both counts over data that exists.
func TestHeroPivotBelowInputFloor(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	d := OverviewData{Timeline: lowInputBuckets(20), TimelineDim: "day", Mode: HeroLeverage}
	// h=6 is below the pivot's chart floor; h=16 is above it and still lands here
	// because no segment clears the floor.
	for _, h := range []int{6, 16} {
		out := ansiHero.ReplaceAllString(heroBodyMemo(c, d, lay, 77, h, -1), "")
		if strings.Contains(out, "no rows in range") {
			t.Errorf("h=%d: pivot claims an empty range over %d buckets:\n%s", h, len(d.Timeline), out)
		}
		if strings.Contains(out, "press t to change range") {
			t.Errorf("h=%d: pivot offers the range hint for a floor problem:\n%s", h, out)
		}
		if !strings.Contains(out, "leverage skipped") {
			t.Errorf("h=%d: pivot does not say why nothing is plotted:\n%s", h, out)
		}
		// The magnitudes need no floor to be true, so they stay.
		for _, want := range []string{"input", "output", "cache"} {
			if !strings.Contains(out, want) {
				t.Errorf("h=%d: below-floor pivot dropped the %q magnitude:\n%s", h, want, out)
			}
		}
		if got := len(strings.Split(out, "\n")); got != h {
			t.Errorf("h=%d: below-floor pivot rendered %d lines", h, got)
		}
	}
}

// TestHeroKeepsEmptyTreatment: the honest empty/zero states are upstream of the
// hero rewrite and must keep working — an empty range is "no rows", never a
// blank pane pair.
func TestHeroKeepsEmptyTreatment(t *testing.T) {
	c := heroTestCtx()
	lay := ComputeLayout(120, 30)
	for _, mode := range []HeroMode{HeroTrend, HeroLeverage} {
		d := OverviewData{TimelineDim: "day", Mode: mode}
		out := ansiHero.ReplaceAllString(heroBodyMemo(c, d, lay, 77, 16, -1), "")
		if !strings.Contains(out, "no rows in range") {
			t.Errorf("mode=%d empty timeline lost its treatment:\n%s", mode, out)
		}
	}
}
