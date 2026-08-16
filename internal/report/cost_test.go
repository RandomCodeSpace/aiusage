package report

import (
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/pricing"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// fixedPricer values every charge at one micro-USD per token, except for the
// models named in miss, which it refuses to price.
type fixedPricer struct {
	miss map[string]bool
}

func (p fixedPricer) Price(c pricing.Charge) (int64, string, bool) {
	if p.miss[c.Model] {
		return 0, "", false
	}
	tokens := c.Tokens()
	if !c.AdditiveReasoning {
		tokens -= c.Reasoning
	}
	return tokens, "embedded-test", true
}

// TestCostStringCopy pins the display copy: exact costs get a plain dollar
// amount, display-priced ones a tilde, and an unpriced bucket a dash — never
// "$0.00", which would read as a free request.
func TestCostStringCopy(t *testing.T) {
	cases := []struct {
		name string
		cost Cost
		want string
	}{
		{"exact", Cost{MicroUSD: 1_230_000, Known: true}, "$1.23"},
		{"approximate", Cost{MicroUSD: 1_230_000, Known: true, Approximate: true}, "~$1.23"},
		{"unpriced", Cost{}, "-"},
		{"genuinely zero", Cost{MicroUSD: 0, Known: true}, "$0.00"},
		{"sub-cent widens", Cost{MicroUSD: 3400, Known: true}, "$0.0034"},
		{"sub-cent approximate", Cost{MicroUSD: 3400, Known: true, Approximate: true}, "~$0.0034"},
		{"below four decimals", Cost{MicroUSD: 42, Known: true}, "<$0.0001"},
		{"cent boundary", Cost{MicroUSD: 10_000, Known: true}, "$0.01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cost.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveCostsFoldsUnpricedIntoBuckets checks the display fallback lands in
// the right bucket, marks only that bucket approximate, and propagates the
// tilde to the total.
func TestResolveCostsFoldsUnpricedIntoBuckets(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{Keys: map[string]string{"tool": model.ToolClaudeCode}, Events: 2, CostMicroUSD: 5_000},
			{Keys: map[string]string{"tool": model.ToolCodex}, Events: 3, CostMicroUSD: 1_000, UnpricedEvents: 1},
		},
		Totals: store.Bucket{Events: 5, CostMicroUSD: 6_000, UnpricedEvents: 1},
	}
	groups := []store.UnpricedGroup{{
		Keys:  map[string]string{"tool": model.ToolCodex},
		Tool:  model.ToolCodex,
		Model: "gpt-5",
		Input: 400,
	}}

	groups[0].Events = 1

	costs := ResolveCosts(sum, groups, fixedPricer{})
	if got := costs.Buckets[0]; got.MicroUSD != 5_000 || got.Approximate || !got.Known {
		t.Errorf("fully-stamped bucket = %+v, want exact 5000", got)
	}
	if got := costs.Buckets[1]; got.MicroUSD != 1_400 || !got.Approximate {
		t.Errorf("mixed bucket = %+v, want approximate 1400", got)
	}
	if got := costs.Totals; got.MicroUSD != 6_400 || !got.Approximate {
		t.Errorf("totals = %+v, want approximate 6400 (the tilde is inherited)", got)
	}
	if costs.Buckets[0].String() != "$0.0050" || costs.Buckets[1].String() != "~$0.0014" {
		t.Errorf("rendered = %q / %q", costs.Buckets[0].String(), costs.Buckets[1].String())
	}
}

// TestResolveCostsUnpriceableStaysDash proves a bucket the ladder cannot price
// at all renders as unpriced instead of collapsing to zero dollars.
func TestResolveCostsUnpriceableStaysDash(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{Keys: map[string]string{"tool": model.ToolCopilot}, Events: 4, UnpricedEvents: 4},
		},
		Totals: store.Bucket{Events: 4, UnpricedEvents: 4},
	}
	groups := []store.UnpricedGroup{{
		Keys:  map[string]string{"tool": model.ToolCopilot},
		Tool:  model.ToolCopilot,
		Model: "mystery-model",
		Input: 9_999,
	}}

	costs := ResolveCosts(sum, groups, fixedPricer{miss: map[string]bool{"mystery-model": true}})
	if got := costs.Buckets[0].String(); got != "-" {
		t.Errorf("unpriceable bucket = %q, want %q", got, model.UnpricedMark)
	}
	if got := costs.Totals.String(); got != "-" {
		t.Errorf("unpriceable total = %q, want %q", got, model.UnpricedMark)
	}
}

