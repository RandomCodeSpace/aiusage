package tui

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// async_test.go covers issue #4 chunk 1a/1c: stable quantized cache keys, the
// load-generation token, stat-before-query, and the "zero store queries on the
// UI thread" acceptance for navigation.

// newPinnedModel builds a loaded model whose data clock is pinned BEFORE the
// first load, so every load generation resolves the same instant and cache keys
// never roll over an hour/day boundary mid-test.
func newPinnedModel(t *testing.T, src DataSource, fixed time.Time) Model {
	t.Helper()
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	m.data.now = func() time.Time { return fixed }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return loadOnce(tm.(Model))
}

// TestNavigationRunsZeroQueriesOnUIThread asserts the two UI-thread halves of a
// navigation round-trip — the keypress Update that dispatches the load, and the
// dataLoadedMsg Update that applies it — run ZERO DataSource queries even on a
// cold cache. All querying happens inside the returned cmd (the background
// flight).
func TestNavigationRunsZeroQueriesOnUIThread(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)

	var cmd tea.Cmd
	n := queriesDuring(f, func() {
		tm, c := m.Update(keyMsg("2")) // By-Tool, cold cache
		m, cmd = tm.(Model), c
	})
	if n != 0 {
		t.Fatalf("keypress Update ran %d queries on the UI thread, want 0", n)
	}
	if cmd == nil {
		t.Fatal("navigation did not dispatch a load cmd")
	}
	if m.fresh != FreshCutIn {
		t.Fatalf("navigation freshness = %v, want cutIn", m.fresh)
	}

	msg := cmd() // the background flight: queries happen here, off the UI thread
	if f.queries() == 0 {
		t.Fatal("background flight ran no queries on a cold cache")
	}

	n = queriesDuring(f, func() {
		m = send(m, msg) // apply: reload from the warm cache
	})
	if n != 0 {
		t.Fatalf("apply-side reload ran %d queries on the UI thread, want 0", n)
	}
	if m.fresh != FreshLive {
		t.Fatalf("freshness after apply = %v, want live", m.fresh)
	}
	if len(m.byTool.Rows) == 0 {
		t.Fatal("by-tool rows empty after the load applied")
	}
}

// TestWarmNavigationZeroQueries is the warm-cache acceptance: a full navigation
// sweep (view switches, drill+back, range cycle, sort cycle) that repeats an
// already-performed sequence must run ZERO DataSource queries anywhere — UI
// thread and background flight alike — because quantized windows keep every
// cache key stable.
func TestWarmNavigationZeroQueries(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)

	// State-idempotent sweep: ends at (Overview, 7d, SortTotal, no crumbs).
	sweep := []string{
		"2", "3", "4", "enter", "esc", "1",
		"t", "t", "t", "t",
		"s", "s", "s",
	}
	for _, k := range sweep {
		m = step(t, m, keyMsg(k)) // pass 1: warms the cache
	}
	n := queriesDuring(f, func() {
		for _, k := range sweep {
			m = step(t, m, keyMsg(k)) // pass 2: must be fully warm
		}
	})
	if n != 0 {
		t.Fatalf("warm navigation sweep ran %d queries, want 0", n)
	}
}

// TestStaleFlightDropped locks the load-generation contract: a flight whose
// generation was superseded before it landed is dropped whole — it must not
// clear the loading state, advance lastMTime, or overwrite view data.
func TestStaleFlightDropped(t *testing.T) {
	f := &fakeData{}
	m := newTestModel(t, f)

	tm, cmdA := m.Update(keyMsg("2")) // gen G: By-Tool load in flight
	m = tm.(Model)
	tm, cmdB := m.Update(keyMsg("3")) // gen G+1 supersedes it
	m = tm.(Model)
	msgA, msgB := cmdA(), cmdB()

	m = send(m, msgA) // stale flight lands first: dropped
	if m.fresh != FreshCutIn {
		t.Fatalf("stale flight changed freshness to %v, want cutIn", m.fresh)
	}

	// A synthetic stale message must not advance lastMTime either.
	before := m.lastMTime
	m = send(m, dataLoadedMsg{gen: m.loadGen - 1, now: time.Now(), mtime: time.Now()})
	if !m.lastMTime.Equal(before) {
		t.Fatal("stale flight advanced lastMTime")
	}

	m = send(m, msgB) // current generation applies
	if m.fresh != FreshLive {
		t.Fatalf("current-generation apply freshness = %v, want live", m.fresh)
	}
	if m.view != ViewByModel || len(m.byModel.Rows) == 0 {
		t.Fatalf("current-generation apply missing: view=%v rows=%d", m.view, len(m.byModel.Rows))
	}
}

