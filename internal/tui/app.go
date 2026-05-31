// Package tui implements the read-only Bubble Tea terminal UI for aiusage. The
// root model (Model) routes four views — Overview, By-Tool, By-Model,
// Sessions/Browse — with a drill-down stack, a breadcrumb, a scrub crosshair and
// full keyboard + mouse navigation, querying the store through a small cached
// data layer. It never writes to the store.
package tui

import (
	"os"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/sysmon"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// Options configures the TUI. DBPath is shown in the header; Since/Until, when
// both set, seed a custom starting range (otherwise the default range applies).
// StatePath points at the small ui-state.json that persists the last range + tab
// across launches (empty disables persistence — e.g. in tests).
type Options struct {
	DBPath    string
	StatePath string
	Since     time.Time
	Until     time.Time
}

// Run launches the TUI over the given store. It blocks until the user quits.
// Mouse support is enabled via tea.WithMouseCellMotion so the nav rail, tabs,
// rows, bars and KPI tiles are all clickable and the wheel scrolls/scrubs.
func Run(st store.Store, opt Options) error {
	m := NewModel(st, opt)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// Model is the root Bubble Tea model.
type Model struct {
	data    *Data
	keys    KeyMap
	th      Theme
	vctx    views.Ctx
	zoneMgr *zone.Manager

	dbPath        string
	statePath     string // ui-state.json path ("" disables persistence)
	reducedMotion bool

	view      View
	rng       Range
	sort      Sort
	crumbs    []Crumb
	filter    string
	filterUI  textinput.Model
	filtering bool

	// Scrub crosshair state (read by every pane on render).
	scrubIndex  int
	scrubPinned bool

	// View data (populated by per-view loaders in load.go).
	overview views.OverviewData
	tlData   views.TimelineData
	byTool   views.ByToolData
	byModel  views.ByModelData
	browse   views.Browse

	help     help.Model
	showHelp bool

	// Async loading + live refresh state.
	spin      spinner.Model
	loaded    bool      // first dataLoadedMsg has arrived (dashboard is live)
	loading   bool      // a load cmd is in flight (drives the refreshing hint)
	lastMTime time.Time // db file mtime at the last successful load (live poll)

	// Container resource gauges (CPU/mem/disk for the current pod, not the node).
	// mon samples on its own tick; sys holds the latest reading for the Overview
	// gauge strip. Live system state — deliberately never persisted to the DB.
	mon *sysmon.Monitor
	sys sysmon.Snapshot

	// Double-click tracking for mouse drill.
	lastClickZone string
	lastClickAt   time.Time

	width  int
	height int
	lay    views.Layout // responsive frame, recomputed on every WindowSizeMsg
	err    error
}

// tlCursor exposes the timeline scrub index under the legacy name so existing
// tests and call sites keep reading the cursor through one accessor.
func (m Model) tlCursor() int { return m.scrubIndex }

// NewModel builds the root model.
func NewModel(src DataSource, opt Options) Model {
	th := NewTheme()
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/ "

	zm := zone.New()

	h := help.New()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(th.Accent)

	vctx := buildCtx(th, zm)
	b := views.NewBrowse()
	// PaddingRight(1) on the per-cell Header/Cell styles gives each column a
	// 1-cell gutter so values never abut (the column budget in browse.go reserves
	// for it). The Selected style wraps the WHOLE row, so it must NOT add padding
	// — doing so would push the cursor row one column wider than the others and
	// make it wrap. The accent left-marker is baked into the row content instead.
	b.ApplyStyles(
		lipgloss.NewStyle().Bold(true).Foreground(th.Muted).PaddingRight(1),
		lipgloss.NewStyle().Foreground(th.Text).PaddingRight(1),
		lipgloss.NewStyle().Bold(true).Foreground(th.Text).Background(th.SurfaceHi),
	)

	// Restore the last range + tab if a state file exists; otherwise default to a
	// 7-day window on the Overview tab. Restoring is best-effort: an unknown or
	// missing key falls back to the default.
	rng := Range7d
	view := ViewOverview
	if st := LoadUIState(opt.StatePath); st != (UIState{}) {
		if r, ok := RangeFromKey(st.Range); ok {
			rng = r
		}
		if v, ok := viewFromKey(st.Tab); ok {
			view = v
		}
	}

	// Resource gauges measure the filesystem the user works in (the workspace
	// dir) rather than a host-backed overlay on "/". CPU/memory come from the
	// container's cgroup. Getwd failing just disables the disk gauge.
	wd, _ := os.Getwd()

	return Model{
		data:          NewData(src),
		keys:          DefaultKeyMap(),
		th:            th,
		vctx:          vctx,
		zoneMgr:       zm,
		dbPath:        opt.DBPath,
		statePath:     opt.StatePath,
		reducedMotion: detectReducedMotion(),
		view:          view,
		rng:           rng,
		sort:          SortTotal,
		filterUI:      ti,
		browse:        b,
		help:          h,
		spin:          sp,
		mon:           sysmon.New(wd),
	}
}

// persistUI writes the current range + tab to the state file (best-effort). It is
// called whenever the tab or range changes so a relaunch lands where we left off.
func (m Model) persistUI() {
	SaveUIState(m.statePath, UIState{Range: m.rng.Key(), Tab: m.view.Key()})
}

// detectReducedMotion honours NO_COLOR / AIUSAGE_REDUCED_MOTION so motion can be
// disabled in CI, recordings, and accessibility-sensitive setups.
func detectReducedMotion() bool {
	return os.Getenv("NO_COLOR") != "" || os.Getenv("AIUSAGE_REDUCED_MOTION") != ""
}

// buildCtx assembles the views.Ctx from a theme + the format helpers + the
// shared zone manager.
func buildCtx(th Theme, zm *zone.Manager) views.Ctx {
	return views.Ctx{
		Panel:      th.Idle(),
		Focused:    th.Focused(),
		PanelTitle: th.PanelTitle,
		Stat:       th.Stat,
		StatLabel:  th.StatLabel,
		Subtle:     th.Subtle,
		Number:     th.Number,
		Faint:      lipgloss.NewStyle().Foreground(th.Faint),

		NowColor:    th.Now,
		AccentColor: th.Accent,
		FaintColor:  th.Faint,
		BorderColor: th.Border,
		GoodColor:   th.Positive,
		WarnColor:   th.Warn,
		// Token-series colors use the ANSI palette so they adapt to the user's
		// terminal theme. input=green(2), output=blue(4), cache=red(1) — avoiding
		// the reserved cyan(6) accent and yellow(3) "now"/scrub. cache combines the
		// DB read+creation sub-types (split kept only for cost calc).
		Comp: views.CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("1")),

		Humanize:   HumanizeTokens,
		PadLeft:    PadLeft,
		PadRight:   PadRight,
		Truncate:   Truncate,
		Percent:    Percent,
		Delta:      Delta,
		ToolAccent: th.ToolAccent,
		ToolGlyph:  th.ToolGlyph,
		Zone:       zm,
	}
}

// Init opens the program instantly and schedules the first data load off the UI
// thread. The spinner animates only until the first dataLoadedMsg arrives; the
// 10s refresh tick stats the db file and reloads only when its mtime changes,
// so steady-state cost is one os.Stat per tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.loadCmd(), refreshTickCmd(), sysTickCmd())
}

