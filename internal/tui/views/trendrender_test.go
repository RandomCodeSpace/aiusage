package views

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/runes"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// trendrender_test.go is the rendering smoke gate for the candidate trend
// treatments (ticket #65). It is a prototype harness, not a pixel contract: it
// pins the things that would make a side-by-side dishonest — a panic, two
// candidates that are secretly the same picture, a smoothed curve that dips
// below zero on an idle stretch, or a real bucket with no marker on it.

// allTrends is the switcher's cycle, in order.
var allTrends = []TrendRender{TrendCurrent, TrendSmooth, TrendColumns, TrendRidge, TrendHeat}

// TestTrendCycleVisitsEveryCandidate: x must reach every candidate and come back
// to the shipped renderer, or one of them is unreachable in the prototype.
func TestTrendCycleVisitsEveryCandidate(t *testing.T) {
	got := []TrendRender{TrendCurrent}
	for tr := TrendCurrent.Next(); tr != TrendCurrent; tr = tr.Next() {
		got = append(got, tr)
		if len(got) > len(allTrends) {
			t.Fatalf("x cycle does not return to the baseline: %v", got)
		}
	}
	if len(got) != len(allTrends) {
		t.Fatalf("x cycle visits %v, want %v", got, allTrends)
	}
	for i, tr := range allTrends {
		if got[i] != tr {
			t.Fatalf("x cycle position %d = %v, want %v", i, got[i], tr)
		}
	}
}

// spikeIdleBuckets is the shape the interpolation research measured against:
// a spike, a run of idle buckets, another spike — plus a real gap, so a
// treatment that smooths across an outage is caught here too.
//
// Cache deliberately does NOT track input. A fixed multiple would make the
// self-scaled heat lanes render three identical strips, which proves the
// scaling arithmetic and nothing about whether the lanes can disagree; cache
// here peaks where input is idle and still runs the ~100x larger that forced
// the two-pane split in the first place.
func spikeIdleBuckets() []store.Bucket {
	// day index -> (input, cache) tokens; days missing from this list are the gap.
	shape := []struct {
		day       int
		in, cache int64
	}{
		{0, 4_000_000, 500_000_000},
		{1, 0, 90_000_000},
		{2, 0, 0},
		{3, 0, 12_000_000},
		{4, 0, 0},
		{5, 3_200_000, 620_000_000},
		{6, 0, 40_000_000},
		// Deliberately tiny: under one band row of the lane's peak at every
		// geometry the tests render, so it exercises the eighth-block cap and the
		// smallest-honest-mark rule on the height channel.
		{7, 120_000, 150_000_000},
		// days 8 and 9 are absent: an outage wider than 1.5 bucket steps.
		{10, 5_000_000, 300_000_000},
		{11, 0, 0},
		{12, 0, 210_000_000},
		{13, 2_400_000, 480_000_000},
	}
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 0, len(shape))
	for _, s := range shape {
		out = append(out, store.Bucket{
			Keys:          map[string]string{"day": base.AddDate(0, 0, s.day).Format("2006-01-02")},
			Input:         s.in,
			Output:        s.in / 3,
			CacheRead:     s.cache * 4 / 5,
			CacheCreation: s.cache / 5,
			Total:         s.in + s.in/3 + s.cache,
		})
	}
	return out
}

// trendTestCtx is heroTestCtx pinned to one candidate treatment.
func trendTestCtx(tr TrendRender) Ctx {
	c := heroTestCtx()
	c.Trend = tr
	return c
}

func trendTestData() OverviewData {
	return OverviewData{
		Timeline:    spikeIdleBuckets(),
		TimelineDim: "day",
		Mode:        HeroTrend,
		Cursor:      5,
	}
}

// renderTrendHero renders the hero panel for one treatment at (w, h).
func renderTrendHero(tr TrendRender, w, h int) string {
	return heroPanel(trendTestCtx(tr), trendTestData(), w, h, ComputeLayout(w+40, h+10), false)
}

