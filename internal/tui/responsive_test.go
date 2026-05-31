package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestResponsiveNoOverflow is the core anti-overflow gate: it drives every view
// across a wide matrix of terminal sizes — from phone-tiny (40×16) and very
// short (×8/×10/×12) up to ultrawide (240×50), plus sub-floor sizes that must
// fall back to the resize card — and asserts the rendered frame never overflows.
// For each (view, w, h): no panic, every line's display width ≤ w, no more lines
// than h, and a non-empty frame. This is what makes "responsive on all devices"
// a checked invariant rather than a hope.
func TestResponsiveNoOverflow(t *testing.T) {
	widths := []int{30, 40, 44, 48, 56, 64, 72, 80, 100, 120, 140, 160, 200, 240}
	heights := []int{6, 8, 10, 12, 16, 20, 24, 30, 40, 50}
	allViews := []View{ViewOverview, ViewByTool, ViewByModel, ViewBrowse}

	for _, w := range widths {
		for _, h := range heights {
			m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db"})
			tm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m = loadOnce(tm.(Model))
			for _, v := range allViews {
				m.view = v
				m.reload()
				assertNoOverflow(t, m, v, w, h, false)
				// Also exercise the expanded help overlay, which claims body rows.
				m.showHelp = true
				m.layout()
				assertNoOverflow(t, m, v, w, h, true)
				m.showHelp = false
				m.layout()
			}
		}
	}
}

// assertNoOverflow renders m and checks the frame fits within w×h with no panic.
func assertNoOverflow(t *testing.T, m Model, v View, w, h int, help bool) {
	t.Helper()
	var out string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic rendering view %v at %dx%d (help=%v): %v", v, w, h, help, r)
			}
		}()
		out = m.View()
	}()
	if out == "" {
		t.Fatalf("empty frame for view %v at %dx%d (help=%v)", v, w, h, help)
	}
	lines := strings.Split(out, "\n")
	if len(lines) > h {
		t.Fatalf("view %v at %dx%d (help=%v): %d lines > height %d", v, w, h, help, len(lines), h)
	}
	for i, ln := range lines {
		if lw := lipgloss.Width(ln); lw > w {
			t.Fatalf("view %v at %dx%d (help=%v): line %d width %d > %d:\n%q", v, w, h, help, i, lw, w, ln)
		}
	}
}

// TestResponsiveTooSmallCard verifies sub-floor terminals render the resize card
// (not a garbled dashboard) and that it fits exactly.
func TestResponsiveTooSmallCard(t *testing.T) {
	for _, c := range []struct{ w, h int }{{20, 6}, {39, 20}, {120, 9}, {30, 30}} {
		m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db"})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: c.w, Height: c.h})
		m = loadOnce(tm.(Model))
		out := m.View()
		if !strings.Contains(out, "too small") {
			t.Fatalf("%dx%d did not render the resize card:\n%s", c.w, c.h, out)
		}
		for _, ln := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(ln); lw > c.w {
				t.Fatalf("resize card line width %d > %d at %dx%d", lw, c.w, c.w, c.h)
			}
		}
	}
}