// toggleHelp flips the help overlay. It must NOT set m.help.ShowAll: that field
// is read by the footer's m.help.View, so flipping it here made the footer ALSO
// expand to the full keymap — producing two full help panels (footer + overlay)
// at once. The footer stays the one-line hint; renderHelpOverlay sets ShowAll on
// its own local copy for the expanded panel.
func (m *Model) toggleHelp() {
	m.showHelp = !m.showHelp
	m.layout()
}

// setView switches the active tab, persists it, and reloads.
func (m *Model) setView(v View) {
	m.view = v
	m.persistUI()
	m.reload()
}

// applyPaneFocus lights the single interactive pane of each view (its trend,
// bars or table). Every other panel is read-only and never wears the ring, so
// the focus indicator always marks exactly where arrows / Enter / wheel act.
func (m *Model) applyPaneFocus() {
	m.overview.ActivePane = views.PaneOverviewHero
	m.byTool.ActivePane = views.PaneByXBars
	m.byModel.ActivePane = views.PaneByXBars
	m.browse.SetFocusedPane(views.PaneBrowseTable)
	m.tlData.Focused = true
}

// drill descends one level in Browse or drills a bar into Browse. It pushes a
// crumb and preserves state for Back. Drilling stops at Sessions — the deepest
// Browse level has no further descent.
func (m Model) drill() (tea.Model, tea.Cmd) {
	switch m.view {
	case ViewBrowse:
		return m.drillBrowse()
	case ViewByTool:
		if b, ok := m.selectedByToolBucket(); ok {
			return m.drillIntoBrowse("tool", b.Keys["tool"])
		}
	case ViewByModel:
		if b, ok := m.selectedByModelBucket(); ok {
			return m.drillIntoBrowse("model", b.Keys["model"])
		}
	}
	return m, nil
}