// TestUnpricedGroupIsNeverBilledAsOneLongRequest pins the aggregate rule at the
// seam that needs it. UnpricedGroups arrive pre-aggregated per model, so a
// group's token counts are a SUM over many requests, not a prompt size: without
// the aggregate marker, a thousand short turns add up past the model's
// long-context threshold and the whole group is billed off the second rate card.
func TestUnpricedGroupIsNeverBilledAsOneLongRequest(t *testing.T) {
	const threshold = 272_000
	rates := pricing.Rates{
		Input:  5e-06,
		Output: 3e-05,
		Long:   pricing.LongContext{Threshold: threshold, Input: 1e-05, Output: 4.5e-05},
	}
	e := pricing.New(pricing.Options{Overrides: map[string]pricing.Rates{"gpt-5.6-sol": rates}})

	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{{
			Keys: map[string]string{"tool": model.ToolOpenCode}, Events: 1000, UnpricedEvents: 1000,
		}},
		Totals: store.Bucket{Events: 1000, UnpricedEvents: 1000},
	}
	// 1,000 requests of 1,000 input tokens each: not one of them is long, and
	// their sum is nearly four times the threshold.
	groups := []store.UnpricedGroup{{
		Keys:   map[string]string{"tool": model.ToolOpenCode},
		Tool:   model.ToolOpenCode,
		Model:  "gpt-5.6-sol",
		Events: 1000,
		Input:  1_000_000,
	}}
	if groups[0].Input <= threshold {
		t.Fatal("fixture does not sum past the threshold, so it proves nothing")
	}

	const (
		base = 5_000_000 // 1e6 tokens at 5e-06
		long = 10_000_000
	)
	costs := ResolveCosts(sum, groups, e)
	if got := costs.Totals.MicroUSD; got == long {
		t.Fatalf("group cost = %d: a summed group was billed as one long-context request", got)
	}
	if got := costs.Totals.MicroUSD; got != base {
		t.Errorf("group cost = %d, want %d", got, base)
	}
}

// TestResolveCostsUnvaluedRemainderIsApproximate covers the honesty gap between
// "exact" and "unpriced": a bucket whose stamped rows are exact but which still
// holds rows nothing could value is a FLOOR, so it must wear the tilde rather
// than advertise a precise total it does not have.
func TestResolveCostsUnvaluedRemainderIsApproximate(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{Keys: map[string]string{"tool": model.ToolClaudeCode}, Events: 2, CostMicroUSD: 3_000_000, UnpricedEvents: 1},
		},
		Totals: store.Bucket{Events: 2, CostMicroUSD: 3_000_000, UnpricedEvents: 1},
	}
	groups := []store.UnpricedGroup{{
		Keys:   map[string]string{"tool": model.ToolClaudeCode},
		Tool:   model.ToolClaudeCode,
		Model:  "mystery-model",
		Events: 1,
		Input:  500,
	}}

	costs := ResolveCosts(sum, groups, fixedPricer{miss: map[string]bool{"mystery-model": true}})
	if got := costs.Buckets[0]; got.MicroUSD != 3_000_000 || !got.Approximate || !got.Known {
		t.Errorf("bucket = %+v, want an approximate 3000000 floor", got)
	}
	if got := costs.Buckets[0].String(); got != "~$3.00" {
		t.Errorf("rendered = %q, want ~$3.00", got)
	}
	if got := costs.Totals.String(); got != "~$3.00" {
		t.Errorf("totals = %q, want ~$3.00", got)
	}
}

// TestResolveCostsUngrouped covers the single-bucket case, where the grouping
// key of both the bucket and the unpriced group is empty.
func TestResolveCostsUngrouped(t *testing.T) {
	sum := &store.Summary{
		Buckets: []store.Bucket{{Events: 2, CostMicroUSD: 100, UnpricedEvents: 1}},
		Totals:  store.Bucket{Events: 2, CostMicroUSD: 100, UnpricedEvents: 1},
	}
	groups := []store.UnpricedGroup{{Tool: model.ToolCodex, Model: "gpt-5", Output: 50}}

	costs := ResolveCosts(sum, groups, fixedPricer{})
	if costs.Buckets[0].MicroUSD != 150 || !costs.Buckets[0].Approximate {
		t.Errorf("ungrouped bucket = %+v, want approximate 150", costs.Buckets[0])
	}
}

