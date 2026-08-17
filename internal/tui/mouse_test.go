package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/store"
)

// mouse_test.go drives the mouse-first input model (issues #23 / #24) through
// REAL tea.MouseMsg values fed to Update. Nothing here calls a handler directly:
// the wave-4 lesson was that a path which is wired but unreachable from the live
// message flow is worse than no path at all, so every assertion below travels
// the same route a keypress or a terminal escape sequence does.

// zoneCenter renders the frame, waits for bubblezone's async worker to register
// the named zone, and returns its centre cell.
func zoneCenter(m Model, id string) (x, y int, ok bool) {
	z := resolveZone(m, id)
	if z == nil || z.IsZero() {
		return 0, 0, false
	}
	return (z.StartX + z.EndX) / 2, (z.StartY + z.EndY) / 2, true
}

// pressZone sends a real mouse press of button at the centre of the named zone
// through Update, driving any dispatched load to completion.
func pressZone(t *testing.T, m Model, id string, button tea.MouseButton) (Model, bool) {
	t.Helper()
	x, y, ok := zoneCenter(m, id)
	if !ok {
		return m, false
	}
	return step(t, m, tea.MouseClickMsg{Button: button, X: x, Y: y}), true
}

// mustPress is pressZone with a hard failure when the zone is not on screen —
// an interactive surface that cannot be hit is the bug this suite exists for.
func mustPress(t *testing.T, m Model, id string, button tea.MouseButton) Model {
	t.Helper()
	m2, ok := pressZone(t, m, id, button)
	if !ok {
		t.Fatalf("zone %q is not on screen: it cannot be pressed", id)
	}
	return m2
}

// wheelOver sends a real wheel notch over the centre of the named zone.
func wheelOver(t *testing.T, m Model, id string, button tea.MouseButton) Model {
	t.Helper()
	x, y, ok := zoneCenter(m, id)
	if !ok {
		t.Fatalf("zone %q is not on screen: the wheel cannot be routed to it", id)
	}
	return send(m, tea.MouseWheelMsg{Button: button, X: x, Y: y})
}

// wideRows is the row count of the tall fixture: comfortably more rows than any
// pane holds at the sizes these tests drive, so both the Browse table and the
// By-Tool bars are windowed.
const wideRows = 40

// wideData is a DataSource whose entity groupings are taller than the screen.
// Timeline dimensions fall through to fakeData so the hero still gets real
// date-shaped keys. It exists because a list that FITS cannot expose a
// window/zone mismatch — which is exactly how issue #23's defect shipped.
type wideData struct{ fakeData }

func (d *wideData) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	if len(f.GroupBy) == 1 {
		if rows := wideRowsFor(f.GroupBy[0]); rows != nil {
			d.summarizeCalls.Add(1)
			return &store.Summary{GroupBy: f.GroupBy, Buckets: rows}, nil
		}
	}
	return d.fakeData.Summarize(ctx, f)
}

// wideRowsFor builds wideRows buckets for an entity dimension, descending by
// total so any sort order leaves them in the order they are generated. Names are
// two-digit ("tool-07") so one name is never a substring of another.
func wideRowsFor(dim string) []store.Bucket {
	switch dim {
	case "tool", "model", "project", "session":
	default:
		return nil
	}
	out := make([]store.Bucket, 0, wideRows)
	for i := 0; i < wideRows; i++ {
		total := int64(wideRows-i) * 100_000
		out = append(out, store.Bucket{
			Keys:        map[string]string{dim: fmt.Sprintf("%s-%02d", dim, i)},
			OrderedKeys: []string{dim},
			Events:      int64(wideRows - i),
			Sessions:    int64(wideRows - i),
			Input:       total / 4,
			Output:      total / 4,
			CacheRead:   total / 2,
			Total:       total,
		})
	}
	return out
}

var ansiMouse = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// frameLine returns the y-th line of the rendered frame with SGR stripped —
// what the reader actually sees on that row of the terminal.
func frameLine(m Model, y int) string {
	lines := strings.Split(m.View().Content, "\n")
	if y < 0 || y >= len(lines) {
		return ""
	}
	return ansiMouse.ReplaceAllString(lines[y], "")
}

// assertZoneShowsName is the whole point of the scrolled cases: a zone is a
// promise about the cell the reader is pointing at, so the line the zone covers
// must be the line that displays the entity the zone id names. A zone keyed by
// screen position instead of model index passes every "does it resolve" check
// and still fails this one.
func assertZoneShowsName(t *testing.T, m Model, zoneID, name string) {
	t.Helper()
	z := resolveZone(m, zoneID)
	if z == nil || z.IsZero() {
		return // not on screen: the windowed-out case, checked by the caller
	}
	if got := frameLine(m, z.StartY); !strings.Contains(got, name) {
		t.Fatalf("zone %q covers line %d, which reads %q — it does not show %q",
			zoneID, z.StartY, strings.TrimRight(got, " "), name)
	}
}

