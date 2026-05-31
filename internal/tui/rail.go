package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/tui/views"
)

// rail.go renders the top navigation: a full tab strip when the labels fit, and
// a compact icon row on phone widths. (The former left-hand rail was removed —
// nav is always a top strip.) Both forms mark each entry as a click zone.

// renderTabStrip draws the top tab strip (the primary navigation). The active
// view is highlighted; each tab is a click zone.
func (m Model) renderTabStrip() string {
	var tabs []string
	for _, meta := range viewList {
		// Full label (e.g. "◧ Overview") — the wide strip has room, so no cryptic
		// abbreviations. minTabStripW (64) leaves margin for all four.
		label := meta.glyph + " " + meta.label
		s := m.th.TabInactive
		if meta.v == m.view {
			s = m.th.TabActive
		}
		tabs = append(tabs, m.zoneMark(views.RailZone(int(meta.v)), s.Render(label)))
	}
	return strings.Join(tabs, " ")
}

// renderMiniNav draws the phone-width nav: a compact icon row with the active
// view's icon highlighted and its label appended when there is room (the caller
// MaxWidth-clamps the row). Each icon stays a click zone so mouse nav survives.
func (m Model) renderMiniNav() string {
	icons := make([]string, 0, len(viewList))
	for _, meta := range viewList {
		st := m.th.Subtle
		if meta.v == m.view {
			st = lipgloss.NewStyle().Foreground(m.th.Accent).Bold(true)
		}
		icons = append(icons, m.zoneMark(views.RailZone(int(meta.v)), st.Render(meta.glyph)))
	}
	row := strings.Join(icons, " ")
	for _, meta := range viewList {
		if meta.v == m.view {
			row += "  " + lipgloss.NewStyle().Foreground(m.th.Text).Bold(true).Render(meta.label)
			break
		}
	}
	return row
}
