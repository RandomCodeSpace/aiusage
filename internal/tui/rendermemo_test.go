package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// rendermemo_test.go covers issue #4 chunk 1f at the model level: with
// unchanged data, repeated View() renders never rebuild the braille hero chart
// — including across sys ticks (key excludes m.sys) and scrub steps (key
// excludes the scrub index).

func TestSecondViewRenderDoesNotRebuildChart(t *testing.T) {
	m := newTestModel(t, &fakeData{}) // loaded 120x40 Overview
	_ = m.View().Content
	base := m.heroMemo.Builds()
	if base == 0 {
		t.Fatal("first render did not build the hero chart through the memo")
	}

	_ = m.View().Content
	if got := m.heroMemo.Builds(); got != base {
		t.Fatalf("second render rebuilt the chart: builds %d → %d", base, got)
	}

	// Sys sample: the memo key excludes m.sys, so the 2s tick renders from
	// cache.
	m = send(m, sysTickMsg{})
	_ = m.View().Content
	if got := m.heroMemo.Builds(); got != base {
		t.Fatalf("sys tick rebuilt the chart: builds %d → %d", base, got)
	}

	// Scrub: the key excludes the scrub index — the crosshair is a post-pass
	// overlay, never a braille rebuild.
	m = send(m, keyMsg("right"))
	_ = m.View().Content
	if got := m.heroMemo.Builds(); got != base {
		t.Fatalf("scrub step rebuilt the braille chart: builds %d → %d", base, got)
	}

	// Resize re-keys: w/h are part of the memo key.
	m = send(m, tea.WindowSizeMsg{Width: 110, Height: 38})
	_ = m.View().Content
	if got := m.heroMemo.Builds(); got <= base {
		t.Fatal("resize did not rebuild the chart for the new geometry")
	}
}
