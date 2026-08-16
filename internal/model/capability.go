package model

// capability.go declares, per tool, what this project can actually say about it:
// where a cost figure came from, whether a tool call can be joined to the turn
// that paid for it, how the source reports reasoning tokens, and how well the
// adapter behind it is verified. It is the "capability declaration" of
// CONTEXT.md, in machine-readable form.
//
// IT LIVES IN model AND NOT IN adapter. The dashboard is what shows it, and
// internal/tui may not import internal/adapter (layering: model < adapter,
// store < collect, report, tui). model is the shared floor both sides already
// stand on, which is the same reason FormatCost and the reasoning modes are
// here.
//
// The consequence is that this table is a SECOND statement of facts whose first
// statement is the adapter source, so it can drift. Two things hold it down: the
// reasoning column is DERIVED from reasoningModes rather than restated (there is
// one table of reasoning behaviour, not two), and internal/cmd — the composition
// root, the one package that sees both the registry and this file — fails its
// build when a registered adapter has no entry here
// (TestEveryAdapterDeclaresCapabilities).
//
// Every entry below was read off the adapter source, not off prose. The
// citations are in the per-field comments.

// CostProvenance for a whole tool is declared here; PriceProvenance in
// provenance.go answers the same question for one stored row. A tool declared
// CostVendor is one whose adapter stamps a cost — its rows carry that stamp and
// collect.stampCost is forbidden from overwriting it. A tool declared
// CostComputed has no such adapter code at all, so every row it produces is
// valued from the public rate card or left unpriced.

// ActivityCapture says what a tool's adapter can tell you about its tool calls.
type ActivityCapture string

const (
	// ActivityExact: the adapter emits activity rows AND names the usage row
	// that paid for each call, so the call's share of the turn is derivable.
	ActivityExact ActivityCapture = "exact join"
	// ActivityUnattributed: the adapter emits activity rows but the source gives
	// it no handle onto the usage record, so every call's cost is UNKNOWN — not
	// zero. Codex's token_count records share no identity with its call records;
	// copilot's execute_tool span is a sibling of the chat span, not its parent;
	// goose's usage_ledger rows carry no message id.
	ActivityUnattributed ActivityCapture = "recorded, unattributed"
	// ActivityNone: the adapter emits no activity rows at all. Its surface
	// exposes usage and nothing else.
	ActivityNone ActivityCapture = "none"
)

// ReasoningReport says how a tool's source reports reasoning tokens. It is the
// display face of reasoningModes plus the one state that map expresses by
// ABSENCE: a source with no reasoning counter at all.
type ReasoningReport string

const (
	// ReasoningReportSubset: reasoning is already inside the output count.
	ReasoningReportSubset ReasoningReport = "subset"
	// ReasoningReportAdditive: reasoning is reported beside output, not in it.
	ReasoningReportAdditive ReasoningReport = "additive"
	// ReasoningReportNone: the source carries no reasoning counter, so there is
	// no relationship to state. Distinct from "subset": subset is a claim about
	// a number that exists.
	ReasoningReportNone ReasoningReport = "not reported"
)

// VerificationTier is how well the adapter behind a tool is verified, in
// CONTEXT.md's vocabulary.
type VerificationTier string

const (
	// TierLive: verified against sessions actually run on a real install.
	TierLive VerificationTier = "live"
	// TierFixture: the surface format comes from a trusted source and is
	// verified against constructed fixtures, not against a real log.
	TierFixture VerificationTier = "fixture"
)

// ToolCapability is one tool's declaration.
type ToolCapability struct {
	Tool      string
	Cost      CostProvenance
	Activity  ActivityCapture
	Reasoning ReasoningReport
	Tier      VerificationTier
}

