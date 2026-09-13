package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/store"
)

// Reuse fakeData's query counter and fixtures. Different model costs make a
// sort change move the selected row; revision makes refresh changes observable.
type workspaceSource struct {
	fakeData
	revision int64
}

func (f *workspaceSource) Summarize(ctx context.Context, fl store.Filter) (*store.Summary, error) {
	s, err := f.fakeData.Summarize(ctx, fl)
	if err != nil {
		return nil, err
	}
	s.Totals.Input += f.revision
	for i := range s.Buckets {
		s.Buckets[i].Input += f.revision
		if len(fl.GroupBy) == 1 && fl.GroupBy[0] == "model" {
			s.Buckets[i].CostMicroUSD = int64(i+1) * 1000
		}
	}
	return s, nil
}

func newWorkspaceModel(t *testing.T) (*workspaceSource, Model) {
	t.Helper()
	f := &workspaceSource{}
	m := NewModel(f, Options{}) // exercise the production default directly
	t.Cleanup(m.zoneMgr.Close)
	m.data.now = func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local) }
	m = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	msg := m.loadCmd()()
	before := f.queries()
	m = send(m, msg)
	if got := f.queries() - before; got != 0 {
		t.Fatalf("initial apply ran %d UI queries", got)
	}
	if m.err != nil || m.fresh != FreshLive {
		t.Fatalf("initial load: err=%v fresh=%v", m.err, m.fresh)
	}
	return f, m
}

func workspaceUI(t *testing.T, f *workspaceSource, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	before := f.queries()
	next, cmd := m.Update(msg)
	if got := f.queries() - before; got != 0 {
		t.Fatalf("Update(%T) ran %d UI queries", msg, got)
	}
	return next.(Model), cmd
}

func workspaceApply(t *testing.T, f *workspaceSource, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a background load")
	}
	msg := cmd()
	m, next := workspaceUI(t, f, m, msg)
	if next != nil {
		t.Fatal("complete flight unexpectedly scheduled more work")
	}
	if m.err != nil || m.fresh != FreshLive {
		t.Fatalf("load: err=%v fresh=%v", m.err, m.fresh)
	}
	return m
}

func selectedWorkspaceIdentity(t *testing.T, m Model) string {
	t.Helper()
	b, ok := m.workspaceSelected()
	if !ok {
		t.Fatal("workspace has no selection")
	}
	return workspaceIdentity(b, m.workspaceGroup())
}

func TestWorkspaceProductionDefaultModelFirst(t *testing.T) {
	f, m := newWorkspaceModel(t)
	if m.view != ViewOverview || m.classicOverview || m.heroPivot || m.workspaceGroup() != "model" {
		t.Fatalf("default view=%v classic=%v pivot=%v group=%q", m.view, m.classicOverview, m.heroPivot, m.workspaceGroup())
	}
	if len(m.workspace.rows) != 2 || m.workspace.appliedGroup != "model" {
		t.Fatalf("comparison = %+v", m.workspace)
	}
	if len(m.workspace.appliedScope) != 0 || m.workspace.appliedSpan != m.span() {
		t.Fatalf("initial scope/span = %+v / %+v", m.workspace.appliedScope, m.workspace.appliedSpan)
	}
	before := f.queries()
	frame := m.View().Content
	if !strings.Contains(frame, "Models") || !strings.Contains(frame, "gpt-5") {
		t.Fatalf("model comparison missing:\n%s", frame)
	}
	if f.queries() != before {
		t.Fatal("render queried the source")
	}
}

