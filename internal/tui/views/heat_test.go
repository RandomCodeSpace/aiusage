package views

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// heatTestCtx is a bare context: no elevation, so pad() emits plain spaces and
// the strip's holes are countable.
func heatTestCtx() Ctx {
	return Ctx{Faint: lipgloss.NewStyle()}
}

// heatPlain renders a strip and strips its SGR, leaving the glyph row - which is
// the row that has to carry the data under NO_COLOR.
func heatPlain(vals []float64, w int, peak float64) string {
	c := heatTestCtx()
	return ansiSys.ReplaceAllString(
		heatStrip(c, vals, w, peak, heatConstInk(lipgloss.NewStyle())), "")
}

// TestHeatStripAbsoluteScale: with peak 1 a sample paints the rung its own value
// earns, whatever its neighbours did. This is the sys strips' contract - a
// machine at 50% must not be painted black because nothing else that minute went
// higher.
func TestHeatStripAbsoluteScale(t *testing.T) {
	alone := heatPlain([]float64{0.5}, 1, 1)
	withPeer := heatPlain([]float64{0.5, 0.05}, 2, 1)
	if got := string([]rune(withPeer)[0]); got != alone {
		t.Fatalf("0.5 painted %q beside a neighbour, %q alone", got, alone)
	}
	// The self-scaled reading of the same pair is deliberately different: 0.5 is
	// that series' own peak, so it reaches the top rung.
	self := heatPlain([]float64{0.5, 0.05}, 2, heatPeak([]float64{0.5, 0.05}))
	if string([]rune(self)[0]) == alone {
		t.Fatalf("self-scaled and absolute readings agree (%q); the scales are not distinct", self)
	}
	if got := string([]rune(self)[0]); got != "█" {
		t.Fatalf("a series' own peak = %q, want the top rung █", got)
	}
}

// TestHeatStripFullScaleIsHot: a 0.9 utilisation sample reaches the top of the
// ramp on the absolute scale, so a busy machine reads busy at a glance.
func TestHeatStripFullScaleIsHot(t *testing.T) {
	if got := heatPlain([]float64{0.9}, 1, 1); got != "█" {
		t.Fatalf("0.9 absolute = %q, want █", got)
	}
	if got := heatPlain([]float64{0.05}, 1, 1); got != "░" {
		t.Fatalf("0.05 absolute = %q, want the first rung ░", got)
	}
}

// TestHeatStripHoleVsZeroVsRung is the honesty ladder: a sample that was never
// taken is blank, a sample that exists and is zero is the track mark, and any
// non-zero sample reaches at least the first rung.
func TestHeatStripHoleVsZeroVsRung(t *testing.T) {
	got := heatPlain([]float64{0, 0.0001}, 5, 1)
	if want := "   ·░"; got != want {
		t.Fatalf("ladder = %q, want %q", got, want)
	}
}

// TestHeatStripWindowsNewest: more samples than cells shows the NEWEST w of
// them, dropping the oldest - the rolling window a btop-style strip is.
func TestHeatStripWindowsNewest(t *testing.T) {
	got := heatPlain([]float64{1, 0, 0, 0.9}, 3, 1)
	if want := "··█"; got != want {
		t.Fatalf("window = %q, want %q (oldest sample dropped)", got, want)
	}
}

// TestHeatStripExactWidth: the strip is always exactly w cells, full history or
// none, or the row it sits in would drift.
func TestHeatStripExactWidth(t *testing.T) {
	for _, n := range []int{0, 1, 5, 40} {
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = float64(i+1) / float64(n)
		}
		for w := 1; w <= 24; w++ {
			if got := lipgloss.Width(heatPlain(vals, w, 1)); got != w {
				t.Fatalf("n=%d w=%d: strip is %d cells", n, w, got)
			}
		}
	}
}

// TestHeatStripPeakless: a series that is entirely zero (or has no peak to scale
// against) is all track and no ink - never a full-black row from dividing by a
// zero peak.
func TestHeatStripPeakless(t *testing.T) {
	got := heatPlain([]float64{0, 0, 0}, 3, heatPeak([]float64{0, 0, 0}))
	if want := "···"; got != want {
		t.Fatalf("zero series = %q, want %q", got, want)
	}
}

// TestHeatRampSurvivesNoColor: the six rungs are distinguishable by GLYPH and
// SGR attribute alone, which is what keeps the strips readable with NO_COLOR set
// (two rungs share a glyph and differ by an attribute; none of them share both).
func TestHeatRampSurvivesNoColor(t *testing.T) {
	seen := map[string]bool{}
	for i, r := range heatRamp {
		k := string(r.glyph)
		if r.faint {
			k += "+faint"
		}
		if r.bold {
			k += "+bold"
		}
		if seen[k] {
			t.Fatalf("rung %d (%s) is indistinguishable from an earlier rung", i, k)
		}
		seen[k] = true
	}
}

// TestKPIHeatStripSelfScales: a KPI tile's row reads against its OWN maximum, so
// scaling every value by the same factor renders the identical row - the shape
// is the reading, not the magnitude, which the number above it already states.
func TestKPIHeatStripSelfScales(t *testing.T) {
	c := overviewTestCtx()
	d := overviewTestData()
	s := c.Comp[0]

	small := kpiHeatStrip(c, d, s, 20)
	scaled := d
	scaled.Timeline = make([]store.Bucket, len(d.Timeline))
	copy(scaled.Timeline, d.Timeline)
	for i := range scaled.Timeline {
		scaled.Timeline[i].Input *= 1000
		scaled.Timeline[i].Output *= 1000
		scaled.Timeline[i].CacheRead *= 1000
		scaled.Timeline[i].CacheCreation *= 1000
	}
	if big := kpiHeatStrip(c, scaled, s, 20); big != small {
		t.Fatalf("self-scaled row moved when the series was scaled 1000x:\n %q\n %q", small, big)
	}
	if plain := ansiSys.ReplaceAllString(small, ""); !strings.ContainsAny(plain, "░▒▓█·") {
		t.Fatalf("KPI row carries no heat glyphs: %q", plain)
	}
}

// TestKPIHeatStripMemoKeysOnWidth: the memo must not hand a 20-cell row back for
// a 24-cell tile. Width is part of the key, and the rendered row proves it.
func TestKPIHeatStripMemoKeysOnWidth(t *testing.T) {
	c := overviewTestCtx()
	d := overviewTestData()
	d.Gen, d.Memo = 1, NewHeroMemo()
	for _, w := range []int{12, 20, 24} {
		row := kpiHeatStrip(c, d, c.Comp[0], w)
		if got := lipgloss.Width(ansiSys.ReplaceAllString(row, "")); got != w {
			t.Fatalf("memoized row for w=%d is %d cells", w, got)
		}
	}
}
