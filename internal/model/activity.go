package model

import "time"

// ActivityKind marks what sort of invocation an ActivityEvent records. The
// vocabulary is closed and enforced by a CHECK constraint on activity_events:
// a new kind is a schema change, not a string an adapter may invent.
type ActivityKind string

const (
	// ActivityTool is one tool/function call the agent made (Bash, Read,
	// mcp__server__tool, codex's exec, opencode's bash, ...).
	ActivityTool ActivityKind = "tool"
	// ActivitySkill is a skill invocation. Claude Code models these as a tool
	// call named "Skill" whose input names the skill; the skill NAME is what
	// this row carries, so "which skill" is a group-by rather than a parse.
	ActivitySkill ActivityKind = "skill"
	// ActivityHook is a hook firing. It carries no usage of its own, so it
	// never attributes tokens.
	ActivityHook ActivityKind = "hook"
)

// ActivityEvent is one immutable observed AGENT ACTIVITY record: a tool call, a
// skill invocation or a hook firing. It is a sibling of UsageEvent, not a part
// of it — activity is not token accounting, and mixing the two would put rows
// that cost nothing into the ledger that answers "what did this cost".
//
// PRIVACY: this type is names and counts ONLY, by construction. There is no
// field for a tool's INPUT — no command string, no file path, no prompt, no
// leftover `.input` blob — and no raw payload of any kind, so `privacy.no_raw`
// has nothing to drop here. The one input field any adapter reads is the skill
// NAME of a Skill call, which is the fact being recorded rather than content.
// MCP tool names (mcp__server__tool) are names too and are kept verbatim: which
// server got called is the entire point.
//
// COST ATTRIBUTION. An activity row stores NO token counts. It carries
// UsageDedupKey, the dedup key of the usage_events row whose provider record
// contained this call, and CallsInTurn, how many calls shared that one usage
// object. Cost and tokens are derived on READ by joining to the ledger and
// dividing by CallsInTurn (see store.SummarizeActivity).
//
// That shape is the honest one. One assistant turn emits several tool_use
// blocks against a SINGLE usage object, so any per-call token count copied onto
// each row would multiply the turn's real cost by the number of calls in it.
// Storing the number once — in usage_events, where it already lives — and
// referencing it makes over-attribution structurally impossible rather than a
// rule someone has to keep remembering: the ledger stays the single source of
// cost truth, and the split is a documented read-time convention that can be
// changed without rewriting an append-only table.
type ActivityEvent struct {
	// ID is the activity row id (activity_events.id), set by listing queries and
	// left zero on a record that has not been read back from the store.
	ID   int64        `json:"-"`
	Tool string       // agent CLI id (ToolClaudeCode, ...)
	Kind ActivityKind // tool | skill | hook
	// Name is the invoked tool/skill/hook name, verbatim ("Bash", "Read",
	// "mcp__plugin__search", "artifact-design", "stop_hook_summary").
	Name string

	SessionID string    // provider session id
	Project   string    // workspace / cwd path
	Model     string    // model id of the turn, when the source names one
	EventTime time.Time // when the call happened (from the source)
	// ObservedTime is when the daemon read/stored the record.
	ObservedTime time.Time

	// UsageDedupKey is the dedup_key of the usage_events row this call is
	// attributable to, or "" when the source gives no join (codex records its
	// function calls and its token counts in unrelated records; hooks carry no
	// usage at all). It is deliberately NOT a foreign key: a call is an observed
	// fact even when its usage row was skipped as a poison row or predates
	// activity collection, and the read path left-joins, so a missing partner
	// contributes no cost instead of losing the call.
	UsageDedupKey string
	MessageID     string // provider message id (if any)
	RequestID     string // provider request id (if any)
	// TurnSeq is this call's 0-based index among the calls of its turn, and
	// CallsInTurn is how many calls that turn contained (>= 1). Together they
	// are the denominator and the tie-break of the read-time cost split.
	TurnSeq     int
	CallsInTurn int

	SourcePath string // file/db the record came from
	DedupKey   string // globally-unique stable key; inserts conflict-skip on this
}
