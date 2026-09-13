package tui

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
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
	t.Cleanup(m.zoneMgr.Close)
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

func TestOverviewLoadReusesTimelineTotals(t *testing.T) {
	f := &fakeData{}
	m := NewModel(f, Options{DBPath: "/tmp/usage.db"})
	m.data.now = func() time.Time {
		return time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	}
	m.loadNow = m.data.now()
	m.rng = Range7d

	n := queriesDuring(f, func() { m.loadOverview() })
	if n != 4 {
		t.Fatalf("cold Overview load ran %d queries, want 4", n)
	}
	if !reflect.DeepEqual(m.overview.Totals, fakeUsageTotals()) {
		t.Fatalf("Overview totals = %#v, want timeline totals %#v", m.overview.Totals, fakeUsageTotals())
	}

	m.scrubPinned = true
	m.scrubIndex = 1
	m.syncScrub()
	m.scrubPinned = false
	n = queriesDuring(f, func() { m.syncScrub() })
	if n != 0 {
		t.Fatalf("unpinning a warm Overview ran %d queries, want 0", n)
	}
	if !reflect.DeepEqual(m.overview.Totals, fakeUsageTotals()) {
		t.Fatalf("unpin totals = %#v, want cached timeline totals %#v", m.overview.Totals, fakeUsageTotals())
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

// Superseded loads must neither query nor change the current cache clock,
// even when they run after invalidation or after a newer day has loaded.
func TestSupersededFlightRunsNoQueries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		oldNow   time.Time
		newFirst bool
	}{
		{"old first same day", time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local), false},
		{"old last across midnight", time.Date(2026, 9, 12, 23, 59, 0, 0, time.Local), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.db")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			insert := func(id string, at time.Time, total int64) {
				t.Helper()
				_, err := st.InsertEvents(context.Background(), []model.UsageEvent{{Tool: model.ToolCodex, Model: "m", SessionID: id, EventTime: at, InputTokens: total, TotalTokens: total, Kind: model.KindUsage, DedupKey: id}})
				if err != nil {
					t.Fatal(err)
				}
			}
			now := tc.oldNow.Add(-time.Minute)
			insert("initial", now.Add(-time.Minute), 100)
			f := &countingSource{src: st}
			m := newPinnedModel(t, f, now)
			m.dbPath = path
			m.data.now = func() time.Time { return now }
			now = tc.oldNow
			cmdA := m.startLoad() // queued, not executed
			now = tc.oldNow.Add(2 * time.Minute)
			insert("new", tc.oldNow.Add(time.Minute), 200)
			insert("prior", tc.oldNow.Add(time.Minute).AddDate(0, 0, -7), 300)
			m.data.Invalidate()
			cmdB := m.startLoad() // cancels A and owns the refreshed cache
			var msgB tea.Msg
			if tc.newFirst {
				msgB = cmdB()
			}
			clockBefore := m.data.snapshotNow
			queriesBefore := f.calls.Load()
			msgA := cmdA()
			if n := f.calls.Load() - queriesBefore; n != 0 {
				t.Fatalf("superseded flight ran %d queries, want 0", n)
			}
			if !errors.Is(msgA.(dataLoadedMsg).err, context.Canceled) {
				t.Fatalf("superseded flight err = %v, want context.Canceled", msgA.(dataLoadedMsg).err)
			}
			if !m.data.snapshotNow.Equal(clockBefore) {
				t.Errorf("superseded flight changed snapshot clock from %v to %v", clockBefore, m.data.snapshotNow)
			}
			lastMTime := m.lastMTime
			m = send(m, msgA)
			if m.fresh != FreshCutIn || !m.lastMTime.Equal(lastMTime) {
				t.Fatal("superseded result changed freshness or lastMTime")
			}
			if !tc.newFirst {
				msgB = cmdB()
			}
			queriesBefore = f.calls.Load()
			m = send(m, msgB)
			if n := f.calls.Load() - queriesBefore; n != 0 {
				t.Fatalf("current apply ran %d queries, want 0", n)
			}
			if m.fresh != FreshLive || m.err != nil || !m.lastMTime.Equal(msgB.(dataLoadedMsg).mtime) {
				t.Fatalf("current apply freshness=%v err=%v mtime=%v", m.fresh, m.err, m.lastMTime)
			}
			if !m.data.snapshotNow.Equal(now) || m.overview.Totals.Total != 300 || m.overview.Prev.Total != 300 {
				t.Fatalf("current clock=%v total=%d prior=%d, want %v / 300 / 300", m.data.snapshotNow, m.overview.Totals.Total, m.overview.Prev.Total, now)
			}
			_, priorUntil, ok := m.prevWindow()
			if !ok || !priorUntil.Equal(now.AddDate(0, 0, -7)) {
				t.Fatalf("prior endpoint=%v, want %v", priorUntil, now.AddDate(0, 0, -7))
			}
		})
	}
}