// TestTrendTreatmentsRenderAndDiffer is the core smoke gate: no candidate may
// panic at either gate size, every candidate must hold the panel's exact height,
// and — wherever the two detented panes are the body — no two candidates may
// produce the same frame. Below the two-pane floor the hero renders the
// decade-ring log band, which the prototype deliberately leaves untouched, so
// identical frames there are the correct answer rather than a bug.
func TestTrendTreatmentsRenderAndDiffer(t *testing.T) {
	for _, geom := range []struct{ w, h int }{{100, 30}, {42, 12}} {
		lay := ComputeLayout(geom.w+40, geom.h+10)
		inner := geom.w - cardChromeW
		twoPane := heroFrameFor(HeroTrend, lay, inner, geom.h-heroCardChromeH) == heroFrameTwoPane

		seen := map[string]TrendRender{}
		for _, tr := range allTrends {
			out := renderTrendHero(tr, geom.w, geom.h)
			if n := lipglossHeight(out); n != geom.h {
				t.Errorf("%dx%d %v: panel is %d rows, want %d", geom.w, geom.h, tr, n, geom.h)
			}
			if !strings.Contains(ansiHero.ReplaceAllString(out, ""), tr.Chip()) {
				t.Errorf("%dx%d %v: title carries no treatment chip", geom.w, geom.h, tr)
			}
			if !twoPane {
				continue
			}
			if prev, dup := seen[out]; dup {
				t.Errorf("%dx%d: %v renders the same frame as %v", geom.w, geom.h, tr, prev)
			}
			seen[out] = tr
		}
	}
}

// lipglossHeight counts rendered rows without importing lipgloss for one call.
func lipglossHeight(s string) int { return len(strings.Split(s, "\n")) }

// freshPaneFor builds the two-pane hero for one treatment and returns its fresh
// pane, which is the pane both the below-zero and the marker contract live on.
func freshPaneFor(t *testing.T, tr TrendRender, w, h int) (*heroFrame, paneGeom) {
	t.Helper()
	c := trendTestCtx(tr)
	buckets := spikeIdleBuckets()
	f, ok := buildHeroFrame(c, buckets, "day", heroFrameTwoPane, w, h, nil)
	if !ok || len(f.panes) < 1 {
		t.Fatalf("%v: two-pane hero did not build at %dx%d", tr, w, h)
	}
	return f, newPaneGeom(f.panes[0].model)
}

// TestSmoothCurveStaysOnZeroOverIdle is the below-zero test from the research.
// Fritsch-Carlson zeroes the tangent at a zero secant, so a run of idle buckets
// interpolates to exactly zero and never dips under it; uniform Catmull-Rom
// undershoots the same shape by 2/27 of the spike height, which on token counts
// is a negative number of tokens. The assertion is made twice — once on the
// spline itself, where "exactly zero" is checkable, and once on the drawn
// canvas, where an idle column must carry ink on the baseline dot row and
// nowhere above it.
func TestSmoothCurveStaysOnZeroOverIdle(t *testing.T) {
	xs := []float64{0, 1, 2, 3, 4, 5}
	ys := []float64{4_000_000, 0, 0, 0, 0, 3_200_000}
	tan := monotoneTangents(xs, ys)
	for i := 0; i <= 4000; i++ {
		x := 1 + 3*float64(i)/4000 // strictly inside the idle stretch
		if v := evalMonotone(xs, ys, tan, x); v != 0 {
			t.Fatalf("monotone spline at x=%g over an idle stretch = %g, want exactly 0", x, v)
		}
	}
	for i := 0; i <= 5000; i++ {
		x := 5 * float64(i) / 5000
		if v := evalMonotone(xs, ys, tan, x); v < 0 {
			t.Fatalf("monotone spline at x=%g = %g, below zero", x, v)
		}
		if v := evalMonotone(xs, ys, tan, x); v > math.Max(ys[0], ys[5]) {
			t.Fatalf("monotone spline at x=%g = %g, above the data range", x, v)
		}
	}

	// And on the canvas: pick the columns strictly between two idle buckets, so
	// no real-bucket marker is in play, and require the ink to sit on the
	// baseline dot row only.
	f, g := freshPaneFor(t, TrendSmooth, 100, 24)
	m := f.panes[0].model
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	lo := g.dotX(float64(times[2].UnixMilli())/1e3) / 2
	hi := g.dotX(float64(times[3].UnixMilli())/1e3) / 2
	if hi-lo < 4 {
		t.Fatalf("idle stretch spans %d cells, too narrow to assert on", hi-lo)
	}
	// A marker is two dot columns wide, so a bucket on an odd dot column bleeds
	// one cell to the RIGHT. Skipping one cell after the left bucket clears it;
	// nothing bleeds leftwards, so the right bucket needs no margin of its own.
	const bottomRowDots = rune(0x40 | 0x80)
	for x := lo + 2; x < hi; x++ {
		for y := 0; y < g.gh; y++ {
			r := m.Canvas.Cell(canvas.Point{X: g.startX + x, Y: y}).Rune
			if r == 0 || !runes.IsBraillePattern(r) {
				continue
			}
			dots := r - runes.BrailleBlockOffset
			if y != g.gh-1 {
				t.Fatalf("idle column %d carries ink on row %d (of %d) — the curve left the baseline", x, y, g.gh)
			}
			if dots&^bottomRowDots != 0 {
				t.Fatalf("idle column %d baseline cell = %#x, has dots above the zero row", x, dots)
			}
		}
	}
}

