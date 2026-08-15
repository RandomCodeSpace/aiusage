// Package store defines the persistence contract and reporting query types.
// The concrete implementation (sqlite.go) is backed by modernc.org/sqlite
// (pure Go, CGO_ENABLED=0). The schema (schema.sql) enforces append-only
// immutability of usage history via UNIQUE(dedup_key) + no-UPDATE/no-DELETE
// triggers — see PLAN.md.
package store

import (
	"context"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
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
	// "hour","day","week","month","tool","model","provider","project",
	// "session". A provider bucket keyed by the empty string is the one
	// holding the rows whose source never named a billing provider.
	// Empty means a single grand-total bucket.
	GroupBy []string
}

// Bucket is one grouped row of summarised usage.
type Bucket struct {
	// Keys maps each GroupBy dimension to its value for this bucket
	// (e.g. {"day":"2026-05-29","tool":"codex"}). Ordered via OrderedKeys.
	Keys        map[string]string
	OrderedKeys []string // dimension names in GroupBy order
	Events      int64
	// Sessions is the distinct non-empty session_id count within the group,
	// computed store-level (COUNT DISTINCT) so callers never have to
	// materialize one bucket per session just to count them. Distinct counts
	// do not add across buckets.
	Sessions      int64
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	Reasoning     int64
	Total         int64
	// CostMicroUSD sums the costs stamped at collect time (millionths of USD).
	// Rows with no stamped cost contribute nothing to it — they are counted in
	// UnpricedEvents instead and must be display-priced separately, so a bucket
	// with UnpricedEvents > 0 is an UNDERSTATEMENT until that is folded in.
	CostMicroUSD int64
	// UnpricedEvents counts rows in the bucket whose cost_micro_usd is NULL
	// (collected before v3, or a model the pricing ladder could not price).
	UnpricedEvents int64
}

// UnpricedGroup aggregates the rows of one bucket that carry NO stamped cost,
// split by the attributes a price lookup needs (tool for the reasoning-billing
// rule, model/provider for the rate, service tier for the tier rate). Keys
// repeats the Filter.GroupBy dimension values so a caller can fold the
// display-priced result back into the matching Summarize bucket.
type UnpricedGroup struct {
	Keys        map[string]string
	OrderedKeys []string
	Tool        string
	Model       string
	Provider    string
	ServiceTier string

	Events        int64
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	Reasoning     int64
}

// Summary is the result of Summarize: grouped buckets plus a grand total.
type Summary struct {
	GroupBy []string
	Buckets []Bucket
	Totals  Bucket
}

// RollupSummary is the result of SummarizeRollup: the same buckets Summarize
// produces, from the derived rollup instead of the ledger, plus the range those
// buckets actually cover.
//
// Bucket.Sessions is always 0 here. The rollup keeps no session dimension, so
// a distinct-session count is not merely absent but underivable - reading the
// zero as "no sessions" would be a lie the shape cannot prevent, which is why
// this is a distinct type and not a Summary.
type RollupSummary struct {
	GroupBy []string
	Buckets []Bucket
	Totals  Bucket
	// Since and Until are the requested bounds snapped OUTWARD to whole UTC
	// 15-minute buckets - the rollup's resolution - so the buckets can cover
	// slightly more than was asked for. Zero means that bound was open. A
	// caller labelling a range must label these, not the ones it passed in.
	Since time.Time
	Until time.Time
}

// ListOption tunes what ListEvents projects. It exists so the expensive column
// is opt-in at the call site rather than a default every caller pays for.
type ListOption func(*listOptions)

type listOptions struct {
	includeRaw bool
	keyset     bool
	afterID    int64
	limit      int
}