// termSize is one terminal geometry in a size sweep. name() feeds t.Run, which
// is what keeps a sweep honest: the gesture tests below assert fatally, so a
// bare range loop would stop at the first size that broke and report the rest as
// if they had passed. One subtest per size makes every size an independent
// verdict.
type termSize struct{ w, h int }

func (s termSize) name() string { return fmt.Sprintf("%dx%d", s.w, s.h) }

// sizeMatrix is the shared geometry sweep for the gesture tests: a roomy
// terminal and a tighter one, where the rows and bars a press has to hit sit at
// different coordinates while the selection contract must not move.
var sizeMatrix = []termSize{{160, 44}, {120, 40}}

// expireClick pushes the last press outside the double-click window so the NEXT
// press is an ordinary second click rather than a double-click. Both drill, and
// this is what separates the two paths in a test without sleeping.
func expireClick(m Model) Model {
	m.lastClickAt = m.lastClickAt.Add(-2 * doubleClickWindow)
	return m
}

// TestClickSelectsThenSecondClickDrillsBrowse is the uniform row contract on the
// Browse table: the first press selects, a later press on the row that is now
// selected drills. This is the path touch depends on — two taps, no timing.
func TestClickSelectsThenSecondClickDrillsBrowse(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4")) // Browse; cursor starts on row 0

	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
	if m.browse.Cursor() != 1 {
		t.Fatalf("first press: cursor = %d, want 1 (press must select)", m.browse.Cursor())
	}
	if len(m.crumbs) != 0 {
		t.Fatalf("first press drilled: crumbs = %v, want none", m.crumbs)
	}

	m = expireClick(m)
	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
	if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" || m.crumbs[0].Value != "codex" {
		t.Fatalf("second press on the selected row: crumbs = %v, want [tool:codex]", m.crumbs)
	}
}

// TestDoubleClickDrillsBrowseRow: two presses inside the double-click window
// drill outright — the primary mouse gesture.
func TestDoubleClickDrillsBrowseRow(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4"))

	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft) // inside the window
	if len(m.crumbs) != 1 || m.crumbs[0].Value != "codex" {
		t.Fatalf("double-click: crumbs = %v, want [tool:codex]", m.crumbs)
	}
}

// TestClickSelectsThenSecondClickDrillsBars: the same contract on the By-Tool
// bars, which is what "uniformly across every surface with rows" means.
func TestClickSelectsThenSecondClickDrillsBars(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("2")) // By-Tool; bar 0 (claude-code) selected

	m = mustPress(t, m, views.BarZone("codex"), tea.MouseLeft)
	if m.byTool.Selected != 1 {
		t.Fatalf("first press: selection = %d, want 1 (press must select)", m.byTool.Selected)
	}
	if m.view != ViewByTool {
		t.Fatalf("first press drilled: view = %v, want By-Tool", m.view)
	}

	m = expireClick(m)
	m = mustPress(t, m, views.BarZone("codex"), tea.MouseLeft)
	if m.view != ViewBrowse {
		t.Fatalf("second press on the selected bar: view = %v, want Browse", m.view)
	}
	if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" || m.crumbs[0].Value != "codex" {
		t.Fatalf("second press: crumbs = %v, want [tool:codex]", m.crumbs)
	}
}

// TestRightClickBacksOut: the right button is Back everywhere, on the same
// contract Escape carries — it pops the scrub pin, then the drill stack.
func TestRightClickBacksOut(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4"))
	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft) // drill
	if len(m.crumbs) != 1 {
		t.Fatalf("setup: crumbs = %v, want one level", m.crumbs)
	}

	m = mustPress(t, m, views.RowZone(0), tea.MouseRight)
	if len(m.crumbs) != 0 {
		t.Fatalf("right press: crumbs = %v, want the drill popped", m.crumbs)
	}

	// On Overview the pin is what a back pops first.
	m = step(t, m, keyMsg("1"))
	m = send(m, keyMsg("right")) // pin the scrub
	if !m.scrubPinned {
		t.Fatal("setup: scrub not pinned")
	}
	m = mustPress(t, m, views.ZoneRangePill, tea.MouseRight)
	if m.scrubPinned {
		t.Fatal("right press did not unpin the scrub")
	}
	if m.rng != Range7d {
		t.Fatalf("right press over the range pill cycled the range to %v — back must not act on the zone", m.rng)
	}
}

