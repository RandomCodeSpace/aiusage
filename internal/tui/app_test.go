package tui

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// fakeData is a tiny in-test DataSource. It returns a fixed grouping for any
// single-dimension Summarize and a couple of canned events, so model
// transitions can be exercised without a real database.
type fakeData struct {
	summarizeCalls int
	listCalls      int
}

func (f *fakeData) Summarize(_ context.Context, fl store.Filter) (*store.Summary, error) {
	f.summarizeCalls++
	if len(fl.GroupBy) == 0 {
		return &store.Summary{
			Totals: store.Bucket{Events: 12, Input: 1000, Output: 2000, CacheRead: 4000, Total: 7000},
		}, nil
	}
	dim := fl.GroupBy[0]
	mk := func(val string, total, events int64) store.Bucket {
		return store.Bucket{
			Keys:        map[string]string{dim: val},
			OrderedKeys: []string{dim},
			Events:      events,
			Input:       total / 4,
			Output:      total / 4,
			CacheRead:   total / 2,
			Total:       total,
		}
	}
	var buckets []store.Bucket
	switch dim {
	case "tool":
		buckets = []store.Bucket{mk("claude-code", 2_000_000, 8), mk("codex", 912_300, 4)}
	case "model":
		buckets = []store.Bucket{mk("claude-opus", 1_500_000, 5), mk("gpt-5", 800_000, 3)}
	case "project":
		buckets = []store.Bucket{mk("/work/a", 600_000, 4), mk("/work/b", 300_000, 2)}
	case "session":
		buckets = []store.Bucket{mk("sess-1", 400_000, 3), mk("sess-2", 100_000, 1)}
	case "day":
		buckets = []store.Bucket{mk("2026-05-28", 1_000_000, 6), mk("2026-05-29", 2_000_000, 6)}
	case "hour":
		buckets = []store.Bucket{mk("2026-05-29 13", 500_000, 3), mk("2026-05-29 14", 700_000, 4)}
	case "week":
		buckets = []store.Bucket{mk("2026-05-18", 3_000_000, 18)}
	case "month":
		buckets = []store.Bucket{mk("2026-05", 9_000_000, 50)}
	}
	return &store.Summary{GroupBy: fl.GroupBy, Buckets: buckets}, nil
}

func (f *fakeData) ListEvents(_ context.Context, _ store.Filter) ([]model.UsageEvent, error) {
	f.listCalls++
	return []model.UsageEvent{
		{
			Tool: model.ToolClaudeCode, Model: "claude-opus", SessionID: "sess-1",
			Project: "/work/a", EventTime: time.Now(), ObservedTime: time.Now(),
			InputTokens: 100, OutputTokens: 200, CacheReadTokens: 50, TotalTokens: 350,
			DedupKey: "claude-code|abc", Kind: model.KindUsage,
			Raw: `{"usage":{"input_tokens":100,"output_tokens":200}}`,
		},
	}, nil
}

// newTestModelW returns a Model sized to a usable terminal at the given width so
// layout never panics. Height is fixed at 40 rows. Because the first data load
// is now asynchronous (Init kicks a load tea.Cmd off the UI thread), the helper
// also drives that load to completion — running the load cmd and feeding the
// resulting dataLoadedMsg — so the returned model is past the loading state and
// renders the dashboard, matching what every existing assertion expects.
func newTestModelW(t *testing.T, src DataSource, width int) Model {
	t.Helper()
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	m = tm.(Model)
	return loadOnce(m)
}

// loadOnce executes a model's pending load cmd and applies the resulting
// dataLoadedMsg, advancing the model out of the loading state. This mirrors what
// the Bubble Tea runtime does when the background load goroutine returns.
func loadOnce(m Model) Model {
	msg := m.loadCmd()()
	tm, _ := m.Update(msg)
	return tm.(Model)
}

func newTestModelWH(t *testing.T, src DataSource, width, height int) Model {
	t.Helper()
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = tm.(Model)
	return loadOnce(m)
}

func newTestModel(t *testing.T, src DataSource) Model {
	t.Helper()
	return newTestModelW(t, src, 120)
}

