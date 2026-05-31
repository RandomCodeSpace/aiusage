package tui

import (
	"os"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// live.go wires instant-open + async loading + low-CPU live refresh.
//
// Flow:
//
//	Init → tea.Batch(spinner.Tick, loadCmd, refreshTickCmd)
//	  loadCmd      (goroutine) warms the shared query cache → dataLoadedMsg
//	  dataLoadedMsg                → reload() from warm cache, mark loaded, stop spinner
//	  refreshTickMsg (every 10s)   → os.Stat(db); reload only if mtime changed; re-arm tick
//	  spinner.TickMsg              → advance frame only while still loading
//	  manual `r`                   → force reload (Invalidate + loadCmd)
//
// Idle cost: once loaded, the spinner stops ticking and the ONLY recurring
// command is a single os.Stat per 10s. No time.Sleep, no busy loop. A reload
// fires solely when the db file's mtime advances (the daemon wrote new events).

// refreshInterval is how often we stat the db for a live update. 10s keeps idle
// CPU near zero while still feeling live for a usage dashboard.
const refreshInterval = 10 * time.Second

// dataLoadedMsg signals that a background load finished warming the query cache
// for the captured (view, range, filter, drill) snapshot. Update applies it by
// running reload() against the now-warm cache on the UI thread (no I/O there).
type dataLoadedMsg struct {
	// mtime is the db file's modification time observed at load dispatch, used
	// to gate future refresh ticks. Zero when the file could not be stat'd.
	mtime time.Time
}

// refreshTickMsg fires every refreshInterval to drive the live mtime poll.
type refreshTickMsg struct{}

// refreshTickCmd schedules the next live-refresh tick.
func refreshTickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

// loadCmd runs the queries the current view needs OFF the UI thread, warming
// the shared *Data cache, then returns a dataLoadedMsg. It reuses reload() on a
// throwaway copy of the model: that copy shares the *Data pointer (so the cache
// it warms is the live one) but owns value copies of every view struct, so its
// mutations never touch the live model. The real reload() on dataLoadedMsg then
// hits the warm cache and does no SQLite work on the UI thread.
func (m Model) loadCmd() tea.Cmd {
	mc := m
	dbPath := m.dbPath
	return func() tea.Msg {
		mc.reload()
		return dataLoadedMsg{mtime: fileMTime(dbPath)}
	}
}

// fileMTime returns the file's modification time, or the zero time if it cannot
// be stat'd (missing db, permission error). A zero mtime simply means the next
// tick will treat the file as "unchanged" until it appears.
func fileMTime(path string) time.Time {
	if path == "" {
		return time.Time{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// startLoad marks a load in flight and returns the load cmd. Kept as a helper so
// the in-flight flag and the cmd dispatch never drift apart.
func (m *Model) startLoad() tea.Cmd {
	m.loading = true
	return m.loadCmd()
}

// handleRefreshTick stats the db; if its mtime advanced since the last load it
// dispatches a reload, otherwise it just re-arms the tick. The tick is ALWAYS
// re-armed so the live poll never dies.
func (m Model) handleRefreshTick() (Model, tea.Cmd) {
	mt := fileMTime(m.dbPath)
	if !m.loading && mt.After(m.lastMTime) {
		m.data.Invalidate()
		cmd := m.startLoad()
		return m, tea.Batch(cmd, refreshTickCmd())
	}
	return m, refreshTickCmd()
}

// handleDataLoaded applies a finished background load: it reloads the active
// view from the now-warm cache (cheap, no I/O), flips out of the loading state
// and records the observed mtime so the next tick can gate on it.
func (m Model) handleDataLoaded(msg dataLoadedMsg) (Model, tea.Cmd) {
	m.loading = false
	m.loaded = true
	if !msg.mtime.IsZero() {
		m.lastMTime = msg.mtime
	}
	m.reload()
	return m, nil
}

// handleSpinnerTick advances the spinner ONLY while still loading the first
// frame. Once loaded, the tick is swallowed (returns no follow-up cmd) so the
// spinner stops animating and idle cost drops to the 10s stat alone.
func (m Model) handleSpinnerTick(msg spinner.TickMsg) (Model, tea.Cmd) {
	if m.loaded && !m.loading {
		return m, nil
	}
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return m, cmd
}

// renderLoading is the centred, branded first-paint state shown until the first
// dataLoadedMsg arrives: spinner + "loading usage…" + the db path.
func (m Model) renderLoading() string {
	if m.width == 0 || m.height == 0 {
		return "loading usage…"
	}
	wordmark := m.th.PanelTitle.Render("◧ aiusage")
	line := m.spin.View() + " " + m.th.Stat.Render("loading usage…")
	path := ""
	if m.dbPath != "" {
		path = m.th.Subtle.Render(Truncate(m.dbPath, m.width-4))
	}
	block := lipgloss.JoinVertical(lipgloss.Center, wordmark, "", line, path)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
}
