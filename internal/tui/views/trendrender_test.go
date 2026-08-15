package views

import (
	"math"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// trendrender_test.go pins the hero's heat lanes: the slab is uniform, the
// intensity is the value, each lane scales against its own peak, a missing
// bucket stays a hole, and the geometry ladder degrades from tall bands to flat
// strips to the sub-floor pane fallback.

// spikeIdleBuckets is the shape these assertions are made against: a spike, a
// run of idle buckets, another spike - plus a real gap, so a renderer that
// paints across an outage is caught here too.
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
		// Deliberately tiny: a few percent of the lane's peak, so it lands on the
		// bottom rung of the intensity ramp. It is the smallest-honest-mark case -
		// an active bucket that must never paint as an idle one.
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

func trendTestData() OverviewData {
	return OverviewData{
		Timeline:    spikeIdleBuckets(),
		TimelineDim: "day",
		Mode:        HeroTrend,
		Cursor:      5,
	}
}

// renderTrendHero renders the hero panel at (w, h).
func renderTrendHero(w, h int) string {
	return heroPanel(heroTestCtx(), trendTestData(), w, h, ComputeLayout(w+40, h+10), false)
}

// lipglossHeight counts rendered rows without importing lipgloss for one call.
func lipglossHeight(s string) int { return len(strings.Split(s, "\n")) }

// TestTrendHeroHoldsItsHeight is the panel-level smoke gate: the hero holds its
// exact height at the two gate sizes, one above the two-pane floor and one
// below it, where the decade-ring log band is the body instead.
func TestTrendHeroHoldsItsHeight(t *testing.T) {
	for _, geom := range []struct{ w, h int }{{100, 30}, {42, 12}} {
		out := renderTrendHero(geom.w, geom.h)
		if n := lipglossHeight(out); n != geom.h {
			t.Errorf("%dx%d: panel is %d rows, want %d", geom.w, geom.h, n, geom.h)
		}
	}
}

// sgrPrefix is the escape sequence a style writes ahead of its content. It is
// how a canvas cell's ATTRIBUTES are compared without reaching into lipgloss
// internals, which matters where two ramp rungs share a glyph and differ only
// in faint or bold.
func sgrPrefix(s lipgloss.Style) string {
	r := s.Render("X")
	if i := strings.Index(r, "X"); i > 0 {
		return r[:i]
	}
	return ""
}

// heatLanesFor builds the hero trend body and returns its single pane, the pane
// geometry and the lane layout the assertions below index with.
func heatLanesFor(t *testing.T, w, h int) (*timeserieslinechart.Model, paneGeom, heatGeom, []CompSpec) {
	t.Helper()
	c := heroTestCtx()
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

// isHeatInk reports whether r is a rung of the intensity ramp, and not the idle
// track or the blank left by a hole.
func isHeatInk(r rune) bool {
	for _, rung := range heatRamp {
		if rung.glyph == r {
			return true
		}
	}
	return false
}

// heatSlab returns lane li's painted column at plot column x, one cell per band
// row, top row first. A bucket owns its whole band, so this is the entire mark
// the renderer makes for it.
func heatSlab(m *timeserieslinechart.Model, g paneGeom, hg heatGeom, li, x int) []canvas.Cell {
	out := make([]canvas.Cell, 0, hg.bandH)
	for row := hg.heatRow(li); row <= hg.baseRow(li); row++ {
		out = append(out, m.Canvas.Cell(canvas.Point{X: g.startX + x, Y: row}))
	}
	return out
}

// cellInk is a cell's whole visible identity: its glyph plus the attributes it
// is drawn with. Two rungs of the ramp share the '█' glyph and differ only in
// bold, so a rune-only comparison would call them the same ink.
func cellInk(c canvas.Cell) string { return string(c.Rune) + sgrPrefix(c.Style) }

// TestHeatSlabsAreUniform is the contract that separates a heat map from the bar
// chart it replaced: a bucket paints every row of its band with the SAME ink.
// Nothing in a column's shape carries information — no silhouette to measure, no
// tip to read — so the value has nowhere to live but the intensity. The
// assertion covers every plot column, holes and idle buckets included: a lane is
// uniform everywhere or it is not a band.
func TestHeatSlabsAreUniform(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	if !hg.tall || hg.bandH < heatMinTallBand {
		t.Fatalf("expected tall bands at 100x24, got tall=%v bandH=%d", hg.tall, hg.bandH)
	}
	for li := range specs {
		for x := 0; x < g.gw; x++ {
			slab := heatSlab(m, g, hg, li, x)
			want := cellInk(slab[0])
			for row, cell := range slab {
				if got := cellInk(cell); got != want {
					t.Fatalf("lane %d column %d: band row %d inks %q, row 0 inks %q — the slab is not uniform",
						li, x, row, got, want)
				}
			}
		}
	}
}

// TestHeatMarksEveryNonZeroBucket: every bucket that carries tokens paints its
// whole band in ramp ink, and every idle bucket paints its whole band with the
// track mark. The smallest-honest-mark rule says a non-zero bucket may never
// render as an idle one, and an idle one may never render as a hole.
func TestHeatMarksEveryNonZeroBucket(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	if !hg.tall {
		t.Fatalf("expected tall lanes at 100x24, got a band of %d", hg.bandH)
	}
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	for li, s := range specs {
		for i, b := range buckets {
			v := s.Pick(Split(b))
			for row, cell := range heatSlab(m, g, hg, li, heatColOf(g, times[i])) {
				if v > 0 {
					if !isHeatInk(cell.Rune) {
						t.Errorf("lane %s bucket %s (%d tokens): band row %d holds %q, want ramp ink",
							s.Key, b.Keys["day"], v, row, string(cell.Rune))
					}
					continue
				}
				if cell.Rune != heatTrack {
					t.Errorf("lane %s bucket %s (idle): band row %d holds %q, want the track mark %q",
						s.Key, b.Keys["day"], row, string(cell.Rune), string(heatTrack))
				}
			}
		}
	}
}

// TestHeatInkTracksValue: the intensity is the value. Every bucket must paint
// the rung its own fraction of the lane peak maps to, which is what makes the
// picture readable against the "max" printed on the lane rule.
func TestHeatInkTracksValue(t *testing.T) {
	m, g, hg, specs := heatLanesFor(t, 100, 24)
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")
	for li, s := range specs {
		var peak int64
		for _, b := range buckets {
			if v := s.Pick(Split(b)); v > peak {
				peak = v
			}
		}
		if peak == 0 {
			t.Fatalf("lane %s has no non-zero bucket", s.Key)
		}
		for i, b := range buckets {
			v := s.Pick(Split(b))
			if v <= 0 {
				continue
			}
			want := heatRamp[heatRungFor(float64(v)/float64(peak))]
			got := heatSlab(m, g, hg, li, heatColOf(g, times[i]))[0]
			if got.Rune != want.glyph {
				t.Errorf("lane %s bucket %s (%d of %d) inks %q, want the rung glyph %q",
					s.Key, b.Keys["day"], v, peak, string(got.Rune), string(want.glyph))
			}
		}
	}
}

// TestHeatRungIsMonotone pins the ramp itself, where the ordering contract
// lives: more tokens never means less ink, any non-zero fraction reaches the
// first rung, and the peak reaches the last. Zero is not on the ramp at all —
// it is the track mark, so heatRungFor reports no rung for it.
func TestHeatRungIsMonotone(t *testing.T) {
	if r := heatRungFor(0); r != -1 {
		t.Errorf("heatRungFor(0) = %d, want -1 (no rung: zero is the track mark)", r)
	}
	if r := heatRungFor(1e-9); r != 0 {
		t.Errorf("heatRungFor(1e-9) = %d, want the bottom rung 0", r)
	}
	if r := heatRungFor(1); r != len(heatRamp)-1 {
		t.Errorf("heatRungFor(1) = %d, want the top rung %d", r, len(heatRamp)-1)
	}
	if r := heatRungFor(5); r != len(heatRamp)-1 {
		t.Errorf("heatRungFor(5) = %d, want the top rung %d clamped", r, len(heatRamp)-1)
	}
	prev := heatRungFor(0)
	for i := 0; i <= 10_000; i++ {
		f := float64(i) / 10_000
		r := heatRungFor(f)
		if r < prev {
			t.Fatalf("heatRungFor(%g) = %d, below the rung of a smaller fraction (%d)", f, r, prev)
		}
		prev = r
	}
}

// TestHeatDegradesToFlatBands pins the ladder: bands grow to fill the pane while
// they can be at least heatMinTallBand rows tall, and below that the lanes fall
// back to fixed 2-row then 1-row strips before the sub-floor pane fallback takes
// over entirely. The encoding is the same at every rung of that ladder — only
// the thickness of the band changes.
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
		out := renderTrendHero(100, h)
		if n := lipglossHeight(out); n != h {
			t.Errorf("heat panel at 100x%d is %d rows, want %d", h, n, h)
		}
		if !strings.Contains(ansiHero.ReplaceAllString(out, ""), "max ") {
			t.Errorf("heat panel at 100x%d lost its per-lane max readouts:\n%s", h, out)
		}
	}
}

