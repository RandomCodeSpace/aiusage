// Package collect drives one or more collection cycles: it iterates the
// adapter registry, reads each discovered source read-only, and persists the
// observations into the append-only store. EventLevel observations are stored
// directly (deduplicated on DedupKey). Aggregate observations are turned into
// synthetic immutable events via a monotonic-with-reset delta against the last
// stored accumulator state, so a source that later shrinks (compaction,
// deletion, reset) can never reduce a previously-reported total.
package collect

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
	"aiusage/internal/store"
)

// nowFn yields the cycle's observation timestamp. It is a package-level seam so
// tests can simulate polls at distinct instants (in production, real polls are
// minutes apart). Always returns UTC.
var nowFn = func() time.Time { return time.Now().UTC() }

// CycleStats reports the outcome of a single RunCycle.
type CycleStats struct {
	Adapters       int      // adapters iterated
	Sources        int      // sources discovered + collected
	EventsInserted int      // new dedup keys actually written (event + synthetic)
	EventsSeen     int      // observed event-level records (pre-dedup)
	Snapshots      int      // aggregate snapshots observed
	Errors         []string // non-fatal per-adapter / per-source errors
}

// RunCycle performs one full collection pass. Per-source and per-adapter errors
// are non-fatal: they are appended to CycleStats.Errors and collection
// continues. RunCycle only returns a non-nil error for failures that prevent
// the cycle from making any meaningful progress (none currently — the loop is
// fully resilient), so callers may safely run it on a ticker.
func RunCycle(ctx context.Context, reg *adapter.Registry, st store.Store, dc adapter.DiscoverConfig) (CycleStats, error) {
	var stats CycleStats
	observedAt := nowFn()

	for _, ad := range reg.All() {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Adapters++

		sources, err := ad.Discover(ctx, dc)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("discover %s: %v", ad.ID(), err))
			// Discover may still return best-effort sources alongside an error.
		}

		for _, src := range sources {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			stats.Sources++

			obs, err := ad.Collect(ctx, src)
			if err != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("collect %s %s: %v", ad.ID(), src.Path, err))
				// Best-effort: a bad source must not abort the cycle. Still
				// process whatever observations were returned.
			}

			if n, sErr := storeEvents(ctx, st, obs.Events, observedAt); sErr != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("insert events %s %s: %v", ad.ID(), src.Path, sErr))
			} else {
				stats.EventsSeen += len(obs.Events)
				stats.EventsInserted += n
			}

			for _, s := range obs.Snapshots {
				stats.Snapshots++
				n, sErr := storeSnapshot(ctx, st, s, observedAt)
				if sErr != nil {
					stats.Errors = append(stats.Errors, fmt.Sprintf("snapshot %s %s: %v", s.Tool, s.Key, sErr))
					continue
				}
				stats.EventsInserted += n
			}
		}
	}

	return stats, nil
}

// storeEvents stamps ObservedTime on event-level records that lack one, then
// appends them idempotently. Returns the number of new dedup keys inserted.
func storeEvents(ctx context.Context, st store.Store, events []model.UsageEvent, observedAt time.Time) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	stamped := make([]model.UsageEvent, len(events))
	for i, e := range events {
		if e.ObservedTime.IsZero() {
			e.ObservedTime = observedAt
		}
		stamped[i] = e
	}
	return st.InsertEvents(ctx, stamped)
}

