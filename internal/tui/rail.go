package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// rail.go renders the top navigation: a full tab strip when the labels fit, and
// a compact icon row on phone widths. (The former left-hand rail was removed —
// nav is always a top strip.) Both forms mark each entry as a click zone.

// renderTabStrip draws the top tab strip (the primary navigation) as a row of
// chips. Each chip carries a width-invariant marker slot: the active tab wears
// the focus bar, the others a blank cell of the same width — so the strip never
// reflows as the selection moves, and WHICH tab is active survives a monochrome
// terminal, where the accent paint does not. Each tab is a click zone.
func (m Model) renderTabStrip() string {
	var tabs []string
	for _, meta := range viewList {
		// Full label (e.g. "◧ Overview") — the wide strip has room, so no cryptic
		// abbreviations. minTabStripW (64) leaves margin for all four.
		body := meta.glyph + " " + meta.label
		active := meta.v == m.view
		tone := views.ChipCard
		fg := m.th.Muted
		if active {
			tone = views.ChipAccent
		}
		chip := m.vctx.Chip(tone, fg, true, active, body)
		tabs = append(tabs, m.zoneMark(views.RailZone(int(meta.v)), chip))
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
			// The focus bar names the active view here too: at phone width the
			// icons alone are ambiguous once color is gone.
			row += "  " + lipgloss.NewStyle().Foreground(m.th.Accent).Render(views.FocusBar) +
				" " + lipgloss.NewStyle().Foreground(m.th.Text).Bold(true).Render(meta.glyph+" "+meta.label)
			break
		}
	}
	return row
}