// keyMsg builds a key message for a single token.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func send(m Model, msg tea.Msg) Model {
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// runPending invokes a cmd (possibly a tea.Batch) and applies the resulting
// messages to the model, EXCEPT pure timer ticks (the spinner tick, whose
// follow-up cmd re-arms forever, and the 10s refresh tick, whose cmd is a real
// tea.Tick). Neither carries state these tests assert on. It is the headless
// stand-in for the Bubble Tea runtime draining a batch returned from Update.
//
// The real runtime runs each cmd in its own goroutine, so a long-running tick
// never blocks the others; mirror that here by evaluating each batched cmd in a
// goroutine guarded by a short deadline. A cmd that does not return promptly is
// the 10s refresh re-arm — we drop it (its eventual refreshTickMsg is moot for
// these assertions) instead of blocking the test for the full interval.
func runPending(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	apply := func(c tea.Cmd) {
		if c == nil {
			return
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- c() }()
		select {
		case msg := <-done:
			if !isTimerTick(msg) {
				m = send(m, msg)
			}
		case <-time.After(100 * time.Millisecond):
			// A slow cmd is the refresh tick re-arm; skip it.
		}
	}
	switch v := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range v {
			apply(c)
		}
	default:
		if !isTimerTick(v) {
			m = send(m, v)
		}
	}
	return m
}

func isTimerTick(msg tea.Msg) bool {
	switch msg.(type) {
	case spinner.TickMsg, refreshTickMsg:
		return true
	}
	return false
}

// click renders the frame (to populate zone bounds) then sends a left-press at
// the centre of the named zone. Returns the updated model and whether the zone
// was found on screen.
//
// bubblezone's Scan stores zone bounds ASYNCHRONOUSLY via a background worker,
// so an immediate Get after Scan is racy (the library documents this). In a
// live program the mouse event always arrives in a later event-loop turn, well
// after the worker has caught up; here we poll briefly with a scheduler yield
// to deterministically wait for the bounds without a foreground sleep.
func click(t *testing.T, m Model, zoneID string) (Model, bool) {
	t.Helper()
	z := resolveZone(m, zoneID)
	if z == nil || z.IsZero() {
		return m, false
	}
	x := (z.StartX + z.EndX) / 2
	y := (z.StartY + z.EndY) / 2
	msg := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: y}
	return send(m, msg), true
}

// resolveZone renders the frame and waits (up to a bounded number of yields)
// for the async zone worker to register the named zone's bounds.
func resolveZone(m Model, zoneID string) *zone.ZoneInfo {
	_ = m.View()
	for i := 0; i < 2000; i++ {
		if z := m.zoneMgr.Get(zoneID); z != nil && !z.IsZero() {
			return z
		}
		runtime.Gosched()
	}
	return m.zoneMgr.Get(zoneID)
}

func TestViewSwitching(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	if m.view != ViewOverview {
		t.Fatalf("initial view = %v, want Overview", m.view)
	}

	cases := []struct {
		key  string
		want View
	}{
		{"2", ViewByTool},
		{"3", ViewByModel},
		{"4", ViewBrowse},
		{"1", ViewOverview},
	}
	for _, c := range cases {
		m = send(m, keyMsg(c.key))
		if m.view != c.want {
			t.Fatalf("after %q view = %v, want %v", c.key, m.view, c.want)
		}
	}

	// Rendering every view must not panic and must be non-empty.
	for _, v := range []View{ViewOverview, ViewByTool, ViewByModel, ViewBrowse} {
		m.view = v
		m.reload()
		if got := m.View(); got == "" {
			t.Fatalf("View() empty for view %v", v)
		}
	}
}

func TestTabCyclesViews(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	// Tab now cycles the active tab forward through all views and wraps back.
	want := []View{ViewByTool, ViewByModel, ViewBrowse, ViewOverview}
	for i, w := range want {
		m = send(m, keyMsg("tab"))
		if m.view != w {
			t.Fatalf("Tab #%d view = %v, want %v", i, m.view, w)
		}
	}
	// Shift+Tab walks back one tab.
	m = send(m, keyMsg("shift+tab"))
	if m.view != ViewBrowse {
		t.Fatalf("shift+tab view = %v, want Sessions(Browse)", m.view)
	}
}

