package report

import (
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// breakdownSummary groups two tools that account for reasoning differently:
// claude-code keeps it INSIDE output (subset), opencode reports it alongside
// (additive). Each row's components sum to its Total under its own rule.
func breakdownSummary() *store.Summary {
	return &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{
				Keys:          map[string]string{"tool": model.ToolClaudeCode},
				OrderedKeys:   []string{"tool"},
				Events:        10,
				Input:         1000,
				Output:        400, // includes the 150 reasoning tokens
				Reasoning:     150,
				CacheCreation: 200,
				CacheRead:     300,
				Total:         1900, // 1000 + 400 + 200 + 300
			},
			{
				Keys:          map[string]string{"tool": model.ToolOpenCode},
				OrderedKeys:   []string{"tool"},
				Events:        4,
				Input:         500,
				Output:        60,
				Reasoning:     90, // alongside output
				CacheCreation: 10,
				CacheRead:     20,
				Total:         680, // 500 + 60 + 90 + 10 + 20
			},
		},
		Totals: store.Bucket{
			Events:        14,
			Input:         1500,
			Output:        460,
			Reasoning:     240,
			CacheCreation: 210,
			CacheRead:     320,
			Total:         2580,
		},
	}
}

// cell returns the whitespace-split cells of the row whose first cell is label.
func cell(t *testing.T, out, label string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == label {
			return fields
		}
	}
	t.Fatalf("row %q not found in:\n%s", label, out)
	return nil
}

// TestRenderTableBreakdownEmitsComponentColumns: --breakdown replaces the
// combined Cache column with the stored components.
func TestRenderTableBreakdownEmitsComponentColumns(t *testing.T) {
	out := RenderTable(breakdownSummary(), Opt{Breakdown: true})

	for _, want := range []string{colInput, colOutput, colReasoning, colCacheW, colCacheR, colTotal} {
		if !strings.Contains(out, want) {
			t.Errorf("breakdown table missing %q column\n%s", want, out)
		}
	}
	header := cell(t, out, "tool")
	for _, unwanted := range []string{colCache} {
		for _, h := range header {
			if h == unwanted {
				t.Errorf("breakdown kept the combined %q column\n%s", unwanted, out)
			}
		}
	}

	// tool Events Input Output Reasoning CacheW CacheR Total
	row := cell(t, out, model.ToolOpenCode)
	if len(row) != 8 {
		t.Fatalf("breakdown row has %d cells, want 8: %v", len(row), row)
	}
	if row[2] != "500" || row[3] != "60" || row[5] != "10" || row[6] != "20" || row[7] != "680" {
		t.Errorf("breakdown components misplaced: %v\n%s", row, out)
	}
}

// TestRenderTableBreakdownMarksReasoningPerTool pins the schema-v3 accounting
// rule: a reasoning cell says which convention its row was rendered under, and
// the legend spells out how that row reconciles to its Total. Mixed rows never
// claim one convention for both.
func TestRenderTableBreakdownMarksReasoningPerTool(t *testing.T) {
	out := RenderTable(breakdownSummary(), Opt{Breakdown: true})

	subset := cell(t, out, model.ToolClaudeCode)
	if got := subset[4]; got != "150"+markSubset {
		t.Errorf("subset-mode reasoning cell = %q, want %q\n%s", got, "150"+markSubset, out)
	}
	additive := cell(t, out, model.ToolOpenCode)
	if got := additive[4]; got != "90"+markAdditive {
		t.Errorf("additive-mode reasoning cell = %q, want %q\n%s", got, "90"+markAdditive, out)
	}
	totals := cell(t, out, totalsLabel)
	if got := totals[4]; got != "240"+markUnknown {
		t.Errorf("mixed TOTAL reasoning cell = %q, want %q\n%s", got, "240"+markUnknown, out)
	}

	for _, want := range []string{
		markSubset + " reasoning is inside Output",
		markAdditive + " reasoning is alongside Output",
		markUnknown + " reasoning rules differ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("legend missing %q\n%s", want, out)
		}
	}
}