// TestWheelRoutesToPaneUnderPointer: the notch acts on the pane the pointer is
// over, not on whichever pane holds focus. Over the Browse table it scrolls
// rows; over the read-only preview beside it, nothing scrolls.
func TestWheelRoutesToPaneUnderPointer(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4"))

	// Each preview notch is checked from a cursor position the fallback routing
	// would visibly move it from, so neither assertion can pass by clamping.
	if m.browse.Cursor() != 0 {
		t.Fatalf("setup: cursor = %d, want 0", m.browse.Cursor())
	}
	m = wheelOver(t, m, views.ZonePreview, tea.MouseWheelDown)
	if m.browse.Cursor() != 0 {
		t.Fatalf("wheel over the read-only preview moved the table cursor to %d — it must act on the pane under the pointer",
			m.browse.Cursor())
	}

	m = wheelOver(t, m, views.ZoneTable, tea.MouseWheelDown)
	if m.browse.Cursor() != 1 {
		t.Fatalf("wheel over the table: cursor = %d, want 1", m.browse.Cursor())
	}

	m = wheelOver(t, m, views.ZonePreview, tea.MouseWheelUp)
	if m.browse.Cursor() != 1 {
		t.Fatalf("wheel up over the read-only preview moved the table cursor to %d", m.browse.Cursor())
	}

	m = wheelOver(t, m, views.ZoneTable, tea.MouseWheelUp)
	if m.browse.Cursor() != 0 {
		t.Fatalf("wheel up over the table: cursor = %d, want 0", m.browse.Cursor())
	}
}

// TestWheelOverHeroScrubs: over a chart a notch is a scrub step.
func TestWheelOverHeroScrubs(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	if n := len(m.tlData.Buckets); n < 2 {
		t.Fatalf("overview timeline has %d buckets, need >= 2 to scrub", n)
	}
	m = wheelOver(t, m, views.ZoneHero, tea.MouseWheelDown)
	if !m.scrubPinned || m.scrubIndex != 1 {
		t.Fatalf("wheel over the hero: pinned=%v index=%d, want pinned at 1", m.scrubPinned, m.scrubIndex)
	}
	m = wheelOver(t, m, views.ZoneHero, tea.MouseWheelUp)
	if m.scrubIndex != 0 {
		t.Fatalf("wheel up over the hero: index = %d, want 0", m.scrubIndex)
	}
}

// TestWheelOverReadOnlyDetailIsInert: the By-Tool detail card is read-only, so a
// notch over it moves no selection.
func TestWheelOverReadOnlyDetailIsInert(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("2"))
	before := m.byTool.Selected
	m = wheelOver(t, m, views.ZonePreview, tea.MouseWheelDown)
	if m.byTool.Selected != before {
		t.Fatalf("wheel over the read-only detail moved the bar selection %d → %d", before, m.byTool.Selected)
	}
	m = wheelOver(t, m, views.ZoneBars, tea.MouseWheelDown)
	if m.byTool.Selected != before+1 {
		t.Fatalf("wheel over the bars: selection = %d, want %d", m.byTool.Selected, before+1)
	}
}

// TestWheelNeverSwitchesTabsOrViews: a notch over the tab strip must not
// navigate — an accidental view change costs more than reaching for a key.
func TestWheelNeverSwitchesTabsOrViews(t *testing.T) {
	for _, tab := range []string{"1", "2", "3", "4"} {
		m := newTestModelWH(t, &fakeData{}, 160, 44)
		m = step(t, m, keyMsg(tab))
		want := m.view
		for _, b := range []tea.MouseButton{tea.MouseWheelDown, tea.MouseWheelUp} {
			for i := 0; i < int(viewCount); i++ {
				m = wheelOver(t, m, views.RailZone(i), b)
				if m.view != want {
					t.Fatalf("tab %s: a wheel notch over tab %d switched the view to %v", tab, i, m.view)
				}
			}
		}
	}
}