func TestWorkspaceSortChartAndMetricCardsIndependent(t *testing.T) {
	f, m := newWorkspaceModel(t)
	identity := selectedWorkspaceIdentity(t, m)
	// Sort menu: choose total. Chart stays on cost and selection survives movement.
	for _, key := range []string{"s", "down", "enter"} {
		m, _ = workspaceUI(t, f, m, keyMsg(key))
	}
	if m.sort != SortTotal || m.workspace.chart != UsageMetricCost || selectedWorkspaceIdentity(t, m) != identity {
		t.Fatal("sort changed chart or selected identity")
	}
	for _, key := range []string{"c", "down", "down", "down", "enter"} {
		m, _ = workspaceUI(t, f, m, keyMsg(key))
	}
	if m.workspace.chart != UsageMetricCache || m.sort != SortTotal || selectedWorkspaceIdentity(t, m) != identity {
		t.Fatal("chart changed sort or selected identity")
	}
	for _, card := range []struct {
		zone   string
		metric UsageMetric
	}{
		{views.ZoneWorkspaceInput, UsageMetricInput}, {views.ZoneWorkspaceOutput, UsageMetricOutput},
		{views.ZoneWorkspaceCache, UsageMetricCache}, {views.ZoneWorkspaceCost, UsageMetricCost},
	} {
		z := resolveZone(m, card.zone)
		if z == nil || z.IsZero() {
			t.Fatalf("card %s has no mouse zone", card.zone)
		}
		m, _ = workspaceUI(t, f, m, tea.MouseClickMsg{Button: tea.MouseLeft, X: (z.StartX + z.EndX) / 2, Y: (z.StartY + z.EndY) / 2})
		if m.workspace.overlay != string(card.metric)+" inspector" || !strings.Contains(m.workspace.content, "Selected scope: All usage") {
			t.Fatalf("card %s did not open scoped inspector", card.metric)
		}
		if m.sort != SortTotal || m.workspace.chart != UsageMetricCache || selectedWorkspaceIdentity(t, m) != identity {
			t.Fatal("card changed comparison state")
		}
		m, _ = workspaceUI(t, f, m, keyMsg("esc"))
	}
}

func TestWorkspaceMouseSelectsBeforeActivating(t *testing.T) {
	f, m := newWorkspaceModel(t)
	z := resolveZone(m, "workspace-row-1")
	if z == nil || z.IsZero() {
		t.Fatalf("second row has no mouse zone; row0=%+v row1=%+v\n%s", m.zoneMgr.Get("workspace-row-0"), m.zoneMgr.Get("workspace-row-1"), m.View().Content)
	}
	click := tea.MouseClickMsg{Button: tea.MouseLeft, X: (z.StartX + z.EndX) / 2, Y: (z.StartY + z.EndY) / 2}
	var cmd tea.Cmd
	m, cmd = workspaceUI(t, f, m, click)
	if cmd != nil || m.workspace.cursor != 1 || len(m.crumbs) != 0 || m.workspaceRequestedGroup() != "model" {
		t.Fatal("first row click activated instead of selecting")
	}
	selected, _ := m.workspaceSelected()
	m, cmd = workspaceUI(t, f, m, click)
	if cmd == nil || m.workspaceRequestedGroup() != "session" || !slices.Contains(m.crumbs, Crumb{Dim: "model", Value: selected.Keys["model"]}) {
		t.Fatalf("second click failed to open selected scope: %+v", m.crumbs)
	}
	m = workspaceApply(t, f, m, cmd)
	if m.workspaceGroup() != "session" {
		t.Fatal("session grouping did not apply")
	}
}

func TestWorkspaceBackRestoresSelectionAndSort(t *testing.T) {
	f, m := newWorkspaceModel(t)
	identity := selectedWorkspaceIdentity(t, m)
	oldCursor := m.workspace.cursor
	for _, key := range []string{"s", "down", "down", "down", "enter"} {
		m, _ = workspaceUI(t, f, m, keyMsg(key))
	}
	if m.sort != SortName || selectedWorkspaceIdentity(t, m) != identity || m.workspace.cursor == oldCursor {
		t.Fatal("name sort did not move and preserve the selected identity")
	}
	m.workspace.top = 1
	var cmd tea.Cmd
	m, cmd = workspaceUI(t, f, m, keyMsg("enter"))
	m = workspaceApply(t, f, m, cmd)
	m, cmd = workspaceUI(t, f, m, keyMsg("esc"))
	m = workspaceApply(t, f, m, cmd)
	if m.workspaceGroup() != "model" || len(m.crumbs) != 0 || m.sort != SortName || selectedWorkspaceIdentity(t, m) != identity || m.workspace.top != 1 {
		t.Fatalf("Back lost origin: group=%s crumbs=%+v sort=%v cursor=%d top=%d", m.workspaceGroup(), m.crumbs, m.sort, m.workspace.cursor, m.workspace.top)
	}
}

