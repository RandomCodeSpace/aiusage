package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// update.go holds the MVU reducer and the keyboard-handling helpers. The root
// Model definition, construction and navigation-state mutations live in app.go;
// mouse handling in mouse.go; selection/scrub plumbing in select.go.

// Update is the MVU reducer.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		// Re-lay-out from the cache; the first real data arrives via the async
		// load cmd kicked in Init, not here, so resize never blocks on I/O.
		m.reload()
		return m, nil

	case dataLoadedMsg:
		return m.handleDataLoaded(msg)

	case refreshTickMsg:
		return m.handleRefreshTick()

	case sysTickMsg:
		return m.handleSysTick()

	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)

	case tea.MouseMsg:
		return m.updateMouse(msg)

	case tea.KeyMsg:
		if m.filtering {
			return m.updateFiltering(msg)
		}
		return m.updateKey(msg)
	}
	return m.forward(msg)
}

// updateFiltering handles keys while the filter input is focused.
func (m Model) updateFiltering(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.filter = strings.TrimSpace(m.filterUI.Value())
		m.filtering = false
		m.filterUI.Blur()
		m.reload()
		return m, nil
	case "esc":
		m.filtering = false
		m.filterUI.Blur()
		m.filterUI.SetValue(m.filter)
		return m, nil
	}
	var cmd tea.Cmd
	m.filterUI, cmd = m.filterUI.Update(msg)
	return m, cmd
}

// updateKey handles global navigation keys.
func (m Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		// Clear the (alt) screen before quitting. On terminals that fully restore
		// the main screen on alt-screen exit this is invisible; on terminals with
		// imperfect alt-screen handling (some mobile SSH clients) it leaves a blank
		// screen instead of the dashboard's leftover ("residue"). ClearScreen only
		// touches the alt buffer, so the main-screen scrollback is never wiped.
		return m, tea.Sequence(tea.ClearScreen, tea.Quit)

	case key.Matches(msg, m.keys.Help):
		m.toggleHelp()
		return m, nil

	case key.Matches(msg, m.keys.NextPane):
		m.setView(nextView(m.view))
		return m, nil
	case key.Matches(msg, m.keys.PrevPane):
		m.setView(prevView(m.view))
		return m, nil

	case key.Matches(msg, m.keys.View1):
		m.setView(ViewOverview)
		return m, nil
	case key.Matches(msg, m.keys.View2):
		m.setView(ViewByTool)
		return m, nil
	case key.Matches(msg, m.keys.View3):
		m.setView(ViewByModel)
		return m, nil
	case key.Matches(msg, m.keys.View4):
		m.setView(ViewBrowse)
		return m, nil

	case key.Matches(msg, m.keys.Range):
		return m.cycleRange(), nil

	case key.Matches(msg, m.keys.Sort):
		m.sort = m.sort.Next()
		m.reload()
		return m, nil

	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		m.filterUI.SetValue(m.filter)
		return m, m.filterUI.Focus()

	case key.Matches(msg, m.keys.Refresh):
		// Force a reload: drop the cache and re-warm off the UI thread. The last
		// frame stays on screen (with a refreshing hint) until the load lands.
		m.data.Invalidate()
		return m, tea.Batch(m.startLoad(), m.spin.Tick)

	case key.Matches(msg, m.keys.Enter):
		return m.drill()

	case key.Matches(msg, m.keys.Back):
		return m.back()

	case key.Matches(msg, m.keys.Left):
		m.handleLeftRight(-1)
		return m, nil
	case key.Matches(msg, m.keys.Right):
		return m.handleRightKey()

	case key.Matches(msg, m.keys.Bottom):
		m.handleEnd()
		return m, nil
	case key.Matches(msg, m.keys.Top):
		m.handleHome()
		return m, nil
	}

	return m.forward(msg)
}

// handleLeftRight scrubs the Overview trend left/right (the only horizontal
// interaction; the other tabs use ↑/↓ on their bars/table).
func (m *Model) handleLeftRight(dir int) {
	if m.view == ViewOverview {
		m.scrubBy(dir)
	}
}

// handleRightKey: on Browse a right-arrow drills (== Enter); otherwise scrub.
func (m Model) handleRightKey() (tea.Model, tea.Cmd) {
	if m.view == ViewBrowse {
		return m.drill()
	}
	m.handleLeftRight(+1)
	return m, nil
}

// handleHome jumps to the start of the focused scrollable / scrubs to the first
// bucket.
func (m *Model) handleHome() {
	switch m.view {
	case ViewOverview:
		m.scrubIndex = 0
		m.scrubPinned = true
		m.syncScrub()
	case ViewBrowse:
		m.browse.SetCursor(0)
		m.syncBrowsePreview()
	case ViewByTool, ViewByModel:
		m.setSelection(0)
	}
}

// handleEnd jumps to the end / scrubs to the live edge (unpins scrub).
func (m *Model) handleEnd() {
	switch m.view {
	case ViewOverview:
		n := len(m.tlData.Buckets)
		if n > 0 {
			m.scrubIndex = n - 1
		}
		m.scrubPinned = false
		m.syncScrub()
	case ViewBrowse:
		m.browse.SetCursor(m.browseRowCount() - 1)
		m.syncBrowsePreview()
	case ViewByTool, ViewByModel:
		m.setSelection(m.selectionCount() - 1)
	}
}

// forward routes navigation keys to the active view's interactive component:
// the Browse table or the By-Tool/By-Model bar selection. The Overview trend is
// driven by handleLeftRight (scrub), not here.
func (m Model) forward(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case ViewBrowse:
		m.browse, cmd = m.browse.Update(msg)
		m.syncBrowsePreview()
	case ViewByTool, ViewByModel:
		m.moveSelectionFromKey(msg)
	}
	return m, cmd
}

// scrubBy moves the scrub crosshair by dir buckets, pinning it, and re-prices
// the dependent panels via syncScrub.
func (m *Model) scrubBy(dir int) {
	n := len(m.tlData.Buckets)
	if n == 0 {
		return
	}
	m.scrubPinned = true
	m.scrubIndex += dir
	if m.scrubIndex < 0 {
		m.scrubIndex = 0
	}
	if m.scrubIndex >= n {
		m.scrubIndex = n - 1
	}
	m.syncScrub()
}

// cycleRange advances the range, resetting scrub + drill path, then reloads and
// persists the new range so it is restored on the next launch.
func (m Model) cycleRange() Model {
	m.rng = m.rng.Next()
	m.crumbs = nil
	m.scrubIndex = 0
	m.scrubPinned = false
	m.persistUI()
	m.reload()
	return m
}
