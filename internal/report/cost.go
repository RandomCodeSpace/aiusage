package report

import (
	"strings"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/pricing"
	"github.com/RandomCodeSpace/aiusage/store"
)

// Cost is the resolved display cost of one bucket.
type Cost struct {
	// MicroUSD is the cost in millionths of a dollar.
	MicroUSD int64
	// Approximate marks costs computed from a rate card at collect or display time.
	Approximate bool
	// lowerBound marks a known sum that excludes unpriced rows.
	lowerBound bool
	// machineApproximate preserves the established JSON approximation contract.
	machineApproximate bool
	resolved           bool
	// Known is false when nothing in the bucket could be priced at all. Such a
	// bucket renders as model.UnpricedMark rather than a zero.
	Known bool
}

// String renders vendor costs as "$1.23", rate-card estimates as "~$1.23",
// and unknown costs as "-". A leading "≥" marks a sum with unpriced rows.
// Sub-cent amounts widen to four decimals, and anything smaller than
// that renders as "<$0.0001" — a real charge must never print as $0.00.
func (c Cost) String() string {
	value := model.FormatCost(c.MicroUSD, c.Approximate, c.Known)
	if c.Known && c.lowerBound {
		return "≥" + value
	}
	return value
}

// Costs holds the resolved cost per summary bucket (aligned by index with
// Summary.Buckets) plus the grand total.
type Costs struct {
	Buckets []Cost
	Totals  Cost
}

// Pricer values a charge with the price table in effect right now. It is
// satisfied by *pricing.Engine.
type Pricer interface {
	Price(c pricing.Charge) (int64, string, bool)
}

// ResolveCosts folds the costs stamped at collect time together with a
// display-time valuation of the rows that carry none (everything collected
// before schema v3, plus any model the ladder could not price at ingest).
//
// Stamped costs come from Summarize; the unpriced rows arrive pre-aggregated
// per model from store.UnpricedGroups, because valuing them needs the model,
// provider and tier, none of which survive a bucket-level SUM. A bucket that
// receives any computed price is marked approximate. Remaining unpriced rows
// independently make the sum a lower bound.
func ResolveCosts(sum *store.Summary, groups []store.UnpricedGroup, p Pricer) *Costs {
	if sum == nil {
		return nil
	}
	out := &Costs{Buckets: make([]Cost, len(sum.Buckets))}
	index := make(map[string]int, len(sum.Buckets))
	valued := make([]int64, len(sum.Buckets))
	for i, b := range sum.Buckets {
		index[bucketKey(b.Keys, sum.GroupBy)] = i
		out.Buckets[i] = stampedCost(b)
	}
	out.Totals = stampedCost(sum.Totals)

	var totalValued int64
	if p != nil {
		for _, g := range groups {
			micro, ok := priceGroup(p, g)
			if !ok {
				continue
			}
			if i, found := index[bucketKey(g.Keys, sum.GroupBy)]; found {
				out.Buckets[i].MicroUSD += micro
				out.Buckets[i].Approximate = true
				out.Buckets[i].machineApproximate = true
				out.Buckets[i].Known = true
				valued[i] += g.Events
			}
			out.Totals.MicroUSD += micro
			out.Totals.Approximate = true
			out.Totals.machineApproximate = true
			out.Totals.Known = true
			totalValued += g.Events
		}
	}

	// A partial valuation is a lower bound, independently of its provenance.
	for i, b := range sum.Buckets {
		out.Buckets[i].lowerBound = out.Buckets[i].Known && valued[i] < b.UnpricedEvents
		out.Buckets[i].machineApproximate = out.Buckets[i].machineApproximate || out.Buckets[i].lowerBound
	}
	out.Totals.lowerBound = out.Totals.Known && totalValued < sum.Totals.UnpricedEvents
	out.Totals.machineApproximate = out.Totals.machineApproximate || out.Totals.lowerBound

	return out
}

// stampedCost is a bucket's cost before any display-time pricing: the sum
// stamped at collect, known only while the bucket holds at least one row that
// carries one. It is the starting point of ResolveCosts and the whole answer
// when no pricer is available.
func stampedCost(b store.Bucket) Cost {
	return Cost{
		MicroUSD:    b.CostMicroUSD,
		Known:       b.Events > b.UnpricedEvents,
		Approximate: b.ComputedCostEvents > 0,
		lowerBound:  b.Events > b.UnpricedEvents && b.UnpricedEvents > 0,
		resolved:    true,
	}
}

// priceGroup values one unpriced group at the current table. The cache-write
// TTL split is not stored, so every cache write is valued at the 5m rate — the
// cheaper of the two, and the approximate marker already covers the difference.
//
// The group is a SUM over many events, so it is charged as an aggregate: its
// token totals are not any one request's prompt, and letting a thousand short
// turns add up past a model's long-context threshold would bill the whole group
// off the long card. That under-values a group that really did hold long
// requests, which is the same direction the 5m cache-write assumption errs in
// and is already covered by the tilde.
func priceGroup(p Pricer, g store.UnpricedGroup) (int64, bool) {
	micro, _, ok := p.Price(pricing.Charge{
		Model:             g.Model,
		Provider:          g.Provider,
		ServiceTier:       g.ServiceTier,
		Input:             g.Input,
		Output:            g.Output,
		Reasoning:         g.Reasoning,
		CacheRead:         g.CacheRead,
		CacheWrite5m:      g.CacheCreation,
		AdditiveReasoning: model.ReasoningModeFor(g.Tool) == model.ReasoningAdditive,
		Aggregate:         true,
	})
	return micro, ok
}

// bucketKey joins a bucket's grouping values into a comparable identity. The
// separator is a NUL so it cannot occur inside a model id, project path or
// session id and fuse two distinct buckets into one.
func bucketKey(keys map[string]string, order []string) string {
	if len(order) == 0 {
		return ""
	}
	parts := make([]string, len(order))
	for i, dim := range order {
		parts[i] = keys[dim]
	}
	return strings.Join(parts, "\x00")
}
