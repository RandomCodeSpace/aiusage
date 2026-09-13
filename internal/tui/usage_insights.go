package tui

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/RandomCodeSpace/aiusage/store"
)

// UsageMetric names an inspectable counter in the usage ledger.
type UsageMetric string

const (
	UsageMetricInput  UsageMetric = "Input"
	UsageMetricOutput UsageMetric = "Output"
	UsageMetricCache  UsageMetric = "Cache"
	UsageMetricCost   UsageMetric = "Cost"
	UsageMetricTotal  UsageMetric = "Total"
)

// UsageInsightContext contains already priced aggregates from one captured
// range and scope. Contributors must be a partition of that scope. Helpers do
// not query, reprice, or infer unavailable per-event telemetry.
type UsageInsightContext struct {
	Totals                 store.Bucket
	Timeline, Contributors []store.Bucket
	ScopeLabel, RangeLabel string
	Scope                  []Crumb
	Stale                  bool
}

// UsageSuggestion is a local inspection hypothesis with its original filters.
type UsageSuggestion struct {
	Title, Evidence, Action, Limits string
	Scope                           []Crumb
}

// UsageMetricInspector renders full precision values for a scrollable detail
// view. The caller supplies the same scope for totals, history and contributors.
func UsageMetricInspector(metric UsageMetric, c UsageInsightContext) string {
	lines := []string{string(metric) + " details", "", "Selected range: " + c.RangeLabel, "Selected scope: " + c.ScopeLabel}
	if c.Stale {
		lines = append(lines, "Stale snapshot: refresh failed or is pending; values are from the last loaded scope.")
	}
	if c.Totals.Events == 0 {
		return strings.Join(append(lines, "", "No usage events in this range and scope. Cost and token measurements are unavailable."), "\n")
	}
	lines = append(lines, "", usageMetricValue(metric, c.Totals), fmt.Sprintf("Usage events: %d", c.Totals.Events))
	switch metric {
	case UsageMetricInput, UsageMetricOutput, UsageMetricTotal:
		lines = append(lines,
			fmt.Sprintf("Input: %d tokens", c.Totals.Input),
			fmt.Sprintf("Output: %d tokens", c.Totals.Output),
			fmt.Sprintf("Reasoning: %d tokens", c.Totals.Reasoning),
			fmt.Sprintf("Provider-authoritative total: %d tokens", c.Totals.Total))
	case UsageMetricCache:
		lines = append(lines, fmt.Sprintf("Cache read: %d tokens", c.Totals.CacheRead), fmt.Sprintf("Cache write: %d tokens", c.Totals.CacheCreation),
			"Cache total is cache read plus cache write. These token counts do not measure a request cache hit rate.")
	case UsageMetricCost:
		lines = append(lines, usagePricingCoverage(c.Totals), "Computed prices use the configured pricing ladder. Known cost excludes unpriced events; it is not a complete bill when coverage is partial.")
	}
	if metric != UsageMetricCost {
		if usageMetricNumber(metric, c.Totals).Sign() == 0 {
			lines = append(lines, "Usage events are present; the recorded "+strings.ToLower(string(metric))+" counter is zero.")
		}
		lines = append(lines, "Token components can overlap across providers. Total uses the stored provider-authoritative counter, not a sum of components.",
			"Aggregate counters cannot distinguish an upstream omitted component from a reported zero.")
	}
	lines = append(lines, "", "Timeline values")
	if len(c.Timeline) == 0 {
		lines = append(lines, "No timeline buckets supplied.")
	}
	for _, b := range c.Timeline {
		lines = append(lines, usageBucketLabel(b)+" · "+usageMetricValue(metric, b))
		if metric == UsageMetricCost && b.Events > 0 {
			lines = append(lines, "  "+usagePricingCoverage(b))
		}
	}
	lines = append(lines, "", "Contributors, largest known value first")
	contributors := append([]store.Bucket(nil), c.Contributors...)
	sort.SliceStable(contributors, func(i, j int) bool {
		return usageMetricNumber(metric, contributors[i]).Cmp(usageMetricNumber(metric, contributors[j])) > 0
	})
	if len(contributors) == 0 {
		lines = append(lines, "No contributor buckets supplied.")
	}
	for _, b := range contributors {
		lines = append(lines, usageBucketLabel(b)+" · "+usageMetricValue(metric, b))
		if metric == UsageMetricCost && b.Events > 0 {
			lines = append(lines, "  "+usagePricingCoverage(b))
		}
	}
	return strings.Join(lines, "\n")
}