// TestHeatFallsBackBelowItsFloor: a pane too short for one lane per series must
// still build a hero body, and it must be the two detented braille panes rather
// than nothing at all. That fallback is the bottom of the built-body ladder.
//
// It takes a FOURTH series to reach it. The lanes need two rows per series and
// the two panes need nine body rows, so at the three series the hero actually
// carries the lanes always win and the fallback is structural rather than
// reachable; a wider component descriptor is what puts the two floors back in
// the order the fallback was written for.
func TestHeatFallsBackBelowItsFloor(t *testing.T) {
	c := heroTestCtx()
	c.Comp = append(append([]CompSpec{}, c.Comp...), CompSpec{
		Key: "extra", Label: "extra", Short: "extra", Glyph: "▪",
		Color: lipgloss.Color("5"),
		Pick:  func(x Components) int64 { return x.Input },
	})
	buckets := spikeIdleBuckets()
	times := bucketTimes(buckets, "day")

	const h = 9
	if _, ok := newHeatGeom(h-2, len(c.Comp)); ok {
		t.Fatalf("%d lanes still fit %d rows; the fixture no longer falls below the heat floor",
			len(c.Comp), h-2)
	}
	f, ok := buildTwoPaneFrame(c, buckets, times, "day", 100, h, &detentLock{})
	if !ok {
		t.Fatal("the sub-floor fallback did not build")
	}
	if len(f.panes) != 2 {
		t.Fatalf("the sub-floor fallback built %d panes, want the two detented panes", len(f.panes))
	}
}