// TestDragAcrossHeroScrubs: dragging with the button held over the chart scrubs
// continuously. Cell-motion reporting only emits motion while a button is down,
// so this is the terminal-side twin of the wheel path mobile clients take.
func TestDragAcrossHeroScrubs(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	x, y, ok := zoneCenter(m, views.ZoneHero)
	if !ok {
		t.Fatal("hero zone is not on screen")
	}

	// The first motion only arms the drag: there is no previous column to
	// measure against, so it must not scrub.
	m = send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x, Y: y})
	if m.scrubPinned {
		t.Fatalf("the first drag motion scrubbed (index %d) — it only arms the drag", m.scrubIndex)
	}

	m = send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x + 5, Y: y})
	if !m.scrubPinned || m.scrubIndex != 1 {
		t.Fatalf("drag right: pinned=%v index=%d, want pinned at 1", m.scrubPinned, m.scrubIndex)
	}
	m = send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x - 5, Y: y})
	if m.scrubIndex != 0 {
		t.Fatalf("drag left: index = %d, want 0", m.scrubIndex)
	}

	// Motion with no button held is not a drag (and never arrives under
	// cell-motion reporting); motion off the chart drops the drag rather than
	// being reinterpreted.
	m = send(m, tea.MouseMotionMsg{Button: tea.MouseNone, X: x + 20, Y: y})
	if m.scrubIndex != 0 {
		t.Fatalf("button-less motion scrubbed to %d", m.scrubIndex)
	}
	m = send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x, Y: 0}) // header row
	if m.scrubIndex != 0 {
		t.Fatalf("motion off the chart scrubbed to %d", m.scrubIndex)
	}
	m = send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x + 40, Y: y})
	if m.scrubIndex != 0 {
		t.Fatalf("a drag that left the chart kept scrubbing: index = %d, want 0 (re-armed)", m.scrubIndex)
	}
}

// TestClickSortChipCyclesSort: the sort label renders as an action chip, so
// pressing it acts — the "if it looks interactive, pressing it acts" bar.
func TestClickSortChipCyclesSort(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	before := m.sort
	m = mustPress(t, m, views.ZoneSort, tea.MouseLeft)
	if m.sort == before {
		t.Fatalf("sort chip press left the sort at %v", before)
	}
}

// TestClickFilterChipReopensInput: the active-filter chip is the way back into
// the filter without knowing the `/` key.
func TestClickFilterChipReopensInput(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4"))
	m = send(m, keyMsg("/"))
	for _, r := range "codex" {
		m = send(m, keyMsg(string(r)))
	}
	m = step(t, m, keyMsg("enter"))
	if m.filter != "codex" || m.filtering {
		t.Fatalf("setup: filter=%q filtering=%v", m.filter, m.filtering)
	}

	m = mustPress(t, m, views.ZoneFilter, tea.MouseLeft)
	if !m.filtering {
		t.Fatal("filter chip press did not reopen the input")
	}
	if got := m.filterUI.Value(); got != "codex" {
		t.Fatalf("filter input = %q, want the current term codex", got)
	}
}

// TestBrowseRowZonesFollowTheScrolledWindow: on a list taller than the pane the
// row zones must track the WINDOW, not the screen. The press is aimed at the
// zone of a row that is only reachable after scrolling, and the row it selects
// must be the row that cell displays.
func TestBrowseRowZonesFollowTheScrolledWindow(t *testing.T) {
	m := newTestModelWH(t, &wideData{}, 160, 44)
	m = step(t, m, keyMsg("4")) // Browse, dim=tool, wideRows rows

	for i := 0; i < wideRows; i++ { // wheel the cursor to the last row
		m = wheelOver(t, m, views.ZoneTable, tea.MouseWheelDown)
	}
	if got := m.browse.Cursor(); got != wideRows-1 {
		t.Fatalf("cursor = %d after %d notches, want %d", got, wideRows, wideRows-1)
	}
	top := m.browse.WindowTop()
	if top == 0 {
		t.Fatalf("the table never scrolled (window top %d of %d rows) — the fixture is not exercising the defect",
			top, wideRows)
	}

	// Every row zone on screen must sit on the line that shows that row.
	for i := 0; i < wideRows; i++ {
		assertZoneShowsName(t, m, views.RowZone(i), fmt.Sprintf("tool-%02d", i))
	}
	// Rows scrolled out of the window carry no zone at all: a stale zone would
	// hand a press to a row the reader cannot see.
	if z := resolveZone(m, views.RowZone(0)); z != nil && !z.IsZero() {
		t.Fatalf("row 0 is scrolled out of the window but still resolves to %v", *z)
	}

	// The first visible row: pressing it selects it, and pressing it again drills
	// into it — the uniform contract, on a scrolled list.
	want := fmt.Sprintf("tool-%02d", top)
	m = expireClick(m)
	m = mustPress(t, m, views.RowZone(top), tea.MouseLeft)
	if got, _ := m.browse.SelectedValue(); got != want {
		t.Fatalf("press on the first visible row selected %q, want %q", got, want)
	}
	m = expireClick(m)
	m = mustPress(t, m, views.RowZone(top), tea.MouseLeft)
	if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" || m.crumbs[0].Value != want {
		t.Fatalf("second press drilled into %v, want [tool:%s]", m.crumbs, want)
	}
}

