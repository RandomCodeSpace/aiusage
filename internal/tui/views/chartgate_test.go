package views

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

// chartgate_test.go holds the width gate for a built hero chart (issue #48).
// The gate is charged EXACTLY ONCE, on the post-chrome inner width, and its
// value is measured rather than chosen: the tests below pin both halves.

// TestChartWidthGateIsChargedOnce: no terminal width may pass ComputeLayout's
// column gate and then be refused by the hero's own gate. The two are derived
// from one constant (minChartInnerW), so a column wide enough for ChartFull
// always leaves an inner width wide enough for a frame - before, ComputeLayout
// granted ChartFull on MainW >= 48 and heroFrameFor re-tested the SAME 48
// against the width left after the card's 4 columns of chrome, so 75..80 column
// terminals (80 among them, the classic size) got no chart at any height.
func TestChartWidthGateIsChargedOnce(t *testing.T) {
	for w := MinUsableW; w <= 260; w++ {
		for _, h := range []int{MinUsableH, 18, 22, 30, 44, 60} {
			lay := ComputeLayout(w, h)
			if lay.TooSmall || lay.ChartMode != ChartFull {
				continue
			}
			col := lay.MainW
			if !lay.SidePanel {
				col = lay.BodyW
			}
			inner := col - cardChromeW
			for _, mode := range []HeroMode{HeroTrend, HeroLeverage} {
				// A body tall enough for every kind: only the width is on trial.
				if heroFrameFor(mode, lay, inner, minHeroTwoPaneH) == heroFrameNone {
					t.Fatalf("%dx%d mode=%d: the layout grants ChartFull on a %d-column pane but the hero refuses the %d columns the card chrome leaves",
						w, h, mode, col, inner)
				}
			}
		}
	}
}

// TestChartColumnGateIsDerivedFromTheInnerOne pins the derivation itself: the
// column gate is the inner gate plus the card chrome, never a second number
// that happens to agree.
func TestChartColumnGateIsDerivedFromTheInnerOne(t *testing.T) {
	if minChartW != minChartInnerW+cardChromeW {
		t.Fatalf("minChartW = %d, want minChartInnerW+cardChromeW = %d", minChartW, minChartInnerW+cardChromeW)
	}
	var l Layout
	l.MainW = minChartW
	l.applyHeightFlags(minChartH)
	if l.ChartMode != ChartFull {
		t.Fatalf("a %d-column pane does not get ChartFull; the column gate is not the inner gate plus chrome", minChartW)
	}
	l.MainW = minChartW - 1
	l.applyHeightFlags(minChartH)
	if l.ChartMode == ChartFull {
		t.Fatalf("a %d-column pane still gets ChartFull; the width gate no longer degrades", minChartW-1)
	}
}

// gateDate matches one complete axis label ("07/08").
var gateDate = regexp.MustCompile(`\d\d/\d\d`)
var gateDigit = regexp.MustCompile(`\d`)

// chartReads reports why a rendered hero body does NOT read at (w,h), or "" when
// it does. Everything it checks survives an SGR strip, which is the channel the
// gate has to hold: the pane must state its own scale in words, label its time
// axis without cutting a date in half, plot braille, and fit its box.
func chartReads(out string, w, h int) string {
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		return fmt.Sprintf("%d lines, want %d", len(lines), h)
	}
	for i, ln := range lines {
		if lw := lipgloss.Width(ln); lw > w {
			return fmt.Sprintf("line %d is %d cells wide, want <= %d", i, lw, w)
		}
	}
	heads := 0
	for _, ln := range lines {
		if !strings.Contains(ln, "tokens") && !strings.Contains(ln, "fresh") &&
			!strings.Contains(ln, "cache ") && !strings.Contains(ln, "leverage") {
			continue
		}
		heads++
		// A pane states its own scale in words. The braille bodies say it as a
		// SCALE readout per division; a heat lane says it as the peak its
		// intensities are read against. Both survive an SGR strip, and neither
		// pane kind may drop its own.
		if strings.Contains(ln, "max ") {
			continue
		}
		if !strings.Contains(ln, "SCALE ") || !strings.Contains(ln, "/div") {
			return fmt.Sprintf("pane header %q drops its scale readout", ln)
		}
	}
	if heads == 0 {
		return "no pane names itself"
	}
	axis := ""
	for i, ln := range lines {
		if i+1 >= len(lines) {
			continue
		}
		if strings.Contains(ln, "└") || isRuleRow(ln) {
			axis = lines[i+1]
		}
	}
	if axis == "" {
		return "no x axis label row"
	}
	if n := len(gateDate.FindAllString(axis, -1)); n < 2 {
		return fmt.Sprintf("x axis carries %d complete labels: %q", n, axis)
	}
	if gateDigit.MatchString(gateDate.ReplaceAllString(axis, "")) {
		return fmt.Sprintf("x axis carries a cut label: %q", axis)
	}
	if !hasBraille(out) && !hasHeatInk(out) {
		return "nothing plotted"
	}
	return ""
}

