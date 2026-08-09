package collect

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// BenchmarkRunCycle measures one steady-state collection cycle over a small
// fixed fixture: an event-level adapter re-emitting 100 already-stored events
// (the INSERT OR IGNORE dedup path) and an aggregate adapter whose snapshot no
// longer grows (the zero-delta state path). The clock is frozen so synthetic
// dedup keys stay stable and the store never grows across iterations, keeping
// the baseline flat.
func BenchmarkRunCycle(b *testing.B) {
	events := make([]model.UsageEvent, 100)
	for i := range events {
		events[i] = model.UsageEvent{
			Tool:         model.ToolClaudeCode,
			Model:        "claude-opus",
			SessionID:    "sess-bench",
			Project:      "/work/bench",
			EventTime:    refDay.Add(time.Duration(i) * time.Minute),
			InputTokens:  400,
			OutputTokens: 600,
			TotalTokens:  1000,
			DedupKey:     "cc|bench|" + strconv.Itoa(i),
			Kind:         model.KindUsage,
		}
	}
	evAd := &fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Events: events}
		},
	}
	agAd := &fakeAdapter{
		id: model.ToolHermes, class: model.Aggregate,
		emit: func(int) adapter.Observation {
			return adapter.Observation{Snapshots: []model.AggregateSnapshot{{
				Tool: model.ToolHermes, Key: "sess-agg", SessionID: "sess-agg",
				InputTokens: 500, TotalTokens: 500,
			}}}
		},
	}
	reg := adapter.NewRegistry(evAd, agAd)
	st := newFakeStore()
	ctx := context.Background()

	restore := setNow(func() time.Time { return refDay.Add(6 * time.Hour) })
	defer restore()

	// First cycle inserts everything; iterations then measure the steady state.
	if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
		b.Fatalf("warm-up cycle: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
			b.Fatalf("cycle: %v", err)
		}
	}
}
