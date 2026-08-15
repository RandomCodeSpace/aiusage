# CONTEXT

Glossary of the ubiquitous language for aiusage. Terms only — no
implementation. When a term here conflicts with usage in an issue or a
commit, this file wins; change it deliberately.

## Core nouns

- **Harness** — an AI coding agent CLI/app whose usage we observe (Claude
  Code, Codex, Goose, ...). A harness is the thing that spends tokens.
- **Surface** — the local artifact a harness writes that we read: a
  transcript file, a SQLite database, an OTEL export, a session-state
  ledger. One harness may expose several surfaces; an adapter picks the
  authoritative one.
- **Adapter** — the read-only component that turns one harness's surface
  into observations. Strictly observational: no writes, no locks, no
  rotation.
- **Ledger** — `usage_events`: the append-only, immutable record of token
  usage. History that outlives its sources; compaction upstream can never
  shrink it.
- **Activity** — a recorded tool/skill/hook invocation (a *call*), stored
  beside the ledger, never inside it.
- **Turn context** — a property of a whole usage turn naming what it ran
  under: agent, skill, mcp_tool, mcp_server, plugin. One value per
  dimension per turn.
- **Dimension** — one of the turn-context axes above, plus tool-call
  attribution. Together: six partitions of the same dollars.

## Invariants as vocabulary

- **Partition invariant** — each dimension's attribution is honest alone;
  summing across dimensions double-counts. Queries take exactly one
  dimension.
- **Attribution ceiling** — attributed tokens for any dimension over any
  window may never exceed the ledger total for the same window.
- **Unattributed** — calls whose cost is *unknown*, never *zero*. Rendered
  as unknown, never ranked as free.
- **Unpriced** — events carrying no cost because no price table names
  their model. Distinct from $0.

## Coverage vocabulary

- **Coverage bar** — usage (tokens/cost) is the floor for a supported
  harness; activity and turn attribution are captured wherever the surface
  exposes them, declared per adapter.
- **Capability declaration** — the adapter's machine-readable statement of
  what it captures (usage / activity / attribution) and its verification
  tier.
- **Live-tier** — an adapter verified against sessions actually run on a
  real install of the harness.
- **Fixture-tier** — an adapter whose surface format is derived from
  trusted source (e.g. another MIT tool's parser) and verified against
  constructed fixtures; labeled unverified-against-live until a real log
  promotes it.
- **Unsupportable** — a harness with no locally readable surface (e.g. no
  ledger exists). Documented, not adapted.

## Named bug classes

Checked against every new adapter before merge:

- **Split-identity records** — one usage identity spans several source
  records; usage collapses, activity/context must union.
- **Cumulative-vs-event counting** — a surface re-exports cumulative
  counters (OTEL metrics, growing totals); events come from spans/deltas,
  never from summing re-exports.
- **Assigned-not-accumulated columns** — a surface column holds the last
  value, not a running total; reading it as a total misreports.
