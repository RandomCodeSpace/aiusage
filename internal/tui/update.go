package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// update.go holds the MVU reducer and the keyboard-handling helpers. The root
// Model definition, construction and navigation-state mutations live in app.go;
// mouse handling in mouse.go; selection/scrub plumbing in select.go.

// Update is the MVU reducer.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Resize is pure relayout: the view-model is size-independent and
		// View() re-renders it at the new geometry (the hero memo is keyed on
		// w/h), so no data reload — and therefore no store access — belongs
		// here on any path, warm or cold.
		m.layout()
		return m, nil

	case dataLoadedMsg:
		return scheduleDetail(m.handleDataLoaded(msg))

	case detailDebounceMsg:
		return m.handleDetailDebounce(msg)

	case detailLoadedMsg:
		return scheduleDetail(m.handleDetailLoaded(msg))

	case refreshTickMsg:
		return m.handleRefreshTick()

	case sysTickMsg:
		return m.handleSysTick(msg)

	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)

	case tea.MouseMsg:
		return scheduleDetail(m.updateMouse(msg))

	case tea.KeyPressMsg:
		if m.filtering {
			return m.updateFiltering(msg)
		}
		return scheduleDetail(m.updateKey(msg))
	}
	return m.forward(msg)
}

// updateFiltering handles keys while the filter input is focused.
func (m Model) updateFiltering(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		// Quit must work while typing, so the chord is matched before the input
		// swallows it. Only the chord: the Quit binding also carries plain "q",
		// which has to stay a typable filter character.
		return m, tea.Sequence(tea.ClearScreen, tea.Quit)
	case "enter":
		m.filter = strings.TrimSpace(m.filterUI.Value())
		m.filtering = false
		m.filterUI.Blur()
		cmd := m.startLoad()
		return m, cmd
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
func (m Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		cmd := m.setView(nextView(m.view))
		return m, cmd
	case key.Matches(msg, m.keys.PrevPane):
		cmd := m.setView(prevView(m.view))
		return m, cmd

	case key.Matches(msg, m.keys.View1):
		cmd := m.setView(ViewOverview)
		return m, cmd
	case key.Matches(msg, m.keys.View2):
		cmd := m.setView(ViewByTool)
		return m, cmd
	case key.Matches(msg, m.keys.View3):
		cmd := m.setView(ViewByModel)
		return m, cmd
	case key.Matches(msg, m.keys.View4):
		cmd := m.setView(ViewBrowse)
		return m, cmd
	case key.Matches(msg, m.keys.View5):
		cmd := m.setView(ViewActivity)
		return m, cmd

	case key.Matches(msg, m.keys.Range):
		return m.cycleRange()

	case key.Matches(msg, m.keys.StepBack):
		return m.stepWindow(-1)
	case key.Matches(msg, m.keys.StepFwd):
		return m.stepWindow(+1)

	case key.Matches(msg, m.keys.Pivot):
		// Pure presentation: the pivot re-reads the timeline already applied, so
		// it never dispatches a load. Only Overview owns a hero.
		if m.view == ViewOverview {
			m.heroPivot = !m.heroPivot
		}
		return m, nil

	case key.Matches(msg, m.keys.Sort):
		m.sort = m.sort.Next()
		cmd := m.startLoad()
		return m, cmd

	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		m.filterUI.SetValue(m.filter)
		return m, m.filterUI.Focus()

	case key.Matches(msg, m.keys.Refresh):
		// Force a reload: drop the cache and re-warm off the UI thread. The last
		// frame stays on screen behind the "◐ sync" chip until the load lands —
		// the chip is the refresh signal, so no spinner tick is re-armed here.
		m.data.Invalidate()
		cmd := m.startLoad()
		return m, cmd

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
	m.rowChosen = false // moved by key, not by a press (see Model.rowChosen)
	switch m.view {
	case ViewOverview:
		m.scrubIndex = 0
		m.scrubPinned = true
		m.syncScrub()
	case ViewBrowse:
		m.browse.SetCursor(0)
		m.syncBrowsePreview()
	case ViewByTool, ViewByModel, ViewActivity:
		m.setSelection(0)
	}
}

// handleEnd jumps to the end / scrubs to the live edge (unpins scrub).
func (m *Model) handleEnd() {
	m.rowChosen = false // moved by key, not by a press (see Model.rowChosen)
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
	case ViewByTool, ViewByModel, ViewActivity:
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
		m.rowChosen = false // moved by key, not by a press (see Model.rowChosen)
	case ViewByTool, ViewByModel, ViewActivity:
		m.moveSelectionFromKey(msg)
		m.rowChosen = false
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

// cycleRange advances the range, resetting scrub + drill path, persists the new
// range so it is restored on the next launch, and dispatches an async load. The
// window steps back to the present: a "7d" step offset means nothing once the
// span changes width.
func (m Model) cycleRange() (Model, tea.Cmd) {
	m.rng = m.rng.Next()
	m.step = 0
	m.crumbs = nil
	m.scrubIndex = 0
	m.scrubPinned = false
	m.syncStepKeys()
	m.persistUI()
	cmd := m.startLoad()
	return m, cmd
}

// stepWindow shifts the reporting window by dir whole calendar spans — one day
// for the day range, one week for 7d, thirty days for 30d — reusing the range's
// local-midnight quantization (issue #4, 1a) so a stepped window lands on the
// same bucket boundaries, and therefore the same kind of stable cache key, as
// the live one. It rides the ordinary async load path: nothing queries here.
//
// Stepping stops at the present — the window never moves past the live one —
// and the open-ended "all" range has no span to step. The drill path survives
// (the same filters over a different window); the scrub pin does not, since the
// timeline underneath it is replaced.
func (m Model) stepWindow(dir int) (Model, tea.Cmd) {
	if !m.rng.Steppable() {
		return m, nil
	}
	next := m.step + dir
	if next > 0 {
		next = 0
	}
	if next == m.step {
		return m, nil
	}
	m.step = next
	m.scrubIndex = 0
	m.scrubPinned = false
	m.syncStepKeys()
	cmd := m.startLoad()
	return m, cmd
}
