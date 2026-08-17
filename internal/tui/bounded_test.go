package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/store"
)

// pricedSource is fakeData with cost qualifiers on every bucket: some rows
// unpriced (so every total is a floor) and some priced from a rate card (so
// every total is an estimate). It exists to drive the marks through the WHOLE
// dashboard rather than through one renderer.
type pricedSource struct{ fakeData }

func (s *pricedSource) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	sum, err := s.fakeData.Summarize(ctx, f)
	if err != nil {
		return nil, err
	}
	qualify := func(b *store.Bucket) {
		b.CostMicroUSD = 2_345_600
		b.UnpricedEvents = 3
		b.ComputedCostEvents = 4
	}
	out := *sum
	out.Buckets = append([]store.Bucket(nil), sum.Buckets...)
	for i := range out.Buckets {
		qualify(&out.Buckets[i])
	}
	qualify(&out.Totals)
	return &out, nil
}

// Every surface that renders a cost TOTAL bounds it. A floor presented as a
// total is the one lie the cost work exists to avoid, and it must not depend on
// which tab the reader is looking at.
func TestEveryCostTotalIsBounded(t *testing.T) {
	// Overview's cost tile is subject to a rule that predates this work: it may
	// appear only in the rows the KPI strip already occupies, so at 100 columns
	// there is no tile to mark. The widths below are the ones where each surface
	// actually renders a total.
	for _, tab := range []struct {
		key, name string
		widths    []int
	}{
		{"1", "Overview", []int{160, 200}},
		{"2", "By-Tool", []int{100, 160, 200}},
		{"3", "By-Model", []int{100, 160, 200}},
		{"4", "Sessions", []int{100, 160, 200}},
	} {
		for _, w := range tab.widths {
			m := newTestModelWH(t, &pricedSource{}, w, 44)
			m = step(t, m, keyMsg(tab.key))
			out := ansiFold.ReplaceAllString(m.View().Content, "")
			if !strings.Contains(out, "≥") {
				t.Errorf("%s at w=%d does not bound its cost total:\n%s", tab.name, w, out)
			}
			if !strings.Contains(out, "~") {
				t.Errorf("%s at w=%d does not mark its rate-card prices:\n%s", tab.name, w, out)
			}
		}
	}
}

// Where the Overview KPI strip cannot afford a cost tile it shows NONE, rather
// than an unmarked one. Losing the tile is a documented trade (the hero outranks
// it); losing only its marks would be a lie.
func TestOverviewDropsTheWholeTileNotItsMarks(t *testing.T) {
	m := newTestModelWH(t, &pricedSource{}, 100, 44)
	out := ansiFold.ReplaceAllString(m.View().Content, "")
	if strings.Contains(out, "spend") {
		t.Fatalf("the cost tile IS present at w=100; this test's premise is stale:\n%s", out)
	}
	if strings.Contains(out, "$") {
		t.Errorf("a dropped cost tile still left a bare amount on screen:\n%s", out)
	}
}

// The rider is the first thing to go as the pane narrows, and the BOUND is the
// last: a terminal that cannot fit "3 unpriced" must still say the number is a
// floor. This is the Activity footnote's width-ladder discipline applied to the
// cost tile.
func TestBoundSurvivesNarrowerThanItsRider(t *testing.T) {
	// By-Tool renders its total in the detail card, which exists at every width
	// that grants a side panel — so the ladder is observable across a real
	// range rather than only where the KPI strip can afford a tile.
	wide := newTestModelWH(t, &pricedSource{}, 200, 44)
	wide = step(t, wide, keyMsg("2"))
	narrow := newTestModelWH(t, &pricedSource{}, 90, 44)
	narrow = step(t, narrow, keyMsg("2"))

	w := ansiFold.ReplaceAllString(wide.View().Content, "")
	n := ansiFold.ReplaceAllString(narrow.View().Content, "")

	if !strings.Contains(w, "unpriced") {
		t.Errorf("a 200-column By-Tool does not name the unpriced count:\n%s", w)
	}
	if !strings.Contains(n, "≥") {
		t.Errorf("a 90-column By-Tool dropped the bound along with the rider:\n%s", n)
	}
}

// A window where everything is priced by the harness renders neither mark. The
// bare form has to stay reachable, or it stops meaning anything.
func TestExactWindowsCarryNoMarks(t *testing.T) {
	m := newTestModelWH(t, &exactSource{}, 200, 44)
	m = step(t, m, keyMsg("2"))
	out := ansiFold.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(out, "$2.35") {
		t.Fatalf("the cost total is missing:\n%s", out)
	}
	if strings.Contains(out, "≥") {
		t.Errorf("a fully priced window rendered a bound:\n%s", out)
	}
	if strings.Contains(out, "~$") {
		t.Errorf("a vendor-priced window rendered the estimate mark:\n%s", out)
	}
}

// exactSource prices every row, all of it vendor-reported.
type exactSource struct{ fakeData }

func (s *exactSource) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	sum, err := s.fakeData.Summarize(ctx, f)
	if err != nil {
		return nil, err
	}
	out := *sum
	out.Buckets = append([]store.Bucket(nil), sum.Buckets...)
	for i := range out.Buckets {
		out.Buckets[i].CostMicroUSD = 2_345_600
	}
	out.Totals.CostMicroUSD = 2_345_600
	return &out, nil
}