// TestBarZonesFollowTheWindowedPanel: the bars panel is a fixed-height card, so
// a tall list has to be windowed. Rendering past the fold and letting the frame
// clamp cut the overflow costs the hidden rows their zones AND the panel its own
// end marker — with which the wheel hit-test silently falls back to the focused
// pane. All three are asserted here.
func TestBarZonesFollowTheWindowedPanel(t *testing.T) {
	m := newTestModelWH(t, &wideData{}, 160, 44)
	m = step(t, m, keyMsg("2")) // By-Tool, wideRows bars

	// The long tail is folded, so the displayed list is shorter than wideRows
	// and its last row is the fold. Everything below is driven off the rendered
	// list rather than off wideRows, which is what keeps this a test of the
	// WINDOW rather than of the fold threshold.
	shown := len(m.byTool.Rows)
	if shown >= wideRows || m.byTool.FoldIndex != shown-1 {
		t.Fatalf("expected the tail folded into a trailing row: %d rows, fold at %d (of %d tools)",
			shown, m.byTool.FoldIndex, wideRows)
	}

	// wheelOver fails the test if the pane zone does not resolve, which is the
	// end-marker case: the notch has to reach the bars, not the fallback.
	for i := 0; i < wideRows; i++ {
		m = wheelOver(t, m, views.ZoneBars, tea.MouseWheelDown)
	}
	if got := m.byTool.Selected; got != shown-1 {
		t.Fatalf("selection = %d after %d notches over the bars, want %d", got, wideRows, shown-1)
	}

	for i := 0; i < m.byTool.FoldIndex; i++ {
		name := fmt.Sprintf("tool-%02d", i)
		assertZoneShowsName(t, m, views.BarZone(name), name)
	}

	// The selected row is on screen: a selection the reader cannot see is the
	// same bug as a row they cannot click. Here that row is the fold, which
	// carries its own zone precisely because it names no tool.
	if _, _, ok := zoneCenter(m, views.ZoneFold); !ok {
		t.Fatalf("the selected fold row is not on screen — the panel scrolled its selection out of the window")
	}
	m = expireClick(m)
	m = mustPress(t, m, views.ZoneFold, tea.MouseLeft)
	if m.byTool.Selected != m.byTool.FoldIndex {
		t.Fatalf("press on the fold zone selected row %d, want the fold row %d",
			m.byTool.Selected, m.byTool.FoldIndex)
	}
}

// TestInteractiveZonesResolve is the hit-test coverage gate issue #23 asked for:
// every surface the input model acts on must resolve to a zone at representative
// sizes. A Marked-but-unresolvable zone is exactly how TUIUX-10 shipped. The
// tall/scrolled half of the gate — the gap #23 named explicitly — is
// TestInteractiveZonesScrolled below.
func TestInteractiveZonesResolve(t *testing.T) {
	sizes := []struct{ w, h int }{{160, 44}, {120, 40}}
	for _, sz := range sizes {
		chrome := []string{
			views.ZoneRangePill, views.ZoneHelp, views.ZoneFreshness,
			views.ZoneSort, views.CrumbZone(0),
		}
		for i := 0; i < int(viewCount); i++ {
			chrome = append(chrome, views.RailZone(i))
		}
		perTab := map[string][]string{
			"1": {views.ZoneHero},
			"2": {views.ZoneBars, views.ZonePreview, views.BarZone("claude-code"), views.BarZone("codex")},
			"3": {views.ZoneBars, views.ZonePreview, views.BarZone("claude-opus")},
			"4": {views.ZoneTable, views.ZonePreview, views.RowZone(0), views.RowZone(1)},
		}
		for tab, want := range perTab {
			m := newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg(tab))
			for _, id := range append(append([]string{}, chrome...), want...) {
				if _, _, ok := zoneCenter(m, id); !ok {
					t.Errorf("%dx%d tab %s: zone %q does not resolve — the surface is unreachable by mouse",
						sz.w, sz.h, tab, id)
				}
			}
		}
	}
}