// TestSupersededFlightRunsNoQueries is the cost half of the generation
// contract: dropping a stale flight's RESULT is not enough, it must also stop
// paying for it. Superseding a flight cancels its context, so when the doomed
// goroutine finally runs it issues zero queries and reports the cancellation —
// while the current generation still applies normally.
func TestSupersededFlightRunsNoQueries(t *testing.T) {
	f := &fakeData{}
	m := newTestModel(t, f)

	// Two range cycles: both land on windows the startup load never warmed, so
	// the flights have real queries to run and cancellation has something to
	// prevent.
	tm, cmdA := m.Update(keyMsg("t")) // gen G: context taken
	m = tm.(Model)
	tm, cmdB := m.Update(keyMsg("t")) // gen G+1 supersedes it: G's context cancelled
	m = tm.(Model)

	var msgA tea.Msg
	n := queriesDuring(f, func() { msgA = cmdA() })
	if n != 0 {
		t.Fatalf("superseded flight ran %d queries, want 0", n)
	}
	if !errors.Is(msgA.(dataLoadedMsg).err, context.Canceled) {
		t.Fatalf("superseded flight err = %v, want context.Canceled", msgA.(dataLoadedMsg).err)
	}

	m = send(m, msgA) // still dropped on the generation check, cancelled or not
	if m.fresh != FreshCutIn {
		t.Fatalf("superseded flight changed freshness to %v, want cutIn", m.fresh)
	}
	m = send(m, cmdB()) // the surviving generation is unaffected
	if m.fresh != FreshLive {
		t.Fatalf("current-generation apply freshness = %v, want live", m.fresh)
	}
	if len(m.overview.ByTool) == 0 {
		t.Fatal("current-generation apply produced no overview data")
	}
}

// blockingSource blocks every Summarize until its context is cancelled, so a
// test can hold a flight mid-query and prove the cancellation reaches the
// DataSource itself (what interrupts a real SQLite aggregation).
type blockingSource struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingSource) Summarize(ctx context.Context, _ store.Filter) (*store.Summary, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// The activity half blocks on the same terms: whichever query a flight opens
// first, the cancellation has to reach it.
func (b *blockingSource) SummarizeActivity(ctx context.Context, _ store.ActivityFilter) (*store.ActivitySummary, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingSource) TopActivity(ctx context.Context, _ store.ActivityFilter, _ store.ActivityOrder, _ int) ([]store.ActivityBucket, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingSource) SummarizeTurnContext(ctx context.Context, _ model.TurnDimension, _ store.ActivityFilter) (*store.TurnContextSummary, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingSource) TopTurnContext(ctx context.Context, _ model.TurnDimension, _ store.ActivityFilter, _ store.ActivityOrder, _ int) ([]store.TurnContextBucket, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSupersededFlightCancelsRunningQuery proves the flight context reaches the
// DataSource: a query already executing when the next generation dispatches is
// cancelled mid-run, not merely ignored on arrival.
func TestSupersededFlightCancelsRunningQuery(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{})}
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(Model)

	tm, cmdA := m.Update(keyMsg("2"))
	m = tm.(Model)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmdA() }()
	<-src.entered // the flight is inside Summarize

	m.Update(keyMsg("3")) // supersede: this must cancel the running query

	select {
	case msg := <-done:
		if !errors.Is(msg.(dataLoadedMsg).err, context.Canceled) {
			t.Fatalf("interrupted flight err = %v, want context.Canceled", msg.(dataLoadedMsg).err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseding a flight did not cancel the query it was running")
	}
}

// TestRefreshTickWhileLoadingStillDispatches: the mtime poll no longer defers
// to an in-flight load. An advanced mtime supersedes it with a new generation;
// an unchanged mtime still dispatches nothing.
func TestRefreshTickWhileLoadingStillDispatches(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	t0 := time.Now().Add(-time.Hour)
	touchDB(t, db, t0)

	f := &fakeData{}
	m := newLoadedModel(t, f, db)

	tm, _ := m.Update(keyMsg("r")) // a load is now in flight
	m = tm.(Model)
	if m.fresh != FreshCutIn {
		t.Fatalf("manual refresh freshness = %v, want cutIn", m.fresh)
	}

	// Unchanged mtime: no new dispatch even while loading.
	gen := m.loadGen
	tm, _ = m.Update(refreshTickMsg{})
	m = tm.(Model)
	if m.loadGen != gen {
		t.Fatal("unchanged mtime dispatched a load")
	}

	// Advanced mtime: dispatches a superseding generation despite the flight.
	touchDB(t, db, time.Now().Add(time.Hour))
	tm, cmd := m.Update(refreshTickMsg{})
	m = tm.(Model)
	if m.loadGen != gen+1 {
		t.Fatalf("advanced mtime while loading: loadGen = %d, want %d", m.loadGen, gen+1)
	}
	if cmd == nil {
		t.Fatal("advanced mtime produced no command")
	}
}

// gateSource blocks its first Summarize until released, so a test can hold a
// query "in flight" across an Invalidate.
type gateSource struct {
	noActivity
	release chan struct{}
	calls   atomic.Int64
}

func (g *gateSource) Summarize(context.Context, store.Filter) (*store.Summary, error) {
	if g.calls.Add(1) == 1 {
		<-g.release
	}
	return &store.Summary{Totals: store.Bucket{Events: 1, Total: 10}}, nil
}

// TestInFlightLoadDoesNotRepolluteInvalidatedCache: a query that was already in
// flight when Invalidate ran must not deposit its (pre-invalidation) result in
// the fresh cache — the next identical query has to hit the source again.
func TestInFlightLoadDoesNotRepolluteInvalidatedCache(t *testing.T) {
	g := &gateSource{release: make(chan struct{})}
	d := NewData(g)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = d.Totals(context.Background(), now, Span{R: RangeAll}, nil)
	}()
	for g.calls.Load() == 0 { // wait until the query is in flight
		time.Sleep(time.Millisecond)
	}
	d.Invalidate()
	close(g.release)
	<-done

	if _, err := d.Totals(context.Background(), now, Span{R: RangeAll}, nil); err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if got := g.calls.Load(); got != 2 {
		t.Fatalf("source calls = %d, want 2 (in-flight result must not survive Invalidate)", got)
	}
}