// TestRenderTableBreakdownUniformToolMarksTotals: when every contributing row
// shares one convention, the TOTAL row carries it too instead of degrading to
// "mixed".
func TestRenderTableBreakdownUniformToolMarksTotals(t *testing.T) {
	sum := breakdownSummary()
	sum.Buckets = sum.Buckets[:1] // claude-code only
	sum.Totals = sum.Buckets[0]
	sum.Totals.Keys = nil

	out := RenderTable(sum, Opt{Breakdown: true})
	if got := cell(t, out, totalsLabel)[4]; got != "150"+markSubset {
		t.Errorf("uniform TOTAL reasoning cell = %q, want %q\n%s", got, "150"+markSubset, out)
	}
	if strings.Contains(out, markAdditive+" reasoning") || strings.Contains(out, markUnknown+" reasoning") {
		t.Errorf("legend listed a convention the table never used\n%s", out)
	}
}

// TestRenderTableBreakdownWithoutToolGroupingIsHonest: without a tool column
// the rule cannot be resolved, so the rows say so rather than picking one.
func TestRenderTableBreakdownWithoutToolGrouping(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"day"},
		Buckets: []store.Bucket{{
			Keys:        map[string]string{"day": "2026-08-09"},
			OrderedKeys: []string{"day"},
			Events:      3,
			Input:       10,
			Output:      20,
			Reasoning:   5,
			Total:       35,
		}},
		Totals: store.Bucket{Events: 3, Input: 10, Output: 20, Reasoning: 5, Total: 35},
	}

	out := RenderTable(sum, Opt{Breakdown: true})
	if got := cell(t, out, "2026-08-09")[4]; got != "5"+markUnknown {
		t.Errorf("ungrouped reasoning cell = %q, want %q\n%s", got, "5"+markUnknown, out)
	}
	if !strings.Contains(out, "group by tool") {
		t.Errorf("legend does not say how to resolve the rule\n%s", out)
	}
}

// TestRenderTableBreakdownNoReasoningNoLegend: rows without reasoning tokens
// carry no marker and the table gets no legend to explain.
func TestRenderTableBreakdownNoReasoningNoLegend(t *testing.T) {
	out := RenderTable(sampleSummary(), Opt{Breakdown: true})
	if strings.Contains(out, "reasoning is inside Output") || strings.Contains(out, "reasoning rules differ") {
		t.Errorf("legend rendered for a table with no reasoning tokens\n%s", out)
	}
	row := cell(t, out, "codex")
	if row[4] != "0" {
		t.Errorf("zero-reasoning cell = %q, want a bare 0\n%s", row[4], out)
	}
}

// TestRenderTableDefaultUnchangedByBreakdownWork guards the default rendering:
// no components, no markers, no legend.
func TestRenderTableDefaultUnchangedByBreakdownWork(t *testing.T) {
	out := RenderTable(breakdownSummary(), Opt{})
	header := cell(t, out, "tool")
	if len(header) != 6 { // tool Events Input Output Cache Total
		t.Fatalf("default header has %d columns, want 6: %v", len(header), header)
	}
	if strings.Contains(out, colReasoning) || strings.Contains(out, colCacheW) {
		t.Errorf("default table leaked breakdown columns\n%s", out)
	}
	if strings.Contains(out, "reasoning is") {
		t.Errorf("default table carries the breakdown legend\n%s", out)
	}
	// Cache stays the combined column.
	if got := cell(t, out, model.ToolClaudeCode)[4]; got != "500" {
		t.Errorf("combined cache cell = %q, want 500\n%s", got, out)
	}
}

// TestRenderTableBreakdownKeepsCostColumnLast: the Cost column stays trailing
// and right-aligned when the component columns widen the table.
func TestRenderTableBreakdownKeepsCostColumnLast(t *testing.T) {
	sum := breakdownSummary()
	costs := &Costs{
		Buckets: []Cost{{MicroUSD: 1_500_000, Known: true}, {MicroUSD: 250_000, Known: true}},
		Totals:  Cost{MicroUSD: 1_750_000, Known: true},
	}
	out := RenderTable(sum, Opt{Breakdown: true, Costs: costs})

	header := cell(t, out, "tool")
	if header[len(header)-1] != colCost {
		t.Fatalf("last header = %q, want %q: %v", header[len(header)-1], colCost, header)
	}
	row := cell(t, out, model.ToolClaudeCode)
	if row[len(row)-1] != "$1.50" {
		t.Errorf("last cell = %q, want $1.50\n%s", row[len(row)-1], out)
	}
	if row[len(row)-2] != "1.9K" {
		t.Errorf("total cell = %q, want the humanised total\n%s", row[len(row)-2], out)
	}
}