// TestInteractiveZonesScrolled is the scrolled half of the coverage gate: the
// same "every acted-on surface resolves" check, run on lists taller than the
// pane and after the cursor/selection has been driven to the far end. Issue #23
// calls this out by name — a suite whose every fixture fits on screen cannot
// see either of the defects it filed.
func TestInteractiveZonesScrolled(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{160, 44}, {120, 40}, {80, 24}} {
		// Browse: drive the cursor to the last row, then the pane, the preview and
		// the cursor's own row must all still resolve.
		m := newTestModelWH(t, &wideData{}, sz.w, sz.h)
		m = step(t, m, keyMsg("4"))
		for i := 0; i < wideRows; i++ {
			m = send(m, keyMsg("down"))
		}
		last := fmt.Sprintf("tool-%02d", wideRows-1)
		for _, id := range []string{views.ZoneTable, views.ZonePreview, views.RowZone(wideRows - 1)} {
			if _, _, ok := zoneCenter(m, id); !ok {
				t.Errorf("%dx%d Browse scrolled to the end: zone %q does not resolve", sz.w, sz.h, id)
			}
		}
		assertZoneShowsName(t, m, views.RowZone(wideRows-1), last)

		// By-Tool: same, driven to the end of the list. The long tail is folded
		// there, so the far end is the fold row and its zone is the one that has
		// to resolve — a fold row nobody can press is a fold nobody can open.
		m = newTestModelWH(t, &wideData{}, sz.w, sz.h)
		m = step(t, m, keyMsg("2"))
		for i := 0; i < wideRows; i++ {
			m = send(m, keyMsg("down"))
		}
		for _, id := range []string{views.ZoneBars, views.ZoneFold} {
			if _, _, ok := zoneCenter(m, id); !ok {
				t.Errorf("%dx%d By-Tool scrolled to the end: zone %q does not resolve", sz.w, sz.h, id)
			}
		}
		// The last MAJOR tool still has a bar zone of its own.
		major := fmt.Sprintf("tool-%02d", m.byTool.FoldIndex-1)
		m2 := newTestModelWH(t, &wideData{}, sz.w, sz.h)
		m2 = step(t, m2, keyMsg("2"))
		for i := 0; i < m2.byTool.FoldIndex-1; i++ {
			m2 = send(m2, keyMsg("down"))
		}
		assertZoneShowsName(t, m2, views.BarZone(major), major)
	}
}

// TestMouseSelectionRunsNoUIThreadQueries: the mouse paths obey the same
// invariant the keyboard ones do — a press or a wheel sweep prices its detail
// from cache and defers a miss to the debounced background flight. Nothing here
// may touch the store on the UI thread.
func TestMouseSelectionRunsNoUIThreadQueries(t *testing.T) {
	f := &fakeData{}
	m := newTestModelWH(t, f, 160, 44)
	m = step(t, m, keyMsg("4"))
	x, y, ok := zoneCenter(m, views.RowZone(1))
	if !ok {
		t.Fatal("row zone 1 is not on screen")
	}
	tx, ty, ok := zoneCenter(m, views.ZoneTable)
	if !ok {
		t.Fatal("table zone is not on screen")
	}

	n := queriesDuring(f, func() {
		tm, _ := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
		m = tm.(Model)
		for i := 0; i < 8; i++ {
			b := tea.MouseWheelDown
			if i%2 == 1 {
				b = tea.MouseWheelUp
			}
			tm, _ = m.Update(tea.MouseWheelMsg{Button: b, X: tx, Y: ty})
			m = tm.(Model)
		}
	})
	if n != 0 {
		t.Fatalf("mouse select + wheel sweep ran %d UI-thread queries, want 0", n)
	}
}

// TestDoubleClickWindowBounds keeps the gesture's one timing constant honest:
// two presses on the same zone inside the window are a double-click, and the
// same pair outside it is not.
func TestDoubleClickWindowBounds(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m.lastClickZone = views.RowZone(0)
	m.lastClickAt = time.Now()
	if !m.isDoubleClick(views.RowZone(0)) {
		t.Fatal("a press inside the window is not a double-click")
	}
	if m.isDoubleClick(views.RowZone(1)) {
		t.Fatal("a press on a different zone counted as a double-click")
	}
	m.lastClickAt = time.Now().Add(-2 * doubleClickWindow)
	if m.isDoubleClick(views.RowZone(0)) {
		t.Fatal("a press outside the window counted as a double-click")
	}
}

