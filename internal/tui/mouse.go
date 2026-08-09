package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// mouse.go is the mouse-first input model resolved on issue #24 and built on
// #23. The bar it was written to is "just like web": if it looks interactive,
// pressing it acts; lists scroll; drill and back behave like link navigation
// with the breadcrumb as history. Keyboard is an accelerator layer over the
// same state, never a requirement.
//
// The contract, uniform across EVERY surface that has rows:
//
//   - A left press SELECTS the row and syncs its detail pane.
//   - A left press on the row that is ALREADY selected DRILLS. Touch therefore
//     has a complete path (tap, tap again) that depends on no double-tap timing.
//   - A double press within doubleClickWindow DRILLS outright.
//   - A right press BACKS OUT, exactly as Escape does.
//
// Wheel notches hit-test the pane UNDER THE POINTER (bubblezone), falling back
// to the active view's one interactive pane when no pane resolves. A notch over
// a chart steps the scrub; over a table or bar list it moves the cursor; over a
// read-only pane it does nothing. It NEVER switches tabs or views — an
// accidental view change costs more than reaching for a key.
//
// Touch rides the same paths: a tap is a press, and a drag across the chart
// arrives either as wheel notches (most mobile SSH clients) or as motion with
// the button held (terminals that translate it), both of which scrub. There is
// deliberately no long-press action — mobile SSH clients hijack it for text
// selection — and hover gates nothing.

// doubleClickWindow is the max gap between two presses on the same zone to
// count as a double-click (drill). Timestamps are fine here — this is a live
// program, not a resumable workflow.
const doubleClickWindow = 400 * time.Millisecond

// updateMouse handles a tea.MouseMsg. All hit-testing goes through the shared
// zone manager; keyboard and mouse mutate the SAME focus/cursor/scrub state so
// they never diverge.
func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			return m.wheel(msg, -1), nil
		case tea.MouseWheelDown:
			return m.wheel(msg, +1), nil
		}
	case tea.MouseClickMsg:
		switch msg.Button {
		case tea.MouseLeft:
			return m.click(msg)
		case tea.MouseRight:
			// Right-press is Back everywhere, on the same contract as Escape: pop
			// the scrub pin, else pop the drill stack. It needs no zone — backing
			// out is the one action that means the same thing over every cell.
			m.dragging = false
			return m.back()
		}
	case tea.MouseMotionMsg:
		return m.drag(msg), nil
	case tea.MouseReleaseMsg:
		m.dragging = false
		return m, nil
	}
	return m, nil
}

// click resolves a left-press to a zone and acts on it.
func (m Model) click(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
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
			cmd := m.setView(View(idx))
			return m, cmd
		}
		return m, nil

	case zid == views.ZoneRangePill:
		return m.cycleRange()

	case zid == views.ZoneHelp:
		m.toggleHelp()
		return m, nil

	case zid == views.ZoneFreshness:
		// The freshness chip is where you act on staleness: force refresh, same
		// contract as the `r` key.
		m.data.Invalidate()
		cmd := m.startLoad()
		return m, cmd

	case zid == views.ZoneSort:
		// The sort label renders as an action chip, so it acts: same contract as
		// the `s` key.
		m.sort = m.sort.Next()
		cmd := m.startLoad()
		return m, cmd

	case zid == views.ZoneFilter:
		// The active-filter chip reopens the input on the current term (`/`).
		m.filtering = true
		m.filterUI.SetValue(m.filter)
		return m, m.filterUI.Focus()

	case strings.HasPrefix(zid, "crumb:"):
		if depth, err := strconv.Atoi(strings.TrimPrefix(zid, "crumb:")); err == nil {
			cmd := m.popCrumbsTo(depth)
			return m, cmd
		}
		return m, nil

	case strings.HasPrefix(zid, "bar:"):
		name := strings.TrimPrefix(zid, "bar:")
		drill := dbl || m.barSelected(name)
		m.selectBar(name)
		if drill {
			return m.drill()
		}
		return m, nil

	case strings.HasPrefix(zid, "row:"):
		idx, err := strconv.Atoi(strings.TrimPrefix(zid, "row:"))
		if err != nil {
			return m, nil
		}
		drill := dbl || m.browse.Cursor() == idx
		m.browse.SetCursor(idx)
		m.syncBrowsePreview()
		if drill {
			return m.drill()
		}
		return m, nil

	default:
		// Clicks on read-only panels do nothing — only the interactive surface
		// (bars/rows, handled above) and the nav/range/help/sort/filter/crumb
		// chrome respond.
		return m, nil
	}
}

