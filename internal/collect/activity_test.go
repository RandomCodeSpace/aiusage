package collect

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// activityObs builds an observation of one turn that cost `total` tokens and
// contained `calls` tool calls sharing that one usage record.
func activityObs(key string, total int64, calls int, when time.Time) adapter.Observation {
	e := model.UsageEvent{
		Tool: model.ToolClaudeCode, Model: "m", SessionID: "s",
		EventTime: when, InputTokens: total, TotalTokens: total,
		MessageID: key, DedupKey: key, Kind: model.KindUsage,
	}
	obs := adapter.Observation{Events: []model.UsageEvent{e}}
	for i := range calls {
		obs.Activity = append(obs.Activity, model.ActivityEvent{
			Tool: model.ToolClaudeCode, Kind: model.ActivityTool, Name: "Bash",
			SessionID: "s", EventTime: when,
			UsageDedupKey: key, MessageID: key,
			TurnSeq: i, CallsInTurn: calls,
			DedupKey: key + "|act|" + string(rune('0'+i)),
		})
	}
	return obs
}

// TestCycleStoresActivityAlongsideUsage: a cycle carries both ledgers, counts
// them separately, and is idempotent on a re-read.
func TestCycleStoresActivityAlongsideUsage(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	obs := activityObs("u-1", 900, 3, refDay.Add(time.Hour))
	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return obs },
	})

	stats, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if stats.EventsInserted != 1 {
		t.Fatalf("events inserted = %d, want 1", stats.EventsInserted)
	}
	if stats.ActivitySeen != 3 || stats.ActivityInserted != 3 {
		t.Fatalf("activity seen/inserted = %d/%d, want 3/3", stats.ActivitySeen, stats.ActivityInserted)
	}

	// A second pass re-reads the same source: nothing new in either ledger.
	stats2, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("second RunCycle: %v", err)
	}
	if stats2.ActivityInserted != 0 || stats2.EventsInserted != 0 {
		t.Fatalf("re-read inserted %d events / %d activity, want 0/0",
			stats2.EventsInserted, stats2.ActivityInserted)
	}
}

// TestCycleActivityNeverOutRunsTheLedger is the collector-level statement of
// the table's invariant: whatever a pass stores, the attributed tokens over a
// window cannot exceed the usage the same window recorded.
func TestCycleActivityNeverOutRunsTheLedger(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(call int) adapter.Observation {
			// Turns with different call counts, including the multi-call turn
			// that would inflate a naive implementation.
			switch call {
			case 0:
				return activityObs("u-1", 900, 3, refDay.Add(time.Hour))
			default:
				return activityObs("u-2", 500, 1, refDay.Add(2*time.Hour))
			}
		},
	})

	for range 2 {
		if _, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}); err != nil {
			t.Fatalf("RunCycle: %v", err)
		}
	}

	since, until := refDay, refDay.Add(24*time.Hour)
	ledger := windowTotal(t, st, since, until)
	acts, err := st.SummarizeActivity(ctx, store.ActivityFilter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if acts.Totals.AttributedTotal > ledger {
		t.Fatalf("attributed %d tokens over a window the ledger recorded %d for",
			acts.Totals.AttributedTotal, ledger)
	}
	if acts.Totals.Calls != 4 {
		t.Fatalf("calls = %d, want 4", acts.Totals.Calls)
	}
	// 3*(900/3) + 1*(500/1) = 1400, and the ledger holds 1400.
	if acts.Totals.AttributedTotal != ledger {
		t.Fatalf("attributed %d, want the full %d (every turn here has calls)",
			acts.Totals.AttributedTotal, ledger)
	}
}

// TestActivityHasNoRawToStrip: privacy.no_raw drops audit payloads, and
// activity has none by construction. The cycle must still collect it.
func TestActivityHasNoRawToStrip(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation {
			obs := activityObs("u-1", 100, 2, refDay.Add(time.Hour))
			obs.Events[0].Raw = `{"usage":{"input_tokens":100}}`
			return obs
		},
	})

	stats, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{}, WithoutRaw())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if stats.ActivityInserted != 2 {
		t.Fatalf("activity inserted = %d, want 2 under no_raw", stats.ActivityInserted)
	}
	if st.events[0].Raw != "" {
		t.Fatalf("no_raw left a raw payload on the usage event: %q", st.events[0].Raw)
	}
}
