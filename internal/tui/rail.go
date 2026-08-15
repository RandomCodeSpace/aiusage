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
//
// The strip has two rungs, picked by width the way the header picks its range
// chip. Labelled chips are the wide form; below that every chip drops to its
// glyph and the ACTIVE tab's name is appended once. The rung exists because the
// caller MaxWidth-clamps this row: an overflowing strip does not merely look
// cut, it loses the clipped tabs' zone markers, so the last tab would stop
// being clickable at exactly the widths where the fifth one no longer fits.
func (m Model) renderTabStrip() string {
	full := m.tabChips(true)
	// The strip is rendered inside the header bar's Padding(0,1).
	budget := m.frameW() - 2
	if lipgloss.Width(full) <= budget {
		return full
	}
	strip := m.tabChips(false)
	for _, meta := range viewList {
		if meta.v != m.view {
			continue
		}
		name := "  " + lipgloss.NewStyle().Foreground(m.th.Text).Bold(true).Render(meta.glyph+" "+meta.label)
		if lipgloss.Width(strip)+lipgloss.Width(name) <= budget {
			strip += name
		}
		break
	}
	return strip
}

// tabChips renders the chip row, with or without the labels.
func (m Model) tabChips(labels bool) string {
	tabs := make([]string, 0, len(viewList))
	for _, meta := range viewList {
		body := meta.glyph
		if labels {
			body += " " + meta.label
		}
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
