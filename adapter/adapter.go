// Package adapter defines the read-only interface every agent-CLI integration
// implements, plus the registry that wires them together.
//
// CRITICAL: adapters are strictly observational. They MUST only read
// already-produced local files/DBs. They must never write, modify, lock for
// writing, rotate, or otherwise influence agent files. Open SQLite sources
// read-only (immutable=1 / mode=ro) so a poll can never disturb the agent.
package adapter

import (
	"context"

	"github.com/RandomCodeSpace/aiusage/model"
)

// Source is a concrete usage source discovered by an adapter — typically a
// file, directory, or database belonging to one agent CLI.
type Source struct {
	Tool  string            // owning tool id (model.ToolXxx)
	Class model.SourceClass // EventLevel or Aggregate
	Path  string            // primary path (file or db)
	Label string            // human-friendly label for `sources`
	Meta  map[string]string // adapter-specific extras (e.g. session id, dir)
}

// MetaNoUsage marks a discovered source that carries NO token usage — activity
// or turn context only.
//
// It exists because "does this tool have a data source" is asked by surfaces
// that mean TOKENS: doctor decides whether to print a tool's enablement
// checklist from it, and the TUI's By-Tool footnote states whether a token
// source exists at all (issue #44). Copilot discovers one session-state source
// per session for its skills and hooks while its tokens come only from an
// OPT-IN OTEL export, so counting those would tell a user with the export
// switched off that their token source is present and would suppress the
// checklist that is the only way to turn it on. The value is unread; presence
// of the key is the mark.
const MetaNoUsage = "no_usage"

// CarriesUsage reports whether this source can produce usage events. Sources
// say yes unless they mark themselves otherwise, so an adapter that never
// thinks about it behaves exactly as before.
func (s Source) CarriesUsage() bool {
	if s.Meta == nil {
		return true
	}
	_, marked := s.Meta[MetaNoUsage]
	return !marked
}

// CountUsageSources returns how many of these sources can produce usage events.
func CountUsageSources(srcs []Source) int {
	n := 0
	for _, s := range srcs {
		if s.CarriesUsage() {
			n++
		}
	}
	return n
}

// Observation is the result of reading a Source once. EventLevel adapters fill
// Events; Aggregate adapters fill Snapshots. An adapter may return both.
type Observation struct {
	Events    []model.UsageEvent
	Snapshots []model.AggregateSnapshot
	// Activity is the agent ACTIVITY observed in the same read: which tool was
	// called, which skill was invoked, which hook fired. It is a second,
	// independent output stream, not a derivative of Events — a source may
	// report calls it reports no usage for, and vice versa.
	//
	// An adapter that can tie a call to the usage record it rode in on MUST set
	// ActivityEvent.UsageDedupKey to that event's DedupKey and CallsInTurn to
	// the number of calls sharing it; that pairing is the whole cost
	// attribution, and it is only sound when both come from the SAME provider
	// record. An adapter that cannot must leave the key empty rather than
	// guess: a timestamp-nearest match would invent an attribution the source
	// does not support.
	//
	// PRIVACY: names and counts only. Never put a tool's input anywhere in
	// here — model.ActivityEvent has no field that would hold one.
	Activity []model.ActivityEvent
	// TurnContexts records what each observed usage event was produced UNDER —
	// which subagent, which skill, which MCP tool and server, which plugin. It
	// is a third independent stream and a property of the TURN, not of a call:
	// at most one value per (usage event, dimension), keyed by that event's
	// DedupKey, never divided among anything.
	//
	// One usage event may appear here several times, once per DIMENSION, and
	// each of those rows names the turn's FULL cost because each answers a
	// different question. They are partitions, not shares. A consumer must pin
	// exactly one dimension per query; summing across them counts the same
	// tokens once per context the turn carried. See model.TurnContext.
	//
	// An adapter MUST only emit one for a usage event it is also emitting in
	// Events, and must leave the stream empty when its source records no such
	// thing rather than inferring a context from adjacency — "the last skill
	// call I saw" is a guess that would keep charging a skill long after it
	// returned.
	//
	// PRIVACY: the NAME only — agent type, skill, MCP server/tool, plugin.
	// Never inputs, arguments, prompts or results; model.TurnContext has no
	// field that would hold one.
	TurnContexts []model.TurnContext
	// Checkpoint, when non-nil, is the source's new incremental state and MUST
	// only be set once the read completed (a partial read that advances the
	// checkpoint would skip the unread remainder forever). The collector
	// persists it in the same transaction as this observation's data; nil
	// leaves any stored checkpoint untouched.
	Checkpoint *model.SourceCheckpoint
}

