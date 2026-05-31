package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/tui/views"
)

// View renders the whole frame. The nav adapts to width (left rail → top tab
// strip → phone-width icon row), chrome rows fold out when height is tight, the
// body is sized by the central layout, and the whole frame is bounded to the
// terminal so nothing ever overflows. Below the usable floor a resize card is
// shown instead. The frame is run through the shared zone manager's Scan so the
// rail/tabs, rows, bars, KPI tiles and breadcrumbs stay mouse-resolvable.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "loading…"
	}
	// Below the absolute floor nothing fits — show a resize card and stop.
	if m.lay.TooSmall {
		return m.scan(m.renderTooSmall())
	}
	// Until the first background load lands, show the branded loading state. The
	// program is already open and interactive; this never blocks on a query.
	if !m.loaded {
		return m.scan(m.clampFrame(m.renderLoading()))
	}

	bl := m.bodyLayout()
	bodyH := bl.BodyH
	body := m.clampBlock(m.renderBody(bl), bl.BodyW, bodyH)

	rows := make([]string, 0, 6)
	if m.lay.ShowHeader {
		rows = append(rows, m.renderHeader())
	}

	// Navigation is a top strip in every layout: a full tab strip while the
	// labels fit, else a compact icon row on phone widths.
	if m.lay.Nav == views.NavMini {
		rows = append(rows, m.miniNavRow())
	} else {
		rows = append(rows, m.tabStripRow())
	}
	if m.lay.ShowBreadcrumb {
		rows = append(rows, m.renderBreadcrumb())
	}
	rows = append(rows, body)

	if m.showHelp {
		rows = append(rows, m.clampBlock(m.renderHelpOverlay(), m.width, m.helpRows()))
	}
	if m.lay.ShowFooter {
		rows = append(rows, m.renderFooter())
	}

	return m.scan(m.clampFrame(lipgloss.JoinVertical(lipgloss.Left, rows...)))
}

// scan runs the assembled frame through the shared zone manager (no-op headless).
func (m Model) scan(frame string) string {
	if m.zoneMgr != nil {
		return m.zoneMgr.Scan(frame)
	}
	return frame
}

// clampBlock bounds a block to w×h cells (ANSI-aware) so a miscomputing view can
// never push a line past its column budget or shove sibling rows off-screen.
func (m Model) clampBlock(s string, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return lipgloss.NewStyle().MaxWidth(w).MaxHeight(h).Render(s)
}

// clampFrame bounds the whole frame to the terminal as a final overflow guard.
func (m Model) clampFrame(s string) string {
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(s)
}

// tabStripRow wraps the medium-width tab strip in the header bar, width-clamped.
func (m Model) tabStripRow() string {
	bar := m.th.HeaderBar.Render(m.renderTabStrip())
	return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
}

// miniNavRow wraps the phone-width icon nav in the header bar, width-clamped.
func (m Model) miniNavRow() string {
	bar := m.th.HeaderBar.Render(m.renderMiniNav())
	return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
}

// renderTooSmall renders the centered resize card shown below the usable floor.
// Each line is truncated to the terminal width BEFORE centering so a long line
// can never wrap mid-word, then the block is placed in the exact w×h frame.
func (m Model) renderTooSmall() string {
	w := m.width
	block := lipgloss.JoinVertical(lipgloss.Center,
		m.th.Title.Render(Truncate("terminal too small", w)),
		m.th.Subtle.Render(Truncate("resize to ≥ "+strconv.Itoa(views.MinUsableW)+"×"+strconv.Itoa(views.MinUsableH), w)),
		m.th.Subtle.Render(Truncate("now "+strconv.Itoa(m.width)+"×"+strconv.Itoa(m.height), w)),
	)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
}

