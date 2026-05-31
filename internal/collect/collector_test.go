package collect

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
	"aiusage/internal/store"
)

// ---------------------------------------------------------------------------
// In-test fake store.
//
// The real SQLite-backed store (store.Open) is assembled in a sibling package
// during integration. To keep this leaf package self-checkable, the tests use a
// faithful in-memory implementation of store.Store that reproduces the two
// behaviours the collector depends on:
//   - InsertEvents is INSERT OR IGNORE on DedupKey (append-only, idempotent).
//   - LastState / UpsertState keep exactly one row per (tool, key) accumulator.
// Integration swaps this for store.Open(tmpfile); the collector code is store-
// agnostic via the store.Store interface.
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu     sync.Mutex
	dedup  map[string]struct{}
	events []model.UsageEvent
	state  map[string]model.AggregateSnapshot // key: tool|key
}

var _ store.Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{
		dedup: map[string]struct{}{},
		state: map[string]model.AggregateSnapshot{},
	}
}

func (s *fakeStore) InsertEvents(_ context.Context, events []model.UsageEvent) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inserted := 0
	for _, e := range events {
		if e.DedupKey == "" {
			return inserted, errors.New("empty dedup key")
		}
		if _, ok := s.dedup[e.DedupKey]; ok {
			continue // INSERT OR IGNORE
		}
		s.dedup[e.DedupKey] = struct{}{}
		s.events = append(s.events, e)
		inserted++
	}
	return inserted, nil
}

func (s *fakeStore) LastState(_ context.Context, tool, key string) (*model.AggregateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.state[tool+"|"+key]; ok {
		cp := v
		return &cp, nil
	}
	return nil, nil
}

func (s *fakeStore) UpsertState(_ context.Context, st model.AggregateSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[st.Tool+"|"+st.Key] = st
	return nil
}

// Summarize sums total_tokens over events whose EventTime is in [Since, Until).
// Zero bounds are treated as open. Grouping is not needed by these tests, so a
// single grand-total bucket is returned.
func (s *fakeStore) Summarize(_ context.Context, f store.Filter) (*store.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b store.Bucket
	for _, e := range s.events {
		if !f.Since.IsZero() && e.EventTime.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !e.EventTime.Before(f.Until) {
			continue
		}
		b.Events++
		b.Input += e.InputTokens
		b.Output += e.OutputTokens
		b.CacheCreation += e.CacheCreationTokens
		b.CacheRead += e.CacheReadTokens
		b.Reasoning += e.ReasoningTokens
		b.Total += e.TotalTokens
	}
	return &store.Summary{GroupBy: f.GroupBy, Totals: b}, nil
}

func (s *fakeStore) ListEvents(_ context.Context, _ store.Filter) ([]model.UsageEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.UsageEvent, len(s.events))
	copy(out, s.events)
	return out, nil
}

func (s *fakeStore) SourceStats(context.Context) ([]store.SourceStat, error) { return nil, nil }
func (s *fakeStore) Stats(context.Context) (store.DBStats, error)            { return store.DBStats{}, nil }
func (s *fakeStore) Close() error                                            { return nil }

func windowTotal(t *testing.T, st *fakeStore, since, until time.Time) int64 {
	t.Helper()
	sum, err := st.Summarize(context.Background(), store.Filter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return sum.Totals.Total
}

// ---------------------------------------------------------------------------
// Configurable fake adapter.
// ---------------------------------------------------------------------------

type fakeAdapter struct {
	id    string
	class model.SourceClass
	// emit returns the observation for each Collect call; the int is the
	// 0-based call index, letting a test change behaviour across cycles.
	emit        func(call int) adapter.Observation
	discoverErr error
	collectErr  error
	mu          sync.Mutex
	calls       int
}

func (a *fakeAdapter) ID() string          { return a.id }
func (a *fakeAdapter) DisplayName() string { return a.id }

func (a *fakeAdapter) Discover(_ context.Context, _ adapter.DiscoverConfig) ([]adapter.Source, error) {
	return []adapter.Source{{Tool: a.id, Class: a.class, Path: a.id + "/src", Label: a.id}}, a.discoverErr
}

func (a *fakeAdapter) Collect(_ context.Context, _ adapter.Source) (adapter.Observation, error) {
	a.mu.Lock()
	call := a.calls
	a.calls++
	a.mu.Unlock()
	return a.emit(call), a.collectErr
}

// fixed reference date for deterministic event-time windows.
var refDay = time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// (a) delta helpers.
// ---------------------------------------------------------------------------

func TestFieldDelta(t *testing.T) {
	cases := []struct {
		name      string
		last, cur int64
		want      int64
	}{
		{"increasing", 100, 250, 150},
		{"holds steady", 100, 100, 0},
		{"decreasing reset takes current", 1000, 30, 30},
		{"reset to zero", 1000, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fieldDelta(c.last, c.cur); got != c.want {
				t.Fatalf("fieldDelta(%d,%d)=%d want %d", c.last, c.cur, got, c.want)
			}
			if got := fieldDelta(c.last, c.cur); got < 0 {
				t.Fatalf("fieldDelta produced negative %d", got)
			}
		})
	}
}

