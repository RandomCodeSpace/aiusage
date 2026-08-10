package views

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// herofloor_test.go covers the SECOND gate of issue #48: the vertical one. The
// KPI strip used to claim its rows first and hand the hero the remainder, so a
// short body left the chart under its own floor however wide the terminal was.
// The hero now reserves minHeroPanelH and the strip fills what is left.

// overviewTestCtx is heroTestCtx plus the helpers the KPI tiles and the by-tool
// side panel call (a nil func field would panic instead of failing an assertion).
func overviewTestCtx() Ctx {
	c := heroTestCtx()
	c.Percent = func(v, tot int64) string { return "50%" }
	c.Delta = func(cur, prev int64) (string, int) { return "= 0", 0 }
	c.ToolAccent = func(string) compat.AdaptiveColor {
		return compat.AdaptiveColor{Light: lipgloss.Color("5"), Dark: lipgloss.Color("5")}
	}
	c.ToolGlyph = func(string) string { return "◆" }
	return c
}

// overviewTestData is a loaded Overview: a real timeline for the hero plus the
// totals and by-tool rows the strip and the side panel price.
func overviewTestData() OverviewData {
	d := heroTestData(HeroTrend)
	d.Totals = store.Bucket{Input: 2_000_000, Output: 700_000, CacheRead: 300_000_000,
		Total: 302_700_000, Events: 120}
	d.Prev = store.Bucket{Input: 1_000_000, Output: 400_000, CacheRead: 150_000_000,
		Total: 151_400_000, Events: 60}
	d.ByTool = []store.Bucket{
		{Keys: map[string]string{"tool": "claude-code"}, Input: 1_500_000, CacheRead: 200_000_000, Total: 201_500_000},
		{Keys: map[string]string{"tool": "codex"}, Input: 500_000, CacheRead: 100_000_000, Total: 101_200_000},
	}
	d.RangeLbl = "7d"
	return d
}

// overviewFloorLayout is a body of exactly h rows on a width whose KPI strip
// naturally wants more than one tile row - which is the only case where the
// yield is observable.
func overviewFloorLayout(h int) Layout { return ComputeLayout(80, 60).WithBodyHeight(h) }

// TestKPIStripYieldsRowsToTheHero: on a body that cannot carry both, the strip
// drops trailing tile rows so the hero keeps its floor. This is the rule that
// already forbade the cost tile from costing the hero a row, applied to the
// whole strip.
func TestKPIStripYieldsRowsToTheHero(t *testing.T) {
	c, d := overviewTestCtx(), overviewTestData()
	lay := overviewFloorLayout(minHeroBodyH)

	free := overviewKPIs(c, d, lay, 1<<20)
	if lipgloss.Height(free) <= kpiTileH {
		t.Fatalf("pick a width whose strip wants more than one tile row: %d rows unbudgeted", lipgloss.Height(free))
	}
	held := overviewKPIs(c, d, lay, kpiBudget(lay))
	if lipgloss.Height(held) >= lipgloss.Height(free) {
		t.Fatalf("the strip claimed all %d of its rows on a %d-row body; the hero pays for them",
			lipgloss.Height(free), lay.BodyH)
	}
	// It yields rows, it does not vanish: the component tiles are the strip's floor.
	mono := ansiHero.ReplaceAllString(held, "")
	for _, want := range []string{"input", "output", "cache"} {
		if !strings.Contains(mono, want) {
			t.Fatalf("the strip yielded its %q tile as well:\n%s", want, mono)
		}
	}

	out := ansiHero.ReplaceAllString(Overview(c, d, lay), "")
	if !strings.Contains(out, "SCALE ") {
		t.Fatalf("the hero has no built chart on a %d-row body after the strip yielded:\n%s", lay.BodyH, out)
	}
}

// TestOverviewHeroFloorIsPinned holds the documented floor in place: at
// minHeroBodyH the hero still builds a chart, one row under it the hero is a
// strip. The floor is a stated constant (one tile row + the reserve row +
// minHeroPanelH), not whatever the arithmetic happens to leave.
func TestOverviewHeroFloorIsPinned(t *testing.T) {
	c, d := overviewTestCtx(), overviewTestData()
	for _, tc := range []struct {
		h    int
		want bool
	}{{minHeroBodyH, true}, {minHeroBodyH - 1, false}} {
		lay := overviewFloorLayout(tc.h)
		out := ansiHero.ReplaceAllString(Overview(c, d, lay), "")
		if got := strings.Contains(out, "SCALE "); got != tc.want {
			verb := "builds no chart"
			if got {
				verb = "builds a chart"
			}
			t.Fatalf("a %d-row body %s; minHeroBodyH says %v (floor = %d)\n%s",
				tc.h, verb, tc.want, minHeroBodyH, out)
		}
	}
}

// TestOverviewNarrowBodyFitsItsBudget: on a body with no side panel the compact
// by-tool card sits UNDER the hero, so its rows are now part of the hero's
// budget instead of an overflow the frame clamp cuts off. That matters here
// because the hero can grow into a chart at these widths, which would otherwise
// have pushed the card off the bottom entirely.
//
// The matrix starts one row under the hero floor. Below that the composition's
// own minimums (one tile row, the hero's three-row minimum, the by-tool card)
// exceed any body, and the frame clamp in package tui bounds the render - as it
// did before this change. Wide bodies are excluded for a different pre-existing
// reason: the by-tool SIDE panel sizes itself to its content, so a short body
// overflows on the right column no matter what the hero does.
func TestOverviewNarrowBodyFitsItsBudget(t *testing.T) {
	c, d := overviewTestCtx(), overviewTestData()
	for _, w := range []int{44, 56, 60, 70} {
		for _, h := range []int{minHeroBodyH - 1, minHeroBodyH, 18, 24, 36} {
			lay := ComputeLayout(w, 60).WithBodyHeight(h)
			if lay.SidePanel {
				t.Fatalf("%d columns grew a side panel; pick a narrower width", w)
			}
			out := Overview(c, d, lay)
			if got := lipgloss.Height(out); got > lay.BodyH {
				t.Fatalf("%dx%d: Overview rendered %d rows into a %d-row body",
					w, h, got, lay.BodyH)
			}
			for i, ln := range strings.Split(out, "\n") {
				if lw := lipgloss.Width(ln); lw > lay.BodyW {
					t.Fatalf("%dx%d: line %d is %d cells wide, body is %d", w, h, i, lw, lay.BodyW)
				}
			}
		}
	}
}

// TestKPITileHeightMatchesTheBudget keeps kpiTileH honest: it is what the strip
// is charged for one tile row and therefore the floor minHeroBodyH is built on,
// so it may not drift from what kpiTile renders.
func TestKPITileHeightMatchesTheBudget(t *testing.T) {
	c := overviewTestCtx()
	tile := kpiTile(c, kpiSpec{label: "input", foot: "tokens", value: 5, prev: 4,
		spark: "▁▂▃▄", shareVal: 1, shareTot: 2}, 18)
	if got := lipgloss.Height(tile); got != kpiTileH {
		t.Fatalf("a KPI tile renders %d rows, kpiTileH budgets %d", got, kpiTileH)
	}
}
