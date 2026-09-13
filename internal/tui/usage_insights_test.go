package tui

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/store"
)

func TestUsageMetricInspectorPricingStates(t *testing.T) {
	cases := []struct {
		name   string
		b      store.Bucket
		want   []string
		absent string
	}{
		{"empty", store.Bucket{}, []string{"No usage events", "measurements are unavailable"}, "$0.000000"},
		{"reported zero", store.Bucket{Events: 2}, []string{"Known reported cost: $0.000000", "Priced 2/2 usage events; reported 2; computed 0; unpriced 0"}, "Cost unknown"},
		{"unknown", store.Bucket{Events: 2, UnpricedEvents: 2}, []string{"Cost unknown: all usage events are unpriced", "Priced 0/2 usage events"}, "$0.000000"},
		{"partial zero", store.Bucket{Events: 2, UnpricedEvents: 1}, []string{"≥ $0.000000; partial coverage", "reported 1; computed 0; unpriced 1"}, "Cost unknown"},
		{"computed partial", store.Bucket{Events: 4, UnpricedEvents: 1, ComputedCostEvents: 2, CostMicroUSD: 1_234_567}, []string{"Known cost with computed estimates: ≥ $1.234567", "reported 1; computed 2; unpriced 1"}, "Known reported cost:"},
		{"large", store.Bucket{Events: 1, CostMicroUSD: math.MaxInt64}, []string{"$9223372036854.775807"}, "$9223372036854.775808"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out := UsageMetricInspector(UsageMetricCost, UsageInsightContext{Totals: tt.b, ScopeLabel: "model=selected", RangeLabel: "Today"})
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			if strings.Contains(out, tt.absent) {
				t.Errorf("unexpected %q in:\n%s", tt.absent, out)
			}
		})
	}
}

func TestUsageMetricInspectorPreservesAuthoritativeCounters(t *testing.T) {
	c := UsageInsightContext{Totals: store.Bucket{Events: 1, Input: 100, Output: 20, CacheRead: 50, CacheCreation: 10, Reasoning: 7, Total: 120}}
	for metric, value := range map[UsageMetric]string{UsageMetricInput: "Input: 100 tokens", UsageMetricOutput: "Output: 20 tokens", UsageMetricCache: "Cache: 60 tokens", UsageMetricTotal: "Total: 120 tokens"} {
		out := UsageMetricInspector(metric, c)
		if !strings.Contains(out, value) || !strings.Contains(out, "components can overlap") {
			t.Errorf("%s inspector:\n%s", metric, out)
		}
		if strings.Contains(out, "180 tokens") || strings.Contains(out, "187 tokens") {
			t.Errorf("invented total:\n%s", out)
		}
	}
	out := UsageMetricInspector(UsageMetricCache, UsageInsightContext{Totals: store.Bucket{Events: 1, CacheRead: math.MaxInt64, CacheCreation: math.MaxInt64}})
	if !strings.Contains(out, "Cache: 18446744073709551614 tokens") {
		t.Fatalf("cache sum overflow:\n%s", out)
	}
	out = UsageMetricInspector(UsageMetricInput, UsageInsightContext{Totals: store.Bucket{Events: 1}})
	if !strings.Contains(out, "recorded input counter is zero") || !strings.Contains(out, "cannot distinguish an upstream omitted component") {
		t.Fatalf("zero lost reporting limits:\n%s", out)
	}
}

func TestUsageMetricInspectorScopeTimelineAndContributors(t *testing.T) {
	contributors := []store.Bucket{
		{Keys: map[string]string{"model": "small"}, Events: 1, Input: 1},
		{Keys: map[string]string{"model": "large"}, Events: 1, Input: 100},
	}
	c := UsageInsightContext{
		Totals: store.Bucket{Events: 2, Input: 101}, ScopeLabel: "tool=codex / model=selected", RangeLabel: "Sep 1 to Sep 2", Stale: true,
		Contributors: contributors,
		Timeline: []store.Bucket{
			{Keys: map[string]string{"day": "2026-09-01"}},
			{Keys: map[string]string{"day": "2026-09-02"}, Events: 2, Input: 101},
		},
	}
	out := UsageMetricInspector(UsageMetricInput, c)
	for _, want := range []string{"Selected scope: tool=codex / model=selected", "Selected range: Sep 1 to Sep 2", "Stale snapshot", "day=2026-09-01 · No usage events", "day=2026-09-02 · Input: 101 tokens"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "model=large") > strings.Index(out, "model=small") {
		t.Errorf("contributors not ranked:\n%s", out)
	}
	if contributors[0].Keys["model"] != "small" {
		t.Fatal("inspector mutated caller contributor order")
	}
}

func TestBuildUsageSuggestionsScopeAndEvidence(t *testing.T) {
	c := UsageInsightContext{
		Totals:     store.Bucket{Events: 4, UnpricedEvents: 1, CostMicroUSD: 7, CacheCreation: 20, CacheRead: 5},
		ScopeLabel: "tool=codex / model=selected", RangeLabel: "Today", Scope: []Crumb{{Dim: "tool", Value: "codex"}, {Dim: "model", Value: "selected"}}, Stale: true,
	}
	got := BuildUsageSuggestions(c)
	if len(got) != 2 {
		t.Fatalf("want two supported suggestions, got %#v", got)
	}
	for _, s := range got {
		if !reflect.DeepEqual(s.Scope, c.Scope) {
			t.Fatalf("scope lost: %#v", s.Scope)
		}
		if !strings.Contains(s.Evidence, "tool=codex / model=selected; Today") || !strings.Contains(s.Limits, "stale snapshot") || s.Action == "" {
			t.Errorf("incomplete suggestion: %#v", s)
		}
	}
	if !strings.Contains(got[0].Evidence, "1 of 4 usage events are unpriced") || !strings.Contains(got[0].Evidence, "$0.000007") {
		t.Errorf("wrong pricing evidence: %s", got[0].Evidence)
	}
	if got[0].Metric != UsageMetricCost {
		t.Errorf("pricing target = %q, want Cost", got[0].Metric)
	}
	if !strings.Contains(got[1].Evidence, "20 cache write tokens and 5 cache read tokens") || !strings.Contains(got[1].Limits, "no savings or hit rate") {
		t.Errorf("wrong cache evidence: %#v", got[1])
	}
	if got[1].Metric != UsageMetricCache {
		t.Errorf("cache target = %q, want Cache", got[1].Metric)
	}
	c.Scope[0].Value = "changed"
	if got[0].Scope[0].Value != "codex" {
		t.Fatal("suggestion scope aliases caller")
	}
	got[0].Scope[1].Value = "changed"
	if got[1].Scope[1].Value != "selected" {
		t.Fatal("suggestions alias each other's scope")
	}
}

func TestBuildUsageSuggestionsOnlySupportedHypotheses(t *testing.T) {
	for _, b := range []store.Bucket{{}, {Events: 1}, {Events: 1, Input: 1_000_000, CostMicroUSD: 1_000_000_000}} {
		if got := BuildUsageSuggestions(UsageInsightContext{Totals: b}); len(got) != 0 {
			t.Fatalf("unsupported recommendation: %#v", got)
		}
	}
	got := BuildUsageSuggestions(UsageInsightContext{Totals: store.Bucket{Events: 1, CacheRead: 5}})
	if len(got) != 1 || got[0].Title != "Inspect cache read coverage" || !strings.Contains(got[0].Limits, "Writes may precede the selected range") {
		t.Fatalf("read evidence: %#v", got)
	}
}
