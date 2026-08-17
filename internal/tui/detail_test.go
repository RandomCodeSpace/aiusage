package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

// detail_test.go covers issue #4 chunk 1d/1e: debounced + coalescing detail
// loads, the prewarmed scrub compositions, store-level session counts and the
// single-query modelOwners.

// scrubData is fakeData with a 30-bucket day timeline (and the matching
// [day, tool] composition), so a scrub sweep has 30 positions to cross.
type scrubData struct{ fakeData }

func scrubDays() []store.Bucket {
	base := time.Date(2026, 7, 11, 0, 0, 0, 0, time.Local)
	out := make([]store.Bucket, 30)
	for i := range out {
		day := base.AddDate(0, 0, i).Format("2006-01-02")
		out[i] = store.Bucket{
			Keys:        map[string]string{"day": day},
			OrderedKeys: []string{"day"},
			Events:      int64(i + 1),
			Input:       int64(1000 * (i + 1)),
			Total:       int64(4000 * (i + 1)),
		}
	}
	return out
}

func (s *scrubData) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	if len(f.GroupBy) > 0 && f.GroupBy[0] == "day" {
		s.summarizeCalls.Add(1)
		days := scrubDays()
		if len(f.GroupBy) == 1 {
			return &store.Summary{GroupBy: f.GroupBy, Buckets: days}, nil
		}
		// [day, tool]: split each day between the two canned tools, index-
		// aligned dominance as in fakeCross.
		tools := []string{"claude-code", "codex"}
		var out []store.Bucket
		for i, d := range days {
			for j, tool := range tools {
				w := int64(1)
				if j == i%len(tools) {
					w = 2
				}
				out = append(out, store.Bucket{
					Keys:        map[string]string{"day": d.Keys["day"], f.GroupBy[1]: tool},
					OrderedKeys: f.GroupBy,
					Events:      d.Events * w / 3,
					Total:       d.Total * w / 3,
				})
			}
		}
		return &store.Summary{GroupBy: f.GroupBy, Buckets: out}, nil
	}
	return s.fakeData.Summarize(ctx, f)
}

// newScrubModel builds a loaded, clock-pinned Overview model over a 30-day
// timeline.
func newScrubModel(t *testing.T) (*scrubData, Model) {
	t.Helper()
	f := &scrubData{}
	m := NewModel(f, Options{DBPath: "/tmp/usage.db"})
	m.data.now = func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local) }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = loadOnce(tm.(Model))
	if n := len(m.tlData.Buckets); n != 30 {
		t.Fatalf("timeline buckets = %d, want 30", n)
	}
	if n := len(m.scrubComp); n != 30 {
		t.Fatalf("scrub compositions = %d, want 30 (prewarm missing)", n)
	}
	return f, m
}

// TestScrubSweepWarmZeroQueries is the 1d acceptance: a wheel sweep across all
// 30 buckets with a warm cache runs ZERO store queries on the UI thread — and
// dispatches no commands at all, because pinned repricing is fully local
// (timeline bucket + prewarmed composition).
func TestScrubSweepWarmZeroQueries(t *testing.T) {
	f, m := newScrubModel(t)

	wheel := tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 5, Y: 5}
	n := queriesDuring(&f.fakeData, func() {
		for i := 0; i < 29; i++ {
			tm, cmd := m.Update(wheel)
			m = tm.(Model)
			if cmd != nil {
				t.Fatalf("wheel step %d dispatched a command (want fully local)", i)
			}
		}
	})
	if n != 0 {
		t.Fatalf("warm scrub sweep ran %d queries, want 0", n)
	}
	if m.scrubIndex != 29 || !m.scrubPinned {
		t.Fatalf("sweep ended at index %d pinned=%v, want 29 pinned", m.scrubIndex, m.scrubPinned)
	}
	// The KPI strip repriced to the final bucket and the side bars + readout
	// repriced from the prewarmed composition.
	if got, want := m.overview.Totals.Total, m.tlData.Buckets[29].Total; got != want {
		t.Fatalf("scrubbed totals = %d, want bucket total %d", got, want)
	}
	if len(m.overview.ByTool) == 0 {
		t.Fatal("scrubbed side bars empty (composition not applied)")
	}
	// Index 29: 29%2 == 1 → codex dominates that day.
	if m.tlData.TopTool != "codex" {
		t.Fatalf("top tool at bucket 29 = %q, want codex", m.tlData.TopTool)
	}
}

