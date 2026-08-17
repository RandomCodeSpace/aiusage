package model

import "testing"

// The reasoning column is DERIVED from reasoningModes rather than restated in
// the capability table, so the pricing engine and the dashboard can never
// disagree about what a tool reports. This pins the derivation, including the
// one state the map expresses by absence.
func TestReasoningReportFollowsTheBillingModes(t *testing.T) {
	for tool, mode := range reasoningModes {
		got := ReasoningReportFor(tool)
		want := ReasoningReportSubset
		if mode == ReasoningAdditive {
			want = ReasoningReportAdditive
		}
		if got != want {
			t.Errorf("ReasoningReportFor(%q) = %q, want %q (billing mode %q)", tool, got, want, mode)
		}
	}
}

// A tool ABSENT from reasoningModes reports no reasoning count at all. It must
// NOT inherit ReasoningModeFor's conservative subset fallback: "we bill it as a
// subset because we do not know" and "the source reports a subset" are
// different claims, and only the first is true of crush, kimi-code, goose and
// cline.
func TestToolsWithNoReasoningCounterSaySo(t *testing.T) {
	for _, tool := range []string{ToolCrush, ToolKimiCode, ToolGoose, ToolCline} {
		if _, ok := reasoningModes[tool]; ok {
			t.Fatalf("%q is now in reasoningModes; this test's premise is stale", tool)
		}
		if got := ReasoningReportFor(tool); got != ReasoningReportNone {
			t.Errorf("ReasoningReportFor(%q) = %q, want %q", tool, got, ReasoningReportNone)
		}
		// The pricing fallback is unchanged and deliberately different.
		if got := ReasoningModeFor(tool); got != ReasoningSubset {
			t.Errorf("ReasoningModeFor(%q) = %q, want the conservative %q", tool, got, ReasoningSubset)
		}
	}
}

// CapabilityFor fills Reasoning; a caller must never receive an entry with an
// empty one just because the literal in the table omits it.
func TestCapabilityForFillsReasoning(t *testing.T) {
	for _, c := range toolCapabilities {
		got, ok := CapabilityFor(c.Tool)
		if !ok {
			t.Fatalf("CapabilityFor(%q) not found in its own table", c.Tool)
		}
		if got.Reasoning == "" {
			t.Errorf("CapabilityFor(%q) left Reasoning empty", c.Tool)
		}
		if got.Reasoning != ReasoningReportFor(c.Tool) {
			t.Errorf("CapabilityFor(%q).Reasoning = %q, want %q", c.Tool, got.Reasoning, ReasoningReportFor(c.Tool))
		}
	}
}

// An unknown tool is unknown. Answering with a plausible default would put four
// invented facts on screen under a real tool's name.
func TestCapabilityForRefusesUnknownTools(t *testing.T) {
	if got, ok := CapabilityFor("not-a-harness"); ok {
		t.Errorf("CapabilityFor(unknown) = %+v, ok=true; want no declaration", got)
	}
}

// Exactly the four adapters that stamp a cost declare CostVendor. This is the
// same fact PriceProvenance classifies per row, asserted per tool, so the two
// halves of the provenance story cannot drift apart.
func TestVendorCostDeclarationsMatchTheStampingAdapters(t *testing.T) {
	want := map[string]bool{
		ToolCopilot:  true,
		ToolCrush:    true,
		ToolGoose:    true,
		ToolPi:       true,
		ToolOpenClaw: true,
	}
	for _, c := range ToolCapabilities() {
		if got := c.Cost == CostVendor; got != want[c.Tool] {
			t.Errorf("%q declares Cost=%q; vendor-stamping adapters are %v", c.Tool, c.Cost, keysOf(want))
		}
	}
}

// Exactly the adapters that set UsageDedupKey on their activity rows declare an
// exact join; exactly those that emit rows without one declare unattributed.
// Everything else emits no activity at all.
func TestActivityDeclarationsMatchTheAdapters(t *testing.T) {
	exact := map[string]bool{
		ToolClaudeCode: true, ToolCline: true, ToolDSH: true,
		ToolOpenCode: true, ToolPi: true, ToolOpenClaw: true,
	}
	unattributed := map[string]bool{ToolCodex: true, ToolCopilot: true, ToolGoose: true}

	for _, c := range ToolCapabilities() {
		want := ActivityNone
		switch {
		case exact[c.Tool]:
			want = ActivityExact
		case unattributed[c.Tool]:
			want = ActivityUnattributed
		}
		if c.Activity != want {
			t.Errorf("%q declares Activity=%q, want %q", c.Tool, c.Activity, want)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