// toolCapabilities is the table. Reasoning is deliberately absent from the
// literals: CapabilityFor fills it from reasoningModes, so the pricing engine
// and this display can never disagree about what a tool reports.
//
// Cost: CostVendor is exactly the four adapters that call SetCost —
// copilot (copilot/cost.go PriceSourceAIU), crush (crush/crush.go
// PriceSourceReported), goose (goose/goose.go priceSource) and pi's two tools
// (pi/pi.go, a.tool+"-reported"). Every other adapter leaves CostMicroUSD nil
// and the row is priced by the ladder in internal/collect, or not at all.
//
// Activity: ActivityExact is the adapters that set UsageDedupKey on the rows
// they emit — claudecode (claudecode.go mintActivity), clinecli
// (clinecli.go buildActivity), dsh (dsh.go), opencode (opencode.go
// collectActivity) and pi (pi.go calls). ActivityUnattributed is the three that
// emit rows and never set it — codex (codex.go parseCallLine), copilot
// (copilot/activity.go and copilot/events.go) and goose (goose/activity.go,
// which hardcodes ""). The rest reference model.ActivityEvent nowhere.
var toolCapabilities = []ToolCapability{
	{Tool: ToolClaudeCode, Cost: CostComputed, Activity: ActivityExact, Tier: TierLive},
	{Tool: ToolCodex, Cost: CostComputed, Activity: ActivityUnattributed, Tier: TierLive},
	{Tool: ToolCopilot, Cost: CostVendor, Activity: ActivityUnattributed, Tier: TierLive},
	{Tool: ToolOpenCode, Cost: CostComputed, Activity: ActivityExact, Tier: TierLive},
	// hermes is the weakest "live" claim in this table: no local session has
	// ever been read on this machine (see the reasoningModes note on issue #28).
	// It is declared live because the adapter was written against a real
	// deployment; demote it to TierFixture the moment that stops being true.
	{Tool: ToolHermes, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
	// gemini is RETIRED — Antigravity replaced the Gemini CLI and no adapter
	// collects it (see the ToolGemini comment). The entry stays because
	// usage_events is append-only and stored rows still carry the id, so the
	// dashboard still has to describe where their numbers came from.
	{Tool: ToolGemini, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
	{Tool: ToolAgy, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
	{Tool: ToolPi, Cost: CostVendor, Activity: ActivityExact, Tier: TierLive},
	{Tool: ToolOpenClaw, Cost: CostVendor, Activity: ActivityExact, Tier: TierLive},
	// crush is cost-ONLY: one event per growth of sessions.cost, zero tokens.
	{Tool: ToolCrush, Cost: CostVendor, Activity: ActivityNone, Tier: TierLive},
	{Tool: ToolKimiCode, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
	{Tool: ToolReasonix, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
	{Tool: ToolDSH, Cost: CostComputed, Activity: ActivityExact, Tier: TierLive},
	{Tool: ToolQwenCode, Cost: CostComputed, Activity: ActivityNone, Tier: TierLive},
	// goose leaves a NULL cost unpriced rather than stamping 0, so a goose row
	// is either vendor-valued or unpriced and never an estimate.
	{Tool: ToolGoose, Cost: CostVendor, Activity: ActivityUnattributed, Tier: TierLive},
	{Tool: ToolCline, Cost: CostComputed, Activity: ActivityExact, Tier: TierLive},
}

// ReasoningReportFor says how a tool's source reports reasoning tokens. It reads
// reasoningModes rather than a second table: a tool present there reports the
// count with the stated relationship to output, and a tool ABSENT from it
// reports no count at all — which is why ReasoningModeFor's conservative
// subset fallback must not be used here. "Falls back to subset for pricing" and
// "reports a subset" are different statements, and only the first is true of
// crush, kimi-code, goose and cline.
func ReasoningReportFor(tool string) ReasoningReport {
	m, ok := reasoningModes[tool]
	if !ok {
		return ReasoningReportNone
	}
	if m == ReasoningAdditive {
		return ReasoningReportAdditive
	}
	return ReasoningReportSubset
}

// CapabilityFor returns a tool's declaration, with Reasoning filled from
// reasoningModes. ok is false for a tool id this table has not been taught
// about, which a surface must render as "unknown" rather than inventing a row
// of plausible defaults.
func CapabilityFor(tool string) (ToolCapability, bool) {
	for _, c := range toolCapabilities {
		if c.Tool == tool {
			c.Reasoning = ReasoningReportFor(tool)
			return c, true
		}
	}
	return ToolCapability{}, false
}

// ToolCapabilities returns every declaration, in table order, each with its
// Reasoning filled. The slice is a copy.
func ToolCapabilities() []ToolCapability {
	out := make([]ToolCapability, 0, len(toolCapabilities))
	for _, c := range toolCapabilities {
		c.Reasoning = ReasoningReportFor(c.Tool)
		out = append(out, c)
	}
	return out
}
