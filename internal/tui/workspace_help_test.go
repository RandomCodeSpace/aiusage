package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestWorkspaceHelpBudgetMatchesRenderedRows(t *testing.T) {
	for _, size := range [][2]int{{42, 12}, {55, 52}, {120, 40}, {200, 60}} {
		for _, window := range []struct {
			rng  Range
			step int
		}{{Range7d, 0}, {Range7d, -1}, {RangeAll, 0}} {
			t.Run(fmt.Sprintf("%dx%d/%d/%d", size[0], size[1], window.rng, window.step), func(t *testing.T) {
				_, m := newWorkspaceModel(t)
				m.rng, m.step = window.rng, window.step
				m.syncStepKeys()
				m = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				m = send(m, keyMsg("?"))
				want := min(lipgloss.Height(m.renderHelpOverlay()), max(0, m.lay.BodyH-1))
				if m.helpRows() != want {
					t.Fatalf("help budget %d, rendered rows %d", m.helpRows(), want)
				}
				frame := m.View().Content
				if lipgloss.Width(frame) > size[0] || lipgloss.Height(frame) > size[1] {
					t.Fatal("help overflowed the viewport")
				}
			})
		}
	}
}