// TestScrubSweepColdCoalesces: with the query cache fully invalidated (the
// window between a refresh Invalidate and its flight landing), a 30-bucket
// sweep STILL runs zero UI-thread queries — the prewarmed composition is model
// state, not cache. The one interaction that does need the store (unpinning
// back to full-range totals) coalesces into a single debounced background
// flight of a handful of queries, not one per notch.
func TestScrubSweepColdCoalesces(t *testing.T) {
	f, m := newScrubModel(t)
	m.data.Invalidate() // cold cache; on-screen generation intact

	wheel := tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 5, Y: 5}
	n := queriesDuring(&f.fakeData, func() {
		for i := 0; i < 29; i++ {
			tm, cmd := m.Update(wheel)
			m = tm.(Model)
			if cmd != nil {
				t.Fatalf("cold wheel step %d dispatched a command", i)
			}
		}
	})
	if n != 0 {
		t.Fatalf("cold scrub sweep ran %d UI-thread queries, want 0", n)
	}

	// Unpin: the spring-back needs full-range summaries — cache-only misses,
	// so the frame keeps the pinned-era numbers and a debounce timer arms.
	var cmd tea.Cmd
	n = queriesDuring(&f.fakeData, func() {
		tm, c := m.Update(keyMsg("esc"))
		m, cmd = tm.(Model), c
	})
	if n != 0 {
		t.Fatalf("cold unpin ran %d UI-thread queries, want 0", n)
	}
	if cmd == nil {
		t.Fatal("cold unpin did not arm the debounce timer")
	}
	if m.overview.Pinned {
		t.Fatal("unpin did not clear the crosshair")
	}

	// A superseded timer drops without dispatching (the coalescing contract).
	if _, c := m.Update(detailDebounceMsg{seq: m.detailSeq - 1}); c != nil {
		t.Fatal("stale debounce timer dispatched a flight")
	}

	// The current timer dispatches exactly one background flight, and only
	// that flight touches the store: a handful of queries for the final state,
	// bounded well under one-per-bucket.
	tm, c := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("current debounce timer did not dispatch the flight")
	}
	var msg tea.Msg
	flightQ := queriesDuring(&f.fakeData, func() { msg = c() })
	if flightQ == 0 || flightQ > 6 {
		t.Fatalf("coalesced flight ran %d queries, want 1..6 (not one per bucket)", flightQ)
	}

	// Applying the flight is again query-free and lands the full-range totals.
	n = queriesDuring(&f.fakeData, func() { m = send(m, msg) })
	if n != 0 {
		t.Fatalf("flight apply ran %d UI-thread queries, want 0", n)
	}
	if got := m.overview.Totals.Total; got != 7000 {
		t.Fatalf("spring-back totals = %d, want full-range 7000", got)
	}
}