// TestFirstPressOnPreSelectedRowSelects: every view opens with a selection
// nobody made — Browse on row 0, the bars on bar 0 — so the FIRST press on it
// must select, not drill. Testing the drill on cursor equality alone let one
// stray tap on the top row of a fresh view descend a level (issue #43), which is
// precisely the "tap, tap again, no timing" story the model was chosen for. Run
// at two sizes because the row and bar geometry moves with the layout while the
// state contract must not. Each size is a SUBTEST: the assertions below are
// fatal, and a bare loop would let a break at the first size hide the second.
func TestFirstPressOnPreSelectedRowSelects(t *testing.T) {
	for _, sz := range sizeMatrix {
		t.Run(sz.name(), func(t *testing.T) {
			// Browse: the cursor sits on row 0 and no press has touched it.
			m := newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("4"))
			if m.browse.Cursor() != 0 {
				t.Fatalf("setup cursor = %d, want the default 0", m.browse.Cursor())
			}

			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 0 {
				t.Fatalf("the first press on the pre-selected row drilled to %v", m.crumbs)
			}
			if m.browse.Cursor() != 0 {
				t.Fatalf("the first press left the cursor at %d, want 0", m.browse.Cursor())
			}

			// Now the row IS one a press selected, so the next press drills.
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" || m.crumbs[0].Value != "claude-code" {
				t.Fatalf("second press on the selected row: crumbs = %v, want [tool:claude-code]", m.crumbs)
			}

			// By-Tool: bar 0 is selected before the reader has touched anything.
			m = newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("2"))
			if m.byTool.Selected != 0 {
				t.Fatalf("setup selection = %d, want the default 0", m.byTool.Selected)
			}

			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("claude-code"), tea.MouseLeft)
			if m.view != ViewByTool {
				t.Fatalf("the first press on the pre-selected bar drilled into %v", m.view)
			}
			if m.byTool.Selected != 0 {
				t.Fatalf("the first press left the selection at %d, want 0", m.byTool.Selected)
			}

			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("claude-code"), tea.MouseLeft)
			if m.view != ViewBrowse || len(m.crumbs) != 1 || m.crumbs[0].Value != "claude-code" {
				t.Fatalf("second press on the selected bar: view = %v crumbs = %v, want Browse [tool:claude-code]",
					m.view, m.crumbs)
			}
		})
	}
}

// TestDrillLeavesTheOpenedLevelUnchosen: a drill replaces the rows under the
// selection, so the level it opens starts on a default cursor nobody chose. The
// press that drilled must NOT carry over into it - otherwise the first tap in
// the new level descends again, which is issue #43 one level down. startLoad is
// the single choke point that drops the flag on every one of those paths, and
// this is the test that pins it: it presses IN the level the drill opens, at
// both the bar -> Browse and the Browse -> Browse call sites. Each size is a
// SUBTEST so a fatal break at the first one still leaves the second measured.
func TestDrillLeavesTheOpenedLevelUnchosen(t *testing.T) {
	for _, sz := range sizeMatrix {
		t.Run(sz.name(), func(t *testing.T) {
			m := newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("2")) // By-Tool

			// Two presses on the same bar: the first selects, the second drills.
			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("codex"), tea.MouseLeft)
			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("codex"), tea.MouseLeft)
			if m.view != ViewBrowse || len(m.crumbs) != 1 || m.crumbs[0].Value != "codex" {
				t.Fatalf("setup: view = %v crumbs = %v, want Browse [tool:codex]", m.view, m.crumbs)
			}
			if m.browse.Cursor() != 0 {
				t.Fatalf("the opened level starts at cursor %d, want the default 0", m.browse.Cursor())
			}

			// The level that just opened: row 0 is a default nobody pressed, so the
			// first press on it selects.
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 1 {
				t.Fatalf("the first press in the level the bar drill opened descended again: crumbs = %v", m.crumbs)
			}
			if m.browse.Cursor() != 0 {
				t.Fatalf("the first press in the opened level left the cursor at %d, want 0", m.browse.Cursor())
			}

			// Now the row IS one a press selected, so the next press drills - and
			// that drill opens another level on another unchosen default.
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 2 || m.crumbs[1].Dim != "model" || m.crumbs[1].Value != "claude-opus" {
				t.Fatalf("second press in the opened level: crumbs = %v, want [tool:codex model:claude-opus]", m.crumbs)
			}

			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 2 {
				t.Fatalf("the first press in the level the Browse drill opened descended again: crumbs = %v", m.crumbs)
			}
		})
	}
}

