package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// View wraps the rendered frame in a tea.View that declares the terminal modes
// the dashboard needs: the alt screen and cell-motion mouse reporting (clicks +
// wheel on the nav, tabs, rows, bars and KPI tiles).
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// render renders the whole frame. The nav adapts to width (left rail → top tab
// strip → phone-width icon row), chrome rows fold out when height is tight, the
// body is sized by the central layout, and the whole frame is bounded to the
// terminal so nothing ever overflows. Below the usable floor a resize card is
// shown instead. The frame is run through the shared zone manager's Scan so the
// rail/tabs, rows, bars, KPI tiles and breadcrumbs stay mouse-resolvable.
func (m Model) render() string {
	if m.width == 0 || m.height == 0 {
		// Pre-size frame: renderLoading is the ONE loading path and degrades to
		// its own bare-text form at zero size — no second hand-rolled string.
		return m.renderLoading()
	}
	// Below the absolute floor nothing fits — show a resize card and stop. The
	// card owns the raw terminal: it is not worth spending two rows on the app
	// frame when the message is "there is no room".
	if m.lay.TooSmall {
		return m.scan(m.renderTooSmall())
	}
	// Until the first dataset applies, the branded loading state owns the frame
	// (the program is already open and interactive; this never blocks on a
	// query). A cold FAILURE falls through instead: the dashboard chrome renders
	// with the full-body error panel — the only place that panel exists.
	if m.fresh == FreshCold && m.err == nil {
		return m.scan(m.appFrame(m.renderLoading()))
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
	if m.bannerRows() > 0 {
		rows = append(rows, m.renderStallBanner())
	}
	rows = append(rows, body)

	if m.showHelp {
		rows = append(rows, m.clampBlock(m.renderHelpOverlay(), m.frameW(), m.helpRows()))
	}
	if m.lay.ShowFooter {
		rows = append(rows, m.renderFooter())
	}

	return m.scan(m.appFrame(m.clampFrame(lipgloss.JoinVertical(lipgloss.Left, rows...))))
}

// appFrame wraps the assembled chrome in the ONE border the design language
// allows (issue #22). The interior is exactly frameW×frameH — which is what
// ComputeLayout was handed, so nothing inside can overflow it.
//
// The border is drawn by hand rather than through a bordered lipgloss style.
// That style re-lays out every line of the frame through the generic
// border/padding/width machinery, and on a 120x40 dashboard it measured 1.4ms
// and 19k allocations per View — a third of the whole frame budget for two
// glyphs a row. Here each border cell is rendered once and reused, and the only
// per-line work is a width scan and one padding run.
func (m Model) appFrame(inner string) string {
	w, h := m.frameW(), m.frameH()
	edge := m.th.AppFrame()
	side := edge.Render("│")
	ground := lipgloss.NewStyle().Background(m.th.Bg)

	var b strings.Builder
	b.Grow(len(inner) + h*(2*len(side)+8))
	b.WriteString(edge.Render("╭" + strings.Repeat("─", w) + "╮"))

	lines := strings.Split(inner, "\n")
	for i := 0; i < h; i++ {
		row := ""
		if i < len(lines) {
			row = lines[i]
		}
		b.WriteString("\n")
		b.WriteString(side)
		b.WriteString(row)
		if pad := w - lipgloss.Width(row); pad > 0 {
			b.WriteString(ground.Render(strings.Repeat(" ", pad)))
		}
		b.WriteString(side)
	}

	b.WriteString("\n")
	b.WriteString(edge.Render("╰" + strings.Repeat("─", w) + "╯"))
	return b.String()
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

// clampFrame bounds the assembled chrome to the app frame's interior as a final
// overflow guard, before the frame border is drawn around it.
func (m Model) clampFrame(s string) string {
	return lipgloss.NewStyle().MaxWidth(m.frameW()).MaxHeight(m.frameH()).Render(s)
}

// tabStripRow wraps the medium-width tab strip in the header bar, width-clamped.
func (m Model) tabStripRow() string {
	bar := m.th.HeaderBar.Render(m.renderTabStrip())
	return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(bar)
}

// miniNavRow wraps the phone-width icon nav in the header bar, width-clamped.
func (m Model) miniNavRow() string {
	bar := m.th.HeaderBar.Render(m.renderMiniNav())
	return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(bar)
}

// renderTooSmall renders the centered resize card shown below the usable floor.
// Each line is truncated to the terminal width BEFORE centering so a long line
// can never wrap mid-word, then the block is placed in the exact w×h frame.
//
// The target it prints is the TERMINAL floor (MinTermW/H), not the layout's
// interior floor: the card is compared against the size the reader can see and
// change, and the app frame costs two cells the interior minimums do not name.
func (m Model) renderTooSmall() string {
	w := m.width
	block := lipgloss.JoinVertical(lipgloss.Center,
		m.th.Title.Render(Truncate("terminal too small", w)),
		m.th.Subtle.Render(Truncate("resize to ≥ "+strconv.Itoa(views.MinTermW)+"×"+strconv.Itoa(views.MinTermH), w)),
		m.th.Subtle.Render(Truncate("now "+strconv.Itoa(m.width)+"×"+strconv.Itoa(m.height), w)),
	)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
}

func (m Model) renderHeader() string {
	// iw is the content area inside the bar's Padding(0,1).
	iw := m.frameW() - 2
	if iw < 1 {
		iw = 1
	}

	wordmark := m.th.Wordmark.Render("◧ aiusage")
	left := wordmark
	if !m.compact() {
		left += " " + m.th.Subtle.Render("command center")
	}

	// The range chip carries the WINDOW, not just the range: once [ / ] step it
	// back the label names the window's first day, so a past window is never read
	// as the live one. The guillemets are the chip's mono channel — they say the
	// window is steppable.
	rangePill := m.zoneMark(views.ZoneRangePill,
		m.headerChip("RANGE ‹ "+m.spanLabel()+" ›", m.th.Accent))
	help := m.zoneMark(views.ZoneHelp, m.headerChip("? help", m.th.Muted))

	right := ""
	// Ingest heartbeat (real collector pulse) + query-freshness chip. The chip
	// is a click zone: the indicator is where you act (left-press = force
	// refresh). Never blanks the frame.
	right += m.heartbeatCell() + " "
	right += m.zoneMark(views.ZoneFreshness, m.freshnessChip()) + " "
	right += rangePill
	if m.reducedMotion {
		// Surface the reduced-motion state; the dashboard renders all charts
		// instantly with no animation when this is set (NO_COLOR /
		// AIUSAGE_REDUCED_MOTION), so motion never adds input latency.
		right += " " + m.headerChip("·still·", m.th.Muted)
	}
	// The db path is the first thing to drop when space is tight; only show it
	// when the wordmark + range + help + path comfortably fit.
	if m.dbPath != "" {
		path := m.th.Subtle.Render("  " + Truncate(m.dbPath, 40))
		if lipgloss.Width(left)+lipgloss.Width(right)+lipgloss.Width(path)+lipgloss.Width(help)+3 <= iw {
			right += path
		}
	}
	right += " " + help

	gap := iw - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	bar := m.th.HeaderBar.Render(left + strings.Repeat(" ", gap) + right)
	return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(bar)
}

// headerChip renders a state/range/action chip on the header bar: one cell of
// padding either side, painted at the chip step of the elevation ladder. The
// glyph+word inside is what survives a monochrome terminal; the paint is what
// makes it read as a chip on a color one.
func (m Model) headerChip(body string, fg compat.AdaptiveColor) string {
	return m.vctx.Chip(views.ChipTop, fg, false, false, body)
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
	// The sort chip is an action chip, so it is a press target: the zone wraps the
	// WHOLE chip, padding included, not just the label.
	sortLbl := m.zoneMark(views.ZoneSort, m.headerChip("sort "+m.sort.Label(), m.th.Accent))
	iw := m.frameW() - 2
	if iw < 1 {
		iw = 1
	}
	gap := iw - lipgloss.Width(crumb) - lipgloss.Width(sortLbl)
	if gap < 1 {
		gap = 1
	}
	bar := m.th.FooterBar.Render(crumb + strings.Repeat(" ", gap) + sortLbl)
	return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(bar)
}

// renderBody renders the active view into the body region described by lay.
// The full-body error panel exists ONLY while cold: with a prior good frame,
// handleDataLoaded holds the picture and the failure lives in the freshness
// chip — four healthy panels are never blanked for one failed load.
func (m Model) renderBody(lay views.Layout) string {
	if m.err != nil && m.fresh == FreshCold {
		return m.renderErrorPanel(lay)
	}
	switch m.view {
	case ViewOverview:
		ov := m.overview
		ov.Sys = m.sysGauges() // inject live resource gauges at render time
		ov.Gen = m.dataGen     // render-memo dataset identity (applied generation)
		ov.Memo = m.heroMemo
		ov.Mode = m.heroMode()
		return views.Overview(m.vctx, ov, lay)
	case ViewByTool:
		return views.ByTool(m.vctx, m.byTool, lay)
	case ViewByModel:
		return views.ByModel(m.vctx, m.byModel, lay)
	case ViewBrowse:
		b := m.browse
		// The chevron must state the truth for the CURRENT depth, and the depth is
		// navigation state rather than loaded data, so it is injected at render
		// time (like the Overview sys gauges) instead of being carried through the
		// load path where a stale value could outlive a drill.
		b.SetDrillable(m.browseDrillable())
		return b.View()
	}
	return ""
}

func (m Model) renderFooter() string {
	if m.filtering {
		bar := m.th.FooterBar.Render(m.filterUI.View())
		return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(bar)
	}
	m.help.ShowAll = false // footer is ALWAYS the one-line hint; full help lives in the overlay
	hint := m.help.View(m.keys)
	if m.filter != "" {
		hint = m.zoneMark(views.ZoneFilter, m.headerChip("filter:"+m.filter, m.th.Accent)) + " " + hint
	}
	bar := m.th.FooterBar.Render(hint)
	return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(bar)
}

// renderHelpOverlay renders the expanded help as a painted card (no border —
// the app frame is the only box).
func (m Model) renderHelpOverlay() string {
	m.help.ShowAll = true
	content := m.help.View(m.keys)
	w := m.frameW()
	if w < 3 {
		w = 3
	}
	panel := m.th.Idle().Width(w).Render(content)
	return lipgloss.NewStyle().MaxWidth(m.frameW()).Render(panel)
}