// TestResolveCostsNilPricerKeepsStamped checks the degraded path: with no
// pricer the stamped costs still render, exact and un-tilded.
func TestResolveCostsNilPricerKeepsStamped(t *testing.T) {
	sum := &store.Summary{
		Buckets: []store.Bucket{{Events: 1, CostMicroUSD: 2_500_000}},
		Totals:  store.Bucket{Events: 1, CostMicroUSD: 2_500_000},
	}
	costs := ResolveCosts(sum, nil, nil)
	if got := costs.Totals.String(); got != "$2.50" {
		t.Errorf("totals = %q, want $2.50", got)
	}
}

// TestRenderTableCostColumn checks the Cost column appears only when costs are
// supplied, stays right-aligned with the other metrics, and carries the totals
// row's own value.
func TestRenderTableCostColumn(t *testing.T) {
	sum := sampleSummary()
	sum.Buckets[0].Events = 1200
	sum.Totals.Events = 1242

	plain := RenderTable(sum, Opt{})
	if strings.Contains(plain, colCost) {
		t.Errorf("Cost column rendered without Opt.Costs\n%s", plain)
	}

	costs := &Costs{
		Buckets: []Cost{
			{MicroUSD: 1_500_000, Known: true},
			{MicroUSD: 250_000, Known: true, Approximate: true},
		},
		Totals: Cost{MicroUSD: 1_750_000, Known: true, Approximate: true},
	}
	out := RenderTable(sum, Opt{Costs: costs})
	for _, want := range []string{colCost, "$1.50", "~$0.25", "~$1.75"} {
		if !strings.Contains(out, want) {
			t.Errorf("cost table missing %q\n%s", want, out)
		}
	}

	// Every row keeps the same column count as the header.
	lines := strings.Split(out, "\n")
	want := len(strings.Fields(lines[0]))
	for i, l := range lines {
		if strings.HasPrefix(l, "-") || strings.TrimSpace(l) == "" {
			continue
		}
		if got := len(strings.Fields(l)); got != want {
			t.Errorf("line %d has %d fields, want %d: %q", i, got, want, l)
		}
	}
}

// TestRenderTableCostUngrouped covers the layout branch that has no grouping
// columns: a label column is prepended after the cost column was appended, so
// the alignment maths must still land on the right cell.
func TestRenderTableCostUngrouped(t *testing.T) {
	sum := &store.Summary{
		Buckets: []store.Bucket{{Events: 3, Input: 10, Output: 20, Total: 30}},
		Totals:  store.Bucket{Events: 3, Input: 10, Output: 20, Total: 30},
	}
	costs := &Costs{
		Buckets: []Cost{{MicroUSD: 990_000, Known: true}},
		Totals:  Cost{MicroUSD: 990_000, Known: true},
	}
	out := RenderTable(sum, Opt{Costs: costs})

	// The ungrouped layout prepends an unnamed label column, so cell counts are
	// only comparable via the trailing cost cell: every content line must end in
	// its own cost, and the header in the column name.
	lines := strings.Split(out, "\n")
	if got := strings.TrimSpace(lines[0]); !strings.HasSuffix(got, colCost) {
		t.Errorf("header does not end with the Cost column: %q", got)
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "-") || strings.TrimSpace(l) == "" || i == 0 {
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(l), "$0.99") {
			t.Errorf("line %d does not end with the cost cell: %q", i, l)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), totalsLabel) {
		t.Errorf("last line is not the totals row: %q", lines[len(lines)-1])
	}
}

// TestRenderTableCostShorterThanBuckets guards the render against a caller
// mismatch: a short cost slice must degrade to the unpriced marker, not panic.
func TestRenderTableCostShorterThanBuckets(t *testing.T) {
	out := RenderTable(sampleSummary(), Opt{Costs: &Costs{Buckets: nil, Totals: Cost{}}})
	if !strings.Contains(out, model.UnpricedMark) {
		t.Errorf("short cost slice did not render the unpriced marker\n%s", out)
	}
}