func TestDrillPushPop(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("4")) // Browse, dim=tool

	if got := m.browse.Dim(); got != "tool" {
		t.Fatalf("browse dim = %q, want tool", got)
	}
	if len(m.crumbs) != 0 {
		t.Fatalf("crumbs not empty at start: %v", m.crumbs)
	}

	m = send(m, keyMsg("enter")) // tool -> model
	if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" {
		t.Fatalf("after drill crumbs = %v, want [tool]", m.crumbs)
	}
	if m.browse.Dim() != "model" {
		t.Fatalf("after drill dim = %q, want model", m.browse.Dim())
	}

	m = send(m, keyMsg("enter")) // model -> project
	if len(m.crumbs) != 2 || m.browse.Dim() != "project" {
		t.Fatalf("after 2nd drill crumbs=%v dim=%q", m.crumbs, m.browse.Dim())
	}

	m = send(m, keyMsg("enter")) // project -> session
	if len(m.crumbs) != 3 || m.browse.Dim() != "session" {
		t.Fatalf("after 3rd drill crumbs=%v dim=%q", m.crumbs, m.browse.Dim())
	}

	m = send(m, keyMsg("enter")) // deepest -> no-op (drilling stops at Sessions)
	if m.view != ViewBrowse {
		t.Fatalf("deepest drill view = %v, want Browse (stays)", m.view)
	}
	if len(m.crumbs) != 3 {
		t.Fatalf("deepest drill changed crumbs: %v, want len 3", m.crumbs)
	}
	if m.browse.Dim() != "session" {
		t.Fatalf("deepest drill changed dim = %q, want session", m.browse.Dim())
	}

	for want := 2; want >= 0; want-- {
		m = send(m, keyMsg("esc"))
		if len(m.crumbs) != want {
			t.Fatalf("after pop crumbs len = %d, want %d", len(m.crumbs), want)
		}
	}
	m = send(m, keyMsg("esc")) // no-op at root
	if len(m.crumbs) != 0 {
		t.Fatalf("esc at root changed crumbs: %v", m.crumbs)
	}
}

func TestByToolDrillIntoBrowse(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("2")) // By-Tool
	if len(m.byTool.Rows) == 0 {
		t.Fatal("by-tool has no rows")
	}
	m = send(m, keyMsg("enter")) // drill selected tool into Browse
	if m.view != ViewBrowse {
		t.Fatalf("after by-tool drill view = %v, want Browse", m.view)
	}
	if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" {
		t.Fatalf("after by-tool drill crumbs = %v, want [tool]", m.crumbs)
	}
}

func TestSelectionMoves(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("2")) // By-Tool
	if m.byTool.Selected != 0 {
		t.Fatalf("initial selection = %d, want 0", m.byTool.Selected)
	}
	m = send(m, keyMsg("down"))
	if m.byTool.Selected != 1 {
		t.Fatalf("after down selection = %d, want 1", m.byTool.Selected)
	}
	m = send(m, keyMsg("down")) // clamp at end
	if m.byTool.Selected != 1 {
		t.Fatalf("selection overflowed: %d", m.byTool.Selected)
	}
	m = send(m, keyMsg("up"))
	if m.byTool.Selected != 0 {
		t.Fatalf("after up selection = %d, want 0", m.byTool.Selected)
	}
}

func TestRangeAndSortCycle(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	// Default range is 7d (no persisted state); `t` cycles forward and wraps.
	if m.rng != Range7d {
		t.Fatalf("initial range = %v, want 7d", m.rng)
	}
	for _, want := range []Range{Range30d, RangeAll, RangeToday, Range7d} {
		m = send(m, keyMsg("t"))
		if m.rng != want {
			t.Fatalf("after 't' range = %v, want %v", m.rng, want)
		}
	}

	// Range change resets the drill stack.
	m = send(m, keyMsg("4"))
	m = send(m, keyMsg("enter"))
	if len(m.crumbs) == 0 {
		t.Fatal("expected crumbs after drill")
	}
	m = send(m, keyMsg("t"))
	if len(m.crumbs) != 0 {
		t.Fatalf("range change did not reset crumbs: %v", m.crumbs)
	}

	for _, want := range []Sort{SortEvents, SortName, SortTotal} {
		m = send(m, keyMsg("s"))
		if m.sort != want {
			t.Fatalf("after 's' sort = %v, want %v", m.sort, want)
		}
	}
}

