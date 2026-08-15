package tui

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

var ansiAct = regexp.MustCompile("\x1b\\[[0-9;:?]*[a-zA-Z]")

// plainFrame renders a model and strips every escape sequence, leaving the text
// channel the assertions read (and the channel a monochrome terminal shows).
func plainFrame(m Model) string { return ansiAct.ReplaceAllString(m.View().Content, "") }

// The Activity tab must be reachable both ways: by its digit and by cycling.
// The cycle assertion is derived from viewList rather than written out, so a
// sixth tab cannot be added without Tab reaching it.
func TestActivityTabReachable(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = step(t, m, keyMsg("5"))
	if m.view != ViewActivity {
		t.Fatalf("after '5' view = %v, want Activity", m.view)
	}

	m = step(t, m, keyMsg("1"))
	seen := map[View]bool{m.view: true}
	for range viewList {
		m = step(t, m, keyMsg("tab"))
		seen[m.view] = true
	}
	for _, meta := range viewList {
		if !seen[meta.v] {
			t.Errorf("Tab cycling never reached %q (key %s)", meta.label, meta.key)
		}
	}
}

// The persisted tab round-trips: a saved "activity" restores onto the Activity
// tab, which is what makes the state file worth writing at all.
func TestActivityTabPersists(t *testing.T) {
	if got := ViewActivity.Key(); got != "5" {
		t.Fatalf("ViewActivity.Key() = %q, want \"5\"", got)
	}
	if v, ok := viewFromKey("5"); !ok || v != ViewActivity {
		t.Fatalf("viewFromKey(\"5\") = %v, %v; want Activity, true", v, ok)
	}

	path := filepath.Join(t.TempDir(), "ui-state.json")
	SaveUIState(path, UIState{Range: Range7d.Key(), Tab: ViewActivity.Key()})
	if got := LoadUIState(path).Tab; got != "5" {
		t.Fatalf("persisted tab = %q, want \"5\"", got)
	}
	if v := NewModel(&fakeData{}, Options{StatePath: path}).view; v != ViewActivity {
		t.Fatalf("restored view = %v, want Activity", v)
	}
}

// Every tab must be clickable at every nav width. The fifth tab pushed the
// labelled strip past 74 columns, where the row's MaxWidth clamp would have
// taken its zone marker with it.
func TestActivityTabStripFitsAtEveryWidth(t *testing.T) {
	for _, w := range []int{56, 60, 74, 90, 100, 120, 160} {
		m := newTestModelWH(t, &fakeData{}, w, 30)
		strip := m.renderTabStrip()
		if got, budget := lipgloss.Width(strip), m.frameW()-2; got > budget {
			t.Errorf("width %d: tab strip is %d cells, budget is %d", w, got, budget)
		}
		for _, meta := range viewList {
			if !strings.Contains(strip, zoneOpen(m, views.RailZone(int(meta.v)))) {
				t.Errorf("width %d: tab %q has no click zone", w, meta.label)
			}
		}
	}
}

// zoneOpen returns the marker the zone manager wraps an id in, so a test can
// assert the id survived into the rendered row.
func zoneOpen(m Model, id string) string {
	return m.zoneMgr.Mark(id, "")
}

// The rendered Activity frame must fit its terminal at wide, narrow and short
// geometries, with the long mcp__ names in the page.
func TestActivityFrameGeometry(t *testing.T) {
	for _, geo := range []struct{ w, h int }{{160, 44}, {100, 30}, {74, 20}, {60, 16}, {44, 12}} {
		m := newTestModelWH(t, &fakeData{}, geo.w, geo.h)
		m = step(t, m, keyMsg("5"))
		frame := m.View().Content
		lines := strings.Split(frame, "\n")
		if len(lines) > geo.h {
			t.Errorf("%dx%d: frame is %d rows", geo.w, geo.h, len(lines))
		}
		for i, l := range lines {
			if got := lipgloss.Width(l); got > geo.w {
				t.Errorf("%dx%d: line %d is %d cells: %q", geo.w, geo.h, i, got, ansiAct.ReplaceAllString(l, ""))
			}
		}
	}
}

