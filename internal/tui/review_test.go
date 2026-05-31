package tui

import (
	"testing"
)

// review_test.go holds focused regression tests called out in code review.

// TestTabCyclesAllViews verifies Tab/Shift+Tab move between tabs (not within-view
// panes — there is no pane cycle anymore) and wrap around.
func TestTabCyclesAllViews(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	if m.view != ViewOverview {
		t.Fatalf("initial view = %v, want Overview", m.view)
	}
	// Forward through every tab and back to the start.
	want := []View{ViewByTool, ViewByModel, ViewBrowse, ViewOverview}
	for i, w := range want {
		m = send(m, keyMsg("tab"))
		if m.view != w {
			t.Fatalf("Tab #%d view = %v, want %v", i, m.view, w)
		}
	}
	// Shift+Tab walks back one.
	m = send(m, keyMsg("shift+tab"))
	if m.view != ViewBrowse {
		t.Fatalf("after Shift+Tab view = %v, want Sessions(Browse)", m.view)
	}
}

// TestBarSelectionMovesWithArrows confirms the single interactive surface on a
// bar view responds to ↑/↓ once that tab is active (no focus step needed).
func TestBarSelectionMovesWithArrows(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("3")) // By-Model
	if m.view != ViewByModel {
		t.Fatalf("view = %v, want By-Model", m.view)
	}
	before := m.byModel.Selected
	m = send(m, keyMsg("down"))
	if m.byModel.Selected == before {
		t.Fatalf("down did not move By-Model selection from %d", before)
	}
	m = send(m, keyMsg("up"))
	if m.byModel.Selected != before {
		t.Fatalf("up did not return selection to %d, got %d", before, m.byModel.Selected)
	}
}

// TestSelectionClampOnShrink ensures the by-model selection stays in range even
// if a stale index is set (e.g. after a filter shrinks the rows).
func TestSelectionClampOnShrink(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("3")) // By-Model
	m.setSelection(50)       // deliberately out of range
	if m.byModel.Selected >= len(m.byModel.Rows) {
		t.Fatalf("selection %d not clamped to rows %d", m.byModel.Selected, len(m.byModel.Rows))
	}
}
