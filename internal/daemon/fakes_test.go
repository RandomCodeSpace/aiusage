package daemon

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// The fakes here are deliberately thin. These tests are about a LIFECYCLE — the
// lock, the pidfile, the version stamp, the executable watch — not about what a
// pass collects; collect's own tests own that. So the adapter emits whatever it
// is told to and the store remembers what it was handed, and neither reproduces
// a store invariant that is checked one package down.

// fakeAdapter discovers exactly one source and emits a fixed observation.
type fakeAdapter struct {
	id   string
	emit func() adapter.Observation
}

func (a *fakeAdapter) ID() string          { return a.id }
func (a *fakeAdapter) DisplayName() string { return a.id }

// Capabilities satisfies the interface. Nothing in the daemon reads it — the
// declaration is a display fact — so the fake states the most conservative
// thing it can.
func (a *fakeAdapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      a.id,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(a.id),
		Tier:      model.TierFixture,
	}
}

func (a *fakeAdapter) Discover(context.Context, adapter.DiscoverConfig) ([]adapter.Source, error) {
	return []adapter.Source{{Tool: a.id, Class: model.EventLevel, Path: a.id + "/src", Label: a.id}}, nil
}

func (a *fakeAdapter) Collect(context.Context, adapter.Source) (adapter.Observation, error) {
	return a.emit(), nil
}

// idleAdapter emits nothing: the lifecycle tests care that a cycle RAN, not
// what it found.
func idleAdapter() *fakeAdapter {
	return &fakeAdapter{id: model.ToolCodex, emit: func() adapter.Observation { return adapter.Observation{} }}
}

// fakeStore is an in-memory collect.Store: the six methods a pass consumes, and
// a read-back the tests use to see that the immediate first cycle really ran.
// Dedup is on DedupKey, the one store behaviour a repeated pass depends on.
type fakeStore struct {
	mu     sync.Mutex
	dedup  map[string]struct{}
	events []model.UsageEvent
	state  map[string]model.AggregateSnapshot
}

func newFakeStore() *fakeStore {
	return &fakeStore{dedup: map[string]struct{}{}, state: map[string]model.AggregateSnapshot{}}
}

func (s *fakeStore) EnsureRollup(context.Context) (bool, error) { return false, nil }

func (s *fakeStore) Checkpoint(context.Context, string, string) (*model.SourceCheckpoint, error) {
	return nil, nil
}

func (s *fakeStore) ApplyBatch(_ context.Context, b store.ObservationBatch) (store.Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.Applied{Events: s.insertLocked(b.Events)}, nil
}

func (s *fakeStore) ApplyEvents(_ context.Context, events []model.UsageEvent, _ *model.SourceCheckpoint) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertLocked(events), nil
}

func (s *fakeStore) LastState(_ context.Context, tool, key string) (*model.AggregateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.state[tool+"|"+key]; ok {
		return &st, nil
	}
	return nil, nil
}

func (s *fakeStore) ApplySnapshot(_ context.Context, events []model.UsageEvent, st model.AggregateSnapshot, _ *model.SourceCheckpoint) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.insertLocked(events)
	if len(events) == 0 || n > 0 {
		s.state[st.Tool+"|"+st.Key] = st
	}
	return n, nil
}

func (s *fakeStore) insertLocked(events []model.UsageEvent) int {
	n := 0
	for _, e := range events {
		if _, seen := s.dedup[e.DedupKey]; seen {
			continue
		}
		s.dedup[e.DedupKey] = struct{}{}
		s.events = append(s.events, e)
		n++
	}
	return n
}

// stored returns how many distinct events the fake holds.
func (s *fakeStore) stored() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// refDay is a fixed date, so an event a test seeds carries a deterministic
// time rather than one that moves with the clock.
var refDay = time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)

// discard swallows the daemon's log output. The lifecycle tests that DO read
// the log build their own buffer.
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
