// Package store defines the persistence contract and reporting query types.
// The concrete implementation (sqlite.go) is backed by modernc.org/sqlite
// (pure Go, CGO_ENABLED=0). The schema (schema.sql) enforces append-only
// immutability of usage history via UNIQUE(dedup_key) + no-UPDATE/no-DELETE
// triggers — see PLAN.md.
package store

import (
	"context"
	"time"

	"aiusage/internal/model"
)

// Filter selects and groups usage for reporting.
type Filter struct {
	Since time.Time // inclusive lower bound on event_time (zero = open)
	Until time.Time // exclusive upper bound on event_time (zero = open)

	Tools    []string // restrict to these tools (empty = all)
	Models   []string // restrict to these models (empty = all)
	Projects []string // restrict to these projects (empty = all)
	Sessions []string // restrict to these sessions (empty = all)

	// GroupBy lists grouping dimensions, applied in order. Valid values:
	// "hour","day","week","month","tool","model","project","session".
	// Empty means a single grand-total bucket.
	GroupBy []string
}

// Bucket is one grouped row of summarised usage.
type Bucket struct {
	// Keys maps each GroupBy dimension to its value for this bucket
	// (e.g. {"day":"2026-05-29","tool":"codex"}). Ordered via OrderedKeys.
	Keys          map[string]string
	OrderedKeys   []string // dimension names in GroupBy order
	Events        int64
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	Reasoning     int64
	Total         int64
}

// Summary is the result of Summarize: grouped buckets plus a grand total.
type Summary struct {
	GroupBy []string
	Buckets []Bucket
	Totals  Bucket
}

// SourceStat summarises stored usage per tool for the `sources` command.
type SourceStat struct {
	Tool         string
	Models       []string
	Sessions     int64
	Events       int64
	Total        int64
	FirstEvent   time.Time
	LastEvent    time.Time
	LastObserved time.Time
}

// DBStats describes the database as a whole for the `doctor` command.
type DBStats struct {
	Path          string
	Events        int64
	Snapshots     int64
	DistinctTools int64
	DistinctModel int64
	SizeBytes     int64
	EarliestEvent time.Time
	LatestEvent   time.Time
	SchemaVersion int
}

// Store is the persistence interface used by the collector and reporting.
type Store interface {
	// InsertEvents appends usage events idempotently (INSERT OR IGNORE on
	// dedup_key) in a single transaction. Returns the count actually inserted
	// (i.e. new dedup keys). Never updates or deletes existing rows.
	InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error)

	// LastState returns the most recent observed counters for an aggregate
	// accumulator cell (tool, key), or nil if none exists yet. Drives the
	// monotonic-with-reset delta. This is mutable accumulator STATE, not
	// history — the immutable history lives in usage_events as the deltas.
	LastState(ctx context.Context, tool, key string) (*model.AggregateSnapshot, error)

	// UpsertState records the latest observed counters for (tool, key),
	// replacing any previous value (one row per cell).
	UpsertState(ctx context.Context, s model.AggregateSnapshot) error

	// Summarize aggregates usage matching Filter, grouped per Filter.GroupBy.
	Summarize(ctx context.Context, f Filter) (*Summary, error)

	// ListEvents returns raw events matching Filter (ordered by event_time).
	// Used by export.
	ListEvents(ctx context.Context, f Filter) ([]model.UsageEvent, error)

	// SourceStats returns per-tool stored stats.
	SourceStats(ctx context.Context) ([]SourceStat, error)

	// Stats returns whole-database diagnostics.
	Stats(ctx context.Context) (DBStats, error)

	// Close releases the database handle.
	Close() error
}
