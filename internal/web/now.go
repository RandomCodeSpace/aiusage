package web

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/RandomCodeSpace/aiusage/store"
)

// Cost is a summed cost with the two facts a reader needs to print it
// honestly: how many rows had no price at all (the sum is a floor) and how
// many were priced from a public rate card rather than by the vendor (the sum
// is an estimate). The glyphs are the client's job; the counts are this side's.
type Cost struct {
	MicroUSD int64 `json:"micro_usd"`
	Events   int64 `json:"events"`
	Unpriced int64 `json:"unpriced"`
	Computed int64 `json:"computed"`
}

func costOf(b store.Bucket) Cost {
	return Cost{MicroUSD: b.CostMicroUSD, Events: b.Events, Unpriced: b.UnpricedEvents, Computed: b.ComputedCostEvents}
}

// Now is the operational snapshot behind the dashboard's first view.
type Now struct {
	GeneratedAt time.Time `json:"generated_at"`
	Watermark   time.Time `json:"watermark,omitzero"`
	RollupStale bool      `json:"rollup_stale"`

	LastHour Window     `json:"last_hour"`
	Today    Window     `json:"today"`
	Hours    []HourBar  `json:"hours"`
	Harness  []Harness  `json:"harnesses"`
	Idle     []Harness  `json:"idle"`
	Models   []Share    `json:"models_24h"`
	Unpriced Unpriced7d `json:"unpriced_7d"`
}

// Window is one time window's totals.
type Window struct {
	Cost
	Tokens    int64 `json:"tokens"`
	Sessions  int64 `json:"sessions"`
	Harnesses int   `json:"harnesses"`
}

// HourBar is one local-hour bucket of the last day.
type HourBar struct {
	Start    time.Time `json:"start"`
	MicroUSD int64     `json:"micro_usd"`
	Tokens   int64     `json:"tokens"`
}

// Harness is one tool's row on the freshness board.
type Harness struct {
	Tool      string    `json:"tool"`
	LastEvent time.Time `json:"last_event,omitzero"`
	Sessions  int64     `json:"sessions_1h"`
	Tokens    int64     `json:"tokens_1h"`
	Cost      Cost      `json:"cost_1h"`
	Spark     []int64   `json:"spark_24h"`
	CostFrom  string    `json:"cost_from,omitempty"`
	Tier      string    `json:"tier,omitempty"`
}

// Share is one model's slice of the last day.
type Share struct {
	Model  string `json:"model"`
	Cost   Cost   `json:"cost"`
	Tokens int64  `json:"tokens"`
}

// Unpriced7d names what the last week could not price.
type Unpriced7d struct {
	Events int64    `json:"events"`
	Models []string `json:"models"`
}

// idleAfter is how long a harness may be silent before it moves from the
// board to the idle list.
const idleAfter = 7 * 24 * time.Hour

