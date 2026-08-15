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

// TestCycleStoresSkillContextsAndNeitherAttributionInflates is the pipeline-level
// statement of the whole feature.
//
// It runs a cycle over turns that overlap in every awkward way — a multi-call
// turn inside a skill, a turn inside a skill that calls nothing at all, and a
// turn under no skill — and asserts BOTH attributions independently against the
// ledger the same pass stored. Neither may exceed it, and neither may be zero,
// which is the way a bound gets satisfied by attributing nothing.
func TestCycleStoresSkillContextsAndNeitherAttributionInflates(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	when := refDay.Add(time.Hour)

	// u-1: 1000 tokens, 2 calls, skill "alpha"  -> calls take 500 each
	// u-2:  500 tokens, 0 calls, skill "alpha"  -> invisible to activity
	// u-3:  300 tokens, 1 call,  no skill
	obs := activityObs("u-1", 1000, 2, when)
	obs.SkillContexts = append(obs.SkillContexts, model.SkillContext{
		UsageDedupKey: "u-1", Tool: model.ToolClaudeCode, Skill: "alpha",
		SessionID: "s", EventTime: when,
	})
	ctxOnly := activityObs("u-2", 500, 0, when)
	ctxOnly.SkillContexts = append(ctxOnly.SkillContexts, model.SkillContext{
		UsageDedupKey: "u-2", Tool: model.ToolClaudeCode, Skill: "alpha",
		SessionID: "s", EventTime: when,
	})
	plain := activityObs("u-3", 300, 1, when)

	obs.Events = append(obs.Events, ctxOnly.Events...)
	obs.Events = append(obs.Events, plain.Events...)
	obs.Activity = append(obs.Activity, plain.Activity...)
	obs.SkillContexts = append(obs.SkillContexts, ctxOnly.SkillContexts...)

	reg := adapter.NewRegistry(&fakeAdapter{
		id: model.ToolClaudeCode, class: model.EventLevel,
		emit: func(int) adapter.Observation { return obs },
	})

	stats, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if stats.SkillContextsSeen != 2 || stats.SkillContextsInserted != 2 {
		t.Fatalf("skill contexts seen/inserted = %d/%d, want 2/2",
			stats.SkillContextsSeen, stats.SkillContextsInserted)
	}
	if stats.EventsInserted != 3 || stats.ActivityInserted != 3 {
		t.Fatalf("events/activity inserted = %d/%d, want 3/3",
			stats.EventsInserted, stats.ActivityInserted)
	}

	const ledger int64 = 1800 // 1000 + 500 + 300

	window := store.ActivityFilter{Since: when.Add(-time.Hour), Until: when.Add(time.Hour)}
	skills, err := st.SummarizeSkillCost(ctx, window)
	if err != nil {
		t.Fatalf("SummarizeSkillCost: %v", err)
	}
	// alpha owns u-1 and u-2 whole: 1500. u-3 belongs to no skill.
	if skills.Totals.TotalTokens != 1500 {
		t.Fatalf("skill-attributed tokens = %d, want 1500", skills.Totals.TotalTokens)
	}
	if skills.Totals.Turns != 2 {
		t.Fatalf("skill turns = %d, want 2 (the tool-less turn must count)", skills.Totals.Turns)
	}

	acts, err := st.SummarizeActivity(ctx, window)
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	// u-1 splits 500/500, u-3 contributes 300, u-2 has no calls at all.
	if acts.Totals.AttributedTotal != 1300 {
		t.Fatalf("call-attributed tokens = %d, want 1300", acts.Totals.AttributedTotal)
	}

	for _, c := range []struct {
		name string
		got  int64
	}{
		{"skill", skills.Totals.TotalTokens},
		{"call", acts.Totals.AttributedTotal},
	} {
		if c.got > ledger {
			t.Fatalf("%s attribution %d exceeds the ledger's %d", c.name, c.got, ledger)
		}
		if c.got == 0 {
			t.Fatalf("%s attribution is zero; the bound would hold vacuously", c.name)
		}
	}

	// A second pass re-reads the same source: a turn's skill context is not
	// recorded twice, so its cost is not served twice either.
	stats2, err := RunCycle(ctx, reg, st, adapter.DiscoverConfig{})
	if err != nil {
		t.Fatalf("second RunCycle: %v", err)
	}
	if stats2.SkillContextsInserted != 0 {
		t.Fatalf("re-read inserted %d skill contexts, want 0", stats2.SkillContextsInserted)
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
