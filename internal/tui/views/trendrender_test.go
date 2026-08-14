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

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// trendrender_test.go is the rendering smoke gate for the candidate trend
// treatments (ticket #65). It is a prototype harness, not a pixel contract: it
// pins the things that would make a side-by-side dishonest — a panic, two
// candidates that are secretly the same picture, a smoothed curve that dips
// below zero on an idle stretch, or a real bucket with no marker on it.

// allTrends is the switcher's cycle, in order.
var allTrends = []TrendRender{TrendCurrent, TrendSmooth, TrendColumns, TrendRidge}

// spikeIdleBuckets is the shape the interpolation research measured against:
// a spike, a run of idle buckets, another spike — plus a real gap, so a
// treatment that smooths across an outage is caught here too. Every series
// carries the shape so both hero panes exercise it.
func spikeIdleBuckets() []store.Bucket {
	// day index -> input tokens; the days missing from this list are the gap.
	shape := []struct {
		day int
		v   int64
	}{
		{0, 4_000_000},
		{1, 0},
		{2, 0},
		{3, 0},
		{4, 0},
		{5, 3_200_000},
		{6, 0},
		{7, 900_000},
		// days 8 and 9 are absent: an outage wider than 1.5 bucket steps.
		{10, 5_000_000},
		{11, 0},
		{12, 0},
		{13, 2_400_000},
	}
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 0, len(shape))
	for _, s := range shape {
		out = append(out, store.Bucket{
			Keys:          map[string]string{"day": base.AddDate(0, 0, s.day).Format("2006-01-02")},
			Input:         s.v,
			Output:        s.v / 3,
			CacheRead:     s.v * 120,
			CacheCreation: s.v * 4,
			Total:         s.v * 125,
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
		TrendCurrent: "current", TrendSmooth: "a", TrendColumns: "b", TrendRidge: "c",
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
