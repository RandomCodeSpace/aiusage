package tui

import (
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// doubleClickWindow is the max gap between two presses on the same zone to
// count as a double-click (drill). Timestamps are fine here — this is a live
// program, not a resumable workflow.
const doubleClickWindow = 400 * time.Millisecond

// updateMouse handles a tea.MouseMsg. All hit-testing goes through the shared
// zone manager; keyboard and mouse mutate the SAME focus/cursor/scrub state so
// they never diverge. Drag events are ignored.
func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.wheel(-1), nil
	case tea.MouseButtonWheelDown:
		return m.wheel(+1), nil
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress {
			return m.click(msg)
		}
	}
	return m, nil
}

// click resolves a left-press to a zone and acts on it.
func (m Model) click(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	zid := m.zoneAt(msg)
	if zid == "" {
		return m, nil
	}

	dbl := m.isDoubleClick(zid)
	m.lastClickZone = zid
	m.lastClickAt = time.Now()

	switch {
	case strings.HasPrefix(zid, "rail:"):
		if idx, err := strconv.Atoi(strings.TrimPrefix(zid, "rail:")); err == nil && idx >= 0 && idx < int(viewCount) {
			m.setView(View(idx))
		}
		return m, nil

	case zid == views.ZoneRangePill:
		return m.cycleRange(), nil

	case zid == views.ZoneHelp:
		m.toggleHelp()
		return m, nil

	case strings.HasPrefix(zid, "crumb:"):
		if depth, err := strconv.Atoi(strings.TrimPrefix(zid, "crumb:")); err == nil {
			m.popCrumbsTo(depth)
		}
		return m, nil

	case strings.HasPrefix(zid, "bar:"):
		name := strings.TrimPrefix(zid, "bar:")
		m.selectBar(name)
		if dbl {
			return m.drill()
		}
		return m, nil

	case strings.HasPrefix(zid, "row:"):
		if idx, err := strconv.Atoi(strings.TrimPrefix(zid, "row:")); err == nil {
			m.browse.SetCursor(idx)
			m.syncBrowsePreview()
			if dbl {
				return m.drill()
			}
		}
		return m, nil

	default:
		// Clicks on read-only panels do nothing — only the interactive surface
		// (bars/rows, handled above) and the nav/range/help/crumb chrome respond.
		return m, nil
	}
}

// wheel routes a scroll (dir -1 up / +1 down) to the focused scrollable, or
// scrubs the chart when a chart pane is focused.
func (m Model) wheel(dir int) Model {
	switch m.view {
	case ViewOverview:
		m.scrubBy(dir)
	case ViewBrowse:
		if dir < 0 {
			m.browse.SetCursor(m.browse.Cursor() - 1)
		} else {
			m.browse.SetCursor(m.browse.Cursor() + 1)
		}
		m.syncBrowsePreview()
	case ViewByTool, ViewByModel:
		m.moveSelection(dir)
	}
	return m
}

// zoneAt returns the most specific registered zone id under the mouse, or "".
// It checks the small registry of known ids (cheap; the set is bounded).
func (m Model) zoneAt(msg tea.MouseMsg) string {
	if m.zoneMgr == nil {
		return ""
	}
	// Most specific first: rail entries, then per-item, then pane bodies.
	var candidates []string
	for i := 0; i < int(viewCount); i++ {
		candidates = append(candidates, views.RailZone(i))
	}
	candidates = append(candidates, m.itemZoneCandidates()...)
	candidates = append(candidates, views.ZoneRangePill, views.ZoneHelp)
	for _, id := range candidates {
		if z := m.zoneMgr.Get(id); !z.IsZero() && z.InBounds(msg) {
			return id
		}
	}
	return ""
}

// itemZoneCandidates lists the per-item zones currently on screen (bars, rows,
// crumbs) so zoneAt can resolve them before the coarser pane-body zones.
func (m Model) itemZoneCandidates() []string {
	var out []string
	// Breadcrumbs.
	for i := 0; i <= len(m.crumbs); i++ {
		out = append(out, views.CrumbZone(i))
	}
	switch m.view {
	case ViewByTool:
		for _, b := range m.byTool.Rows {
			out = append(out, views.BarZone(b.Keys["tool"]))
		}
	case ViewByModel:
		for _, b := range m.byModel.Rows {
			out = append(out, views.BarZone(b.Keys["model"]))
		}
	case ViewBrowse:
		for i := 0; i < m.browseRowCount(); i++ {
			out = append(out, views.RowZone(i))
		}
	}
	return out
}

// isDoubleClick reports whether the current press on zid is the second of a
// double-click within the window.
func (m Model) isDoubleClick(zid string) bool {
	return zid == m.lastClickZone && time.Since(m.lastClickAt) <= doubleClickWindow
}