// The unattributed volume reaches the frame at every geometry — it is the
// disclosure that makes the ranking above it readable.
func TestActivityFrameStatesUnattributed(t *testing.T) {
	for _, geo := range []struct{ w, h int }{{120, 40}, {100, 30}, {74, 20}, {60, 16}} {
		m := newTestModelWH(t, &fakeData{}, geo.w, geo.h)
		m = step(t, m, keyMsg("5"))
		out := plainFrame(m)
		if !strings.Contains(out, "unattributed") && !strings.Contains(out, "no token join") {
			t.Errorf("%dx%d: frame never names the unattributed calls:\n%s", geo.w, geo.h, out)
		}
		if !strings.Contains(out, "1.0K") {
			t.Errorf("%dx%d: frame does not state the unattributed count:\n%s", geo.w, geo.h, out)
		}
	}
}

// orderSource records the ranking metric every TopActivity call asks for.
type orderSource struct {
	fakeData
	mu     sync.Mutex
	orders []store.ActivityOrder
}

func (s *orderSource) TopActivity(ctx context.Context, f store.ActivityFilter, by store.ActivityOrder, limit int) ([]store.ActivityBucket, error) {
	s.mu.Lock()
	s.orders = append(s.orders, by)
	s.mu.Unlock()
	return s.fakeData.TopActivity(ctx, f, by, limit)
}

func (s *orderSource) seen(want store.ActivityOrder) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orders {
		if o == want {
			return true
		}
	}
	return false
}

// Cycling the sort key must reach every ranking metric the ledger supports, and
// the page must actually be re-ranked (not re-labelled).
func TestActivitySortCyclesEveryOrder(t *testing.T) {
	src := &orderSource{}
	m := newTestModel(t, src)
	m = step(t, m, keyMsg("5"))

	for _, want := range []store.ActivityOrder{store.ActivityByTokens, store.ActivityByCalls, store.ActivityByCost} {
		if !cycleUntilOrder(t, m, src, want) {
			t.Errorf("sort cycle never ranked by %q (orders: %v)", want, src.orders)
		}
	}
}

// cycleUntilOrder presses the sort key until src has seen the wanted ranking
// metric, bounded by the length of the sort cycle.
func cycleUntilOrder(t *testing.T, m Model, src *orderSource, want store.ActivityOrder) bool {
	t.Helper()
	for range sortOrder {
		if src.seen(want) {
			return true
		}
		m = step(t, m, keyMsg("s"))
	}
	return src.seen(want)
}

// Ranking by calls must put the busiest invocation first even when its cost is
// unknown — the ordering is the store's, and the tab must not re-sort it.
func TestActivityRankOrderFollowsTheStore(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = step(t, m, keyMsg("5"))
	m = step(t, m, keyMsg("s")) // total -> events == calls
	if m.sort != SortEvents {
		t.Fatalf("sort = %v, want events", m.sort)
	}
	if len(m.activity.Rows) == 0 {
		t.Fatal("no activity rows loaded")
	}
	if got := m.activity.Rows[0].Keys["name"]; got != "exec" {
		t.Errorf("top row by calls = %q, want the 790-call codex row \"exec\"", got)
	}
	if m.activity.OrderLbl != "calls" {
		t.Errorf("order label = %q, want calls", m.activity.OrderLbl)
	}
}

// Sorting by name re-orders the page in memory without asking the store for a
// different one (there is no name ranking in SQL).
func TestActivityNameSortOrdersThePage(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = step(t, m, keyMsg("5"))
	m = step(t, m, keyMsg("s")) // events
	m = step(t, m, keyMsg("s")) // name
	if m.sort != SortName {
		t.Fatalf("sort = %v, want name", m.sort)
	}
	names := make([]string, 0, len(m.activity.Rows))
	for _, b := range m.activity.Rows {
		names = append(names, b.Keys["name"])
	}
	if !sortedAsc(names) {
		t.Errorf("name sort left the page unordered: %v", names)
	}
}

func sortedAsc(v []string) bool {
	for i := 1; i < len(v); i++ {
		if v[i-1] > v[i] {
			return false
		}
	}
	return true
}