// touchingSource wraps fakeData with a hook fired on every Summarize, to
// simulate a daemon write landing mid-flight.
type touchingSource struct {
	fakeData
	onQuery func()
}

func (s *touchingSource) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	if s.onQuery != nil {
		s.onQuery()
	}
	return s.fakeData.Summarize(ctx, f)
}

// TestLoadCmdStatsBeforeQuerying closes the lost-update window: a daemon write
// landing between the flight's stat and its queries must NOT be credited to
// lastMTime, so the next refresh tick re-detects and re-renders it. (The old
// order — query, then stat — credited the write without ever rendering it.)
func TestLoadCmdStatsBeforeQuerying(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	touchDB(t, db, t0)

	src := &touchingSource{}
	m := newLoadedModel(t, src, db)

	t1 := time.Now().Add(time.Hour).Truncate(time.Second)
	var once sync.Once
	src.onQuery = func() { once.Do(func() { touchDB(t, db, t1) }) }

	tm, cmd := m.Update(keyMsg("r")) // flight: stat (t0), then queries (write lands)
	m = runPending(t, tm.(Model), cmd)

	if !m.lastMTime.Equal(t0) {
		t.Fatalf("lastMTime = %v, want the pre-write %v (mid-flight write was credited)", m.lastMTime, t0)
	}
	tm, _ = m.Update(refreshTickMsg{})
	if tm.(Model).fresh != FreshCutIn {
		t.Fatal("next tick did not re-detect the mid-flight write")
	}
}

