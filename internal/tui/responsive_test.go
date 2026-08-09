package tui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

var ansiResp = regexp.MustCompile("\x1b\\[[0-9;:]*m")

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

// TestResponsiveNoOverflowTallLists runs the same gate on groupings taller than
// any pane, where the Browse table and the bars panel are windowed. Overflow and
// windowing are the same question asked twice: the window exists so the frame
// clamp never has to cut anything, so if a windowed pane still overflows, the
// window is sized wrong.
func TestResponsiveNoOverflowTallLists(t *testing.T) {
	for _, w := range []int{40, 56, 80, 120, 160, 240} {
		for _, h := range []int{12, 16, 24, 40, 50} {
			m := NewModel(&wideData{}, Options{DBPath: "/tmp/usage.db"})
			tm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m = loadOnce(tm.(Model))
			for _, v := range []View{ViewByTool, ViewByModel, ViewBrowse} {
				m.view = v
				m.reload()
				// Both ends of the list: the last page is the short one.
				for _, at := range []int{0, wideRows - 1} {
					m.browse.SetCursor(at)
					m.byTool.Selected, m.byModel.Selected = at, at
					assertNoOverflow(t, m, v, w, h, false)
				}
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
		out = m.View().Content
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
		out := m.View().Content
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

// TestTooSmallCardTargetIsReachable: the size the card tells the reader to
// resize to must actually clear the floor, and one cell under it must not. The
// card prints a terminal size while the layout's minimums describe the app
// frame's INTERIOR, so the two numbers differ by the frame — which is exactly
// the way this message goes stale.
func TestTooSmallCardTargetIsReachable(t *testing.T) {
	target := "resize to ≥ " + strconv.Itoa(views.MinTermW) + "×" + strconv.Itoa(views.MinTermH)

	sub := newSizedModel(t, views.MinTermW-1, views.MinTermH-1)
	out := sub.View().Content
	if !strings.Contains(out, "too small") {
		t.Fatalf("%dx%d does not render the resize card", views.MinTermW-1, views.MinTermH-1)
	}
	if !strings.Contains(ansiResp.ReplaceAllString(out, ""), target) {
		t.Fatalf("resize card does not advertise %q:\n%s", target, out)
	}

	at := newSizedModel(t, views.MinTermW, views.MinTermH)
	if at.lay.TooSmall {
		t.Fatalf("the advertised %dx%d is still below the floor — the card asks for a size it rejects",
			views.MinTermW, views.MinTermH)
	}
	if strings.Contains(at.View().Content, "too small") {
		t.Fatalf("the advertised %dx%d still renders the resize card", views.MinTermW, views.MinTermH)
	}
	for _, c := range []struct{ w, h int }{{views.MinTermW - 1, views.MinTermH}, {views.MinTermW, views.MinTermH - 1}} {
		if !newSizedModel(t, c.w, c.h).lay.TooSmall {
			t.Fatalf("%dx%d is one cell under the advertised floor but renders the dashboard", c.w, c.h)
		}
	}
}

// newSizedModel builds a loaded model at an exact terminal size.
func newSizedModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return loadOnce(tm.(Model))
}