func TestWorkspacePendingGroupingKeepsAppliedMeaning(t *testing.T) {
	f, m := newWorkspaceModel(t)
	oldRows := slices.Clone(m.workspace.rows)
	oldScope := m.workspaceContext(true).Scope
	for _, key := range []string{"o", "down"} {
		m, _ = workspaceUI(t, f, m, keyMsg(key))
	}
	m, cmd := workspaceUI(t, f, m, keyMsg("enter"))
	if m.workspaceRequestedGroup() != "tool" || m.workspaceGroup() != "model" || !reflect.DeepEqual(m.workspace.rows, oldRows) {
		t.Fatal("pending grouping reinterpreted displayed rows")
	}
	if !reflect.DeepEqual(m.workspaceContext(true).Scope, oldScope) {
		t.Fatal("pending grouping changed selected evidence scope")
	}
	held, openCmd := workspaceUI(t, f, m, keyMsg("enter"))
	if openCmd != nil || len(held.crumbs) != 0 {
		t.Fatal("pending load allowed activation of reinterpreted rows")
	}
	m = workspaceApply(t, f, m, cmd)
	if m.workspaceGroup() != "tool" || len(m.workspace.rows) != 2 || m.workspace.rows[0].Keys["tool"] == "" {
		t.Fatal("new grouping did not apply after its load")
	}
}

func TestWorkspaceSuggestionInspectKeepsCapturedContextAndReturns(t *testing.T) {
	f, m := newWorkspaceModel(t)
	m, _ = workspaceUI(t, f, m, keyMsg("down"))
	selected := &m.workspace.rows[m.workspace.cursor]
	selected.UnpricedEvents = 1
	selected.CostMicroUSD = 7
	selected.CacheCreation = 20
	identity := selectedWorkspaceIdentity(t, m)
	m.fresh = FreshStale
	captured := m.workspaceContext(true)
	before := f.queries()
	m, _ = workspaceUI(t, f, m, keyMsg("u"))
	if len(m.workspace.suggestions) != 2 || len(m.workspace.menu) != 2 || m.workspace.overlay != "Suggestions" {
		t.Fatalf("suggestion menu = overlay %q suggestions %#v menu %#v", m.workspace.overlay, m.workspace.suggestions, m.workspace.menu)
	}
	m, _ = workspaceUI(t, f, m, keyMsg("down"))
	m, cmd := workspaceUI(t, f, m, keyMsg("enter"))
	if cmd != nil || m.workspace.overlay != "Cache inspector" {
		t.Fatalf("second suggestion opened overlay %q cmd=%v", m.workspace.overlay, cmd)
	}
	for _, want := range []string{"Inspect cache writes and reuse", "Cache details", "Selected range: " + captured.RangeLabel, "Selected scope: " + captured.ScopeLabel, "Stale snapshot"} {
		if !strings.Contains(m.workspace.content, want) {
			t.Errorf("Cache inspector missing %q:\n%s", want, m.workspace.content)
		}
	}
	if f.queries() != before || selectedWorkspaceIdentity(t, m) != identity {
		t.Fatal("suggestion inspect queried the source or changed selection")
	}

	m, cmd = workspaceUI(t, f, m, keyMsg("esc"))
	if cmd != nil || m.workspace.overlay != "Suggestions" || len(m.workspace.menu) != 2 || m.workspace.menuCursor != 1 || selectedWorkspaceIdentity(t, m) != identity {
		t.Fatalf("Back lost suggestion list or origin: overlay=%q menu=%v cursor=%d identity=%q", m.workspace.overlay, m.workspace.menu, m.workspace.menuCursor, selectedWorkspaceIdentity(t, m))
	}
	before = f.queries()
	m = mustPress(t, m, "workspace-action-0", tea.MouseLeft)
	if m.workspace.overlay != "Cost inspector" || !strings.Contains(m.workspace.content, "Inspect missing price coverage") {
		t.Fatalf("mouse opened %q:\n%s", m.workspace.overlay, m.workspace.content)
	}
	if f.queries() != before || selectedWorkspaceIdentity(t, m) != identity {
		t.Fatal("mouse suggestion inspect queried the source or changed selection")
	}
}