// A fresh database has no activity rows at all. That must render the honest
// no-rows treatment inside a whole frame, not a broken one.
func TestActivityEmptyLedgerRendersWholeFrame(t *testing.T) {
	m := newTestModelWH(t, &emptySource{}, 100, 30)
	m = step(t, m, keyMsg("5"))
	out := plainFrame(m)
	if !strings.Contains(out, "no rows in range") {
		t.Errorf("empty activity frame missing the no-rows treatment:\n%s", out)
	}
	for i, l := range strings.Split(m.View().Content, "\n") {
		if got := lipgloss.Width(l); got > 100 {
			t.Fatalf("empty frame line %d is %d cells", i, got)
		}
	}
	if m.activity.Selected != 0 {
		t.Errorf("selection = %d on an empty page, want 0", m.activity.Selected)
	}
}

// Revisiting the tab inside one load generation must be free: the three
// activity queries are cached on the same keys the flight warmed.
func TestActivityRevisitHitsTheCache(t *testing.T) {
	src := &fakeData{}
	m := newTestModel(t, src)
	m = step(t, m, keyMsg("5"))

	m = step(t, m, keyMsg("1"))
	again := queriesDuring(src, func() { m = step(t, m, keyMsg("5")) })
	if again != 0 {
		t.Errorf("revisiting Activity ran %d queries, want 0 (warm cache)", again)
	}
	// The warm reading only means something against a cold one: a tab that
	// queried nothing at all would pass the assertion above for free.
	fresh := &fakeData{}
	fm := newTestModel(t, fresh)
	if n := queriesDuring(fresh, func() { _ = step(t, fm, keyMsg("5")) }); n < 3 {
		t.Errorf("first visit to Activity ran %d queries, want the 3 the tab needs", n)
	}
}

// Moving the selection queries nothing: the detail card is a projection of the
// row already on screen.
func TestActivitySelectionQueriesNothing(t *testing.T) {
	src := &fakeData{}
	m := newTestModel(t, src)
	m = step(t, m, keyMsg("5"))
	before := m.activity.Selected
	if n := queriesDuring(src, func() { m = step(t, m, keyMsg("down")) }); n != 0 {
		t.Errorf("moving the Activity selection ran %d queries, want 0", n)
	}
	if m.activity.Selected == before {
		t.Errorf("selection did not move from %d", before)
	}
}

// A press on a rank row selects it. There is no drill on this tab, so a second
// press must not descend anywhere.
func TestActivityRowPressSelects(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 120, 40)
	m = step(t, m, keyMsg("5"))
	if len(m.activity.Rows) < 2 {
		t.Fatal("need at least two rows")
	}
	m, ok := click(t, m, views.ActZone(1))
	if !ok {
		t.Fatalf("no click zone for %s", views.ActZone(1))
	}
	if m.activity.Selected != 1 {
		t.Fatalf("press left selection at %d, want 1", m.activity.Selected)
	}
	m, _ = click(t, m, views.ActZone(1))
	if m.view != ViewActivity {
		t.Errorf("a second press on the row left the tab: view = %v", m.view)
	}
	if len(m.crumbs) != 0 {
		t.Errorf("a second press drilled: crumbs = %v", m.crumbs)
	}
}

// The drill stack narrows the Activity tab the same way it narrows every other:
// a crumb carried onto it must reach the activity filter.
func TestActivityCrumbsNarrowTheFilter(t *testing.T) {
	d := NewData(&fakeData{})
	f := d.activityFilterFor(d.now(), Span{R: Range7d}, []Crumb{
		{Dim: "tool", Value: "codex"},
		{Dim: "session", Value: "sess-1"},
	}, []string{"name"})
	if len(f.Tools) != 1 || f.Tools[0] != "codex" {
		t.Errorf("tool crumb did not reach the filter: %+v", f.Tools)
	}
	if len(f.Sessions) != 1 || f.Sessions[0] != "sess-1" {
		t.Errorf("session crumb did not reach the filter: %+v", f.Sessions)
	}
	if f.Since.IsZero() {
		t.Error("activity filter carries no window")
	}
}