// BuildUsageSuggestions uses evidence in the selected aggregate only. The
// action can open an inspector with Scope even after the selection changes.
func BuildUsageSuggestions(c UsageInsightContext) []UsageSuggestion {
	if c.Totals.Events == 0 {
		return nil
	}
	var out []UsageSuggestion
	add := func(title, evidence, action, limits string) {
		if c.Stale {
			limits += " Evidence is from a stale snapshot; refresh before drawing conclusions."
		}
		out = append(out, UsageSuggestion{Title: title, Evidence: evidence, Action: action, Limits: limits, Scope: append([]Crumb(nil), c.Scope...)})
	}
	t := c.Totals
	if t.UnpricedEvents > 0 {
		add("Inspect missing price coverage",
			fmt.Sprintf("%s; %s: %d of %d usage events are unpriced. %s", c.ScopeLabel, c.RangeLabel, t.UnpricedEvents, t.Events, usageMetricValue(UsageMetricCost, t)),
			"Inspect Cost in this captured scope to locate unpriced contributors and check model/provider pricing configuration.",
			"Coverage counts usage events, not requests. Completing price coverage can raise the known cost; this is not a savings estimate.")
	}
	if t.CacheCreation > 0 {
		add("Inspect cache writes and reuse",
			fmt.Sprintf("%s; %s: %d cache write tokens and %d cache read tokens recorded.", c.ScopeLabel, c.RangeLabel, t.CacheCreation, t.CacheRead),
			"Inspect Cache in this captured scope and compare timeline and contributors. Check whether stable prefixes can be reused before changing cache behavior.",
			"Write and read tokens are aggregate counters, not matched cache operations. This is a hypothesis to inspect; no savings or hit rate is established.")
	} else if t.CacheRead > 0 {
		add("Inspect cache read coverage",
			fmt.Sprintf("%s; %s: %d cache read tokens and zero recorded cache write tokens.", c.ScopeLabel, c.RangeLabel, t.CacheRead),
			"Inspect Cache in this captured scope to see which timeline buckets and contributors report reads.",
			"Writes may precede the selected range or be omitted upstream. Aggregate counters establish neither cache effectiveness nor savings.")
	}
	return out
}

func usageMetricNumber(metric UsageMetric, b store.Bucket) *big.Int {
	switch metric {
	case UsageMetricInput:
		return big.NewInt(b.Input)
	case UsageMetricOutput:
		return big.NewInt(b.Output)
	case UsageMetricCache:
		return new(big.Int).Add(big.NewInt(b.CacheRead), big.NewInt(b.CacheCreation))
	case UsageMetricCost:
		return big.NewInt(b.CostMicroUSD)
	default:
		return big.NewInt(b.Total)
	}
}

func usageMetricValue(metric UsageMetric, b store.Bucket) string {
	if b.Events == 0 {
		return "No usage events"
	}
	if metric != UsageMetricCost {
		value := string(metric) + ": " + usageMetricNumber(metric, b).String() + " tokens"
		if metric == UsageMetricCache {
			value += fmt.Sprintf("; read %d, write %d", b.CacheRead, b.CacheCreation)
		}
		return value
	}
	if b.UnpricedEvents >= b.Events {
		return "Cost unknown: all usage events are unpriced"
	}
	// USD is exact to the stored microUSD, including values beyond float64's
	// integer precision. big.Int also makes signed formatting safe at MinInt64.
	micro := big.NewInt(b.CostMicroUSD)
	sign := ""
	if micro.Sign() < 0 {
		sign = "-"
		micro.Abs(micro)
	}
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(micro, big.NewInt(1_000_000), fraction)
	amount := fmt.Sprintf("%s$%s.%06d", sign, whole.String(), fraction.Int64())
	label := "Known reported cost: "
	if b.ComputedCostEvents > 0 {
		label = "Known cost with computed estimates: "
	}
	if b.UnpricedEvents > 0 {
		amount = "≥ " + amount + "; partial coverage"
	}
	return label + amount
}

func usagePricingCoverage(b store.Bucket) string {
	priced := b.Events - b.UnpricedEvents
	return fmt.Sprintf("Priced %d/%d usage events; reported %d; computed %d; unpriced %d.",
		priced, b.Events, priced-b.ComputedCostEvents, b.ComputedCostEvents, b.UnpricedEvents)
}

func usageBucketLabel(b store.Bucket) string {
	keys := b.OrderedKeys
	if len(keys) == 0 {
		for key := range b.Keys {
			keys = append(keys, key)
		}
		sort.Strings(keys)
	}
	var labels []string
	for _, key := range keys {
		value := b.Keys[key]
		if value == "" {
			value = "unknown"
		}
		labels = append(labels, key+"="+value)
	}
	if len(labels) == 0 {
		return "Scope total"
	}
	return strings.Join(labels, " · ")
}