func TestWorkspaceSuggestionCaptureSurvivesLaterData(t *testing.T) {
	f, m := newWorkspaceModel(t)
	m.workspace.rows[m.workspace.cursor].CacheCreation = 123
	m, _ = workspaceUI(t, f, m, keyMsg("o"))
	m, _ = workspaceUI(t, f, m, keyMsg("u"))
	if m.workspace.chooser != "" {
		t.Fatal("suggestion menu retained an inline chooser")
	}
	captured := m.workspace.suggestionContext
	// A pending refresh can apply while the user is reading the suggestion list.
	m.workspace.rows = slices.Clone(m.workspace.rows)
	m.workspace.rows[m.workspace.cursor].CacheCreation = 999
	m.workspace.appliedRange = "a later period"
	m.workspace.appliedScope = []Crumb{{Dim: "provider", Value: "another provider"}}
	m, cmd := workspaceUI(t, f, m, keyMsg("enter"))
	if cmd != nil || !strings.Contains(m.workspace.content, "Cache write: 123 tokens") ||
		!strings.Contains(m.workspace.content, "Selected range: "+captured.RangeLabel) ||
		!strings.Contains(m.workspace.content, "Selected scope: "+captured.ScopeLabel) {
		t.Fatalf("inspector lost captured evidence: %s", m.workspace.content)
	}
	m, _ = workspaceUI(t, f, m, keyMsg("esc"))
	if m.workspace.overlay != "Suggestions" || len(m.workspace.menu) == 0 {
		t.Fatal("Back did not restore the suggestion list")
	}
	m, _ = workspaceUI(t, f, m, keyMsg("esc"))
	if m.workspace.overlay != "" || m.workspace.chooser != "" {
		t.Fatal("Back did not restore the workspace")
	}
}

func TestWorkspaceCacheMissRetainsAtomicSnapshotAndRecovers(t *testing.T) {
	f, m := newWorkspaceModel(t)
	oldOverview, oldRows := m.overview, slices.Clone(m.workspace.rows)
	f.revision = 12345
	m.data.Invalidate()
	cmd := m.startLoad()
	msg := cmd()
	// Evict only the comparison after its flight. The new overview remains
	// warm, so this catches a partial apply that mixes generations.
	filter := m.data.filterFor(m.qctx(), m.qnow(), m.span(), m.crumbs, []string{"model"})
	key := cacheKey(filter)
	m.data.mu.Lock()
	entry := m.data.cache.m[key]
	if entry != nil {
		m.data.cache.ll.Remove(entry)
		delete(m.data.cache.m, key)
	}
	m.data.mu.Unlock()
	if entry == nil {
		t.Fatal("flight did not warm model comparison")
	}
	m, retry := workspaceUI(t, f, m, msg)
	if !reflect.DeepEqual(m.overview, oldOverview) || !reflect.DeepEqual(m.workspace.rows, oldRows) || m.fresh != FreshCutIn || retry == nil {
		t.Fatal("comparison cache miss replaced part of the displayed snapshot or failed to schedule recovery")
	}
	m, flight := workspaceUI(t, f, m, detailDebounceMsg{seq: m.detailSeq})
	if flight == nil {
		t.Fatal("detail debounce did not dispatch recovery")
	}
	before := f.queries()
	recovered := flight()
	if f.queries() <= before {
		t.Fatal("recovery did not refill the evicted comparison")
	}
	m, next := workspaceUI(t, f, m, recovered)
	if next != nil || m.fresh != FreshLive || m.detailWanted || m.err != nil {
		t.Fatalf("recovery did not settle: fresh=%v err=%v wanted=%v", m.fresh, m.err, m.detailWanted)
	}
	if m.overview.Totals.Input != oldOverview.Totals.Input+f.revision || m.workspace.rows[0].Input != oldRows[0].Input+f.revision {
		t.Fatal("recovery did not apply overview and comparison from the new snapshot")
	}
}