// TestSmoothMarksEveryRealBucket pins the honesty treatment: candidate A draws a
// marker at every REAL bucket coordinate and never at a curve sample, which is
// the trap Observable Plot documents for built-in line markers. The anchor dot
// of each marker is the bucket's own coordinate, so it must be lit in the
// composited cell that covers it.
func TestSmoothMarksEveryRealBucket(t *testing.T) {
	f, g := freshPaneFor(t, TrendSmooth, 100, 24)
	m := f.panes[0].model
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	if len(times) != len(buckets) {
		t.Fatalf("test buckets do not all parse: %d times for %d buckets", len(times), len(buckets))
	}
	for i, b := range buckets {
		p := canvas.Point{
			X: g.dotX(float64(times[i].UnixMilli()) / 1e3),
			Y: g.dotY(float64(b.Input)),
		}
		cell := m.Canvas.Cell(canvas.Point{X: g.startX + p.X/2, Y: p.Y / 4})
		if !runes.IsBraillePattern(cell.Rune) {
			t.Fatalf("bucket %d (%s): cell at dot (%d,%d) holds %q, not a braille pattern",
				i, b.Keys["day"], p.X, p.Y, string(cell.Rune))
		}
		if bit := brailleDotBit(p.X%2, p.Y%4); (cell.Rune-runes.BrailleBlockOffset)&bit == 0 {
			t.Errorf("bucket %d (%s): no marker dot at its own coordinate (%d,%d)",
				i, b.Keys["day"], p.X, p.Y)
		}
	}
}

// brailleDotBit is the dot-number mapping of the Unicode Braille Patterns block.
// It is NOT row-major: dots 7 and 8 were appended after the original six, so the
// bottom row is bits 0x40 and 0x80.
//
//	[0][3] = [0x01][0x08]
//	[1][4]   [0x02][0x10]
//	[2][5]   [0x04][0x20]
//	[6][7]   [0x40][0x80]
func brailleDotBit(col, row int) rune {
	left := [4]rune{0x01, 0x02, 0x04, 0x40}
	right := [4]rune{0x08, 0x10, 0x20, 0x80}
	if col == 0 {
		return left[row]
	}
	return right[row]
}