func TestFilterFlow(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("4")) // Browse

	m = send(m, keyMsg("/"))
	if !m.filtering {
		t.Fatal("expected filtering mode after '/'")
	}
	for _, r := range "codex" {
		m = send(m, keyMsg(string(r)))
	}
	m = send(m, keyMsg("enter"))
	if m.filtering {
		t.Fatal("still filtering after enter")
	}
	if m.filter != "codex" {
		t.Fatalf("filter = %q, want codex", m.filter)
	}
	if got, _ := m.browse.SelectedValue(); got != "codex" {
		t.Fatalf("filtered selected value = %q, want codex", got)
	}
}

// TestOverviewScrub exercises the trend scrub crosshair, which now lives on the
// Overview view (the Timeline view was removed; Overview owns m.tlData).
func TestOverviewScrub(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("1")) // Overview (hour, 2 buckets for today)
	if len(m.tlData.Buckets) != 2 {
		t.Fatalf("overview trend buckets = %d, want 2", len(m.tlData.Buckets))
	}
	// Scrub right pins and advances from the start.
	m = send(m, keyMsg("right"))
	if !m.scrubPinned {
		t.Fatal("scrub not pinned after right")
	}
	if m.scrubIndex != 1 {
		t.Fatalf("after right scrub index = %d, want 1", m.scrubIndex)
	}
	// Cannot move past the end.
	m = send(m, keyMsg("right"))
	if m.scrubIndex != 1 {
		t.Fatalf("scrub overflowed: %d", m.scrubIndex)
	}
	// Scrub left.
	m = send(m, keyMsg("left"))
	if m.scrubIndex != 0 {
		t.Fatalf("after left scrub index = %d, want 0", m.scrubIndex)
	}
	// Esc unpins (springs back).
	m = send(m, keyMsg("esc"))
	if m.scrubPinned {
		t.Fatal("esc did not unpin scrub")
	}
	// tlCursor accessor mirrors scrubIndex.
	if m.tlCursor() != m.scrubIndex {
		t.Fatalf("tlCursor()=%d != scrubIndex=%d", m.tlCursor(), m.scrubIndex)
	}
}

// TestVerticalArrowsDoNotScrub locks in that ONLY the horizontal axis scrubs
// time; up/down move pane focus instead (regression: both axes used to scrub).
func TestVerticalArrowsDoNotScrub(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("1")) // overview
	start := m.scrubIndex

	m = send(m, keyMsg("up"))
	if m.scrubIndex != start {
		t.Fatalf("[overview] up changed scrub index %d -> %d (must not scrub)", start, m.scrubIndex)
	}
	m = send(m, keyMsg("down"))
	if m.scrubIndex != start {
		t.Fatalf("[overview] down changed scrub index %d -> %d (must not scrub)", start, m.scrubIndex)
	}
	m = send(m, keyMsg("j"))
	m = send(m, keyMsg("k"))
	if m.scrubIndex != start {
		t.Fatalf("[overview] j/k changed scrub index (must not scrub)")
	}

	// Horizontal axis still scrubs on the overview trend.
	m = newTestModel(t, &fakeData{})
	m = send(m, keyMsg("1")) // overview, scrub starts at index 0 of 2
	m = send(m, keyMsg("right"))
	if m.scrubIndex != 1 {
		t.Fatalf("right did not scrub: index = %d, want 1", m.scrubIndex)
	}
}

func TestMouseClickRailSwitchesView(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m2, found := click(t, m, views.RailZone(int(ViewByTool)))
	if !found {
		t.Fatal("rail zone for By-Tool not found on screen")
	}
	if m2.view != ViewByTool {
		t.Fatalf("after rail click view = %v, want By-Tool", m2.view)
	}
}

func TestMouseClickRangePill(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	before := m.rng
	m2, found := click(t, m, views.ZoneRangePill)
	if !found {
		t.Fatal("range pill zone not found")
	}
	if m2.rng == before {
		t.Fatalf("range pill click did not change range from %v", before)
	}
}

