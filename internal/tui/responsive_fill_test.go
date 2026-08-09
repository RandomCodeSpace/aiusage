package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// TestPanelsFillBodyHeight guards the big-screen responsiveness fix: every
// tab's body must extend to (within a row or two of) the bottom of the region
// it was handed, instead of rendering as a short block that leaves a large band
// of empty terminal below it. Regression target: panels that sized themselves
// to content height and ignored the body height they were given.
//
// It asserts on the body's rendered height rather than on a panel's bottom
// border, because the design language has no per-panel borders any more
// (issue #22) — cards are painted blocks, and a painted blank row and an unpainted
// one are indistinguishable once SGR is stripped.
func TestPanelsFillBodyHeight(t *testing.T) {
	for _, sz := range [][2]int{{120, 30}, {160, 40}, {200, 50}} {
		for _, tab := range []struct{ key, name string }{
			{"1", "Overview"}, {"2", "ByTool"}, {"3", "ByModel"}, {"4", "Sessions"},
		} {
			m := newTestModelWH(t, &fakeData{}, sz[0], sz[1])
			m = step(t, m, keyMsg(tab.key))

			bl := m.bodyLayout()
			// Clamped exactly as render() clamps it, so this measures the shortfall
			// (the bug) and not a view overrunning, which the frame already bounds.
			body := m.clampBlock(m.renderBody(bl), bl.BodyW, bl.BodyH)
			got := lipgloss.Height(body)
			if got < bl.BodyH-2 {
				t.Errorf("%s @%dx%d: body is %d rows of the %d it was handed — not filling the body height",
					tab.name, sz[0], sz[1], got, bl.BodyH)
			}
		}
	}
}