// TestOwnershipKeepsCoincidentSeriesVisible is the issue #67 contract: with two
// series running over the same cells, majority ownership must leave BOTH with
// visible cells. The shipped renderer writes the incoming style on every draw,
// so the earlier series renders zero visible cells wherever the later one shares
// its cells — decided purely by argument order.
func TestOwnershipKeepsCoincidentSeriesVisible(t *testing.T) {
	// Input and output are 3:1 here, close enough on a shared pane to share
	// cells across most of the range.
	count := func(tr TrendRender) (in, out int) {
		f, _ := freshPaneFor(t, tr, 100, 24)
		frame := f.panes[0].model.View()
		specs, _ := heroPaneSeries(trendTestCtx(tr))
		if len(specs) < 2 {
			t.Fatal("fresh pane must carry two series")
		}
		return strings.Count(frame, ansiFor(specs[0])), strings.Count(frame, ansiFor(specs[1]))
	}
	in, out := count(TrendSmooth)
	if in == 0 || out == 0 {
		t.Errorf("candidate A: coincident series render in=%d out=%d styled runs, want both non-zero", in, out)
	}
}

// ansiFor is the SGR prefix one series' style emits, used to count the cells a
// series actually owns in a rendered frame.
func ansiFor(s CompSpec) string {
	r := s.Style().Render("X")
	if i := strings.Index(r, "X"); i > 0 {
		return r[:i]
	}
	return r
}

// heatLanesFor builds candidate D and returns its single pane, the pane
// geometry and the lane layout the assertions below index with.
func heatLanesFor(t *testing.T, w, h int) (*timeserieslinechart.Model, paneGeom, heatGeom, []CompSpec) {
	t.Helper()
	c := trendTestCtx(TrendHeat)
	f, ok := buildHeroFrame(c, spikeIdleBuckets(), "day", heroFrameTwoPane, w, h, nil)
	if !ok || len(f.panes) != 1 {
		t.Fatalf("heat frame did not build at %dx%d", w, h)
	}
	m := f.panes[0].model
	g := newPaneGeom(m)
	hg, ok := newHeatGeom(g.gh, len(c.Comp))
	if !ok {
		t.Fatalf("heat lanes do not fit %d rows", g.gh)
	}
	return m, g, hg, c.Comp
}

// heatColOf is the plot column a bucket time owns, matching paneGeom's mapping.
func heatColOf(g paneGeom, ts time.Time) int {
	x := float64(ts.UnixMilli()) / 1e3
	return int(math.Round((x - g.minX) / (g.maxX - g.minX) * float64(g.gw-1)))
}

// isHeatInk reports whether r is ink a column can be built from — a ramp shade
// or an eighth-block cap — and not the idle track or the blank left by a hole.
func isHeatInk(r rune) bool {
	for _, rung := range heatRamp {
		if rung.glyph == r {
			return true
		}
	}
	return r >= runes.LowerBlockOne && r <= runes.FullBlock
}

// heatColumnTop returns the topmost band row of lane li carrying column ink at
// plot column x, and the rune there. row = -1 when the column is empty.
func heatColumnTop(m *timeserieslinechart.Model, g paneGeom, hg heatGeom, li, x int) (int, rune) {
	for row := hg.heatRow(li); row <= hg.baseRow(li); row++ {
		r := m.Canvas.Cell(canvas.Point{X: g.startX + x, Y: row}).Rune
		if isHeatInk(r) {
			return row, r
		}
	}
	return -1, 0
}

// heatColumnCells is a column's height in whole-cell units at plot column x:
// how many band rows it reaches. Used to compare two buckets' heights.
func heatColumnCells(m *timeserieslinechart.Model, g paneGeom, hg heatGeom, li, x int) int {
	top, _ := heatColumnTop(m, g, hg, li, x)
	if top < 0 {
		return 0
	}
	return hg.baseRow(li) - top + 1
}

