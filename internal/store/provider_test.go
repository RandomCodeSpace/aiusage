package store

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// providerRef is the fixed event time the provider tests file their events at.
var providerRef = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// seedProviders inserts events across two named providers plus rows whose
// source never named one, so a provider grouping has both cases to bucket.
func seedProviders(t *testing.T, st *SQLite) {
	t.Helper()
	mk := func(key, provider string, total int64) model.UsageEvent {
		e := ev(key, model.ToolOpenCode, providerRef, total)
		e.Provider = provider
		return e
	}
	evs := []model.UsageEvent{
		mk("p1", model.ProviderAnthropic, 100),
		mk("p2", model.ProviderAnthropic, 200),
		mk("p3", model.ProviderOpenAI, 50),
		mk("p4", "", 7),
		mk("p5", "", 3),
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed providers: %v", err)
	}
}

// TestSummarizeGroupsByProvider is the issue #38 regression: provider was
// stored on every event and priced against, but groupExpr rejected it, so
// "what am I spending per provider" could not be asked at all. The rows whose
// provider is unknown must form their own bucket keyed by the STORED empty
// string: labelling it is the display layer's job, and a store that answered
// "unknown" here would be indistinguishable from a provider named that.
func TestSummarizeGroupsByProvider(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedProviders(t, st)

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"provider"}})
	if err != nil {
		t.Fatalf("summarize by provider: %v", err)
	}
	if len(sum.Buckets) != 3 {
		t.Fatalf("buckets = %d (%+v), want 3 (anthropic, openai, unknown)", len(sum.Buckets), sum.Buckets)
	}

	got := map[string]int64{}
	for _, b := range sum.Buckets {
		if len(b.OrderedKeys) != 1 || b.OrderedKeys[0] != "provider" {
			t.Fatalf("bucket OrderedKeys = %v, want [provider]", b.OrderedKeys)
		}
		got[b.Keys["provider"]] = b.Total
	}
	want := map[string]int64{model.ProviderAnthropic: 300, model.ProviderOpenAI: 50, "": 10}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("provider %q total = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("provider buckets = %v, want exactly %v", got, want)
	}
	if sum.Totals.Total != 360 {
		t.Errorf("grand total = %d, want 360", sum.Totals.Total)
	}
}

// TestSummarizeProviderQueryCount pins the new dimension to the same SQL cost
// as every other one: a grouped pass plus the narrow distinct-session count,
// and nothing else. A provider grouping that reached for a second aggregate
// (or a per-bucket lookup) would fail here rather than ship.
func TestSummarizeProviderQueryCount(t *testing.T) {
	st := openCounting(t)
	seedProviders(t, st)
	ctx := context.Background()

	n := statementsDuring(func() {
		s, err := st.Summarize(ctx, Filter{GroupBy: []string{"provider"}})
		if err != nil {
			t.Fatalf("Summarize by provider: %v", err)
		}
		if len(s.Buckets) != 3 || s.Totals.Events != 5 {
			t.Fatalf("buckets=%d totals=%d, want 3/5", len(s.Buckets), s.Totals.Events)
		}
	})
	if n != 2 {
		t.Errorf("provider Summarize ran %d statements, want exactly 2 (grouped pass + distinct sessions)", n)
	}
}