// TestSelectionSweepColdCoalesces: By-Tool selection steps whose detail trend
// is cold never query on the UI thread; the queued steps collapse to ONE
// background flight for the final selection.
func TestSelectionSweepColdCoalesces(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)
	m = step(t, m, keyMsg("2")) // By-Tool; row 0's trend is warm from the load

	var cmds []tea.Cmd
	n := queriesDuring(f, func() {
		for _, k := range []string{"down", "up", "down"} {
			tm, c := m.Update(keyMsg(k))
			m = tm.(Model)
			cmds = append(cmds, c)
		}
	})
	if n != 0 {
		t.Fatalf("selection sweep ran %d UI-thread queries, want 0", n)
	}
	// down → row 1 cold: debounce armed. up → row 0 warm: applied locally, no
	// cmd. down → row 1 still cold: debounce re-armed (superseding the first).
	if cmds[0] == nil || cmds[2] == nil {
		t.Fatal("cold selection steps did not arm the debounce timer")
	}
	if cmds[1] != nil {
		t.Fatal("warm selection step dispatched a command")
	}
	if m.byTool.Selected != 1 {
		t.Fatalf("selection = %d, want 1", m.byTool.Selected)
	}
	// SelSessions never needs a query: it reads the store-level distinct count
	// off the selected row.
	if got, want := m.byTool.SelSessions, m.byTool.Rows[1].Sessions; got != want {
		t.Fatalf("SelSessions = %d, want the row's store-level count %d", got, want)
	}

	// The first timer was superseded; only the final one dispatches.
	if _, c := m.Update(detailDebounceMsg{seq: m.detailSeq - 1}); c != nil {
		t.Fatal("superseded debounce timer dispatched a flight")
	}
	tm, c := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("final debounce timer did not dispatch the flight")
	}
	var msg tea.Msg
	flightQ := queriesDuring(f, func() { msg = c() })
	if flightQ != 1 {
		t.Fatalf("coalesced detail flight ran %d queries, want exactly 1 (the final row's trend)", flightQ)
	}
	n = queriesDuring(f, func() { m = send(m, msg) })
	if n != 0 {
		t.Fatalf("detail apply ran %d UI-thread queries, want 0", n)
	}
	if len(m.byTool.SelTrend) == 0 {
		t.Fatal("selected trend not applied after the flight")
	}
}

// TestStaleDetailFlightDropped: a detail flight superseded by a navigation
// (new load generation) is dropped whole.
func TestStaleDetailFlightDropped(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)
	m = step(t, m, keyMsg("2"))
	m = send(m, keyMsg("down")) // cold detail: debounce armed
	tm, c := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("debounce did not dispatch")
	}
	msg := c()

	tm2, _ := m.Update(keyMsg("3")) // navigation supersedes the generation
	m = tm2.(Model)
	if m.fresh != FreshCutIn {
		t.Fatalf("navigation freshness = %v, want cutIn", m.fresh)
	}
	m = send(m, msg) // stale-generation detail flight: dropped whole
	if m.view != ViewByModel {
		t.Fatalf("view = %v, want By-Model", m.view)
	}
	if m.fresh != FreshCutIn {
		t.Fatalf("stale detail flight changed freshness to %v, want cutIn", m.fresh)
	}
	if m.byTool.Selected != 1 {
		t.Fatalf("stale flight mutated selection: %d", m.byTool.Selected)
	}
}

// TestBrowsePreviewCursorMovesStayLocal: Browse cursor moves reprice the
// preview from cache; cold rows arm the debounce instead of querying.
func TestBrowsePreviewCursorMovesStayLocal(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)
	m = step(t, m, keyMsg("4")) // Browse; row 0's preview warm from the load

	n := queriesDuring(f, func() {
		tm, cmd := m.Update(keyMsg("down")) // row 1: cold preview
		m = tm.(Model)
		if cmd == nil {
			t.Fatal("cold preview move did not arm the debounce timer")
		}
	})
	if n != 0 {
		t.Fatalf("browse cursor move ran %d UI-thread queries, want 0", n)
	}

	tm, c := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("debounce did not dispatch")
	}
	var msg tea.Msg
	if q := queriesDuring(f, func() { msg = c() }); q != 1 {
		t.Fatalf("preview flight ran %d queries, want 1", q)
	}
	if q := queriesDuring(f, func() { m = send(m, msg) }); q != 0 {
		t.Fatalf("preview apply ran %d UI-thread queries, want 0", q)
	}
}

