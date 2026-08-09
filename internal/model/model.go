// Package model holds the core domain types shared by adapters, storage,
// the collector and reporting. It depends on nothing else in the project.
package model

import "time"

// SourceClass distinguishes how a source exposes usage data.
type SourceClass string

const (
	// EventLevel sources expose discrete, individually-identifiable usage
	// records (one per API request / message). They deduplicate cleanly via a
	// stable DedupKey and are immune to later file deletion once stored.
	EventLevel SourceClass = "event"
	// Aggregate sources expose only running/cumulative counters. The collector
	// snapshots them and materialises positive deltas as synthetic events using
	// a monotonic-with-reset accumulator (see PLAN.md).
	Aggregate SourceClass = "aggregate"
)

// Tool identifiers — the "tool" categorisation dimension (which agent CLI).
const (
	ToolClaudeCode = "claude-code"
	ToolCodex      = "codex"
	ToolCopilot    = "copilot"
	ToolOpenCode   = "opencode"
	ToolHermes     = "hermes"
	ToolGemini     = "gemini"
	ToolAgy        = "agy"
)

// EventKind marks a normal usage record vs an appended correction. History is
// never rewritten; corrections are appended as KindAdjustment rows.
type EventKind string

const (
	KindUsage      EventKind = "usage"
	KindAdjustment EventKind = "adjustment"
)

// UsageEvent is one immutable observed usage record. Stored append-only and
// deduplicated on DedupKey. All token counts are non-negative.
type UsageEvent struct {
	Tool      string    // agent CLI id (ToolClaudeCode, ...) — categorisation dim
	Model     string    // model id — categorisation dim
	SessionID string    // provider session id
	Project   string    // workspace / cwd path
	EventTime time.Time // when the usage actually occurred (from the source)
	// ObservedTime is when the daemon read/stored the record. For aggregate
	// deltas (no real event time) EventTime is set equal to ObservedTime.
	ObservedTime time.Time

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	ReasoningTokens     int64 // optional subset of output (e.g. codex)
	// TotalTokens is provider-authoritative; each adapter sets it correctly for
	// its provider's accounting (cache tokens are separate for Anthropic but a
	// subset of input for OpenAI/codex — adapters must not double count).
	TotalTokens int64

	RequestID  string // provider request id (if any)
	MessageID  string // provider message id (if any)
	SourcePath string // file/db the record came from
	DedupKey   string // globally-unique stable key; inserts conflict-skip on this
	Kind       EventKind
	// Raw is the raw provider usage payload (optional, for audit). It can carry
	// full transcript content, so it is never marshalled by default; export
	// restores it only behind the explicit --include-raw flag.
	Raw string `json:"-"`
}

// ComputedTotal sums the token components using Anthropic-style accounting
// (cache tokens additive). Adapters that lack a provider total may use it.
func (e UsageEvent) ComputedTotal() int64 {
	return e.InputTokens + e.OutputTokens + e.CacheCreationTokens + e.CacheReadTokens
}

// AggregateSnapshot is one observation of a source's cumulative/growing
// counters for a single accumulator cell. The collector compares the new
// snapshot against the last stored state for the same (Tool, Key) to derive a
// positive delta (monotonic-with-reset), which it materialises as one immutable
// usage event. Used by sources whose per-record totals GROW between polls:
//   - hermes  — per-session running totals; Key = session_id
//   - gemini  — per-turn cumulative snapshots; Key = sourcePath + "|" + turn id
//   - agy     — same shape as gemini once Antigravity emits usage
//
// Key is the accumulator identity (must be stable across polls and unique per
// growing cell). SessionID/Model/Project are the reportable attributes carried
// onto the synthetic event.
type AggregateSnapshot struct {
	Tool         string
	Key          string
	Model        string
	SessionID    string
	Project      string
	ObservedTime time.Time

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	ReasoningTokens     int64
	TotalTokens         int64

	SourcePath string
	Raw        string
}

// SourceCheckpoint is mutable per-source incremental-collection state, keyed by
// (Tool, SourcePath). It lets an adapter skip or tail-read a source whose
// content is unchanged or append-only since the last cycle. It is NOT history:
// losing a checkpoint only costs a full re-read, never data (event dedup keys
// and aggregate_state make re-reads idempotent). The dangerous direction is a
// checkpoint that outruns its data — which is why the store persists it in the
// same transaction as the events it accounts for.
type SourceCheckpoint struct {
	Tool       string
	SourcePath string
	Size       int64  // file size in bytes at the last completed read
	MTimeNS    int64  // file mtime (unix nanoseconds) at the last completed read
	Offset     int64  // byte offset consumed (append-only JSONL tail reads)
	Watermark  int64  // max rowid consumed (database sources)
	State      string // adapter-specific JSON (baselines, manifests, gates)
}
