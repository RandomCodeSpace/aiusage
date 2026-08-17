package cmd

import (
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/tui"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// capability_test.go is the seam that keeps the capability declarations honest.
//
// Each adapter declares its own through the required Capabilities method, so
// there is no table to forget to edit and a sixteenth adapter cannot compile
// without one. What is left to check is the plumbing AROUND that: that the
// declaration names the tool the registry knows the adapter as, that the values
// still match what the adapters actually do, and that the retired list stays a
// claim about tools NOTHING collects. cmd is the composition root and the only
// package that sees both the registry and model, which is where the same
// problem is already solved for the discovery environment
// (TestDiscoveryEnvCoversEveryAdapterVariable).

// A declaration must name the tool the registry knows the adapter as. The map
// is keyed by ID(), so a disagreement would file one adapter's declaration
// under another adapter's name, and every field on screen would describe the
// wrong harness.
func TestCapabilityDeclarationsNameTheirOwnTool(t *testing.T) {
	for _, ad := range defaultRegistry().All() {
		if got := ad.Capabilities().Tool; got != ad.ID() {
			t.Errorf("adapter %q declares Tool=%q; the two must agree or the "+
				"declaration describes the wrong harness", ad.ID(), got)
		}
	}
}

// A declaration must actually say something. An empty field renders as an empty
// line in the By-Tool detail card, which reads as a rendering bug rather than as
// a missing fact.
func TestCapabilityDeclarationsAreComplete(t *testing.T) {
	for tool, c := range toolCapabilities() {
		if c.Cost == "" || c.Activity == "" || c.Reasoning == "" || c.Tier == "" {
			t.Errorf("capability for %q is incomplete: %+v", tool, c)
		}
	}
}

// Reasoning is DERIVED from model's billing modes rather than restated in each
// adapter, so the pricing engine and the dashboard can never disagree about what
// a source reports. An adapter that hardcoded the field would drift silently.
func TestCapabilityReasoningFollowsTheBillingModes(t *testing.T) {
	for tool, c := range toolCapabilities() {
		if want := model.ReasoningReportFor(tool); c.Reasoning != want {
			t.Errorf("%q declares Reasoning=%q, want %q from the billing modes",
				tool, c.Reasoning, want)
		}
	}
}

// Exactly the adapters that stamp a cost declare CostVendor. This is the same
// fact PriceProvenance classifies per row, asserted per tool, so the two halves
// of the provenance story cannot drift apart.
func TestVendorCostDeclarationsMatchTheStampingAdapters(t *testing.T) {
	want := map[string]bool{
		model.ToolCopilot:  true,
		model.ToolCrush:    true,
		model.ToolGoose:    true,
		model.ToolPi:       true,
		model.ToolOpenClaw: true,
	}
	for _, ad := range defaultRegistry().All() {
		c := ad.Capabilities()
		if got := c.Cost == model.CostVendor; got != want[c.Tool] {
			t.Errorf("%q declares Cost=%q; the vendor-stamping adapters are copilot, "+
				"crush, goose, pi and openclaw", c.Tool, c.Cost)
		}
	}
}

// Exactly the adapters that set UsageDedupKey on their activity rows declare an
// exact join; exactly those that emit rows without one declare unattributed.
// Everything else emits no activity at all.
func TestActivityDeclarationsMatchTheAdapters(t *testing.T) {
	exact := map[string]bool{
		model.ToolClaudeCode: true, model.ToolCline: true, model.ToolDSH: true,
		model.ToolOpenCode: true, model.ToolPi: true, model.ToolOpenClaw: true,
	}
	unattributed := map[string]bool{
		model.ToolCodex: true, model.ToolCopilot: true, model.ToolGoose: true,
	}

	for _, ad := range defaultRegistry().All() {
		c := ad.Capabilities()
		want := model.ActivityNone
		switch {
		case exact[c.Tool]:
			want = model.ActivityExact
		case unattributed[c.Tool]:
			want = model.ActivityUnattributed
		}
		if c.Activity != want {
			t.Errorf("%q declares Activity=%q, want %q", c.Tool, c.Activity, want)
		}
	}
}

// A retired tool is one NO adapter collects. The list exists because
// usage_events is append-only and the dashboard still has to describe stored
// rows whose harness is gone; an entry that a registered adapter also claims is
// either a typo or a retirement that never happened, and the merge would hide
// it by overwriting the retired row.
func TestRetiredToolsHaveNoAdapter(t *testing.T) {
	registered := map[string]bool{}
	for _, ad := range defaultRegistry().All() {
		registered[ad.ID()] = true
	}
	for _, c := range model.RetiredCapabilities() {
		if registered[c.Tool] {
			t.Errorf("%q is declared retired and is also a registered adapter; "+
				"one of the two is wrong", c.Tool)
		}
	}
}

// The map the TUI receives covers every registered adapter AND every retired
// tool. A tool the dashboard can show rows for and cannot describe renders "no
// capability declaration" in its detail card, which is honest but useless.
func TestToolCapabilitiesCoversTheRegistryAndTheRetired(t *testing.T) {
	caps := toolCapabilities()
	for _, ad := range defaultRegistry().All() {
		if _, ok := caps[ad.ID()]; !ok {
			t.Errorf("adapter %q is missing from the capability map handed to the TUI", ad.ID())
		}
	}
	for _, c := range model.RetiredCapabilities() {
		if _, ok := caps[c.Tool]; !ok {
			t.Errorf("retired tool %q is missing from the capability map handed to the TUI", c.Tool)
		}
	}
	if want := len(defaultRegistry().All()) + len(model.RetiredCapabilities()); len(caps) != want {
		t.Errorf("capability map has %d entries, want %d: the two sources overlap", len(caps), want)
	}
}

// The declarations are only worth collecting if they reach the dashboard. cmd is
// the composition root — internal/tui must not import adapter — so they travel
// exactly one way: the Capabilities argument of the tui.Options the root command
// hands to Run. Nothing about that argument is load-bearing at compile time, so
// this drives the real command and asserts the values that arrive.
func TestRootHandsCapabilitiesToTheTUI(t *testing.T) {
	isolateState(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	home := t.TempDir()
	cfgPath := offlineConfig(t)

	prevTTY, prevRun, prevFlags := isTTY, runTUI, flags
	t.Cleanup(func() { isTTY, runTUI, flags = prevTTY, prevRun, prevFlags })

	var got tui.Options
	var launched bool
	isTTY = func() bool { return true }
	runTUI = func(_ store.Store, opt tui.Options) error {
		got, launched = opt, true
		return nil
	}

	if out, err := runCmd(t, "--db", db, "--home", home, "--config", cfgPath, "--no-daemon"); err != nil {
		t.Fatalf("root command failed: %v\noutput:\n%s", err, out)
	}
	if !launched {
		t.Fatal("the root command did not launch the TUI; nothing was wired")
	}

	want := toolCapabilities()
	if len(got.Capabilities) != len(want) {
		t.Fatalf("tui.Options.Capabilities has %d entries, want %d", len(got.Capabilities), len(want))
	}
	for tool, c := range want {
		if g, ok := got.Capabilities[tool]; !ok || g != c {
			t.Errorf("tui.Options.Capabilities[%s] = %+v (present=%v), want %+v", tool, g, ok, c)
		}
	}
}