// TestDetailFlightFailureRendersQueryFailed drives a failing detail flight
// through the LIVE message flow (sync miss → debounce → flight → apply): the
// querying loader only flags the flight's discarded copy, so the apply must
// carry the failure onto the real model — rendering the per-pane query-failed
// treatment — and must NOT re-arm the debounce (a failing store would
// otherwise redispatch every ~75ms forever).
func TestDetailFlightFailureRendersQueryFailed(t *testing.T) {
	src := &flakySource{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, src, fixed)
	m = step(t, m, keyMsg("2")) // By-Tool; row 0's trend warm from the load

	src.fail.Store(true)
	m = send(m, keyMsg("down")) // row 1 cold: debounce armed
	tm, c := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("debounce did not dispatch the flight")
	}
	msg := c() // flight fails against the store

	tm, c = m.Update(msg)
	m = tm.(Model)
	if c != nil {
		t.Fatal("failed detail flight re-armed the dispatch loop")
	}
	if !m.byTool.SelTrendErr {
		t.Fatal("failed flight did not land SelTrendErr on the live model")
	}
	if m.byTool.SelTrend != nil {
		t.Fatal("failed flight held a stale trend behind the error flag")
	}
	if out := m.View().Content; !strings.Contains(out, "✕ query failed") {
		t.Fatalf("detail card missing the query-failed treatment:\n%s", out)
	}

	// A warm sync hit clears the flag: row 0's trend is still cached.
	m = send(m, keyMsg("up"))
	if m.byTool.SelTrendErr || len(m.byTool.SelTrend) == 0 {
		t.Fatal("warm sync hit did not clear the query-failed flag")
	}

	// Recovery: the store heals, the retry flight lands the trend.
	src.fail.Store(false)
	m = send(m, keyMsg("down")) // row 1 still cold: debounce re-armed
	tm, c = m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("recovery debounce did not dispatch")
	}
	m = send(m, c())
	if m.byTool.SelTrendErr || len(m.byTool.SelTrend) == 0 {
		t.Fatal("recovered flight did not clear the failure and land the trend")
	}
}

// TestBrowsePreviewFailureRendersQueryFailed is the Browse leg of the same
// contract: a failed preview flight lands PreviewErr through the live flow and
// stops the redispatch loop.
func TestBrowsePreviewFailureRendersQueryFailed(t *testing.T) {
	src := &flakySource{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, src, fixed)
	m = step(t, m, keyMsg("4")) // Browse; row 0's preview warm from the load

	src.fail.Store(true)
	m = send(m, keyMsg("down")) // row 1 cold preview: debounce armed
	tm, c := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if c == nil {
		t.Fatal("debounce did not dispatch the flight")
	}
	msg := c()

	tm, c = m.Update(msg)
	m = tm.(Model)
	if c != nil {
		t.Fatal("failed preview flight re-armed the dispatch loop")
	}
	if !m.browse.PreviewErr() {
		t.Fatal("failed flight did not land PreviewErr on the live model")
	}
	if out := m.View().Content; !strings.Contains(out, "✕ query failed") {
		t.Fatalf("preview pane missing the query-failed treatment:\n%s", out)
	}
}

// TestModelOwnersSingleGroupedQuery is the 1e acceptance: modelOwners runs ONE
// grouped query and reduces the dominant tool per model in memory.
func TestModelOwnersSingleGroupedQuery(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)

	var owners map[string]string
	n := queriesDuring(f, func() { owners = m.modelOwners() })
	if n != 1 {
		t.Fatalf("modelOwners ran %d queries, want 1", n)
	}
	if owners["claude-opus"] != "claude-code" || owners["gpt-5"] != "codex" {
		t.Fatalf("owners = %v, want claude-opus→claude-code, gpt-5→codex", owners)
	}
	// Warm repeat: zero queries (the grouped key is cached).
	if n := queriesDuring(f, func() { m.modelOwners() }); n != 0 {
		t.Fatalf("warm modelOwners ran %d queries, want 0", n)
	}
}
