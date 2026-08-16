package views

import (
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// capTestCtx is byEntityTestCtx with a real money formatter and humanizer, so
// the cost and capability assertions read the numbers the app would render.
func capTestCtx() Ctx {
	c := byEntityTestCtx()
	c.Money = model.FormatCost
	c.Humanize = func(v int64) string { return humanTest(v) }
	return c
}

func capRows() []store.Bucket {
	return []store.Bucket{
		{Keys: map[string]string{"tool": model.ToolCopilot}, OrderedKeys: []string{"tool"},
			Events: 40, Input: 100, Output: 50, CacheRead: 200, Total: 350, CostMicroUSD: 1_890_000},
		{Keys: map[string]string{"tool": model.ToolCodex}, OrderedKeys: []string{"tool"},
			Events: 12, Input: 40, Output: 20, Total: 60, CostMicroUSD: 500_000,
			UnpricedEvents: 3, ComputedCostEvents: 9},
	}
}

// The four capability lines answer the question the numbers raise and cannot:
// a tool showing "-" for cost and one showing a real amount differ because of
// what their SOURCE exposes, not because of what was spent.
func TestByToolDetailCardStatesCapabilities(t *testing.T) {
	c := capTestCtx()
	rows := capRows()

	for _, w := range []int{80, 100, 120, 200} {
		lay := ComputeLayout(w, 44)
		out := ByTool(c, ByToolData{Rows: rows, Grand: 410, Selected: 0, RangeLbl: "7d"}, lay)
		cap, ok := model.CapabilityFor(model.ToolCopilot)
		if !ok {
			t.Fatal("copilot has no capability declaration")
		}
		// Every field states SOMETHING complete — the widest rung that fits, never
		// a truncated one. A narrow pane is allowed a shorter word, not an
		// ellipsis.
		wants := [][]string{
			{"SOURCE"},
			costProvenanceForms(cap.Cost),
			activityCaptureForms(cap.Activity),
			reasoningReportForms(cap.Reasoning),
			{string(cap.Tier)},
		}
		for _, forms := range wants {
			if !containsAny(out, forms) {
				t.Errorf("w=%d: By-Tool detail card states none of %v:\n%s", w, forms, out)
			}
		}
		if strings.Contains(out, "unattribu…") || strings.Contains(out, "vendor-repo…") {
			t.Errorf("w=%d: a capability line was truncated instead of shortened:\n%s", w, out)
		}
	}
}

// The declaration follows the SELECTION. A card that kept showing the first
// tool's capabilities while the cursor sat on another would be worse than
// showing none.
func TestCapabilityLinesFollowTheSelection(t *testing.T) {
	c := capTestCtx()
	lay := ComputeLayout(160, 44)

	copilot, _ := model.CapabilityFor(model.ToolCopilot)
	codex, _ := model.CapabilityFor(model.ToolCodex)
	if copilot.Cost == codex.Cost {
		t.Fatal("the two fixtures declare the same cost provenance; the test cannot tell them apart")
	}

	first := ByTool(c, ByToolData{Rows: capRows(), Grand: 410, Selected: 0, RangeLbl: "7d"}, lay)
	second := ByTool(c, ByToolData{Rows: capRows(), Grand: 410, Selected: 1, RangeLbl: "7d"}, lay)

	if !containsAny(first, costProvenanceForms(copilot.Cost)) {
		t.Errorf("row 0 selected: card does not state copilot's %q:\n%s", copilot.Cost, first)
	}
	if !containsAny(second, costProvenanceForms(codex.Cost)) {
		t.Errorf("row 1 selected: card does not state codex's %q:\n%s", codex.Cost, second)
	}
}

func containsAny(s string, forms []string) bool {
	for _, f := range forms {
		if strings.Contains(s, f) {
			return true
		}
	}
	return false
}

// A tool id the table has not been taught about gets ONE honest line, not four
// plausible defaults under a real tool's name.
func TestUnknownToolSaysItHasNoDeclaration(t *testing.T) {
	c := capTestCtx()
	rows := []store.Bucket{{Keys: map[string]string{"tool": "not-a-harness"},
		OrderedKeys: []string{"tool"}, Events: 1, Total: 10}}
	out := ByTool(c, ByToolData{Rows: rows, Grand: 10, RangeLbl: "7d"}, ComputeLayout(160, 44))
	if !strings.Contains(out, "no capability declaration") {
		t.Errorf("an undeclared tool did not say so:\n%s", out)
	}
}

// By-Model has no capability block: the declarations are per TOOL, and a model
// id has none. Showing its owning tool's would attribute a claim about a harness
// to a model that several harnesses reach.
func TestByModelHasNoCapabilityBlock(t *testing.T) {
	c := capTestCtx()
	rows := []store.Bucket{{Keys: map[string]string{"model": "claude-opus-4"},
		OrderedKeys: []string{"model"}, Events: 3, Output: 100, Reasoning: 31, Total: 300}}
	out := ByModel(c, ByModelData{Rows: rows, Grand: 300, RangeLbl: "7d",
		ModelTool: map[string]string{"claude-opus-4": model.ToolClaudeCode}}, ComputeLayout(160, 44))
	if strings.Contains(out, "SOURCE") {
		t.Errorf("By-Model rendered a per-tool capability block:\n%s", out)
	}
}

// The reasoning share is a By-Model line and only a By-Model line, read against
// OUTPUT — the only denominator it means anything as.
func TestByModelStatesReasoningShareOfOutput(t *testing.T) {
	c := capTestCtx()
	c.Percent = func(v, total int64) string {
		if total == 0 {
			return "0%"
		}
		return itoaTest(v*100/total) + "%"
	}
	rows := []store.Bucket{{Keys: map[string]string{"model": "gpt-5"}, OrderedKeys: []string{"model"},
		Events: 3, Input: 900, Output: 100, Reasoning: 31, Total: 1000}}

	for _, w := range []int{80, 120, 200} {
		out := ByModel(c, ByModelData{Rows: rows, Grand: 1000, RangeLbl: "7d"}, ComputeLayout(w, 44))
		if !strings.Contains(out, "reasoning") || !strings.Contains(out, "31% of output") {
			t.Errorf("w=%d: By-Model detail card does not state the reasoning share:\n%s", w, out)
		}
	}

	// By-Tool must NOT carry it: one line, nowhere else.
	tools := []store.Bucket{{Keys: map[string]string{"tool": model.ToolCodex}, OrderedKeys: []string{"tool"},
		Events: 3, Input: 900, Output: 100, Reasoning: 31, Total: 1000}}
	out := ByTool(c, ByToolData{Rows: tools, Grand: 1000, RangeLbl: "7d"}, ComputeLayout(160, 44))
	if strings.Contains(out, "% of output") {
		t.Errorf("By-Tool rendered the reasoning share line:\n%s", out)
	}
}

// A model whose rows report NO reasoning renders nothing. "0% of output" would
// claim the source measured it and found none, which is a different fact from
// "the source does not report it".
func TestNoReasoningRendersNoLine(t *testing.T) {
	c := capTestCtx()
	rows := []store.Bucket{{Keys: map[string]string{"model": "m"}, OrderedKeys: []string{"model"},
		Events: 3, Output: 100, Reasoning: 0, Total: 1000}}
	out := ByModel(c, ByModelData{Rows: rows, Grand: 1000, RangeLbl: "7d"}, ComputeLayout(160, 44))
	if strings.Contains(out, "of output") {
		t.Errorf("a model with no reasoning count rendered a share line:\n%s", out)
	}
}

// The detail card's cost total is bounded where the window holds unpriced rows
// and marked where any of it was estimated — at wide and narrow geometries
// alike, with only the rider allowed to go.
func TestByToolDetailCostIsBoundedAndMarked(t *testing.T) {
	c := capTestCtx()
	rows := capRows()

	for _, w := range []int{80, 100, 120, 200} {
		lay := ComputeLayout(w, 44)
		out := ByTool(c, ByToolData{Rows: rows, Grand: 410, Selected: 1, RangeLbl: "7d"}, lay)
		if !strings.Contains(out, boundedMark) {
			t.Errorf("w=%d: a selection with unpriced rows is not bounded:\n%s", w, out)
		}
		if !strings.Contains(out, "~") {
			t.Errorf("w=%d: a rate-card-priced selection carries no provenance mark:\n%s", w, out)
		}
	}

	// The vendor-priced, fully-priced row is the bare case.
	out := ByTool(c, ByToolData{Rows: rows, Grand: 410, Selected: 0, RangeLbl: "7d"}, ComputeLayout(200, 44))
	if !strings.Contains(out, "$1.89") {
		t.Errorf("the vendor-priced row's cost is missing:\n%s", out)
	}
}

// Panel widths must survive every addition: the capability block, the reasoning
// line and the bounded cost all live inside the detail card's budget.
func TestDetailCardWidthSurvivesTheNewLines(t *testing.T) {
	c := capTestCtx()
	c.Percent = func(v, total int64) string {
		if total == 0 {
			return "0%"
		}
		return itoaTest(v*100/total) + "%"
	}
	rows := capRows()
	rows[0].Reasoning = 20

	for _, w := range []int{80, 100, 120, 160, 200} {
		lay := ComputeLayout(w, 44)
		if !lay.SidePanel {
			continue
		}
		for _, sel := range []int{0, 1} {
			d := byEntityData{title: "BY TOOL", dim: "tool", rows: rows, grand: 410,
				selected: sel, capability: true, reasoning: true}
			card := detailCard(c, d, lay.SideW, lay.BodyH, false)
			for i, lw := range lineWidths(card) {
				if lw != lay.SideW {
					t.Fatalf("w=%d sel=%d: detail line %d is %d cells, want SideW=%d\n%s",
						w, sel, i, lw, lay.SideW, card)
				}
			}
			if h := len(lineWidths(card)); h > maxInt(lay.BodyH, 3) {
				t.Fatalf("w=%d sel=%d: detail card is %d rows, body is %d", w, sel, h, lay.BodyH)
			}
		}
	}
}
