package cmd

import (
	"context"
	"path/filepath"

	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/report"
	"github.com/RandomCodeSpace/aiusage/pricing"
	"github.com/RandomCodeSpace/aiusage/store"
)

// newPricer builds the pricing ladder for the resolved config: the config
// overrides, the refreshed LiteLLM table cached next to the database, and the
// embedded snapshot. It never touches the network here — only a collection
// cycle refreshes — and never fails: an unreadable cache just drops that rung.
func newPricer(cfg config.Config) *pricing.Engine {
	return pricing.New(pricing.Options{
		DataDir:   filepath.Dir(cfg.DBPath),
		Refresh:   cfg.Pricing.Refresh,
		Overrides: overrideRates(cfg.Pricing.Overrides),
	})
}

// overrideRates converts the config's per-model overrides into pricing rates.
// The config type is separate so package pricing keeps importing only model.
func overrideRates(in map[string]config.ModelRates) map[string]pricing.Rates {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]pricing.Rates, len(in))
	for name, r := range in {
		out[name] = pricing.Rates{
			Input:        r.Input,
			Output:       r.Output,
			CacheRead:    r.CacheRead,
			CacheWrite5m: r.CacheWrite,
			CacheWrite1h: r.CacheWrite1h,
			InputBatch:   r.InputBatch,
			OutputBatch:  r.OutputBatch,
		}
	}
	return out
}

// resolveCosts builds the cost column for a rendered summary: the costs stamped
// at collect time, plus a display-time valuation of the rows that carry none.
// A failure to read the unpriced rows is not fatal — the table still renders,
// showing only the stamped costs, which is an understatement rather than a
// wrong number.
func resolveCosts(ctx context.Context, st *store.Reader, cfg config.Config, sum *store.Summary, filter store.Filter) *report.Costs {
	if sum == nil {
		return nil
	}
	groups, err := st.UnpricedGroups(ctx, filter)
	if err != nil {
		return report.ResolveCosts(sum, nil, nil)
	}
	return report.ResolveCosts(sum, groups, newPricer(cfg))
}

// costNote explains the tilde under a table that contains display-priced rows,
// so an approximate total is never mistaken for the billed amount.
func costNote(costs *report.Costs) string {
	if costs == nil {
		return ""
	}
	approx := costs.Totals.Approximate
	for _, c := range costs.Buckets {
		approx = approx || c.Approximate
	}
	if !approx {
		return ""
	}
	return "~ = includes rows priced at display time from the current table; - = unpriced"
}