// Invalidation can happen before the next dispatch cancels the old context.
// The cache identity must fence that still-running flight's clock writes too,
// including cache-only fallback after a pinned timeline becomes empty.
func TestInvalidatedFlightCannotSeedSnapshot(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local)
		m := newPinnedModel(t, &emptySource{}, now)
		m.scrubPinned = pinned
		cmd := m.startLoad()
		m.data.Invalidate()
		if msg := cmd().(dataLoadedMsg); msg.err != nil {
			t.Fatal(msg.err)
		}
		if !m.data.snapshotNow.IsZero() {
			t.Fatalf("pinned=%v invalidated flight seeded the new snapshot: %v", pinned, m.data.snapshotNow)
		}
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
	entered chan struct{}
	calls   atomic.Int64
}

func (g *gateSource) Summarize(context.Context, store.Filter) (*store.Summary, error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
		<-g.release
	}
	return &store.Summary{Totals: store.Bucket{Events: 1, Total: 10}}, nil
}

// TestInFlightLoadDoesNotRepolluteInvalidatedCache: a query that was already in
// flight when Invalidate ran must not deposit its (pre-invalidation) result in
// the fresh cache — the next identical query has to hit the source again.
func TestInFlightLoadDoesNotRepolluteInvalidatedCache(t *testing.T) {
	g := &gateSource{release: make(chan struct{}), entered: make(chan struct{})}
	release := sync.OnceFunc(func() { close(g.release) })
	t.Cleanup(release)
	d := NewData(g)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = d.Totals(context.Background(), now, Span{R: RangeAll}, nil)
	}()
	select {
	case <-g.entered:
	case <-time.After(time.Second):
		t.Fatal("source did not enter Summarize within one second")
	}
	d.Invalidate()
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("source did not finish Summarize within one second after release")
	}

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

// TestPrevTotalsKeysFollowSnapshot: current and prior queries retain one cap
// until the displayed data refreshes. Refresh must advance both endpoints.
func TestPrevTotalsKeysFollowSnapshot(t *testing.T) {
	for _, r := range []Range{RangeToday, Range7d, Range30d} {
		f := &fakeData{}
		early := time.Date(2026, 8, 9, 9, 5, 30, 0, time.Local)
		m := newPinnedModel(t, f, early)
		m.rng = r
		m.prevTotals()
		_, before, _ := m.prevWindow()
		m.loadNow = early.Add(12 * time.Hour)
		if n := queriesDuring(f, func() { m.prevTotals() }); n != 0 {
			t.Fatalf("%s warm comparison ran %d queries", r.Label(), n)
		}
		_, held, _ := m.prevWindow()
		if !held.Equal(before) {
			t.Fatal("warm snapshot changed its comparison cap")
		}
		m.data.Invalidate()
		if n := queriesDuring(f, func() { m.prevTotals() }); n != 1 {
			t.Fatalf("%s refreshed comparison ran %d queries, want 1", r.Label(), n)
		}
		_, refreshed, _ := m.prevWindow()
		if !refreshed.Equal(m.loadNow.AddDate(0, 0, -r.spanDays())) {
			t.Fatalf("refreshed cap %v does not follow %v", refreshed, m.loadNow)
		}
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
