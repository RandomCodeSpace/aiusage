package views

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

func TestCostDeltaUsesMoneyWithoutTotalBounds(t *testing.T) {
	c := heroTestCtx()
	c.Money = model.FormatCost
	c.Delta = func(int64, int64) (string, int) { return "token-delta", 0 }
	for _, tc := range []struct {
		cur, prev int64
		want      string
	}{
		{1_324_620_000, 2_000_000, "▲ $1322.62"},
		{2_000_000, 1_324_620_000, "▼ $1322.62"},
		{2_000_000, 2_000_000, "= $0.00"},
		{2_000_000, 0, "· —"},
	} {
		d := OverviewData{Totals: store.Bucket{Events: 2, UnpricedEvents: 1, CostMicroUSD: tc.cur}, Prev: store.Bucket{CostMicroUSD: tc.prev}}
		lay := ComputeLayout(260, 40)
		got := ansiHero.ReplaceAllString(overviewKPIs(c, d, lay, kpiBudget(lay)), "")
		if !strings.Contains(got, tc.want) {
			t.Errorf("cost delta missing %q:\n%s", tc.want, got)
		}
		if strings.Contains(got, "▲ ≥") || strings.Contains(got, "▼ ≥") {
			t.Error("delta inherited total lower bound")
		}
		for _, w := range []int{42, 60, 80, 120, 160} {
			lay = ComputeLayout(w, 40)
			for _, line := range strings.Split(overviewKPIs(c, d, lay, kpiBudget(lay)), "\n") {
				if lipgloss.Width(line) > lay.BodyW {
					t.Errorf("cost delta exceeded body width %d", lay.BodyW)
				}
			}
		}
	}
}

func TestAlphabeticalActivityCapNamesItsRanking(t *testing.T) {
	c := activityTestCtx()
	for _, pivot := range []string{"", string(model.DimensionAgent)} {
		d := turnContextTestData(model.DimensionAgent)
		if pivot == "" {
			d.Pivot = ""
			d.Rows = []store.ActivityBucket{{Keys: map[string]string{"name": "a"}, Calls: 2}, {Keys: map[string]string{"name": "b"}, Calls: 1}, {Keys: map[string]string{"name": "c"}, Calls: 1}}
		}
		d.OrderLbl = "name"
		d.Limit = 3
		unit := "calls"
		if pivot != "" {
			unit = "turns"
		}
		for _, w := range []int{42, 60, 80, 120, 160} {
			lay := ComputeLayout(w, 40)
			out := ansiHero.ReplaceAllString(Activity(c, d, lay), "")
			if strings.Contains(out, "by name") && !strings.Contains(out, "sorted by name") {
				t.Fatalf("alphabetical display claimed name-ranked rows:\n%s", out)
			}
			if !strings.Contains(out, "top 3") || !strings.Contains(out, unit) || !(strings.Contains(out, "name") || strings.Contains(out, "A–Z")) {
				t.Fatalf("capped title incomplete at %d:\n%s", w, out)
			}
		}
		d.Limit = 0
		if out := Activity(c, d, ComputeLayout(160, 40)); strings.Contains(out, "top 3") {
			t.Fatal("uncapped list claims a cap")
		}
	}
}