// TestKeyboardMovementDoesNotConferSelection is the keyboard twin of
// TestWheelDoesNotConferSelection: arrows, Home and End move the selection, they
// do not choose it. If a key counted as the selecting press, the next press on
// the row it landed on would drill on first touch - issue #43 again, one row
// down. All four keyboard clears (both forward branches, handleHome, handleEnd)
// are driven here through real key messages, at two sizes because the row and
// bar geometry the press has to hit moves with the layout. Each size is a
// SUBTEST so a fatal break at the first one still leaves the second measured.
func TestKeyboardMovementDoesNotConferSelection(t *testing.T) {
	for _, sz := range sizeMatrix {
		t.Run(sz.name(), func(t *testing.T) {
			// forward: the Browse branch, driven by an arrow key.
			m := newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("4"))
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft) // row 0 chosen by press
			m = send(m, keyMsg("down"))
			if m.browse.Cursor() != 1 {
				t.Fatalf("setup: the arrow left the cursor at %d, want 1", m.browse.Cursor())
			}
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
			if len(m.crumbs) != 0 {
				t.Fatalf("a press on the row the ARROW KEY moved to drilled: crumbs = %v", m.crumbs)
			}
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
			if len(m.crumbs) != 1 || m.crumbs[0].Value != "codex" {
				t.Fatalf("second press on the pressed row: crumbs = %v, want [tool:codex]", m.crumbs)
			}

			// handleEnd: the jump to the live edge / last row.
			m = newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("4"))
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			last := m.browseRowCount() - 1
			if last < 1 {
				t.Fatalf("the fixture has %d Browse rows, need >= 2 for End to move", m.browseRowCount())
			}
			m = send(m, tea.KeyPressMsg{Code: tea.KeyEnd})
			if m.browse.Cursor() != last {
				t.Fatalf("End left the cursor at %d, want %d", m.browse.Cursor(), last)
			}
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(last), tea.MouseLeft)
			if len(m.crumbs) != 0 {
				t.Fatalf("a press on the row END moved to drilled: crumbs = %v", m.crumbs)
			}
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(last), tea.MouseLeft)
			if len(m.crumbs) != 1 || m.crumbs[0].Value != "codex" {
				t.Fatalf("second press after End: crumbs = %v, want [tool:codex]", m.crumbs)
			}

			// handleHome: the jump back to the first row.
			m = newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("4"))
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(1), tea.MouseLeft) // row 1 chosen by press
			if m.browse.Cursor() != 1 {
				t.Fatalf("setup: press left the cursor at %d, want 1", m.browse.Cursor())
			}
			m = send(m, tea.KeyPressMsg{Code: tea.KeyHome})
			if m.browse.Cursor() != 0 {
				t.Fatalf("Home left the cursor at %d, want 0", m.browse.Cursor())
			}
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 0 {
				t.Fatalf("a press on the row HOME moved to drilled: crumbs = %v", m.crumbs)
			}
			m = expireClick(m)
			m = mustPress(t, m, views.RowZone(0), tea.MouseLeft)
			if len(m.crumbs) != 1 || m.crumbs[0].Value != "claude-code" {
				t.Fatalf("second press after Home: crumbs = %v, want [tool:claude-code]", m.crumbs)
			}

			// forward: the bars branch, driven by the same arrow key.
			m = newTestModelWH(t, &fakeData{}, sz.w, sz.h)
			m = step(t, m, keyMsg("2"))
			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("claude-code"), tea.MouseLeft) // bar 0 chosen by press
			m = send(m, keyMsg("down"))
			if m.byTool.Selected != 1 {
				t.Fatalf("setup: the arrow left the bar selection at %d, want 1", m.byTool.Selected)
			}
			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("codex"), tea.MouseLeft)
			if m.view != ViewByTool || len(m.crumbs) != 0 {
				t.Fatalf("a press on the bar the ARROW KEY moved to drilled: view = %v crumbs = %v",
					m.view, m.crumbs)
			}
			m = expireClick(m)
			m = mustPress(t, m, views.BarZone("codex"), tea.MouseLeft)
			if m.view != ViewBrowse || len(m.crumbs) != 1 || m.crumbs[0].Value != "codex" {
				t.Fatalf("second press on the pressed bar: view = %v crumbs = %v, want Browse [tool:codex]",
					m.view, m.crumbs)
			}
		})
	}
}

// TestWheelDoesNotConferSelection: a notch scrolls, it does not choose. If the
// wheel counted as the selecting press, the next press on the row it landed on
// would drill on first touch — the same stray descent as issue #43, one row
// down. The keyboard is held to the same rule by the same flag.
func TestWheelDoesNotConferSelection(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4"))

	m = expireClick(m)
	m = mustPress(t, m, views.RowZone(0), tea.MouseLeft) // row 0 chosen by press
	m = wheelOver(t, m, views.ZoneTable, tea.MouseWheelDown)
	if m.browse.Cursor() != 1 {
		t.Fatalf("setup: wheel left the cursor at %d, want 1", m.browse.Cursor())
	}

	m = expireClick(m)
	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
	if len(m.crumbs) != 0 {
		t.Fatalf("a press on the row the WHEEL moved to drilled: crumbs = %v", m.crumbs)
	}

	m = expireClick(m)
	m = mustPress(t, m, views.RowZone(1), tea.MouseLeft)
	if len(m.crumbs) != 1 || m.crumbs[0].Value != "codex" {
		t.Fatalf("second press on the pressed row: crumbs = %v, want [tool:codex]", m.crumbs)
	}
}
