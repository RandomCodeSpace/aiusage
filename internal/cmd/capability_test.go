package cmd

import (
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// capability_test.go is the seam that keeps model.ToolCapabilities honest.
//
// The declarations live in internal/model because internal/tui renders them and
// may not import internal/adapter (layering). That puts them one package away
// from the source they describe, so they can drift — an adapter can be
// registered, or learn to read a vendor price, without anyone touching the
// table. cmd is the composition root and the ONLY package that sees both, which
// is where the same problem is already solved for the discovery environment
// (TestDiscoveryEnvCoversEveryAdapterVariable).

// Every registered adapter must declare its capabilities. A tool the dashboard
// can show rows for and cannot describe renders "no capability declaration" in
// its detail card, which is honest but useless — and the fix is one line in a
// table nobody would think to open.
func TestEveryAdapterDeclaresCapabilities(t *testing.T) {
	for _, ad := range defaultRegistry().All() {
		if _, ok := model.CapabilityFor(ad.ID()); !ok {
			t.Errorf("adapter %q has no entry in model.ToolCapabilities; "+
				"the By-Tool detail card cannot describe where its numbers come from", ad.ID())
		}
	}
}

// The reverse direction is NOT an error, but it must be a deliberate one: a
// declared tool with no adapter is retired history (ToolGemini), whose rows are
// still in the append-only ledger and still need describing. This pins the list
// of such entries so a typo in a tool id shows up as a new one rather than as a
// silently unreachable row.
func TestOnlyRetiredToolsDeclareWithoutAnAdapter(t *testing.T) {
	registered := map[string]bool{}
	for _, ad := range defaultRegistry().All() {
		registered[ad.ID()] = true
	}
	retired := map[string]bool{model.ToolGemini: true}

	for _, c := range model.ToolCapabilities() {
		if registered[c.Tool] || retired[c.Tool] {
			continue
		}
		t.Errorf("capability entry %q matches no registered adapter and is not a known retired tool; "+
			"either it is a typo or the retired list needs updating", c.Tool)
	}
}

// A declaration must actually say something. An empty field renders as an empty
// line in the detail card, which reads as a rendering bug rather than as a
// missing fact.
func TestCapabilityDeclarationsAreComplete(t *testing.T) {
	for _, c := range model.ToolCapabilities() {
		if c.Cost == "" || c.Activity == "" || c.Reasoning == "" || c.Tier == "" {
			t.Errorf("capability for %q is incomplete: %+v", c.Tool, c)
		}
	}
}