func (s *Server) buildNow(ctx context.Context) (*Now, error) {
	now := s.now()
	out := &Now{GeneratedAt: now}

	var err error
	if out.Watermark, err = s.src.IngestWatermark(ctx); err != nil {
		return nil, err
	}
	if out.RollupStale, err = s.src.RollupStale(ctx); err != nil {
		return nil, err
	}

	hour, err := s.src.Summarize(ctx, store.Filter{Since: now.Add(-time.Hour), Until: now, GroupBy: []string{"tool"}})
	if err != nil {
		return nil, fmt.Errorf("last hour: %w", err)
	}
	out.LastHour = window(hour.Totals, len(hour.Buckets))
	byTool := make(map[string]store.Bucket, len(hour.Buckets))
	for _, b := range hour.Buckets {
		byTool[b.Keys["tool"]] = b
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	today, err := s.src.Summarize(ctx, store.Filter{Since: midnight, Until: now, GroupBy: []string{"tool"}})
	if err != nil {
		return nil, fmt.Errorf("today: %w", err)
	}
	out.Today = window(today.Totals, len(today.Buckets))

	// One query feeds both the hourly bars and every sparkline: local-hour
	// buckets per tool over the last day.
	dayStart := now.Truncate(time.Hour).Add(-23 * time.Hour)
	day, err := s.src.Summarize(ctx, store.Filter{Since: dayStart, Until: now, GroupBy: []string{"tool", "hour"}})
	if err != nil {
		return nil, fmt.Errorf("last day: %w", err)
	}
	slot := make(map[string]int, 24)
	out.Hours = make([]HourBar, 24)
	for i := range out.Hours {
		t := dayStart.Add(time.Duration(i) * time.Hour)
		out.Hours[i].Start = t
		slot[t.In(now.Location()).Format("2006-01-02 15")] = i
	}
	sparks := make(map[string][]int64)
	for _, b := range day.Buckets {
		i, ok := slot[b.Keys["hour"]]
		if !ok {
			continue
		}
		out.Hours[i].MicroUSD += b.CostMicroUSD
		out.Hours[i].Tokens += b.Total
		tool := b.Keys["tool"]
		if sparks[tool] == nil {
			sparks[tool] = make([]int64, 24)
		}
		sparks[tool][i] += b.Total
	}

	models, err := s.src.Summarize(ctx, store.Filter{Since: now.Add(-24 * time.Hour), Until: now, GroupBy: []string{"model"}})
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	for _, b := range models.Buckets {
		out.Models = append(out.Models, Share{Model: b.Keys["model"], Cost: costOf(b), Tokens: b.Total})
	}
	sort.SliceStable(out.Models, func(i, j int) bool {
		if out.Models[i].Cost.MicroUSD != out.Models[j].Cost.MicroUSD {
			return out.Models[i].Cost.MicroUSD > out.Models[j].Cost.MicroUSD
		}
		return out.Models[i].Tokens > out.Models[j].Tokens
	})

	weekFilter := store.Filter{Since: now.Add(-idleAfter), Until: now}
	week, err := s.src.Summarize(ctx, weekFilter)
	if err != nil {
		return nil, fmt.Errorf("week: %w", err)
	}
	out.Unpriced.Events = week.Totals.UnpricedEvents
	out.Unpriced.Models = []string{}
	if out.Unpriced.Events > 0 {
		groups, err := s.src.UnpricedGroups(ctx, weekFilter)
		if err != nil {
			return nil, fmt.Errorf("unpriced: %w", err)
		}
		seen := map[string]bool{}
		for _, g := range groups {
			name := g.Model
			if name == "" {
				name = "(no model)"
			}
			if !seen[name] {
				seen[name] = true
				out.Unpriced.Models = append(out.Unpriced.Models, name)
			}
		}
		sort.Strings(out.Unpriced.Models)
	}

	last, err := s.src.LastEventTimes(ctx)
	if err != nil {
		return nil, fmt.Errorf("freshness: %w", err)
	}
	out.Harness = []Harness{}
	out.Idle = []Harness{}
	for tool, at := range last {
		h := Harness{Tool: tool, LastEvent: at, Spark: sparks[tool]}
		if h.Spark == nil {
			h.Spark = make([]int64, 24)
		}
		if b, ok := byTool[tool]; ok {
			h.Sessions, h.Tokens, h.Cost = b.Sessions, b.Total, costOf(b)
		}
		if c, ok := s.caps[tool]; ok {
			h.CostFrom = string(c.Cost)
			h.Tier = string(c.Tier)
		}
		if now.Sub(at) > idleAfter {
			out.Idle = append(out.Idle, Harness{Tool: tool, LastEvent: at, CostFrom: h.CostFrom, Tier: h.Tier})
			continue
		}
		out.Harness = append(out.Harness, h)
	}
	byRecency := func(hs []Harness) func(i, j int) bool {
		return func(i, j int) bool {
			if !hs[i].LastEvent.Equal(hs[j].LastEvent) {
				return hs[i].LastEvent.After(hs[j].LastEvent)
			}
			return hs[i].Tool < hs[j].Tool
		}
	}
	sort.SliceStable(out.Harness, byRecency(out.Harness))
	sort.SliceStable(out.Idle, byRecency(out.Idle))
	return out, nil
}

func window(b store.Bucket, harnesses int) Window {
	return Window{Cost: costOf(b), Tokens: b.Total, Sessions: b.Sessions, Harnesses: harnesses}
}