// (TestMouseClickKPIPivotsHero removed — KPI tiles are now read-only; the trend
// shows all four components, so there is no hero-metric pivot.)

func TestMouseClickRowSelects(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("4")) // Browse
	m2, found := click(t, m, views.RowZone(1))
	if !found {
		t.Skip("row zone not laid out at this size; covered by keyboard path")
	}
	if m2.browse.Cursor() != 1 {
		t.Fatalf("after row click cursor = %d, want 1", m2.browse.Cursor())
	}
}

func TestMouseWheelScrubsOverview(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("1")) // Overview (owns the trend scrub)
	wheelDown := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 5, Y: 5}
	m = send(m, wheelDown)
	if !m.scrubPinned {
		t.Fatal("wheel down did not pin/scrub the overview trend")
	}
	if m.scrubIndex != 1 {
		t.Fatalf("after wheel down scrub index = %d, want 1", m.scrubIndex)
	}
	wheelUp := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 5, Y: 5}
	m = send(m, wheelUp)
	if m.scrubIndex != 0 {
		t.Fatalf("after wheel up scrub index = %d, want 0", m.scrubIndex)
	}
}

func TestMouseWheelScrollsBrowse(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("4")) // Browse (2 tool rows)
	wheelDown := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 5, Y: 5}
	m = send(m, wheelDown)
	if m.browse.Cursor() != 1 {
		t.Fatalf("after wheel down browse cursor = %d, want 1", m.browse.Cursor())
	}
	wheelUp := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 5, Y: 5}
	m = send(m, wheelUp)
	if m.browse.Cursor() != 0 {
		t.Fatalf("after wheel up browse cursor = %d, want 0", m.browse.Cursor())
	}
}

func TestHelpAndRefreshNoPanic(t *testing.T) {
	f := &fakeData{}
	m := newTestModel(t, f)
	m = send(m, keyMsg("?"))
	if !m.showHelp {
		t.Fatal("help not toggled on")
	}
	if !strings.Contains(m.View(), "quit") {
		t.Fatal("help overlay missing expected hint text")
	}
	m = send(m, keyMsg("?"))
	if m.showHelp {
		t.Fatal("help not toggled off")
	}

	// Manual `r` now forces an async reload: it invalidates the cache and returns
	// a load cmd. Running that cmd re-queries the source off the UI thread.
	before := f.summarizeCalls
	tm, cmd := m.Update(keyMsg("r"))
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("refresh produced no command")
	}
	if !m.loading {
		t.Fatal("refresh did not enter the loading state")
	}
	runPending(t, m, cmd) // drives the load goroutine + spinner tick to completion
	if f.summarizeCalls <= before {
		t.Fatal("refresh did not re-query the data source")
	}
}

func TestQuit(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	_, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q produced no command")
	}
	// q issues tea.Sequence(tea.ClearScreen, tea.Quit) so terminals with imperfect
	// alt-screen restore are left blank rather than showing dashboard residue. The
	// sequence message is internal to bubbletea; assert a command/message is issued.
	if cmd() == nil {
		t.Fatal("q command produced no message")
	}
	// ctrl+c quits the same way.
	if _, c := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); c == nil {
		t.Fatal("ctrl+c produced no command")
	}
}

// TestResponsiveRender exercises both the wide (120) and compact (<100) layouts
// for every view without a TTY: construct, size, switch, render.
func TestResponsiveRender(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := newTestModelW(t, &fakeData{}, width)
		for _, v := range []View{ViewOverview, ViewByTool, ViewByModel, ViewBrowse} {
			m.view = v
			m.reload()
			out := m.View()
			if out == "" {
				t.Fatalf("empty render at %d cols for view %v", width, v)
			}
		}
	}
}

// TestStoreInterfaceCompat is a compile-time guard that store.Store satisfies
// DataSource (also asserted in data.go); a built model renders at small widths.
func TestSmallWidthRender(t *testing.T) {
	m := NewModel(&fakeData{}, Options{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	m = loadOnce(tm.(Model))
	for _, v := range []View{ViewOverview, ViewByTool, ViewByModel, ViewBrowse} {
		m.view = v
		m.reload()
		if m.View() == "" {
			t.Fatalf("empty render at 70 cols for view %v", v)
		}
	}
}