// WithKeyset turns ListEvents into one page of a keyset walk: at most limit rows
// with a row id ABOVE afterID, ordered by id. Pass afterID 0 for the first page
// and the last id of a page for the next one.
//
// The order changes with the option, and it has to: ids are AUTOINCREMENT, so
// ordering by id is a total order that a cursor can resume from exactly, while
// the default (event_time, id) order interleaves later-ingested rows with
// earlier event times and would make an id cursor skip them. A caller that wants
// event-time order must page some other way, or not page at all.
//
// A limit of 0 or less means unlimited, which is the point of a cap living at
// the caller: the store enforces the walk, not the policy.
func WithKeyset(afterID int64, limit int) ListOption {
	return func(o *listOptions) {
		o.keyset = true
		o.afterID = afterID
		o.limit = limit
	}
}

// WithRaw restores the raw audit payload to a ListEvents projection. ONLY the
// export --include-raw path may pass it: raw can carry full transcript content
// for rows appended before the usage-object allow-list landed, and every other
// consumer has no use for it. Without it, raw never leaves the database.
func WithRaw() ListOption {
	return func(o *listOptions) { o.includeRaw = true }
}

// Applied reports what one ApplyObservation actually wrote: new dedup keys in
// each of the two append-only ledgers. Rows that collided on an existing dedup
// key count in neither.
type Applied struct {
	Events   int
	Activity int
	// TurnContexts counts new usage_turn_context rows: (turn, dimension) pairs
	// newly recorded as having run under something. ONE turn can contribute up
	// to five of them, one per dimension, so this counts rows and not turns. A
	// pair already carrying a context counts here no more than a duplicate event
	// counts in Events.
	TurnContexts int
}

// ObservationBatch is everything one read of one source commits together. It is
// a struct rather than a parameter list because the set grows: activity joined
// events in v5, skill contexts in v6, all five turn-context dimensions in v7,
// and each addition must land in the SAME transaction as the checkpoint that
// gates its re-read, not in a second call that a crash could separate from the
// first.
type ObservationBatch struct {
	Events   []model.UsageEvent
	Activity []model.ActivityEvent
	// TurnContexts records what each usage event ran UNDER: its subagent, its
	// skill, its MCP tool and server, its plugin. At most one value per (event,
	// dimension), keyed by the event's dedup key — see model.TurnContext.
	TurnContexts []model.TurnContext
	// Checkpoint, when non-nil, is upserted in the same transaction.
	Checkpoint *model.SourceCheckpoint
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
	SchemaVersion int // version recorded in the database, not the binary's
}