// TestHeatCrosshairPaintsWithoutMovingInk: the scrub crosshair and the now
// column are canvas post-passes that touch backgrounds only, so pinning the
// scrub must change the rendered frame while leaving every glyph exactly where
// it was. Over lanes that reads as one full-height band through all three at the
// same instant, which is what makes the lanes comparable at a point in time.
func TestHeatCrosshairPaintsWithoutMovingInk(t *testing.T) {
	c := heroTestCtx()
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
		for row, cell := range heatSlab(m, g, hg, li, mid) {
			if cell.Rune != 0 && cell.Rune != ' ' {
				t.Errorf("lane %d: outage column %d band row %d holds %q, want an explicit hole",
					li, mid, row, string(cell.Rune))
			}
		}
	}
}

// TestHeatLanesSelfScale is the point of the rendering: each lane is scaled
// against its OWN peak, so every lane's largest bucket reaches the top rung of
// the ramp even though cache runs about 100x fresh. On one shared scale the two
// fresh lanes would sit on the bottom rung throughout — the same data fact that
// forced the two-pane split.
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
		for row, cell := range heatSlab(m, g, hg, li, heatColOf(g, times[at])) {
			if cell.Rune != top {
				t.Errorf("lane %s peak (%d tokens) inks %q on band row %d, want the top rung %q",
					s.Key, peaks[li], string(cell.Rune), row, string(top))
			}
		}
	}
	// And the spread the lanes exist to absorb is really there.
	if peaks[len(peaks)-1] < 20*peaks[0] {
		t.Fatalf("cache peak %d is only %.1fx input peak %d — the fixture no longer exercises self-scaling",
			peaks[len(peaks)-1], float64(peaks[len(peaks)-1])/float64(peaks[0]), peaks[0])
	}
	// Each lane must state the number its intensities are a fraction of.
	frame := ansiHero.ReplaceAllString(m.View(), "")
	for li, s := range specs {
		want := "max " + detentHuman(heroTestCtx(), peaks[li])
		if !strings.Contains(frame, want) {
			t.Errorf("lane %s does not declare %q:\n%s", s.Key, want, frame)
		}
	}
}