func TestSnapshotDeltaNilLastTakesCurrent(t *testing.T) {
	cur := model.AggregateSnapshot{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	d := snapshotDelta(nil, cur)
	if d.input != 10 || d.output != 20 || d.total != 30 {
		t.Fatalf("nil-last delta = %+v want full current", d)
	}
}

func TestSnapshotDeltaResetNeverNegative(t *testing.T) {
	last := &model.AggregateSnapshot{InputTokens: 500, OutputTokens: 500, TotalTokens: 1000}
	cur := model.AggregateSnapshot{InputTokens: 5, OutputTokens: 0, TotalTokens: 5}
	d := snapshotDelta(last, cur)
	if d.input != 5 || d.output != 0 || d.total != 5 {
		t.Fatalf("reset delta = %+v want current values", d)
	}
	if d.input < 0 || d.output < 0 || d.total < 0 {
		t.Fatalf("reset delta went negative: %+v", d)
	}
}

func TestSnapshotDeltaGrowthDiffs(t *testing.T) {
	last := &model.AggregateSnapshot{InputTokens: 100, TotalTokens: 100}
	cur := model.AggregateSnapshot{InputTokens: 300, TotalTokens: 300}
	d := snapshotDelta(last, cur)
	if d.input != 200 || d.total != 200 {
		t.Fatalf("growth delta = %+v want diff of 200", d)
	}
}

// ---------------------------------------------------------------------------
// (b) cycle idempotency.
// ---------------------------------------------------------------------------

func TestRunCycleIdempotentEvents(t *testing.T) {
	ev := model.UsageEvent{
		Tool: model.ToolCodex, Model: "gpt", EventTime: refDay.Add(6 * time.Hour),
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		DedupKey: "codex|req-1", Kind: model.KindUsage,
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()
	ctx := context.Background()
	dc := adapter.DiscoverConfig{}

	s1, err := RunCycle(ctx, reg, st, dc)
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if s1.EventsInserted != 1 {
		t.Fatalf("cycle 1 inserted=%d want 1", s1.EventsInserted)
	}

	s2, err := RunCycle(ctx, reg, st, dc)
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if s2.EventsInserted != 0 {
		t.Fatalf("cycle 2 inserted=%d want 0 (idempotent)", s2.EventsInserted)
	}
	if s2.EventsSeen != 1 {
		t.Fatalf("cycle 2 seen=%d want 1", s2.EventsSeen)
	}
}

func TestRunCycleStampsObservedTime(t *testing.T) {
	ev := model.UsageEvent{
		Tool: model.ToolCodex, EventTime: refDay.Add(6 * time.Hour),
		TotalTokens: 10, DedupKey: "codex|req-stamp", Kind: model.KindUsage,
		// ObservedTime intentionally zero.
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	st := newFakeStore()
	if _, err := RunCycle(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	got, _ := st.ListEvents(context.Background(), store.Filter{})
	if len(got) != 1 {
		t.Fatalf("stored %d events want 1", len(got))
	}
	if got[0].ObservedTime.IsZero() {
		t.Fatalf("ObservedTime was not stamped")
	}
}

func TestPerSourceErrorIsNonFatal(t *testing.T) {
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		collectErr: errors.New("boom"),
		emit:       func(int) adapter.Observation { return adapter.Observation{} },
	}
	st := newFakeStore()
	stats, err := RunCycle(context.Background(), adapter.NewRegistry(ad), st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("cycle should not fail on per-source error: %v", err)
	}
	if len(stats.Errors) == 0 {
		t.Fatalf("expected a non-fatal error recorded")
	}
}

// ---------------------------------------------------------------------------
// (c) INVARIANT — event-level: a window total never decreases when the source
// later shrinks (compaction / deletion).
// ---------------------------------------------------------------------------

func TestEventLevelInvariantSurvivesCompaction(t *testing.T) {
	winStart := refDay.Add(6 * time.Hour)              // 06:00
	winEnd := refDay.Add(6*time.Hour + 15*time.Minute) // 06:15

	// 20 events x 100,000 = 2,000,000 tokens inside [06:00,06:15].
	full := make([]model.UsageEvent, 0, 20)
	for i := 0; i < 20; i++ {
		full = append(full, model.UsageEvent{
			Tool:        model.ToolClaudeCode,
			EventTime:   winStart.Add(time.Duration(i) * 30 * time.Second),
			TotalTokens: 100_000,
			DedupKey:    "cc|20260529|" + strconv.Itoa(i),
			Kind:        model.KindUsage,
		})
	}

	ad := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(call int) adapter.Observation {
			if call == 0 {
				return adapter.Observation{Events: full}
			}
			// Simulated compaction: the source now exposes nothing.
			return adapter.Observation{}
		},
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()
	ctx := context.Background()

	if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := windowTotal(t, st, winStart, winEnd); got != 2_000_000 {
		t.Fatalf("after cycle 1 window total=%d want 2,000,000", got)
	}

	// Source compacted to empty; re-poll must not erode the stored history.
	if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := windowTotal(t, st, winStart, winEnd); got != 2_000_000 {
		t.Fatalf("after compaction window total=%d want still 2,000,000", got)
	}
}

// ---------------------------------------------------------------------------
// (d) INVARIANT — aggregate: cumulative materialised total grows with the
// snapshot and never decreases when the snapshot later resets/deletes.
// ---------------------------------------------------------------------------

func TestAggregateInvariantMonotonicWithReset(t *testing.T) {
	const key = "session-xyz"
	snap := func(total int64) adapter.Observation {
		return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
			Tool: model.ToolHermes, Key: key, SessionID: key,
			InputTokens: total, TotalTokens: total,
		}}}
	}

	ad := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(call int) adapter.Observation {
			switch call {
			case 0:
				return snap(900_000)
			case 1:
				return snap(2_000_000)
			default:
				// Reset / deletion: source drops to a tiny value.
				return snap(0)
			}
		},
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()
	ctx := context.Background()

	// Real polls are minutes apart, so each cycle's synthetic-event DedupKey
	// (agg|tool|key|observedUnix) lands on a distinct second. Drive the clock
	// forward between cycles to reproduce that; otherwise two deltas in the same
	// second would collide on dedup.
	clock := refDay.Add(6 * time.Hour)
	restore := setNow(func() time.Time { return clock })
	defer restore()

	// Cycle 1: first observation -> full 900,000 materialised.
	if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := windowTotal(t, st, time.Time{}, time.Time{}); got != 900_000 {
		t.Fatalf("after cycle 1 materialised=%d want 900,000", got)
	}

	// Cycle 2: grows to 2,000,000 -> +1,100,000 delta -> cumulative 2,000,000.
	clock = clock.Add(time.Minute)
	if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := windowTotal(t, st, time.Time{}, time.Time{}); got != 2_000_000 {
		t.Fatalf("after cycle 2 materialised=%d want 2,000,000", got)
	}

	// Cycle 3: snapshot reset to 0 -> no negative delta -> stored stays >= 2,000,000.
	clock = clock.Add(time.Minute)
	if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if got := windowTotal(t, st, time.Time{}, time.Time{}); got < 2_000_000 {
		t.Fatalf("after reset materialised=%d want >= 2,000,000", got)
	}
}