// TestHeatMarksEveryNonZeroBucket: every bucket that carries tokens must carry a
// mark on its lane baseline. The smallest-honest-mark rule says a non-zero
// bucket may never render as an idle one, and an idle one may never render as a
// hole. The baseline is where the assertion lives now that a column's height is
// the value: it is the one row every non-empty column occupies.
func TestHeatMarksEveryNonZeroBucket(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	if !hg.tall {
		t.Fatalf("expected height-modulated lanes at 100x24, got a flat band of %d", hg.bandH)
	}
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	for li, s := range specs {
		for i, b := range buckets {
			p := canvas.Point{X: g.startX + heatColOf(g, times[i]), Y: hg.baseRow(li)}
			got := m.Canvas.Cell(p).Rune
			if v := s.Pick(Split(b)); v > 0 {
				if !isHeatInk(got) {
					t.Errorf("lane %s bucket %s (%d tokens): baseline holds %q, want column ink",
						s.Key, b.Keys["day"], v, string(got))
				}
				continue
			}
			if got != heatTrack {
				t.Errorf("lane %s bucket %s (idle): baseline holds %q, want the track mark %q",
					s.Key, b.Keys["day"], string(got), string(heatTrack))
			}
		}
	}
}

// TestHeatColumnHeightTracksValue is the rework's contract: within a lane the
// vertical space carries the number. A lane's peak bucket must fill its band to
// the top row, and a materially smaller bucket must render materially shorter —
// the ordering of heights must match the ordering of values.
func TestHeatColumnHeightTracksValue(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	if !hg.tall || hg.bandH < heatMinTallBand {
		t.Fatalf("expected a tall band at 100x24, got tall=%v bandH=%d", hg.tall, hg.bandH)
	}
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")

	for li, s := range specs {
		type sample struct {
			v    int64
			cols int
		}
		var peak sample
		var rest []sample
		for i, b := range buckets {
			v := s.Pick(Split(b))
			if v <= 0 {
				continue
			}
			n := heatColumnCells(m, g, hg, li, heatColOf(g, times[i]))
			if v > peak.v {
				if peak.v > 0 {
					rest = append(rest, peak)
				}
				peak = sample{v, n}
				continue
			}
			rest = append(rest, sample{v, n})
		}
		if peak.v == 0 {
			t.Fatalf("lane %s has no non-zero bucket", s.Key)
		}
		// The peak fills the band: its top cell is the band's top row.
		if peak.cols != hg.bandH {
			t.Errorf("lane %s peak (%d tokens) is %d of %d band rows, want the full band",
				s.Key, peak.v, peak.cols, hg.bandH)
		}
		for _, r := range rest {
			if r.cols > peak.cols {
				t.Errorf("lane %s: %d tokens renders %d rows, taller than the peak %d tokens at %d",
					s.Key, r.v, r.cols, peak.v, peak.cols)
			}
			// A bucket under half the peak must not fill the band.
			if 2*r.v < peak.v && r.cols >= hg.bandH {
				t.Errorf("lane %s: %d tokens (under half of %d) still fills all %d rows",
					s.Key, r.v, peak.v, hg.bandH)
			}
		}
	}
}

// TestHeatSubRowPrecision: a value too small for a whole band row still renders,
// as an eighth-block cap on the baseline. That is the smallest honest mark and
// the reason the height channel does not quantize small buckets to nothing.
func TestHeatSubRowPrecision(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	// Lane 0 is input; day 7 (900K) is well under one band row of the 5M peak.
	li := 0
	var small, peak int64
	for _, b := range buckets {
		if v := specs[li].Pick(Split(b)); v > peak {
			peak = v
		}
	}
	small = specs[li].Pick(Split(buckets[7]))
	if small == 0 || float64(small)/float64(peak)*float64(hg.bandH) >= 1 {
		t.Fatalf("fixture bucket 7 (%d of %d) is not a sub-row value at bandH=%d", small, peak, hg.bandH)
	}
	row, r := heatColumnTop(m, g, hg, li, heatColOf(g, times[7]))
	if row != hg.baseRow(li) {
		t.Errorf("sub-row bucket tops at row %d, want the baseline %d", row, hg.baseRow(li))
	}
	if r < runes.LowerBlockOne || r > runes.FullBlock {
		t.Errorf("sub-row bucket renders %q, want an eighth-block cap", string(r))
	}
}

