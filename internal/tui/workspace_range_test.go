package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestWorkspaceRangeLoadsOnlyVisibleAggregates(t *testing.T) {
	f, m := newWorkspaceModel(t)
	before := f.queries()
	m, cmd := workspaceUI(t, f, m, keyMsg("f8"))
	m = workspaceApply(t, f, m, cmd)
	if got := f.queries() - before; got != 2 {
		t.Fatalf("range change ran %d source summaries; workspace needs only timeline and comparison", got)
	}
	if m.rng != Range30d {
		t.Fatalf("range = %v", m.rng)
	}
}

func TestWorkspaceRangeCacheAndActiveRangeDoNotLoad(t *testing.T) {
	f, m := newWorkspaceModel(t)
	before, gen := f.queries(), m.loadGen
	m, cmd := workspaceUI(t, f, m, keyMsg("f7"))
	if cmd != nil || m.loadGen != gen {
		t.Fatal("active range started another load")
	}
	m = mustPress(t, m, "workspace-range-1", tea.MouseLeft)
	if f.queries() != before || m.loadGen != gen {
		t.Fatal("active range click started another load")
	}
	m, cmd = workspaceUI(t, f, m, keyMsg("f8"))
	m = workspaceApply(t, f, m, cmd)
	before = f.queries()
	m, cmd = workspaceUI(t, f, m, keyMsg("f7"))
	if cmd != nil || m.fresh != FreshLive || m.workspace.appliedSpan.R != Range7d || f.queries() != before {
		t.Fatal("cached range did not apply immediately without source work")
	}
	m, cmd = workspaceUI(t, f, m, keyMsg("t"))
	if cmd != nil || m.workspace.appliedSpan.R != Range30d || f.queries() != before {
		t.Fatal("range cycling bypassed the immediate cached path")
	}
	m, _ = workspaceUI(t, f, m, keyMsg("f7"))
	m.data.Invalidate()
	m, cmd = workspaceUI(t, f, m, keyMsg("f8"))
	if cmd == nil || m.fresh != FreshCutIn {
		t.Fatal("invalidated range reused stale cache")
	}
	m = workspaceApply(t, f, m, cmd)
	if f.queries()-before != 2 || m.workspace.appliedSpan.R != Range30d {
		t.Fatal("invalidated range did not reload current evidence")
	}
}

func TestWorkspaceCachedRangeCancelsPendingRead(t *testing.T) {
	f, m := newWorkspaceModel(t)
	before := f.queries()
	m, pending := workspaceUI(t, f, m, keyMsg("f8"))
	if pending == nil {
		t.Fatal("missing uncached range load")
	}
	m, cmd := workspaceUI(t, f, m, keyMsg("f7"))
	if cmd != nil {
		t.Fatal("return to cached range dispatched a load")
	}
	late := pending()
	m, _ = workspaceUI(t, f, m, late)
	if f.queries() != before || m.fresh != FreshLive || m.workspace.appliedSpan.R != Range7d {
		t.Fatal("obsolete range read queried or replaced the cached selection")
	}
}

func TestWorkspaceClassicSummariesLoadOnDemand(t *testing.T) {
	f, m := newWorkspaceModel(t)
	before := f.queries()
	m, cmd := workspaceUI(t, f, m, keyMsg("p"))
	if cmd == nil {
		t.Fatal("entering the classic view did not load its own summaries")
	}
	m = workspaceApply(t, f, m, cmd)
	if got := f.queries() - before; got != 3 {
		t.Fatalf("classic-only summary reads = %d, want 3", got)
	}
	before = f.queries()
	pivot := m.heroPivot
	m, cmd = workspaceUI(t, f, m, keyMsg("p"))
	if cmd != nil || f.queries() != before || m.heroPivot == pivot {
		t.Fatal("classic plot toggle failed to change the plot using already-applied data")
	}
}