func TestWorkspaceViewportBoundsAndDetailReturn(t *testing.T) {
	f, m := newWorkspaceModel(t)
	m, _ = workspaceUI(t, f, m, keyMsg("down"))
	identity := selectedWorkspaceIdentity(t, m)
	for _, size := range [][2]int{{42, 12}, {42, 24}, {55, 52}, {120, 40}, {160, 50}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, _ = workspaceUI(t, f, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			for _, overlay := range []bool{false, true} {
				if overlay {
					m, _ = workspaceUI(t, f, m, keyMsg("d"))
					if m.workspace.overlay == "" {
						t.Fatal("full details unavailable")
					}
				}
				before := f.queries()
				frame := m.View().Content
				lines := strings.Split(frame, "\n")
				if len(lines) > size[1] {
					t.Fatalf("frame height=%d exceeds %d", len(lines), size[1])
				}
				for i, line := range lines {
					if width := lipgloss.Width(line); width > size[0] {
						t.Fatalf("line %d width=%d exceeds %d", i, width, size[0])
					}
				}
				if f.queries() != before {
					t.Fatal("resize render queried source")
				}
				if overlay {
					m, _ = workspaceUI(t, f, m, keyMsg("esc"))
				}
			}
			if selectedWorkspaceIdentity(t, m) != identity {
				t.Fatal("resize/details lost selection")
			}
		})
	}
}

func TestWorkspacePendingFilterDoesNotClaimCompleteContributors(t *testing.T) {
	_, m := newWorkspaceModel(t)
	m.filter = "gpt"
	m = loadOnce(m)
	if len(m.workspace.rows) != 1 {
		t.Fatal("filter fixture must reduce the comparison")
	}
	m.filter = ""
	_ = m.startLoad()
	if got := m.workspaceContext(false).Contributors; len(got) != 0 {
		t.Fatalf("pending filter clear exposed partial contributors: %v", got)
	}
}

func TestWorkspaceCompactMenuMouseAfterScroll(t *testing.T) {
	_, m := newWorkspaceModel(t)
	m = send(m, tea.WindowSizeMsg{Width: 42, Height: 12})
	frame := plainFrame(m)
	for _, label := range []string{"Input", "Output", "Cache", "Cost", "cpu", "mem", "disk", "More"} {
		if !strings.Contains(frame, label) {
			t.Fatalf("tiny workspace lost %s: %s", label, frame)
		}
	}
	m = mustPress(t, m, "workspace-menu-more", tea.MouseLeft)
	m.workspace.menuCursor = len(m.workspace.menu) - 1
	m = mustPress(t, m, fmt.Sprintf("workspace-action-%d", m.workspace.menuCursor), tea.MouseLeft)
	if m.workspace.overlay != "" {
		t.Fatal("last visible menu row did not activate after scrolling")
	}
}

func TestWorkspaceFilterInputRemainsVisible(t *testing.T) {
	_, m := newWorkspaceModel(t)
	m = send(m, keyMsg("/"))
	for _, r := range "needlefilter" {
		m = send(m, keyMsg(string(r)))
	}
	if !m.filtering || !strings.Contains(plainFrame(m), "needlefilter") {
		t.Fatal("workspace footer hid the active filter input")
	}
}
