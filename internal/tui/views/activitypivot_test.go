package views

import (
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// turnContextTestData is the Activity tab in a turn-context pivot: a ranked page
// of values, the coverage grouping by tool, and the per-day turn counts. Only
// claude-code contributes, which is the real shape and the reason the coverage
// note exists.
func turnContextTestData(dim model.TurnDimension) ActivityData {
	val := func(v string, turns, tokens, cost, unpriced, computed int64) store.TurnContextBucket {
		return store.TurnContextBucket{
			Keys:        map[string]string{"value": v, "tool": model.ToolClaudeCode},
			OrderedKeys: []string{"value", "tool"},
			Turns:       turns, Sessions: turns / 4,
			InputTokens: tokens / 4, OutputTokens: tokens / 4, TotalTokens: tokens,
			CostMicroUSD: cost, UnpricedTurns: unpriced, ComputedCostTurns: computed,
		}
	}
	rows := []store.TurnContextBucket{
		val("Explore", 8120, 4_000_000, 6_200_000, 0, 8120),
		val("fork", 3110, 1_400_000, 2_100_000, 40, 3070),
		val("a-very-long-agent-name-that-keeps-going-and-going", 44, 90_000, 120_000, 0, 44),
	}
	return ActivityData{
		Pivot:   string(dim),
		CtxRows: rows,
		CtxTools: []store.TurnContextBucket{
			{Keys: map[string]string{"tool": model.ToolClaudeCode}, Turns: 11274},
		},
		CtxBuckets: []store.TurnContextBucket{
			{Keys: map[string]string{"day": "2026-05-28"}, Turns: 5000},
			{Keys: map[string]string{"day": "2026-05-29"}, Turns: 6274},
		},
		CtxTotals: store.TurnContextBucket{
			Turns: 11274, Sessions: 318, TotalTokens: 5_490_000,
			CostMicroUSD: 8_420_000, UnpricedTurns: 40, ComputedCostTurns: 11234,
		},
		CallsDim: "day", RangeLbl: "7d", OrderLbl: "turns", Limit: 200,
	}
}

// A pivoted tab must fit every geometry exactly as the calls pivot does — the
// long agent names are the same overflow risk the mcp__ ids are.
func TestActivityPivotFitsEveryGeometry(t *testing.T) {
	c := activityTestCtx()
	for _, dim := range model.TurnDimensions() {
		d := turnContextTestData(dim)
		for _, geo := range []struct{ w, h int }{
			{40, 10}, {48, 12}, {56, 14}, {74, 18}, {80, 24}, {100, 28}, {120, 40}, {200, 50},
		} {
			lay := ComputeLayout(geo.w, geo.h)
			if lay.TooSmall {
				continue
			}
			out := Activity(c, d, lay)
			for i, w := range widths(out) {
				if w > lay.BodyW {
					t.Errorf("%s %dx%d: line %d is %d cells, body is %d", dim, geo.w, geo.h, i, w, lay.BodyW)
				}
			}
			if h := heightOf(out); h > lay.BodyH {
				t.Errorf("%s %dx%d: frame is %d rows, body is %d", dim, geo.w, geo.h, h, lay.BodyH)
			}
		}
	}
}

// A turn context counts TURNS, and every label that could be read as a call
// count has to say so. store.ActivityByCalls ranks turns here — one order
// constant, two different things counted.
func TestActivityPivotNamesTurnsNotCalls(t *testing.T) {
	c := activityTestCtx()
	d := turnContextTestData(model.DimensionAgent)

	out := Activity(c, d, ComputeLayout(160, 40))
	if !strings.Contains(out, "turns") {
		t.Errorf("the agent pivot never says 'turns':\n%s", out)
	}
	// The word "calls" belongs to the other ledger. It must not appear as a
	// column heading or a stat label here.
	for _, bad := range []string{"calls  ", " calls\n"} {
		if strings.Contains(out, bad) {
			t.Errorf("the agent pivot labels a column %q:\n%s", strings.TrimSpace(bad), out)
		}
	}
}

// The coverage note is what stops "no skills" reading as "I ran no skills". It
// names the tools whose turns carry the dimension at all.
func TestActivityPivotStatesCoverage(t *testing.T) {
	c := activityTestCtx()
	d := turnContextTestData(model.DimensionSkill)

	for _, w := range []int{80, 120, 200} {
		out := Activity(c, d, ComputeLayout(w, 40))
		if !strings.Contains(out, model.ToolClaudeCode) {
			t.Errorf("w=%d: the skill pivot does not name the contributing tool:\n%s", w, out)
		}
	}

	// An empty partition says the harnesses do not report it, rather than
	// showing a bare zero that reads as "you used none".
	empty := ActivityData{Pivot: string(model.DimensionPlugin), RangeLbl: "7d", OrderLbl: "turns"}
	out := Activity(c, empty, ComputeLayout(120, 40))
	if !strings.Contains(out, "no rows in range") {
		t.Errorf("an empty pivot did not render the honest empty state:\n%s", out)
	}
}

// The pivot's title names the dimension. Two partitions of the same dollars that
// looked identical on screen would be the whole problem back again.
func TestActivityPivotTitlesNameTheDimension(t *testing.T) {
	c := activityTestCtx()
	for _, dim := range model.TurnDimensions() {
		out := Activity(c, turnContextTestData(dim), ComputeLayout(160, 40))
		want := strings.ToUpper(strings.ReplaceAll(string(dim), "_", " "))
		if !strings.Contains(out, want) {
			t.Errorf("the %s pivot does not name itself (%q):\n%s", dim, want, out)
		}
	}
}

// Two partitions must never look alike. The kind column holds five cells, in
// which "mcp_tool" and "mcp_server" truncate to the same string — so the column
// is dropped in a pivot and the TITLE carries the distinction, at every width
// including the narrow ones where the column would have survived.
func TestMCPPivotsAreDistinguishableAtEveryWidth(t *testing.T) {
	c := activityTestCtx()
	for _, geo := range []struct{ w, h int }{{56, 14}, {80, 24}, {120, 40}, {200, 50}} {
		lay := ComputeLayout(geo.w, geo.h)
		tool := Activity(c, turnContextTestData(model.DimensionMCPTool), lay)
		server := Activity(c, turnContextTestData(model.DimensionMCPServer), lay)
		if tool == server {
			t.Errorf("%dx%d: the mcp_tool and mcp_server pivots render identically", geo.w, geo.h)
		}
		if strings.Contains(tool, "mcp_s…") || strings.Contains(server, "mcp_t…") {
			t.Errorf("%dx%d: a pivot rendered a truncated dimension name:\n%s", geo.w, geo.h, tool)
		}
		if !strings.Contains(tool, "MCP TOOL") || !strings.Contains(server, "MCP SERVER") {
			t.Errorf("%dx%d: a pivot does not name its own dimension", geo.w, geo.h)
		}
	}
}

// A turn context has NO divisor and no unattributed concept, so a fully-priced
// pivot renders a bare cost — no "?" treatment, no bound.
func TestActivityPivotHasNoUnattributedTreatment(t *testing.T) {
	c := activityTestCtx()
	d := turnContextTestData(model.DimensionAgent)
	d.CtxRows = d.CtxRows[:1] // Explore: 8120 turns, all joined, all priced
	d.CtxTotals = store.TurnContextBucket{Turns: 8120, TotalTokens: 4_000_000,
		CostMicroUSD: 6_200_000, ComputedCostTurns: 8120}

	out := Activity(c, d, ComputeLayout(160, 40))
	if strings.Contains(out, "unattributed") {
		t.Errorf("a turn-context pivot rendered the unattributed treatment:\n%s", out)
	}
	if strings.Contains(out, boundedMark) {
		t.Errorf("a fully joined, fully priced pivot rendered a bound:\n%s", out)
	}
	// Every turn was priced from a rate card, so the figure is an estimate.
	if !strings.Contains(out, "~$6.20") {
		t.Errorf("a rate-card-priced pivot did not carry the provenance mark:\n%s", out)
	}
}

// Unpriced turns bound the pivot's total exactly as they bound every other
// cost total in the app.
func TestActivityPivotTotalIsBoundedByUnpricedTurns(t *testing.T) {
	c := activityTestCtx()
	d := turnContextTestData(model.DimensionAgent) // CtxTotals carries 40 unpriced

	out := Activity(c, d, ComputeLayout(200, 40))
	if !strings.Contains(out, boundedMark) {
		t.Errorf("a pivot holding unpriced turns is not bounded:\n%s", out)
	}
}

// RowCount and the projections must read the ACTIVE pivot's slice. A clamp
// against the wrong field is how a pivot switch leaves the cursor past the end.
func TestActivityDataReadsTheActivePivot(t *testing.T) {
	calls := activityTestData()
	if got := calls.RowCount(); got != len(calls.Rows) {
		t.Errorf("calls pivot RowCount = %d, want %d", got, len(calls.Rows))
	}
	ctx := turnContextTestData(model.DimensionAgent)
	if got := ctx.RowCount(); got != len(ctx.CtxRows) {
		t.Errorf("agent pivot RowCount = %d, want %d", got, len(ctx.CtxRows))
	}

	// A struct carrying BOTH (which no load produces) reads the pivot, not
	// whichever slice happens to be longer.
	both := ctx
	both.Rows = calls.Rows
	if got := both.RowCount(); got != len(ctx.CtxRows) {
		t.Errorf("RowCount ignored the pivot: %d, want %d", got, len(ctx.CtxRows))
	}
	if got := both.rows(); len(got) != len(ctx.CtxRows) || got[0].name != ctx.CtxRows[0].Keys["value"] {
		t.Errorf("rows() read the wrong ledger for the %q pivot", both.Pivot)
	}
}

// The projection must not lose or rename a number. A turn context's tokens are
// the turns' FULL counts — no share is taken.
func TestTurnContextProjectionIsLossless(t *testing.T) {
	b := store.TurnContextBucket{
		Keys:  map[string]string{"value": "Explore", "tool": model.ToolClaudeCode},
		Turns: 10, Sessions: 3, InputTokens: 40, OutputTokens: 20, TotalTokens: 100,
		CostMicroUSD: 500, UnjoinedTurns: 1, UnpricedTurns: 2, ComputedCostTurns: 7,
	}
	r := rowFromTurnContext(b, string(model.DimensionAgent))
	if r.name != "Explore" || r.tool != model.ToolClaudeCode || r.kind != string(model.DimensionAgent) {
		t.Errorf("identity lost: %+v", r)
	}
	if r.count != 10 || r.sessions != 3 || r.input != 40 || r.output != 20 || r.total != 100 {
		t.Errorf("counts lost: %+v", r)
	}
	if r.cost != 500 || r.unattributed != 1 || r.unpriced != 2 || r.computed != 7 {
		t.Errorf("cost qualifiers lost: %+v", r)
	}
}

func heightOf(s string) int { return len(strings.Split(s, "\n")) }