// storeSnapshot materialises the positive monotonic-with-reset delta for one
// aggregate cell as a synthetic immutable event, then records the new state.
// Returns the number of new dedup keys inserted (0 or 1).
func storeSnapshot(ctx context.Context, st store.Store, s model.AggregateSnapshot, observedAt time.Time) (int, error) {
	last, err := st.LastState(ctx, s.Tool, s.Key)
	if err != nil {
		return 0, fmt.Errorf("last state: %w", err)
	}

	d := snapshotDelta(last, s)

	inserted := 0
	if d.hasPositive() {
		ev := syntheticEvent(s, d, observedAt)
		n, err := st.InsertEvents(ctx, []model.UsageEvent{ev})
		if err != nil {
			return 0, fmt.Errorf("insert synthetic: %w", err)
		}
		inserted = n
		if n == 0 {
			// The synthetic delta collided with an existing dedup_key (two cycles
			// at the same observed instant). Do NOT advance state: leaving the
			// baseline unchanged lets the next poll re-derive this positive delta
			// and insert it under a fresh timestamp, so it can never be lost.
			return inserted, nil
		}
	}

	// Advance state so subsequent polls diff against the latest counters, even
	// when this poll produced no positive delta.
	s.ObservedTime = observedAt
	if err := st.UpsertState(ctx, s); err != nil {
		return inserted, fmt.Errorf("upsert state: %w", err)
	}
	return inserted, nil
}

// delta carries the per-field positive change for one aggregate cell.
type delta struct {
	input         int64
	output        int64
	cacheCreation int64
	cacheRead     int64
	reasoning     int64
	total         int64
}

func (d delta) hasPositive() bool {
	return d.input > 0 || d.output > 0 || d.cacheCreation > 0 ||
		d.cacheRead > 0 || d.reasoning > 0 || d.total > 0
}

// fieldDelta computes the monotonic-with-reset change for a single field:
// if the counter grew or held, take the increment; if it shrank (a reset,
// truncation or deletion) take the current value. Never returns negative.
func fieldDelta(last, cur int64) int64 {
	if cur >= last {
		return cur - last
	}
	return cur
}

// snapshotDelta derives the positive per-field delta of cur relative to last.
// A nil last (first observation of the cell) yields the full current counters.
func snapshotDelta(last *model.AggregateSnapshot, cur model.AggregateSnapshot) delta {
	if last == nil {
		return delta{
			input:         maxZero(cur.InputTokens),
			output:        maxZero(cur.OutputTokens),
			cacheCreation: maxZero(cur.CacheCreationTokens),
			cacheRead:     maxZero(cur.CacheReadTokens),
			reasoning:     maxZero(cur.ReasoningTokens),
			total:         maxZero(cur.TotalTokens),
		}
	}
	return delta{
		input:         fieldDelta(last.InputTokens, cur.InputTokens),
		output:        fieldDelta(last.OutputTokens, cur.OutputTokens),
		cacheCreation: fieldDelta(last.CacheCreationTokens, cur.CacheCreationTokens),
		cacheRead:     fieldDelta(last.CacheReadTokens, cur.CacheReadTokens),
		reasoning:     fieldDelta(last.ReasoningTokens, cur.ReasoningTokens),
		total:         fieldDelta(last.TotalTokens, cur.TotalTokens),
	}
}

func maxZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// syntheticEvent builds the immutable usage event representing one aggregate
// delta. EventTime == ObservedTime because aggregate sources carry no real
// per-record event time. The DedupKey uses nanosecond resolution so distinct
// polls never collide; an exact-instant collision (frozen clock) is handled by
// the caller, which refuses to advance state on a failed insert.
func syntheticEvent(s model.AggregateSnapshot, d delta, observedAt time.Time) model.UsageEvent {
	return model.UsageEvent{
		Tool:                s.Tool,
		Model:               s.Model,
		SessionID:           s.SessionID,
		Project:             s.Project,
		EventTime:           observedAt,
		ObservedTime:        observedAt,
		InputTokens:         d.input,
		OutputTokens:        d.output,
		CacheCreationTokens: d.cacheCreation,
		CacheReadTokens:     d.cacheRead,
		ReasoningTokens:     d.reasoning,
		TotalTokens:         d.total,
		SourcePath:          s.SourcePath,
		DedupKey:            "agg|" + s.Tool + "|" + s.Key + "|" + strconv.FormatInt(observedAt.UnixNano(), 10),
		Kind:                model.KindUsage,
		Raw:                 s.Raw,
	}
}