// TestHeatDegradesToFlatBands pins the ladder: height modulation needs bands of
// at least heatMinTallBand rows, and below that the lanes degrade to the flat
// 2-row then 1-row strips before the pane fallback takes over entirely.
func TestHeatDegradesToFlatBands(t *testing.T) {
	const lanes = 3
	for _, tc := range []struct {
		gh    int
		ok    bool
		tall  bool
		bandH int
	}{
		{gh: 25, ok: true, tall: true, bandH: 7},
		{gh: 15, ok: true, tall: true, bandH: 4},
		{gh: 12, ok: true, tall: true, bandH: 3},
		{gh: 11, ok: true, tall: false, bandH: 2},
		{gh: 9, ok: true, tall: false, bandH: 2},
		{gh: 8, ok: true, tall: false, bandH: 1},
		{gh: 6, ok: true, tall: false, bandH: 1},
		{gh: 5, ok: false},
	} {
		hg, ok := newHeatGeom(tc.gh, lanes)
		if ok != tc.ok {
			t.Fatalf("gh=%d: ok=%v, want %v", tc.gh, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if hg.tall != tc.tall || hg.bandH != tc.bandH {
			t.Errorf("gh=%d: tall=%v bandH=%d, want tall=%v bandH=%d",
				tc.gh, hg.tall, hg.bandH, tc.tall, tc.bandH)
		}
		// Whatever the mode, the lanes must fit the rows they were given.
		if last := hg.baseRow(lanes - 1); last >= tc.gh {
			t.Errorf("gh=%d: last lane baseline at row %d overflows the pane", tc.gh, last)
		}
	}
	// End to end at the one PANEL height where the flat band is what renders:
	// the hero body is chartH = h-heroCardChromeH and the lanes get chartH-2, so
	// a 16-row panel leaves 11 rows and a 17-row panel is the first tall one.
	for _, h := range []int{16, 17} {
		out := renderTrendHero(TrendHeat, 100, h)
		if n := lipglossHeight(out); n != h {
			t.Errorf("heat panel at 100x%d is %d rows, want %d", h, n, h)
		}
		if !strings.Contains(ansiHero.ReplaceAllString(out, ""), "max ") {
			t.Errorf("heat panel at 100x%d lost its per-lane max readouts:\n%s", h, out)
		}
	}
}

// TestHeatCrosshairPaintsWithoutMovingInk: the scrub crosshair and the now
// column are canvas post-passes that touch backgrounds only, so pinning the
// scrub must change the rendered frame while leaving every glyph exactly where
// it was. Over lanes that reads as one full-height band through all three at the
// same instant, which is what makes the lanes comparable at a point in time.
func TestHeatCrosshairPaintsWithoutMovingInk(t *testing.T) {
	c := trendTestCtx(TrendHeat)
	f, ok := buildHeroFrame(c, spikeIdleBuckets(), "day", heroFrameTwoPane, 100, 24, nil)
	if !ok {
		t.Fatal("heat frame did not build")
	}
	loose := f.render(c, -1)
	pinned := f.render(c, 5)
	if loose == pinned {
		t.Fatal("pinning the scrub did not change the rendered heat frame")
	}
	if a, b := ansiHero.ReplaceAllString(loose, ""), ansiHero.ReplaceAllString(pinned, ""); a != b {
		t.Errorf("the crosshair moved glyphs, not just backgrounds:\n%s\n---\n%s", a, b)
	}
	// And the frame must be reusable: the highlight is cleared after rendering.
	if again := f.render(c, -1); again != loose {
		t.Error("the retained heat frame did not come back clean after a pinned render")
	}
}

// TestHeatGapIsAHoleNotAZero: a missing bucket must leave the lane track blank,
// which is the one thing that distinguishes an outage from an idle day. Both
// would otherwise read as "no tokens", and only one of them is a fact about
// usage rather than about collection.
func TestHeatGapIsAHoleNotAZero(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	// Days 8 and 9 are absent; day 7 and day 10 bracket the outage.
	lo := heatColOf(g, times[7])
	hi := heatColOf(g, times[8]) // the bucket AFTER the gap
	if hi-lo < 4 {
		t.Fatalf("gap spans %d columns, too narrow to assert on", hi-lo)
	}
	mid := (lo + hi) / 2
	for li := range specs {
		got := m.Canvas.Cell(canvas.Point{X: g.startX + mid, Y: hg.heatRow(li)}).Rune
		if got != 0 && got != ' ' {
			t.Errorf("lane %d: outage column %d holds %q, want an explicit hole",
				li, mid, string(got))
		}
	}
}

// TestHeatLanesSelfScale is the point of the treatment: each lane is scaled
// against its OWN peak, so every lane's largest bucket reaches BOTH the top of
// its band and the top rung of the ramp even though cache runs about 100x fresh.
// On one shared scale the two fresh lanes would sit flat on the baseline — the
// same data fact that forced the two-pane split.
func TestHeatLanesSelfScale(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	top := heatRamp[len(heatRamp)-1].glyph

	peaks := make([]int64, len(specs))
	for li, s := range specs {
		var at int
		for i, b := range buckets {
			if v := s.Pick(Split(b)); v > peaks[li] {
				peaks[li], at = v, i
			}
		}
		if peaks[li] == 0 {
			t.Fatalf("lane %s has no non-zero bucket to peak on", s.Key)
		}
		x := heatColOf(g, times[at])
		row, r := heatColumnTop(m, g, hg, li, x)
		if row != hg.heatRow(li) {
			t.Errorf("lane %s peak (%d tokens) tops at row %d, want the band top %d",
				s.Key, peaks[li], row, hg.heatRow(li))
		}
		if r != top {
			t.Errorf("lane %s peak (%d tokens) inks %q, want the top rung %q",
				s.Key, peaks[li], string(r), string(top))
		}
	}
	// And the spread the treatment exists to absorb is really there.
	if peaks[len(peaks)-1] < 20*peaks[0] {
		t.Fatalf("cache peak %d is only %.1fx input peak %d — the fixture no longer exercises self-scaling",
			peaks[len(peaks)-1], float64(peaks[len(peaks)-1])/float64(peaks[0]), peaks[0])
	}
	// Each lane must state the number its intensities are a fraction of.
	frame := ansiHero.ReplaceAllString(m.View(), "")
	for li, s := range specs {
		want := "max " + detentHuman(trendTestCtx(TrendHeat), peaks[li])
		if !strings.Contains(frame, want) {
			t.Errorf("lane %s does not declare %q:\n%s", s.Key, want, frame)
		}
	}
}

// TestTrendFrameDump writes one ANSI-stripped frame per treatment when
// AIUSAGE_TREND_FRAMES names a directory. It exists so a reviewer can eyeball
// the candidates without a terminal; a normal test run writes nothing.
func TestTrendFrameDump(t *testing.T) {
	dir := os.Getenv("AIUSAGE_TREND_FRAMES")
	if dir == "" {
		t.Skip("set AIUSAGE_TREND_FRAMES=<dir> to dump candidate frames")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := map[TrendRender]string{
		TrendCurrent: "current", TrendSmooth: "a", TrendColumns: "b",
		TrendRidge: "c", TrendHeat: "d",
	}
	for _, tr := range allTrends {
		var b strings.Builder
		for _, geom := range []struct{ w, h int }{{100, 30}, {74, 20}, {42, 12}} {
			b.WriteString("=== " + tr.Chip() + "  @ " +
				itoa(geom.w) + "x" + itoa(geom.h) + " ===\n")
			b.WriteString(ansiHero.ReplaceAllString(renderTrendHero(tr, geom.w, geom.h), ""))
			b.WriteString("\n\n")
		}
		if err := os.WriteFile(filepath.Join(dir, names[tr]+".txt"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// itoa keeps the dump helper free of a strconv import in a file that needs it
// for nothing else.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