// isRuleRow reports whether ln is nothing but a horizontal hairline. It is how
// the heat lanes' x axis is found: with no Y gutter the pane has no origin
// corner, so the axis is a bare rule row rather than a "└"-anchored one.
func isRuleRow(ln string) bool {
	ln = strings.TrimRight(ln, " ")
	return ln != "" && strings.Trim(ln, "─") == ""
}

// hasHeatInk reports whether s carries a rung of the intensity ramp — the heat
// lanes' equivalent of braille, and what "something was plotted" means on them.
func hasHeatInk(s string) bool {
	for _, r := range s {
		if isHeatInk(r) {
			return true
		}
	}
	return false
}

// gateBuckets builds daily buckets whose magnitudes scale with mul, so the same
// series can be handed to the renderer as a light user (short ring labels) or as
// a fleet (a 10^12 decade pitch, the widest SCALE readout the band can print).
func gateBuckets(n int, mul int64) []store.Bucket {
	base := time.Date(2026, 7, 8, 0, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 0, n)
	for i := 0; i < n; i++ {
		in := int64(2_000_000+200_000*(i%7)) * mul
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

// TestChartWidthFloorIsMeasured is where minChartInnerW's VALUE comes from. At
// the constant every built hero kind still reads with SGR stripped; one column
// narrower at least one of them stops reading (the decade band's header can no
// longer carry both the pane name and its "SCALE 10^N/div" readout, which on a
// log axis is the only thing that states magnitude at all). Lower the constant
// and the first half fails; raise it and the second half fails.
func TestChartWidthFloorIsMeasured(t *testing.T) {
	c := heroTestCtx()
	sets := map[string][]store.Bucket{
		"light": gateBuckets(30, 1),
		"fleet": gateBuckets(30, 20_000),
	}
	kinds := []struct {
		name   string
		kind   heroFrameKind
		bodies []int
	}{
		{"decade ring", heroFrameLog, []int{minHeroLogH, 6, 8, 10, minHeroTwoPaneH - 1}},
		{"two pane", heroFrameTwoPane, []int{minHeroTwoPaneH, 16, 22}},
		{"leverage", heroFrameLeverage, []int{minHeroPivotH, 10, 14}},
	}

	for _, kd := range kinds {
		for name, buckets := range sets {
			for _, h := range kd.bodies {
				f, ok := buildHeroFrame(c, buckets, "day", kd.kind, minChartInnerW, h, nil)
				if !ok {
					t.Fatalf("%s/%s: no frame built at the measured floor %dx%d", kd.name, name, minChartInnerW, h)
				}
				out := ansiHero.ReplaceAllString(f.render(c, -1), "")
				if why := chartReads(out, minChartInnerW, h); why != "" {
					t.Fatalf("%s/%s at the floor (%dx%d): %s\n%s", kd.name, name, minChartInnerW, h, why, out)
				}
			}
		}
	}

	// One column narrower must break something, or the floor is above the
	// measurement and terminals are being denied a chart they could read.
	narrow := minChartInnerW - 1
	broke := ""
	for _, kd := range kinds {
		for name, buckets := range sets {
			for _, h := range kd.bodies {
				f, ok := buildHeroFrame(c, buckets, "day", kd.kind, narrow, h, nil)
				if !ok {
					broke = fmt.Sprintf("%s/%s at %dx%d: no frame builds", kd.name, name, narrow, h)
					continue
				}
				out := ansiHero.ReplaceAllString(f.render(c, -1), "")
				if why := chartReads(out, narrow, h); why != "" {
					broke = fmt.Sprintf("%s/%s at %dx%d: %s", kd.name, name, narrow, h, why)
				}
			}
		}
	}
	if broke == "" {
		t.Fatalf("every hero kind still reads at %d columns, so minChartInnerW=%d is taste, not the measured floor",
			narrow, minChartInnerW)
	}
	t.Logf("floor confirmed: at %d columns %s", narrow, broke)
}