// ---------------------------------------------------------------------------
// daemon: single-instance lock + immediate first cycle + graceful stop.
// ---------------------------------------------------------------------------

func TestRunDaemonSingleInstanceAndImmediateCycle(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "aiusage.pid")

	ev := model.UsageEvent{
		Tool: model.ToolCodex, EventTime: refDay.Add(6 * time.Hour),
		TotalTokens: 42, DedupKey: "codex|daemon-1", Kind: model.KindUsage,
	}
	ad := &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	opt := DaemonOptions{
		Interval: time.Hour, // long, so only the immediate cycle runs
		PIDPath:  pidPath,
		Logger:   log.New(discard{}, "", 0),
	}

	done := make(chan error, 1)
	go func() { done <- RunDaemon(ctx, reg, st, adapter.DiscoverConfig{}, opt) }()

	// Wait for the immediate first cycle to materialise the event and the
	// pidfile + lock to exist.
	waitFor(t, time.Second, func() bool {
		got, _ := st.ListEvents(context.Background(), store.Filter{})
		return len(got) == 1 && fileExists(pidPath) && fileExists(pidPath+".lock")
	})

	// A second daemon on the same pidfile must fail fast on the lock.
	err2 := RunDaemon(context.Background(), reg, st, adapter.DiscoverConfig{}, opt)
	if err2 == nil {
		t.Fatalf("second daemon should have failed to acquire lock")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("daemon did not stop after cancel")
	}

	// Pidfile removed on clean shutdown.
	if fileExists(pidPath) {
		t.Fatalf("pidfile %s should be removed on shutdown", pidPath)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", max)
}

// setNow overrides the collector's observation clock and returns a restore func.
func setNow(fn func() time.Time) func() {
	prev := nowFn
	nowFn = fn
	return func() { nowFn = prev }
}
