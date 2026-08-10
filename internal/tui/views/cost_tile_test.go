package views

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// wide is a roomy layout: these tests are about the cost tile's fit rule, not
// about the strip's row budget, so every case runs where the budget is slack.
var wide = ComputeLayout(160, 40)

// kpiBudget is the row budget Overview hands the strip in a body with no sys
// gauges and a side panel: everything the hero's reserved floor and the
// breathing row do not claim (issue #48).
func kpiBudget(lay Layout) int { return lay.BodyH - heroReserveH - minHeroPanelH }

// The cost tile is the answer to "I cannot see cost anywhere": schema v3 stamps
// a cost on every event and the store hands it to the dashboard, which until now
// rendered nothing. It must show up where there is room for it.
func TestKPIStripShowsCostWhenItFits(t *testing.T) {
	c := heroTestCtx()
	c.Money = model.FormatCost
	d := OverviewData{Totals: store.Bucket{Total: 100, Events: 2, CostMicroUSD: 1_234_500}}

	got := ansiHero.ReplaceAllString(overviewKPIs(c, d, wide, kpiBudget(wide)), "")
	if !strings.Contains(got, "cost") {
		t.Errorf("wide strip has no cost tile:\n%s", got)
	}
	if !strings.Contains(got, "$1.23") {
		t.Errorf("wide strip does not render the stamped cost $1.23:\n%s", got)
	}
}

// A range holding rows nothing could price understates the bill. The tile says
// so — the figure carries the approximate mark and the foot names it partial —
// because a floor presented as a total is the one lie the cost work exists to
// avoid.
func TestKPICostMarksPartialRange(t *testing.T) {
	c := heroTestCtx()
	c.Money = model.FormatCost
	d := OverviewData{Totals: store.Bucket{Total: 100, Events: 5, CostMicroUSD: 2_000_000, UnpricedEvents: 3}}

	got := ansiHero.ReplaceAllString(overviewKPIs(c, d, wide, kpiBudget(wide)), "")
	if !strings.Contains(got, "~$2.00") {
		t.Errorf("a range with unpriced rows must render the approximate mark:\n%s", got)
	}
	if !strings.Contains(got, "partial") {
		t.Errorf("a range with unpriced rows must say so in the tile foot:\n%s", got)
	}
}

// Nothing in range could be priced: the tile shows the unpriced mark rather than
// a zero, which would read as "you spent nothing".
func TestKPICostNeverShowsAFalseZero(t *testing.T) {
	c := heroTestCtx()
	c.Money = model.FormatCost
	d := OverviewData{Totals: store.Bucket{Total: 100, Events: 4, CostMicroUSD: 0, UnpricedEvents: 4}}

	got := ansiHero.ReplaceAllString(overviewKPIs(c, d, wide, kpiBudget(wide)), "")
	if strings.Contains(got, "$0.00") {
		t.Errorf("an entirely unpriced range rendered $0.00; a missing price is not a free request:\n%s", got)
	}
}

// The hero is why the screen exists. The cost tile is allowed to appear only in
// the rows the strip already occupies — at 90 columns a sixth tile wraps and
// takes five rows off the chart, which is too much to pay for a number that
// `summary` prints. This pins both halves: no growth anywhere, and the tile
// present wherever there is slack.
func TestKPICostNeverCostsTheHeroARow(t *testing.T) {
	c := heroTestCtx()
	d := OverviewData{Totals: store.Bucket{Total: 100, Events: 2, CostMicroUSD: 1_000_000}}

	sawTile := false
	for _, w := range []int{60, 80, 90, 100, 120, 140, 160, 200} {
		lay := ComputeLayout(w, 40)

		c.Money = nil
		without := lipgloss.Height(overviewKPIs(c, d, lay, kpiBudget(lay)))
		c.Money = model.FormatCost
		strip := overviewKPIs(c, d, lay, kpiBudget(lay))
		with := lipgloss.Height(strip)

		if with != without {
			t.Errorf("w=%d: strip grew %d -> %d rows for the cost tile; the hero pays for it",
				w, without, with)
		}
		if strings.Contains(ansiHero.ReplaceAllString(strip, ""), "cost") {
			sawTile = true
		}
	}
	if !sawTile {
		t.Error("the cost tile never appeared at any width; the fit rule is too strict to be useful")
	}
}