// drillIntoBrowse pushes a crumb on the given dimension and switches to Browse.
func (m Model) drillIntoBrowse(dim, val string) (tea.Model, tea.Cmd) {
	if val == "" {
		return m, nil
	}
	m.crumbs = append(m.crumbs, Crumb{Dim: dim, Value: val})
	m.view = ViewBrowse
	m.reload()
	return m, nil
}

// drillBrowse descends the Browse drill stack. At the deepest level there is no
// further descent (drilling stops at Sessions).
func (m Model) drillBrowse() (tea.Model, tea.Cmd) {
	dim := m.browse.Dim()
	val, ok := m.browse.SelectedValue()
	if !ok {
		return m, nil
	}
	if len(m.crumbs) >= len(drillDims)-1 {
		return m, nil
	}
	m.crumbs = append(m.crumbs, Crumb{Dim: dim, Value: val})
	m.reload()
	return m, nil
}

// back pops the scrub pin or the drill stack.
func (m Model) back() (tea.Model, tea.Cmd) {
	if m.scrubPinned {
		m.scrubPinned = false
		m.syncScrub()
		return m, nil
	}
	switch m.view {
	case ViewBrowse:
		if len(m.crumbs) > 0 {
			m.crumbs = m.crumbs[:len(m.crumbs)-1]
			m.reload()
		}
	}
	return m, nil
}

// popCrumbsTo pops the drill stack down to the given depth (breadcrumb click).
func (m *Model) popCrumbsTo(depth int) {
	if depth < 0 {
		depth = 0
	}
	if depth < len(m.crumbs) {
		m.crumbs = m.crumbs[:depth]
		m.reload()
	}
}

// layout recomputes the responsive frame from the terminal size and pushes the
// derived body region into the stateful view components (Browse). Called
// on every WindowSizeMsg and whenever the help overlay toggles (it reshapes the
// body). All breakpoint policy lives in views.ComputeLayout — this just applies
// it. bodyLayout() carries the effective body height (after any help reserve).
func (m *Model) layout() {
	m.lay = views.ComputeLayout(m.width, m.height)
	bl := m.bodyLayout()
	m.browse.SetLayout(bl)
	// Bound the help footer so its one-line short help ellipsis-truncates instead
	// of overflowing the frame (FooterBar adds Padding(0,1) = 2 columns).
	if m.width > 2 {
		m.help.Width = m.width - 2
	}
}

// helpReserve is the row budget the expanded help overlay claims from the body.
const helpReserve = 8

// helpRows is how many rows the help overlay occupies (0 when hidden), bounded
// so the body always keeps at least one row.
func (m Model) helpRows() int {
	if !m.showHelp {
		return 0
	}
	hr := helpReserve
	if max := m.lay.BodyH - 1; hr > max {
		hr = max
	}
	if hr < 0 {
		hr = 0
	}
	return hr
}

// bodyHeight is the vertical space available to the active view body after
// reserving rows for the help overlay.
func (m Model) bodyHeight() int {
	h := m.lay.BodyH - m.helpRows()
	if h < 1 {
		h = 1
	}
	return h
}

// bodyLayout is the layout with its body height adjusted for the help reserve;
// the height-derived flags (chart mode, readout, dense) are recomputed so views
// fit the reduced region.
func (m Model) bodyLayout() views.Layout { return m.lay.WithBodyHeight(m.bodyHeight()) }

// compact reports whether the layout is in its tightest (phone) nav mode, used
// for the header's density decisions (e.g. dropping the subtitle).
func (m Model) compact() bool { return m.lay.Nav == views.NavMini }

// zoneMark wraps s in the shared zone manager (no-op when headless).
func (m Model) zoneMark(id, s string) string {
	if m.zoneMgr == nil {
		return s
	}
	return m.zoneMgr.Mark(id, s)
}
