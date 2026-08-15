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

// SkillContext records that ONE usage event was produced while the agent was
// operating inside a skill. It answers "what did skill X cost", which the
// activity ledger alone cannot: an ActivityEvent of kind=skill records the turn
// that INVOKED a skill, not the thousands of turns the skill then went on to
// spend. On this machine's transcripts those are 44 invocation rows against
// 8,039 records of actual work.
//
// SKILL CONTEXT IS A PROPERTY OF THE TURN, NOT A CALL WITHIN IT. That single
// distinction is why this is a separate record type rather than another
// ActivityEvent kind, and it is worth stating precisely.
//
// An ActivityEvent is one CALL. A turn emits several of them against a single
// usage object, so each takes a divided share (calls_in_turn) and the shares sum
// back to the turn. A skill context is not a call — it is the answer to "which
// skill was running when this turn happened", and a turn has AT MOST ONE. The
// source enforces that: Claude Code's `attributionSkill` is a scalar string on
// the record, so a usage row cannot carry two. Measured over 209,435 local
// transcript records, 8,039 carry the field, every one of them a plain string,
// and no (session, request) turn ever carried two distinct values.
//
// Therefore the cost of a skill is the sum over DISTINCT usage rows whose
// context is that skill, with NO division and no divisor to share. Over-
// attribution is not prevented by a rule here, it is unrepresentable: the store
// keys these rows by UsageDedupKey uniquely, so the join to the ledger is 1:1
// and cannot multiply a row's cost.
//
// It is deliberately NOT an ActivityEvent kind. Tool-call attribution and skill-
// context attribution are two different PARTITIONS of the same dollars — "which
// tool was called" and "which skill was running" — in the way cost-by-region and
// cost-by-product are two views of one budget. They are each honest alone and
// meaningless added together. Sharing activity_events would have put that
// mistake one forgotten WHERE clause away: SummarizeActivity grouped by tool,
// with no kind filter, would have silently counted every skill turn twice. A
// separate table makes the sum unexpressible rather than merely discouraged.
//
// NESTING is real and is recorded shallowly, because that is all the source
// offers. A skill may invoke another skill (5 such records locally, e.g.
// superpowers:brainstorming invoking superpowers:writing-plans), but once the
// inner one is running the field names ONLY the inner skill. So cost is
// attributed to the INNERMOST active skill, and an outer skill is not credited
// with what its callee spent. That is the source's own accounting, not a choice
// made here, and inventing a parent chain the transcript does not record would
// be a guess wearing a number.
//
// PRIVACY: the skill NAME and nothing else. There is no field for the skill's
// arguments, its inputs, its prompt or its output — the same allow-list
// discipline ActivityEvent follows, for the same reason.
type SkillContext struct {
	// UsageDedupKey is the dedup_key of the usage_events row this context
	// describes. It is the identity of the record: one usage row, one context.
	UsageDedupKey string
	Tool          string // agent CLI id (ToolClaudeCode, ...)
	// Skill is the skill the turn ran inside, verbatim ("adhd",
	// "superpowers:writing-plans"). Never empty — a context with no skill is
	// not a context.
	Skill string

	SessionID string    // provider session id
	Project   string    // workspace / cwd path
	Model     string    // model id of the turn
	EventTime time.Time // the usage event's own time, copied so both ledgers
	// place the turn in the same instant
	ObservedTime time.Time // when the daemon read/stored the record
	SourcePath   string    // file/db the record came from
}