// DiscoverConfig carries discovery roots and per-tool path overrides.
type DiscoverConfig struct {
	Home      string            // user home directory
	Overrides map[string]string // tool id -> explicit root path (optional)
}

// Root returns the discovery root for a tool: an explicit override if present,
// otherwise the user's home directory.
func (c DiscoverConfig) Root(tool, def string) string {
	if c.Overrides != nil {
		if v, ok := c.Overrides[tool]; ok && v != "" {
			return v
		}
	}
	if def != "" {
		return def
	}
	return c.Home
}

// Adapter reads one agent CLI's local usage data. Implementations MUST be
// read-only and must tolerate missing/partial/corrupt files without erroring
// the whole collection cycle (return best-effort results + a non-fatal error).
type Adapter interface {
	// ID is the stable tool identifier (model.ToolXxx).
	ID() string
	// DisplayName is the human-friendly name ("Claude Code").
	DisplayName() string
	// Discover locates sources under the configured roots. Read-only.
	Discover(ctx context.Context, cfg DiscoverConfig) ([]Source, error)
	// Collect reads a single source and returns its observations. Read-only.
	Collect(ctx context.Context, src Source) (Observation, error)
	// Capabilities declares what this project can actually say about the tool:
	// where a cost figure came from, whether a tool call can be joined to the
	// turn that paid for it, how the source reports reasoning tokens, and how
	// well the adapter is verified. Tool MUST equal ID(), and every field MUST
	// be set — a surface renders an empty field as an empty line, which reads as
	// a rendering fault rather than as a missing fact.
	//
	// It is a REQUIRED method rather than a table somewhere else because the
	// declaration is a statement about this code, and a statement kept beside
	// the code it describes cannot drift from it. A sixteenth adapter does not
	// compile until it declares itself, which beats a guard test reminding
	// someone to edit a map (issue #72, decision 1). The value type lives in
	// model so the dashboard reads it without importing this package.
	//
	// Reasoning is filled from model.ReasoningReportFor rather than restated:
	// there is ONE table of reasoning behaviour, and the pricing engine and this
	// declaration must never disagree about what a source reports.
	Capabilities() model.ToolCapability
}

// Incremental is an optional Adapter capability: given the checkpoint stored
// after the previous cycle, the adapter may skip unchanged data and read only
// what is new, returning the updated checkpoint on the Observation. A nil
// checkpoint (first cycle, or checkpoint lost) MUST behave exactly like
// Collect — a full read. Correctness never depends on the checkpoint; it is
// purely a work-avoidance gate.
type Incremental interface {
	CollectIncremental(ctx context.Context, src Source, cp *model.SourceCheckpoint) (Observation, error)
}

// Registry holds the set of available adapters.
type Registry struct{ adapters []Adapter }

// NewRegistry builds a registry from the given adapters.
func NewRegistry(as ...Adapter) *Registry { return &Registry{adapters: as} }

// All returns every registered adapter.
func (r *Registry) All() []Adapter { return r.adapters }

// Get returns the adapter for id, if registered.
func (r *Registry) Get(id string) (Adapter, bool) {
	for _, a := range r.adapters {
		if a.ID() == id {
			return a, true
		}
	}
	return nil, false
}
