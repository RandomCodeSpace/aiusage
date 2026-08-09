package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// detail.go wires the debounced, coalescing background loads behind the
// selection / scrub / preview interactions.
//
// Flow:
//
//	interaction (key, wheel, click)  → cache-only sync; a miss sets detailWanted
//	scheduleDetail                   → bumps detailSeq, arms a debounce timer
//	detailDebounceMsg (after ~75ms)  → stale seq drops (coalescing); current seq
//	                                   dispatches detailLoadCmd
//	detailLoadCmd (goroutine)        → querying loader on a model copy, warming
//	                                   the shared cache → detailLoadedMsg
//	detailLoadedMsg                  → stale gen/seq drops; otherwise re-run the
//	                                   cache-only sync (now warm) on the UI thread
//
// Coalescing, not just debouncing: every miss bumps detailSeq, so a burst of
// wheel notches leaves a trail of stale timers that all drop on arrival — only
// the LAST timer dispatches, and it dispatches for the final position. A sweep
// therefore costs at most one background flight, never one query per notch.
// Flights also carry the load generation (see live.go): a navigation or
// refresh supersedes any in-flight detail load.

// detailDebounce is how long an interaction burst has to go quiet before the
// coalesced background load dispatches.
const detailDebounce = 75 * time.Millisecond

// detailDebounceMsg fires when a debounce window closes. seq identifies the
// interaction that armed it; a stale seq means a later interaction superseded
// this timer.
type detailDebounceMsg struct{ seq uint64 }

// detailLoadedMsg signals a finished detail flight for (gen, seq). failed
// carries the flight's query failure back to the UI thread: the querying
// loaders record it only on the flight's discarded model copy, so without this
// bit the live model could never render the per-pane query-failed treatment.
type detailLoadedMsg struct {
	gen, seq uint64
	failed   bool
}

// detailDebounceCmd arms the debounce timer for seq.
func detailDebounceCmd(seq uint64) tea.Cmd {
	return tea.Tick(detailDebounce, func(time.Time) tea.Msg { return detailDebounceMsg{seq: seq} })
}

// scheduleDetail converts a detailWanted mark left by a cache-only sync into a
// debounced dispatch. It wraps the interaction handlers' returns in Update so
// no individual handler has to thread the debounce cmd through its call chain.
func scheduleDetail(tm tea.Model, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	m, ok := tm.(Model)
	if !ok || !m.detailWanted {
		return tm, cmd
	}
	m.detailWanted = false
	m.detailSeq++
	dc := detailDebounceCmd(m.detailSeq)
	if cmd == nil {
		return m, dc
	}
	return m, tea.Batch(cmd, dc)
}

// handleDetailDebounce dispatches the coalesced background load, unless a
// later interaction already superseded this timer.
func (m Model) handleDetailDebounce(msg detailDebounceMsg) (Model, tea.Cmd) {
	if msg.seq != m.detailSeq {
		return m, nil
	}
	return m, m.detailLoadCmd()
}

// detailLoadCmd runs the querying loader for the CURRENT selection / scrub
// position OFF the UI thread — by debounce time a queued burst has already
// collapsed to its final state. Like loadCmd it queries on a throwaway copy of
// the model that shares the live *Data cache.
func (m Model) detailLoadCmd() tea.Cmd {
	mc := m
	gen, seq := m.loadGen, m.detailSeq
	return func() tea.Msg {
		mc.loadDetail()
		return detailLoadedMsg{gen: gen, seq: seq, failed: mc.detailFailed()}
	}
}

// detailFailed reports whether the active view's detail loader recorded a
// query failure — read off the flight's model copy, whose flags are otherwise
// discarded with the copy.
func (m *Model) detailFailed() bool {
	switch m.view {
	case ViewOverview:
		return m.err != nil
	case ViewByTool:
		return m.byTool.SelTrendErr
	case ViewByModel:
		return m.byModel.SelTrendErr
	case ViewBrowse:
		return m.browse.PreviewErr()
	}
	return false
}

// loadDetail runs the active view's querying loader (background flights only).
// Overview runs its full loader: the spring-back reprice needs the full-range
// summaries and the scrub composition warmed together.
func (m *Model) loadDetail() {
	switch m.view {
	case ViewOverview:
		m.loadOverview()
	case ViewByTool:
		m.loadByToolDetail()
	case ViewByModel:
		m.loadByModelDetail()
	case ViewBrowse:
		m.loadBrowsePreview()
	}
}

// handleDetailLoaded applies a finished detail flight by re-running the
// UI-thread sync against the now-warm cache. A stale generation (navigation or
// refresh superseded the flight) or stale sequence drops it whole.
func (m Model) handleDetailLoaded(msg detailLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.loadGen || msg.seq != m.detailSeq {
		return m, nil
	}
	switch m.view {
	case ViewOverview:
		// Rebuild the overview coherently from the warm cache: the flight ran
		// the full loader, so this re-derives timeline, composition and totals
		// without touching SQLite (mirrors handleDataLoaded's reload).
		m.loadOverview()
	case ViewByTool:
		m.syncByToolDetail()
	case ViewByModel:
		m.syncByModelDetail()
	case ViewBrowse:
		m.syncBrowsePreview()
	}
	// A failed flight left the cache cold, so the sync twin above just missed
	// and re-armed detailWanted — scheduling again would redispatch the failing
	// query every debounce interval. Render the honest per-pane failure instead;
	// any later successful sync or load clears it. Overview is excluded: it has
	// no per-pane flag and its failure surfaces through m.err.
	if msg.failed && m.detailWanted && m.view != ViewOverview {
		m.detailWanted = false
		m.failDetailPane()
	}
	return m, nil
}

// failDetailPane sets the active view's per-pane query-failed treatment,
// dropping any held stale trend so the failure never renders as good data.
func (m *Model) failDetailPane() {
	switch m.view {
	case ViewByTool:
		m.byTool.SelTrend = nil
		m.byTool.SelTrendErr = true
	case ViewByModel:
		m.byModel.SelTrend = nil
		m.byModel.SelTrendErr = true
	case ViewBrowse:
		m.browse.SetPreview(nil)
		m.browse.SetPreviewErr(true)
	}
}
