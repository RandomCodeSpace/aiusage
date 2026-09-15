package web

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// History is one partition of one window: the same dollars split by exactly
// one dimension. Two partitions never appear in one response, because their
// rows do not add up to anything (CLAUDE.md, "the six partitions").
type History struct {
	Dim   string    `json:"dim"`
	Range string    `json:"range"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	Total Row   `json:"total"`
	Rows  []Row `json:"rows"`
	// Days are the local days of the window, oldest first; Series holds the
	// top rows' cost per day in that order.
	Days   []string `json:"days"`
	Series []Series `json:"series"`
	// Coverage is how many of the window's turns carried this dimension at
	// all. Absent for the usage dimensions, where every row names a value.
	Coverage *Coverage `json:"coverage,omitempty"`
}

// Row is one value of the partition.
type Row struct {
	Value  string `json:"value"`
	Cost   Cost   `json:"cost"`
	Tokens int64  `json:"tokens"`
}

// Series is one value's cost per day.
type Series struct {
	Value    string  `json:"value"`
	MicroUSD []int64 `json:"micro_usd"`
}

// Coverage is turns with the dimension over all turns in the window.
type Coverage struct {
	Turns int64 `json:"turns"`
	Of    int64 `json:"of"`
}

const (
	maxRows   = 25
	maxSeries = 5
)

// usageDims are the partitions every usage row carries. The turn-context
// dimensions come from model.TurnDimensions and are not restated here.
var usageDims = map[string]bool{"tool": true, "model": true, "project": true}

// Dimensions lists every partition the history view accepts, usage first.
func Dimensions() []string {
	out := []string{"tool", "model", "project"}
	for _, d := range model.TurnDimensions() {
		out = append(out, string(d))
	}
	return out
}

func (s *Server) buildHistory(ctx context.Context, dim, rng string) (*History, error) {
	if dim == "" {
		dim = "tool"
	}
	if rng == "" {
		rng = "7d"
	}
	now := s.now()
	since, err := rangeStart(now, rng)
	if err != nil {
		return nil, err
	}
	h := &History{Dim: dim, Range: rng, Since: since, Until: now, Rows: []Row{}, Series: []Series{}}
	h.Days = localDays(since, now)
	dayIndex := make(map[string]int, len(h.Days))
	for i, d := range h.Days {
		dayIndex[d] = i
	}

	switch {
	case usageDims[dim]:
		err = s.usageHistory(ctx, h, dayIndex)
	case model.TurnDimension(dim).Valid():
		err = s.turnHistory(ctx, h, model.TurnDimension(dim), dayIndex)
	default:
		return nil, &badRequest{fmt.Sprintf("unknown dimension %q; one of %v", dim, Dimensions())}
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (s *Server) usageHistory(ctx context.Context, h *History, dayIndex map[string]int) error {
	// The rollup answers a month window in tens of milliseconds where the
	// ledger takes seconds, and every window here starts on a local midnight,
	// which is on a quarter-hour boundary in every real zone, so its buckets
	// fold exactly. It is a derived table, so it is checked before it is read
	// and the ledger is used when it has fallen behind (CLAUDE.md, "a stale
	// rollup is DETECTED, never trusted").
	stale, err := s.src.RollupStale(ctx)
	if err != nil {
		return fmt.Errorf("history %s: %w", h.Dim, err)
	}
	summarize := func(f store.Filter) ([]store.Bucket, store.Bucket, error) {
		if stale {
			sum, err := s.src.Summarize(ctx, f)
			if err != nil {
				return nil, store.Bucket{}, err
			}
			return sum.Buckets, sum.Totals, nil
		}
		sum, err := s.src.SummarizeRollup(ctx, f)
		if err != nil {
			return nil, store.Bucket{}, err
		}
		return sum.Buckets, sum.Totals, nil
	}

	f := store.Filter{Since: h.Since, Until: h.Until, GroupBy: []string{h.Dim}}
	buckets, totals, err := summarize(f)
	if err != nil {
		return fmt.Errorf("history %s: %w", h.Dim, err)
	}
	h.Total = Row{Cost: costOf(totals), Tokens: totals.Total}
	for _, b := range buckets {
		h.Rows = append(h.Rows, Row{Value: b.Keys[h.Dim], Cost: costOf(b), Tokens: b.Total})
	}
	rankRows(h)

	f.GroupBy = []string{h.Dim, "day"}
	daily, _, err := summarize(f)
	if err != nil {
		return fmt.Errorf("history %s by day: %w", h.Dim, err)
	}
	fillSeries(h, dayIndex, func(yield func(value, day string, micro int64)) {
		for _, b := range daily {
			yield(b.Keys[h.Dim], b.Keys["day"], b.CostMicroUSD)
		}
	})
	return nil
}

func (s *Server) turnHistory(ctx context.Context, h *History, dim model.TurnDimension, dayIndex map[string]int) error {
	f := store.ActivityFilter{Since: h.Since, Until: h.Until, GroupBy: []string{"value"}}
	sum, err := s.src.SummarizeTurnContext(ctx, dim, f)
	if err != nil {
		return fmt.Errorf("history %s: %w", dim, err)
	}
	h.Total = Row{Cost: turnCost(sum.Totals), Tokens: sum.Totals.TotalTokens}
	for _, b := range sum.Buckets {
		h.Rows = append(h.Rows, Row{Value: b.Keys["value"], Cost: turnCost(b), Tokens: b.TotalTokens})
	}
	rankRows(h)

	all, err := s.src.Summarize(ctx, store.Filter{Since: h.Since, Until: h.Until})
	if err != nil {
		return fmt.Errorf("history %s coverage: %w", dim, err)
	}
	h.Coverage = &Coverage{Turns: sum.Totals.Turns, Of: all.Totals.Events}

	f.GroupBy = []string{"value", "day"}
	daily, err := s.src.SummarizeTurnContext(ctx, dim, f)
	if err != nil {
		return fmt.Errorf("history %s by day: %w", dim, err)
	}
	fillSeries(h, dayIndex, func(yield func(value, day string, micro int64)) {
		for _, b := range daily.Buckets {
			yield(b.Keys["value"], b.Keys["day"], b.CostMicroUSD)
		}
	})
	return nil
}

func turnCost(b store.TurnContextBucket) Cost {
	return Cost{MicroUSD: b.CostMicroUSD, Events: b.Turns, Unpriced: b.UnpricedTurns, Computed: b.ComputedCostTurns}
}

// rankRows orders by cost, then tokens, and keeps the top maxRows. Ties are
// broken by value so the order is stable across passes.
func rankRows(h *History) {
	sort.SliceStable(h.Rows, func(i, j int) bool {
		a, b := h.Rows[i], h.Rows[j]
		if a.Cost.MicroUSD != b.Cost.MicroUSD {
			return a.Cost.MicroUSD > b.Cost.MicroUSD
		}
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Value < b.Value
	})
	if len(h.Rows) > maxRows {
		h.Rows = h.Rows[:maxRows]
	}
}

// fillSeries builds one day series per top row from a (value, day, cost)
// walk. Values outside the top rows are dropped, not folded: the chart shows
// the leaders and the table shows the rest.
func fillSeries(h *History, dayIndex map[string]int, walk func(yield func(value, day string, micro int64))) {
	n := min(len(h.Rows), maxSeries)
	pos := make(map[string]int, n)
	for i := 0; i < n; i++ {
		pos[h.Rows[i].Value] = i
		h.Series = append(h.Series, Series{Value: h.Rows[i].Value, MicroUSD: make([]int64, len(h.Days))})
	}
	walk(func(value, day string, micro int64) {
		i, ok := pos[value]
		if !ok {
			return
		}
		d, ok := dayIndex[day]
		if !ok {
			return
		}
		h.Series[i].MicroUSD[d] += micro
	})
}

// rangeStart resolves a range name to its local start. Multi-day windows start
// at a local midnight so the day series has whole days in it.
func rangeStart(now time.Time, rng string) (time.Time, error) {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch rng {
	case "today":
		return midnight, nil
	case "7d":
		return midnight.AddDate(0, 0, -6), nil
	case "30d":
		return midnight.AddDate(0, 0, -29), nil
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), nil
	}
	return time.Time{}, &badRequest{fmt.Sprintf("unknown range %q; one of today, 7d, 30d, month", rng)}
}

// localDays lists the local calendar days from since through until's day.
func localDays(since, until time.Time) []string {
	var out []string
	for d := since; !d.After(until); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}
