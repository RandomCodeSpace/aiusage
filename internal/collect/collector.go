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

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// nowFn yields the cycle's observation timestamp. It is a package-level seam so
// tests can simulate polls at distinct instants (in production, real polls are
// minutes apart). Always returns UTC.
var nowFn = func() time.Time { return time.Now().UTC() }

// CycleStats reports the outcome of a single RunCycle.
type CycleStats struct {
	Adapters       int      // adapters iterated
	Sources        int      // sources discovered + collected
	SourcesFailed  int      // sources that recorded errors and stored nothing
	EventsInserted int      // new dedup keys actually written (event + synthetic)
	EventsSeen     int      // observed event-level records (pre-dedup)
	Snapshots      int      // aggregate snapshots observed
	Errors         []string // non-fatal per-adapter / per-source errors
}

// AllFailed reports whether the cycle produced only errors: every discovered
// source failed outright, or discovery itself yielded nothing but errors.
// `once` maps this to its nonzero exit for cron use; partial failure stays 0.
func (s CycleStats) AllFailed() bool {
	return len(s.Errors) > 0 && s.SourcesFailed == s.Sources
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
			errsBefore := len(stats.Errors)
			progressed := false

			obs, err := ad.Collect(ctx, src)
			if err != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("collect %s %s: %v", ad.ID(), src.Path, err))
				// Best-effort: a bad source must not abort the cycle. Still
				// process whatever observations were returned.
			}

			// InsertEvents commits per-row: a non-nil error may accompany a
			// partial insert (skipped poison rows), so n counts regardless.
			n, sErr := storeEvents(ctx, st, obs.Events, observedAt)
			stats.EventsSeen += len(obs.Events)
			stats.EventsInserted += n
			if sErr != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("insert events %s %s: %v", ad.ID(), src.Path, sErr))
			}
			if n > 0 || (sErr == nil && len(obs.Events) > 0) {
				progressed = true
			}

			for _, s := range obs.Snapshots {
				stats.Snapshots++
				n, sErr := storeSnapshot(ctx, st, s, observedAt)
				if sErr != nil {
					stats.Errors = append(stats.Errors, fmt.Sprintf("snapshot %s %s: %v", s.Tool, s.Key, sErr))
					continue
				}
				stats.EventsInserted += n
				progressed = true
			}

			if len(stats.Errors) > errsBefore && !progressed {
				stats.SourcesFailed++
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
// aggregate cell as a synthetic immutable event and records the new state in
// one atomic store operation — a crash between the two would re-derive the
// same delta next cycle under a fresh dedup key, double counting it forever.
// A dedup collision (two cycles at the same observed instant) is handled by
// ApplySnapshot, which keeps the old baseline so the delta is re-derivable.
// Returns the number of new dedup keys inserted (0 or 1).
func storeSnapshot(ctx context.Context, st store.Store, s model.AggregateSnapshot, observedAt time.Time) (int, error) {
	last, err := st.LastState(ctx, s.Tool, s.Key)
	if err != nil {
		return 0, fmt.Errorf("last state: %w", err)
	}

	d := snapshotDelta(last, s)

	var events []model.UsageEvent
	if d.hasPositive() {
		events = []model.UsageEvent{syntheticEvent(s, d, observedAt)}
	}

	// State advances even without a positive delta, so subsequent polls diff
	// against the latest counters.
	s.ObservedTime = observedAt
	n, err := st.ApplySnapshot(ctx, events, s)
	if err != nil {
		return 0, fmt.Errorf("apply snapshot: %w", err)
	}
	return n, nil
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
// delta. EventTime is the adapter-populated per-record time when present
// (gemini/agy record real turn timestamps) so a downtime gap's delta does not
// land as a spike at the restart second; it falls back to the cycle instant.
// The DedupKey stays on the cycle instant at nanosecond resolution so distinct
// polls never collide even when the record timestamp is unchanged; an
// exact-instant collision (frozen clock) is handled by the store's
// ApplySnapshot, which refuses to advance state when the insert was fully
// ignored.
func syntheticEvent(s model.AggregateSnapshot, d delta, observedAt time.Time) model.UsageEvent {
	eventTime := s.ObservedTime
	if eventTime.IsZero() {
		eventTime = observedAt
	}
	return model.UsageEvent{
		Tool:                s.Tool,
		Model:               s.Model,
		SessionID:           s.SessionID,
		Project:             s.Project,
		EventTime:           eventTime,
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