func (m Model) renderHeader() string {
	// iw is the content area inside the bar's Padding(0,1).
	iw := m.width - 2
	if iw < 1 {
		iw = 1
	}

	wordmark := m.th.PanelTitle.Render("◧ aiusage")
	left := wordmark
	if !m.compact() {
		left += " " + m.th.Subtle.Render("command center")
	}

	rangePill := m.zoneMark(views.ZoneRangePill,
		m.th.Subtle.Render("RANGE ")+m.th.CrumbActive.Render("‹ "+m.rng.Label()+" ›"))
	help := m.zoneMark(views.ZoneHelp, m.th.Subtle.Render("? help"))

	right := ""
	// Subtle live/refreshing indicator: a spinner glyph while a background load
	// is in flight, otherwise a steady "live" dot. Never blanks the frame.
	if m.loading {
		right += m.spin.View() + lipgloss.NewStyle().Foreground(m.th.Now).Render("refreshing") + "  "
	} else {
		right += lipgloss.NewStyle().Foreground(m.th.Positive).Render("● live") + "  "
	}
	right += rangePill
	if m.reducedMotion {
		// Surface the reduced-motion state; the dashboard renders all charts
		// instantly with no animation when this is set (NO_COLOR /
		// AIUSAGE_REDUCED_MOTION), so motion never adds input latency.
		right += "  " + m.th.Subtle.Render("·still·")
	}
	// The db path is the first thing to drop when space is tight; only show it
	// when the wordmark + range + help + path comfortably fit.
	if m.dbPath != "" {
		path := m.th.Subtle.Render("  " + Truncate(m.dbPath, 40))
		if lipgloss.Width(left)+lipgloss.Width(right)+lipgloss.Width(path)+lipgloss.Width(help)+3 <= iw {
			right += path
		}
	}
	right += "  " + help

	gap := iw - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	bar := m.th.HeaderBar.Render(left + strings.Repeat(" ", gap) + right)
	return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
}

func (m Model) renderBreadcrumb() string {
	parts := []string{m.zoneMark(views.CrumbZone(0), m.th.CrumbActive.Render("all"))}
	for i, c := range m.crumbs {
		parts = append(parts, m.zoneMark(views.CrumbZone(i+1), m.th.Crumb.Render(c.Dim+":"+c.Value)))
	}
	crumb := strings.Join(parts, m.th.Subtle.Render(" › "))
	if m.scrubPinned && m.overview.ScrubLabel != "" && m.view == ViewOverview {
		crumb += m.th.Subtle.Render(" · ") + lipgloss.NewStyle().Foreground(m.th.Now).Render("◷ "+m.overview.ScrubLabel)
	}
	sortLbl := m.th.Subtle.Render("sort ") + m.th.CrumbActive.Render(m.sort.Label())
	iw := m.width - 2
	if iw < 1 {
		iw = 1
	}
	gap := iw - lipgloss.Width(crumb) - lipgloss.Width(sortLbl)
	if gap < 1 {
		gap = 1
	}
	bar := m.th.FooterBar.Render(crumb + strings.Repeat(" ", gap) + sortLbl)
	return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
}

// renderBody renders the active view into the body region described by lay.
func (m Model) renderBody(lay views.Layout) string {
	if m.err != nil {
		w := lay.BodyW - 2
		if w < 1 {
			w = 1
		}
		return m.th.Errored().Width(w).Render(
			lipgloss.NewStyle().Foreground(m.th.Warn).Render("error: " + m.err.Error()),
		)
	}
	switch m.view {
	case ViewOverview:
		ov := m.overview
		ov.Sys = m.sysGauges() // inject live resource gauges at render time
		return views.Overview(m.vctx, ov, lay)
	case ViewByTool:
		return views.ByTool(m.vctx, m.byTool, lay)
	case ViewByModel:
		return views.ByModel(m.vctx, m.byModel, lay)
	case ViewBrowse:
		return m.browse.View()
	}
	return ""
}

func (m Model) renderFooter() string {
	if m.filtering {
		bar := m.th.FooterBar.Render(m.filterUI.View())
		return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
	}
	m.help.ShowAll = false // footer is ALWAYS the one-line hint; full help lives in the overlay
	hint := m.help.View(m.keys)
	if m.filter != "" {
		hint = m.th.CrumbActive.Render("filter:"+m.filter) + "  " + hint
	}
	bar := m.th.FooterBar.Render(hint)
	return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
}

// renderHelpOverlay renders the expanded help in a bordered panel.
func (m Model) renderHelpOverlay() string {
	m.help.ShowAll = true
	content := m.help.View(m.keys)
	w := m.width - 4
	if w < 1 {
		w = 1
	}
	panel := m.th.Idle().Width(w).Render(content)
	return lipgloss.NewStyle().MaxWidth(m.width).Render(panel)
}