// Store is the persistence interface used by the collector and reporting.
type Store interface {
	// InsertEvents appends usage events idempotently (INSERT .. ON
	// CONFLICT(dedup_key) DO NOTHING — a blanket OR IGNORE would also swallow
	// CHECK violations) in a single transaction. Returns the count actually
	// inserted (i.e. new dedup keys). Never updates or deletes existing rows.
	// A row whose own insert fails (CHECK violation, empty dedup key) is
	// skipped and reported via the returned error while the rest of the batch
	// commits, so the count is meaningful even when the error is non-nil.
	InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error)

	// LastState returns the most recent observed counters for an aggregate
	// accumulator cell (tool, key), or nil if none exists yet. Drives the
	// monotonic-with-reset delta. This is mutable accumulator STATE, not
	// history — the immutable history lives in usage_events as the deltas.
	LastState(ctx context.Context, tool, key string) (*model.AggregateSnapshot, error)

	// UpsertState records the latest observed counters for (tool, key),
	// replacing any previous value (one row per cell).
	UpsertState(ctx context.Context, s model.AggregateSnapshot) error

	// ApplySnapshot atomically appends an aggregate cell's delta events and
	// records its new accumulator state in ONE transaction, so a crash can
	// never persist the events without the state (the next cycle would
	// re-derive the same delta under a fresh dedup key — a permanent double
	// count). When events is non-empty but every dedup key already exists,
	// the state write is skipped: an unchanged baseline lets the next poll
	// re-derive the colliding delta instead of dropping it. A non-nil cp is
	// upserted under the same condition and in the same transaction, so a
	// checkpoint can never claim data whose state write was skipped or
	// rolled back. Returns the number of events actually inserted.
	ApplySnapshot(ctx context.Context, events []model.UsageEvent, state model.AggregateSnapshot, cp *model.SourceCheckpoint) (int, error)

	// Checkpoint returns the stored incremental-collection state for a
	// (tool, source path), or nil when none exists.
	Checkpoint(ctx context.Context, tool, sourcePath string) (*model.SourceCheckpoint, error)

	// ApplyEvents appends usage events (same idempotent semantics as
	// InsertEvents) and upserts the source checkpoint in ONE transaction. A
	// checkpoint persisted outside the event transaction could outrun the
	// events it claims — a crash between the two commits would then skip
	// data forever. A nil cp degrades to a plain event insert; empty events
	// with a non-nil cp writes just the checkpoint.
	ApplyEvents(ctx context.Context, events []model.UsageEvent, cp *model.SourceCheckpoint) (int, error)

	// ApplyObservation appends usage events, appends agent activity rows, and
	// upserts the source checkpoint — all in ONE transaction. It is what a
	// collection cycle calls; ApplyEvents is the events-only shorthand for it.
	//
	// The single transaction is the point. Activity rows reference usage rows
	// by dedup key, and the checkpoint gates the re-read of both: splitting
	// them across transactions would let a crash advance the checkpoint past
	// activity that never landed, losing it permanently, or leave a call
	// pointing at a usage row that rolled back. Activity is appended AFTER the
	// events so the row it names already exists.
	//
	// Activity has the same idempotence as events (ON CONFLICT(dedup_key) DO
	// NOTHING) and the same per-row skip behaviour, so the returned counts stay
	// meaningful when the error is non-nil.
	ApplyObservation(ctx context.Context, events []model.UsageEvent, activity []model.ActivityEvent, cp *model.SourceCheckpoint) (Applied, error)

	// ApplyBatch is ApplyObservation plus the turn contexts, and is what a
	// collection cycle actually calls; ApplyObservation is the shorthand that
	// omits them. Same single transaction, same ordering (events first, so the
	// rows naming them by dedup key find them present), same idempotence: a turn
	// context conflicts on (usage dedup key, dimension) and does nothing, which
	// is what stops a re-read serving a turn's cost twice.
	ApplyBatch(ctx context.Context, b ObservationBatch) (Applied, error)

	// Summarize aggregates usage matching Filter, grouped per Filter.GroupBy.
	Summarize(ctx context.Context, f Filter) (*Summary, error)

	// UnpricedGroups returns the token totals of the matching rows that have
	// NO stamped cost, grouped by Filter.GroupBy plus (tool, model, provider,
	// service_tier). It exists so cost surfaces can value historical and
	// unpriceable rows from the CURRENT price table without materialising one
	// bucket per event. An empty result means every matching row is stamped.
	UnpricedGroups(ctx context.Context, f Filter) ([]UnpricedGroup, error)

	// SummarizeRollup answers the same question as Summarize from the derived
	// rollup: identical numbers, without scanning the ledger. It cannot serve
	// session or provider dimensions, distinct-session counts, or ranges finer
	// than its 15-minute bucket - those stay with Summarize. See
	// SQLite.SummarizeRollup for the exact differences.
	SummarizeRollup(ctx context.Context, f Filter) (*RollupSummary, error)

	// EnsureRollup rebuilds the derived rollup when it disagrees with the
	// ledger (an empty one just created by migration, or one that missed a
	// write), and reports whether it rebuilt. The collector calls it before a
	// pass; it is cheap when the rollup is in step.
	EnsureRollup(ctx context.Context) (bool, error)

	// RebuildRollup rebuilds the derived rollup from usage_events
	// unconditionally, in one transaction. The rollup is derived data: this is
	// always a safe repair, and it is the only direction a disagreement is
	// ever resolved in.
	RebuildRollup(ctx context.Context) error

	// ListEvents returns events matching Filter, ordered by event_time then
	// row id. The projection names its columns and EXCLUDES raw; pass WithRaw
	// to include it (export --include-raw only). Used by export.
	ListEvents(ctx context.Context, f Filter, opts ...ListOption) ([]model.UsageEvent, error)

	// SummarizeActivity aggregates agent activity (tool calls, skill
	// invocations, hook firings) matching ActivityFilter, grouped per its
	// GroupBy, with tokens and cost ATTRIBUTED from the ledger: each call takes
	// its joined usage row's counts divided by the number of calls that shared
	// that turn. The division is integer and every operand non-negative, so the
	// attributed total over any window is at most the same window's
	// usage_events total — the split can understate, never inflate.
	SummarizeActivity(ctx context.Context, f ActivityFilter) (*ActivitySummary, error)

	// TopActivity ranks SummarizeActivity's buckets by one metric and caps them
	// at limit (0 = uncapped), ordering and limiting in SQL. It answers "which
	// skill is expensive" in one query. It needs at least one GroupBy
	// dimension: ranking a single grand-total bucket is not a ranking.
	TopActivity(ctx context.Context, f ActivityFilter, by ActivityOrder, limit int) ([]ActivityBucket, error)

	// SummarizeTurnContext aggregates what the turns that ran UNDER each value of
	// ONE dimension actually cost, grouped per ActivityFilter.GroupBy (which
	// accepts "value" and the queried dimension's own name, and refuses the
	// call-level "kind"/"name"). This is the real answer to "which skill/agent
	// is expensive": TopActivity ranks the turn that INVOKED a skill, which is
	// one call, while the skill's own work is every turn that followed under it,
	// and for a subagent there is no invoking call in the ledger at all.
	//
	// Unlike the activity queries it does NOT divide. Within one dimension each
	// usage event has at most one context and the join is 1:1, so a bucket's
	// cost is the full ledger cost of its turns and the buckets partition the
	// window without overlap. It also counts turns that called no tool at all,
	// which the activity ledger has no row for.
	//
	// THE DIMENSION IS A REQUIRED ARGUMENT, NOT A FILTER. The five dimensions
	// plus activity_events' tool-call attribution are SIX PARTITIONS OF THE SAME
	// DOLLARS: a turn commonly carries three or four contexts at once, each
	// naming its full cost, so a query spanning two of them reports the same
	// tokens twice. An unknown dimension is refused, grouping by "dimension" is
	// refused, and grouping by a dimension OTHER than the queried one is
	// refused — the mixing is unexpressible rather than discouraged.
	SummarizeTurnContext(ctx context.Context, dim model.TurnDimension, f ActivityFilter) (*TurnContextSummary, error)

	// TopTurnContext ranks SummarizeTurnContext's buckets by one metric and caps
	// them at limit (0 = uncapped), ordering and limiting in SQL. It needs at
	// least one GroupBy dimension. ActivityByCalls ranks by turns here.
	TopTurnContext(ctx context.Context, dim model.TurnDimension, f ActivityFilter, by ActivityOrder, limit int) ([]TurnContextBucket, error)

	// SummarizeSkillCost and TopSkillCost are the skill dimension's pre-existing
	// spellings, delegating to the two above with model.DimensionSkill. They are
	// kept because they read better at a call site that only wants skills; they
	// are delegations rather than implementations, so there remains exactly one
	// query builder and one table behind every per-skill number.
	SummarizeSkillCost(ctx context.Context, f ActivityFilter) (*SkillCostSummary, error)
	TopSkillCost(ctx context.Context, f ActivityFilter, by ActivityOrder, limit int) ([]SkillCostBucket, error)

	// SourceStats returns per-tool stored stats.
	SourceStats(ctx context.Context) ([]SourceStat, error)

	// Stats returns whole-database diagnostics.
	Stats(ctx context.Context) (DBStats, error)

	// Close releases the database handle.
	Close() error
}