// wheelTarget names what a wheel notch acts on.
type wheelTarget int

const (
	// wheelNone: the notch is swallowed — the pane under it does not scroll.
	wheelNone wheelTarget = iota
	// wheelScrub: step the timeline crosshair (a chart is under the pointer).
	wheelScrub
	// wheelBars: move the by-tool / by-model bar selection.
	wheelBars
	// wheelTable: move the Browse table cursor.
	wheelTable
)

// wheel routes a scroll (dir -1 up / +1 down) to whatever the notch resolved to.
func (m Model) wheel(msg tea.MouseMsg, dir int) Model {
	switch m.wheelTarget(msg) {
	case wheelScrub:
		m.scrubBy(dir)
	case wheelBars:
		m.moveSelection(dir)
	case wheelTable:
		m.browse.SetCursor(m.browse.Cursor() + dir)
		m.syncBrowsePreview()
	}
	return m
}

// wheelTarget hit-tests the pane under the pointer and maps it to an action.
// Wheel events carry coordinates even on terminals that never report motion, so
// this resolves the pane the reader is actually pointing at rather than the one
// that happens to hold focus; the focused pane is only the fallback for a notch
// that lands on chrome or arrives with no usable coordinates.
//
// The read-only side panes (Browse preview, by-entity detail) resolve to
// wheelNone on purpose: the wheel acts on the pane under the pointer, and
// nothing on those panes scrolls. Scrolling the table from over the preview
// would be exactly the "acted somewhere else" surprise the hit-test exists to
// remove. No target ever switches tabs or views.
func (m Model) wheelTarget(msg tea.MouseMsg) wheelTarget {
	switch m.paneAt(msg) {
	case views.ZoneHero:
		return wheelScrub
	case views.ZoneBars:
		return wheelBars
	case views.ZoneTable:
		return wheelTable
	case views.ZonePreview:
		return wheelNone
	}
	return m.focusedWheelTarget()
}

// focusedWheelTarget is the fallback: each view has exactly one interactive
// pane, so "the focused pane" is unambiguous.
func (m Model) focusedWheelTarget() wheelTarget {
	switch m.view {
	case ViewOverview:
		return wheelScrub
	case ViewByTool, ViewByModel:
		return wheelBars
	case ViewBrowse:
		return wheelTable
	}
	return wheelNone
}

// drag handles pointer motion with a button held. Cell-motion reporting only
// emits motion while a button is down, so this is only ever a drag — there is no
// hover channel here, and none is wanted: hover may enhance, never gate.
//
// Dragging across the hero chart scrubs a step per column crossed. A drag that
// starts or wanders outside the chart is dropped rather than reinterpreted: the
// touch vocabulary has no drag-to-select.
func (m Model) drag(msg tea.MouseMotionMsg) Model {
	if msg.Button != tea.MouseLeft || m.paneAt(msg) != views.ZoneHero {
		m.dragging = false
		return m
	}
	if m.dragging && msg.X != m.dragX {
		dir := +1
		if msg.X < m.dragX {
			dir = -1
		}
		m.scrubBy(dir)
	}
	m.dragging = true
	m.dragX = msg.X
	return m
}

// zoneAt returns the most specific registered zone id under the mouse, or "".
// It checks the small registry of known ids (cheap; the set is bounded).
func (m Model) zoneAt(msg tea.MouseMsg) string {
	if m.zoneMgr == nil {
		return ""
	}
	// Most specific first: rail entries, then per-item, then chrome chips.
	var candidates []string
	for i := 0; i < int(viewCount); i++ {
		candidates = append(candidates, views.RailZone(i))
	}
	candidates = append(candidates, m.itemZoneCandidates()...)
	candidates = append(candidates,
		views.ZoneRangePill, views.ZoneHelp, views.ZoneFreshness,
		views.ZoneSort, views.ZoneFilter)
	for _, id := range candidates {
		if z := m.zoneMgr.Get(id); !z.IsZero() && z.InBounds(msg) {
			return id
		}
	}
	return ""
}

// paneAt returns the coarse pane zone under the mouse, or "". These are the
// whole-pane marks the views already lay down; the wheel routes on them.
func (m Model) paneAt(msg tea.MouseMsg) string {
	if m.zoneMgr == nil {
		return ""
	}
	for _, id := range []string{views.ZoneHero, views.ZoneBars, views.ZoneTable, views.ZonePreview} {
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
