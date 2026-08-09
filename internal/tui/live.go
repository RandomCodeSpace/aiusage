package tui

import (
	"os"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// live.go wires instant-open + async loading + low-CPU live refresh.
//
// Flow:
//
//	Init → tea.Batch(spinner.Tick, loadCmd, refreshTickCmd)
//	  loadCmd      (goroutine) warms the shared query cache → dataLoadedMsg
//	  dataLoadedMsg                → applyReload() from warm cache; FreshLive on
//	                                 success, FreshStale (picture held) on error
//	  refreshTickMsg (every 10s)   → os.Stat(db); reload only if mtime changed; re-arm tick
//	  spinner.TickMsg              → advance frame only during the genuine cold start
//	  manual `r` / chip click      → force reload (Invalidate + startLoad)
//
// Loading/staleness state is the Freshness enum (freshness.go): startLoad cuts
// the chip to FreshCutIn synchronously, the apply lands in one frame.
//
// Every dispatch is stamped with a monotonic load generation (Model.loadGen)
// and the clock resolved for that generation (Model.loadNow). handleDataLoaded
// applies a flight only if its generation is still current — stale flights
// (superseded by a navigation, refresh or invalidation) are dropped, so they
// can neither roll back view state nor advance lastMTime. The generation is
// also the identity of the on-screen dataset: render memoization keys on it.
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
	// gen is the load generation stamped at dispatch. handleDataLoaded drops the
	// message when it no longer matches Model.loadGen (a stale flight).
	gen uint64
	// now is the generation clock the flight's queries resolved against; applied
	// to Model.loadNow so the apply-side reload derives identical cache keys.
	now time.Time
	// mtime is the db file's modification time observed BEFORE the flight
	// queried, used to gate future refresh ticks. Zero when the file could not
	// be stat'd.
	mtime time.Time
	// err is the flight's query failure, if any. The apply holds the last good
	// picture and routes it to the freshness chip (FreshStale); the full-body
	// error panel exists only for a cold failure.
	err error
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
	gen := m.loadGen
	return func() tea.Msg {
		// Stat BEFORE querying: a daemon write landing between the queries and a
		// trailing stat would be credited to lastMTime while being absent from
		// the rendered data, and the refresh tick would then skip it until the
		// NEXT write. Stat-first keeps lastMTime at-or-before the query snapshot
		// so such a write is re-detected by the next tick.
		mt := fileMTime(dbPath)
		if mc.loadNow.IsZero() {
			// Init dispatches before any startLoad stamps a generation clock;
			// resolve it here so the flight and the apply share one instant.
			mc.loadNow = mc.data.now()
		}
		mc.reload()
		return dataLoadedMsg{gen: gen, now: mc.loadNow, mtime: mt, err: mc.err}
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

// startLoad opens a new load generation and returns its load cmd: it bumps the
// generation (superseding any in-flight load, whose apply will now be dropped),
// resolves the generation clock, cuts the freshness chip and dispatches. The
// chip cut is synchronous — the very next frame carries "◐ sync" with the old
// picture still behind it (the J-cut). Cold stays cold: with no picture to
// hold, the branded loading screen owns the frame instead. Kept as the single
// dispatch path so generation, clock and freshness never drift apart.
func (m *Model) startLoad() tea.Cmd {
	m.loadGen++
	m.loadNow = m.data.now()
	// A load replaces the rows under the selection — a drill, a range or sort
	// change, a live refresh — so whatever a press selected is gone. Dropping the
	// drill flag here covers every one of those paths at their single choke
	// point; the cost of being wrong is one extra tap, never a stray descent.
	m.rowChosen = false
	if m.fresh != FreshCold {
		m.fresh = FreshCutIn
	}
	return m.loadCmd()
}

// qnow returns the clock of the current load generation: resolved once per
// dispatch so every query of a generation — in the background flight and in the
// apply-side reload — derives the same windows and therefore the same cache
// keys. Falls back to the live clock before the first dispatch (direct reload()
// calls in tests).
func (m *Model) qnow() time.Time {
	if !m.loadNow.IsZero() {
		return m.loadNow
	}
	return m.data.now()
}

// handleRefreshTick stats the db; if its mtime advanced since the last load it
// dispatches a reload, otherwise it just re-arms the tick. The tick is ALWAYS
// re-armed so the live poll never dies. A load already in flight does not block
// the dispatch: the new generation supersedes it and the stale flight's apply
// is dropped, so fresh data can never lose to an older snapshot.
func (m Model) handleRefreshTick() (Model, tea.Cmd) {
	mt := fileMTime(m.dbPath)
	m.observeIngest(mt) // heartbeat: every stat feeds the ingest pulse
	if mt.After(m.lastMTime) {
		m.data.Invalidate()
		cmd := m.startLoad()
		return m, tea.Batch(cmd, refreshTickCmd())
	}
	return m, refreshTickCmd()
}

// handleDataLoaded applies a finished background load. Success reloads the
// active view from the now-warm cache (cheap, no I/O), flips to FreshLive and
// records the observed mtime so the next tick can gate on it. Failure is the
// L-cut hold: the last good picture stays on screen, the error routes to the
// freshness chip (FreshStale), and lastMTime is deliberately NOT advanced so
// the next mtime poll retries. A flight whose generation is no longer current
// is dropped whole — it must not touch freshness, lastMTime or the view data;
// the superseding dispatch owns them now.
func (m Model) handleDataLoaded(msg dataLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.loadGen {
		return m, nil
	}
	m.loadNow = msg.now
	m.observeIngest(msg.mtime) // the flight's stat feeds the heartbeat either way
	if msg.err != nil {
		m.err = msg.err
		if m.fresh != FreshCold {
			m.fresh = FreshStale // hold the picture; the chip carries the failure
		}
		return m, nil
	}
	// The applied generation is the identity of the on-screen dataset: render
	// memoization keys on it (a dispatch alone must not re-key — the stale frame
	// keeps rendering the old data until its flight lands).
	m.dataGen = msg.gen
	if !msg.mtime.IsZero() {
		m.lastMTime = msg.mtime
	}
	prior := m.fresh
	m.applyReload()
	if m.err != nil {
		// The warm apply itself failed (cache evicted + source down). Same
		// contract as a failed flight: hold the picture unless there is none.
		if prior != FreshCold {
			m.fresh = FreshStale
		}
		return m, nil
	}
	m.fresh = FreshLive
	m.lastLoadAt = msg.now
	return m, nil
}

// handleSpinnerTick advances the spinner ONLY during the genuine cold start
// (the branded loading screen is the sole spinner surface — every refresh path
// signals through the freshness chip instead). Once out of cold, or once a
// cold failure replaced the loading screen with the error panel, the tick is
// swallowed so idle cost drops to the 10s stat alone.
func (m Model) handleSpinnerTick(msg spinner.TickMsg) (Model, tea.Cmd) {
	if m.fresh != FreshCold || m.err != nil {
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
	wordmark := m.th.Wordmark.Render("◧ aiusage")
	line := m.spin.View() + " " + m.th.Stat.Render("loading usage…")
	path := ""
	if m.dbPath != "" {
		path = m.th.Subtle.Render(Truncate(m.dbPath, m.frameW()-4))
	}
	block := lipgloss.JoinVertical(lipgloss.Center, wordmark, "", line, path)
	return lipgloss.Place(m.frameW(), m.frameH(), lipgloss.Center, lipgloss.Center, block)
}