// TestRangeWindowCalendarDays pins the DECIDED 1a semantics: 7d/30d are the
// last N local calendar days including today (local-midnight arithmetic), not
// rolling 168h/720h spans; until stays open.
func TestRangeWindowCalendarDays(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 8, 9, 15, 4, 5, 0, loc)

	cases := []struct {
		r    Range
		want time.Time
	}{
		{RangeToday, time.Date(2026, 8, 9, 0, 0, 0, 0, loc)},
		{Range7d, time.Date(2026, 8, 3, 0, 0, 0, 0, loc)},
		{Range30d, time.Date(2026, 7, 11, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		since, until := c.r.Window(now)
		if !since.Equal(c.want) {
			t.Errorf("%s: since = %v, want %v", c.r.Label(), since, c.want)
		}
		if !until.IsZero() {
			t.Errorf("%s: until = %v, want open (zero)", c.r.Label(), until)
		}
	}
	if since, until := RangeAll.Window(now); !since.IsZero() || !until.IsZero() {
		t.Errorf("all: window = [%v, %v), want fully open", since, until)
	}
}

// TestCacheKeysStableWithinDay: two clocks on the same local day must resolve
// identical cache keys (no re-query); crossing midnight must re-key.
func TestCacheKeysStableWithinDay(t *testing.T) {
	f := &fakeData{}
	d := NewData(f)
	early := time.Date(2026, 8, 9, 0, 0, 1, 0, time.Local)
	late := time.Date(2026, 8, 9, 23, 59, 59, 0, time.Local)

	for _, r := range []Range{RangeToday, Range7d, Range30d} {
		if _, err := d.Totals(context.Background(), early, Span{R: r}, nil); err != nil {
			t.Fatalf("Totals(%s): %v", r.Label(), err)
		}
		if _, _, err := d.Timeline(context.Background(), early, Span{R: r}, nil); err != nil {
			t.Fatalf("Timeline(%s): %v", r.Label(), err)
		}
		n := queriesDuring(f, func() {
			_, _ = d.Totals(context.Background(), late, Span{R: r}, nil)
			_, _, _ = d.Timeline(context.Background(), late, Span{R: r}, nil)
		})
		if n != 0 {
			t.Errorf("%s: same-day clock drift caused %d re-queries, want 0", r.Label(), n)
		}
	}

	nextDay := late.AddDate(0, 0, 1)
	n := queriesDuring(f, func() { _, _ = d.Totals(context.Background(), nextDay, Span{R: Range7d}, nil) })
	if n == 0 {
		t.Error("crossing midnight did not re-key the 7d window")
	}
}

// TestPrevTotalsKeysStableWithinHour: the delta-chip window (which used to read
// raw time.Now per call) must re-key at most once per hour for today and once
// per day for 7d — never per second.
func TestPrevTotalsKeysStableWithinHour(t *testing.T) {
	f := &fakeData{}
	m := newTestModel(t, f)

	m.rng = RangeToday
	m.loadNow = time.Date(2026, 8, 9, 14, 5, 0, 0, time.Local)
	m.prevTotals() // warm
	n := queriesDuring(f, func() {
		m.loadNow = time.Date(2026, 8, 9, 14, 55, 30, 0, time.Local)
		m.prevTotals()
	})
	if n != 0 {
		t.Fatalf("today: same-hour clock drift caused %d re-queries, want 0", n)
	}
	n = queriesDuring(f, func() {
		m.loadNow = time.Date(2026, 8, 9, 15, 5, 0, 0, time.Local)
		m.prevTotals()
	})
	if n == 0 {
		t.Fatal("today: crossing the hour did not re-key the previous-period window")
	}

	m.rng = Range7d
	m.loadNow = time.Date(2026, 8, 9, 9, 0, 0, 0, time.Local)
	m.prevTotals() // warm
	n = queriesDuring(f, func() {
		m.loadNow = time.Date(2026, 8, 9, 21, 30, 45, 0, time.Local)
		m.prevTotals()
	})
	if n != 0 {
		t.Fatalf("7d: same-day clock drift caused %d re-queries, want 0", n)
	}
}

// TestResizeRunsZeroQueries pins plan item 1b: WindowSizeMsg is pure relayout
// on every path — warm cache, cold (invalidated) cache, and mid-flight — and
// never touches the DataSource or dispatches a load.
func TestResizeRunsZeroQueries(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)

	resize := func(w, h int) tea.Cmd {
		var cmd tea.Cmd
		n := queriesDuring(f, func() {
			tm, c := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m, cmd = tm.(Model), c
		})
		if n != 0 {
			t.Fatalf("resize to %dx%d ran %d queries, want 0", w, h, n)
		}
		return cmd
	}

	if cmd := resize(80, 24); cmd != nil {
		t.Fatal("warm resize dispatched a cmd, want none")
	}

	m.data.Invalidate() // cold window: cache dropped, no flight landed yet
	if cmd := resize(160, 50); cmd != nil {
		t.Fatal("cold resize dispatched a cmd, want none")
	}
	if v := m.View().Content; v == "" {
		t.Fatal("cold resize rendered an empty frame")
	}
}
