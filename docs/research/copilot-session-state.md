# Copilot session-state: the vendor ledger we are not reading

Research resolution for issue #69 (map: #68). Measured 2026-08-15 on this
machine against GitHub Copilot CLI **1.0.80**
(`@github/copilot-linux-x64/package.json`, `"version": "1.0.80"`).

No prompt, message, command or tool-argument text from any session appears in
this document. Structure, counters and identifiers only.

## Summary

The survey's premise is **confirmed in its cost claim and refuted in its
location claim**. `events.jsonl` is not the vendor's usage ledger — it carries
a *cumulative* cost counter and no token breakdown outside shutdown. The
per-call, delta-valued, vendor-priced ledger is a **different file the survey
did not name**: `~/.copilot/session-store.db`, table `assistant_usage_events`.

Recommendation in one line: **augment, do not replace.** Keep the OTEL adapter
as the token surface it already is; add `session-store.db` as the authoritative
**cost** surface and `events.jsonl` as the authoritative **activity and
attribution** surface. Each of the three surfaces is authoritative for exactly
one thing, and the boundaries are measured below.

---

## 1. What is actually on disk

`~/.copilot/session-state/` holds one directory per session:

```
6a8b2fa9-6c14-442e-826d-b07b9ff4755c/   (created 08:52:55)
71b459ce-5312-4161-80d1-a22ee666fbad/   (created 08:54:23)
e50c5dbc-9b6f-4b7f-8253-e4a9a645be62/   (created 15:15:33)
```

Per-session contents: `workspace.yaml`, `checkpoints/index.md`,
`rewind-file-snapshots/{tracking.json,index.json}`, `files/`, `research/`, an
`inuse.<pid>.lock` while live, and — **only for sessions that actually took a
turn** — `events.jsonl` and `session.db`.

Two of the three sessions have **no `events.jsonl` at all**: their
`session-store.db` rows show `created_at == updated_at` and no summary, i.e.
they were opened and abandoned without a prompt. `events.jsonl` is written
lazily. An adapter must treat its absence as normal, not as an error.

The per-session `session.db` (28,672 bytes for the one live session) holds
`inbox_entries`, `todos`, `todo_deps` only. **No usage data.** It is not a
usage surface.

### 1.1 events.jsonl record shapes, measured

One file, 1,021,528 bytes, **302 records**, 23 distinct types, all JSON-parsable
(0 unparsable lines):

| type | n | type | n |
|---|---|---|---|
| assistant.message | 46 | permission.requested | 5 |
| assistant.turn_start | 40 | session.auto_mode_resolved | 4 |
| assistant.turn_end | 39 | subagent.started | 3 |
| tool.execution_start | 37 | session.shutdown | 3 |
| tool.execution_complete | 34 | session.resume | 2 |
| hook.start | 21 | permission.completed | 2 |
| hook.end | 18 | skill.invoked | 1 |
| system.message | 15 | subagent.completed | 1 |
| user.message | 14 | session.start | 1 |
| session.usage_checkpoint | 12 | session.model_change | 1 |
|  |  | session.info / session.permissions_changed / system.notification | 1 each |

Every record carries `id`, `timestamp`, `type`, `data`, and (except the first)
`parentId`. Some carry `agentId`.

The two usage-bearing durable shapes:

```
session.usage_checkpoint.data = {
  totalNanoAiu:        number,       # session-wide ACCUMULATED
  totalPremiumRequests: number,      # session-wide ACCUMULATED
  modelCacheState: [ {modelId, cacheExpiresAt, cacheTtlSeconds} ]
}

session.shutdown.data = {
  shutdownType: "routine"|"error", sessionStartTime, eventsFileSizeBytes,
  currentModel, currentTokens, systemTokens, conversationTokens,
  toolDefinitionsTokens,
  tokenDetails: { input|output|cache_read|cache_write: {tokenCount} },
  codeChanges: { linesAdded, linesRemoved, filesModified[] },
  modelMetrics: { "<model>": {
      requests: {count, cost},
      usage: {inputTokens, outputTokens, cacheReadTokens,
              cacheWriteTokens, reasoningTokens},
      totalNanoAiu, tokenDetails{...} } }
}
```

**Correction to the survey.** The ticket states that both durable rows carry
`totalNanoAiu` *and* `modelMetrics`. Measured: `session.usage_checkpoint`
carries **no `modelMetrics` and no token counts at all** — only `totalNanoAiu`,
`totalPremiumRequests` and cache-expiry bookkeeping. The vendor schema agrees:
`UsageCheckpointData.properties` is exactly those three keys with
`additionalProperties: false`. Per-model token breakdown exists only on
`session.shutdown`.

### 1.2 `assistant.usage` is genuinely never on disk

Vendor schema
(`.../copilot-linux-x64/schemas/session-events.schema.json`,
`definitions.AssistantUsageEvent`):

```json
"ephemeral": { "type": "boolean", "const": true,
  "description": "Always true for events that are transient and not persisted
                  to the session event log on disk." }
```

`ephemeral` is in that definition's `required` array with `const: true`, so the
type cannot be emitted non-ephemerally. Measured: **0 `assistant.usage` records
and 0 `session.usage_info` records** in the 302-record file, and **0 records of
any type carry an `ephemeral` field**, which is what a file containing only
durable events looks like.

64 of the schema's 150 event-type definitions are marked
`ephemeral: const true`. The survey's claim is confirmed exactly.

---

## 2. The surface the survey missed: `~/.copilot/session-store.db`

A single global SQLite database (not per-session), `schema_version = 6`, with a
table that is a per-call usage ledger:

```sql
CREATE TABLE assistant_usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    turn_index INTEGER, agent_id TEXT, parent_tool_call_id TEXT,
    model TEXT NOT NULL,
    input_tokens INTEGER, output_tokens INTEGER,
    cache_read_tokens INTEGER, cache_write_tokens INTEGER,
    reasoning_tokens INTEGER,
    total_nano_aiu INTEGER, request_multiplier REAL,
    duration_ms INTEGER, time_to_first_token_ms INTEGER,
    inter_token_latency_ms INTEGER,
    initiator TEXT, api_endpoint TEXT, reasoning_effort TEXT,
    finish_reason TEXT, content_filter_triggered INTEGER,
    token_details_json TEXT,
    created_at TEXT DEFAULT (datetime('now'))
)
```

45 rows, all for session `71b459ce`. `sum(total_nano_aiu) = 18,913,170,000`.

This is the persisted form of the ephemeral `assistant.usage` event: same
fields, same names. It landed recently — changelog 1.0.69, *"Show exact local
assistant usage in Chronicle and session SQL"* — which bounds how far any
backfill can reach (see §6).

The adapter's own package comment is now wrong on this point:

> `internal/adapter/copilot/copilot.go:3` — *"Copilot does not expose
> billing-grade token usage in its session store or events.jsonl (those are
> context-window gauges)."*

The session store exposes exactly billing-grade usage, per call, with the
vendor's own price table attached.

### 2.1 The vendor ships its own price table, inline, per call

`token_details_json` is an array of
`{tokenType, tokenCount, batchSize, costPerBatch}`. Reconstructing
`sum(tokenCount * costPerBatch / batchSize)` reproduces `total_nano_aiu`
**exactly on 45 of 45 rows, zero mismatches**.

The implied table, observed across two models:

| model | token type | nanoAIU/token | AIC per Mtok | USD per Mtok at $0.01/AIC |
|---|---|---:|---:|---:|
| claude-haiku-4.5 | input | 100,000 | 100.00 | $1.000 |
| claude-haiku-4.5 | output | 500,000 | 500.00 | $5.000 |
| claude-haiku-4.5 | cache_read | 10,000 | 10.00 | $0.100 |
| claude-haiku-4.5 | cache_write | 125,000 | 125.00 | $1.250 |
| gpt-5-mini | input | 25,000 | 25.00 | $0.250 |
| gpt-5-mini | output | 200,000 | 200.00 | $2.000 |
| gpt-5-mini | cache_read | 2,500 | 2.50 | $0.025 |
| gpt-5-mini | cache_write | 0 | 0.00 | $0.000 |

All eight rows land on published list prices for those models to the cent. That
is six independent price points across two providers agreeing on a single
constant, which is the strongest available local confirmation of the AIC claim.

### 2.2 The AIC = $0.01 claim, cross-checked against vendor documentation

GitHub Docs, *Usage-based billing for individuals*
(https://docs.github.com/en/copilot/concepts/billing/usage-based-billing-for-individuals):
**"1 AI credit = $0.01 USD."** The same page lists Copilot CLI among the
features that consume AI credits, and describes the conversion as token counts
priced per model and totalled into credits — which is precisely the
`token_details_json` arithmetic above.

So `total_nano_aiu / 1e9 * $0.01` is a vendor-computed cost figure requiring no
price table of ours. Confirmed.

**Honest caveat.** This is list-price *denomination*, not necessarily what the
account is charged. Copilot also meters "premium requests" (see §2.3), plans
include credit allowances, and an org may be on a different arrangement. The
number is the vendor's own valuation of the call; whether it is invoiced is
outside anything readable locally. **Unverified:** whether `nanoAiu` ever
reflects a negotiated rate different from list.

### 2.3 `cost` is not dollars, and `totalPremiumRequests` counts only
user-initiated calls

`ShutdownModelMetricRequests.cost` is described by the schema as *"Cumulative
cost multiplier for requests to this model"*, and it matches
`request_multiplier` in the DB and `github.copilot.cost` in OTEL — 0.33 for
claude-haiku-4.5, 0.0 for gpt-5-mini. **It is a premium-request multiplier, not
a currency amount.** Reading it as money would be wrong by three orders of
magnitude.

Measured, `sum(request_multiplier)` by initiator: user 2.31, agent 3.96,
sub-agent 0.0 (total 6.27). The final
`session.usage_checkpoint.data.totalPremiumRequests` is **2.31** — exactly the
**user-initiated** subset. So agentic-loop and sub-agent calls consume AI
credits but are not billed as premium requests.

---

## 3. Checkpoint semantics: CUMULATIVE (the named bug class, live)

`UsageCheckpointData.totalNanoAiu` is documented as *"Session-wide accumulated
nano-AI units cost at checkpoint time"*. Measured against consecutive records
of the one live session, the 12 checkpoints in timestamp order:

| checkpoint (UTC) | totalNanoAiu | cumsum of per-call `total_nano_aiu` | diff |
|---|---:|---:|---:|
| 08:59:24 | 1,699,605,000 | 1,699,605,000 | 0 |
| 15:16:20 | 3,433,730,000 | 3,433,730,000 | 0 |
| 15:16:33 | 4,188,935,000 | 4,188,935,000 | 0 |
| 15:17:00 | 5,110,295,000 | 5,110,295,000 | 0 |
| 15:17:53 | 7,300,630,000 | 7,300,630,000 | 0 |
| 16:32:49 | 11,796,060,000 | 11,796,060,000 | 0 |
| 16:33:17 | 12,963,205,000 | 12,963,205,000 | 0 |
| 16:38:17 | 13,472,850,000 | 13,472,850,000 | 0 |
| 16:39:29 | 14,508,250,000 | 14,508,250,000 | 0 |
| 16:40:14 | 14,925,510,000 | 14,925,510,000 | 0 |
| 17:06:44 | 16,581,365,000 | 16,581,365,000 | 0 |
| 18:01:21 | 18,913,170,000 | 18,913,170,000 | 0 |

**Twelve consecutive checkpoints, exact to the nano-AIU, zero drift.** The
checkpoint is the running total of `assistant_usage_events.total_nano_aiu`.

Consequences an adapter must honour:

- **Summing checkpoints multiplies cost.** Summing all 12 here yields
  124,893,605,000 nanoAIU against a real 18,913,170,000 — a **6.6x
  overstatement**. This is exactly CONTEXT.md's *cumulative-vs-event counting*
  class.
- **Differencing consecutive checkpoints is correct but lossy.** It yields the
  right session total, but the delta is a bucket of 1–5 model calls with no
  model attribution (the checkpoint names no model — §1.1), so a
  multi-model stretch cannot be split. Checkpoint 16:38:17 onward spans both
  claude-haiku-4.5 and gpt-5-mini with no way to apportion.
- **The counter does not reset on resume.** Two `session.resume` records sit
  between checkpoints (15:15:40 and 16:37:43) and the total keeps climbing
  across both. The schema calls this out: *"for reconstructing aggregate
  accounting on resume"*.
- **`session.shutdown` is a snapshot of the same counter, not an increment.**
  This session has **3** shutdown records; the one at 13:41:28 reports
  `totalNanoAiu` 1,699,605,000 and the one at 16:37:41 reports 12,963,205,000
  — both identical to the checkpoint immediately preceding them. Adding
  shutdown to checkpoints double-counts.
- **`session.shutdown.modelMetrics[*].usage.*` is likewise cumulative
  session-to-date**, not per-shutdown: the 16:37:41 record reports
  `requests.count: 19` and `inputTokens: 419,663` covering the whole session
  including the pre-13:41 portion.

---

## 4. Dedup identity

### 4.1 events.jsonl — clean

`id` is a UUID v4 *"generated when the event is emitted"*, `parentId` links to
*"the chronologically preceding event in the session"*. Measured on 302
records: **302 distinct ids, 0 missing, 0 duplicated, 0 records whose
`parentId` is absent from the file, timestamps monotonically non-decreasing.**

The file is append-only across resume: records from 08:54 survive a shutdown at
13:41, a resume at 15:15, a second shutdown at 16:37 and a second resume. It
was observed growing during this research (300 → 302 records) with the
chain intact.

So the activity dedup key is `copilot|<sessionId>|<event.id>` — the provider's
own identity, globally unique, stable across re-reads, with no positional
component. Same discipline as claude-code's `tool_use` block id.

### 4.2 assistant_usage_events — needs a content-derived key

The table has **no provider-side call identifier**: no `service_request_id`, no
`gen_ai.response.id`, no `apiCallId` column. Its identity options:

- `id` (AUTOINCREMENT, never reused) — stable *within one database file*, but
  restarts at 1 if the store is ever recreated. Not safe alone.
- `(session_id, created_at)` — **45/45 unique** measured, millisecond
  precision.
- `(session_id, turn_index)` — **only 11 distinct for 45 rows.** Not a key.

Recommended: a content-derived key over
`(session_id, created_at, model, total_nano_aiu, input_tokens, output_tokens)`.
It survives a rowid reset, contains no file offset or read position, and
re-derives identically on every pass.

### 4.3 The `turnId` trap

`turnId` in `events.jsonl` and `github.copilot.turn_id` in OTEL are **not
session-unique**. They are the agentic-loop iteration index within one user
interaction and **reset to 0 at every user message**. Measured: the session has
11 user interactions but `turnId` only ever takes values `0`–`6`, restarting at
`0` eleven times.

`assistant_usage_events.turn_index` is a *different* counter — the
user-interaction index, 0..10, matching the 11 rows of the `turns` table.

Keying or joining on `(session_id, turnId)` would collapse 11 interactions into
7 buckets and mis-attribute cost. The session-unique handle is
`interactionId` (`assistant.message.data.interactionId`,
`user.message.data.interactionId`, OTEL `github.copilot.interaction_id`).

---

## 5. OTEL vs session-state: what each has that the other lacks

Reconciled over the same session, `~/.copilot/otel/*.jsonl` (two files, 1.9MB
each) against the two session-state surfaces.

### 5.1 Tokens: they agree exactly

| | calls/spans | input | output | cache_read | cache_write |
|---|---:|---:|---:|---:|---:|
| `assistant_usage_events` | 45 | 975,612 | 15,237 | 811,374 | 56,061 |
| OTEL spans, `gen_ai.operation.name == "chat"` | 43 | 975,612 | 15,237 | 811,374 | 56,061 |

**Every token total matches to the unit.** The row-count difference is
coalescing, not loss: 5 sub-agent calls in the DB appear as 3 OTEL spans (named
`chat gpt-5-mini`, not `chat`) whose token sums equal the 5 DB rows exactly
(input 63,210 / output 4,447 / cache_read 35,840 / cache_write 0 on both
sides).

So the existing adapter is **not** under-counting Copilot tokens. That part of
it is sound and should be left alone.

### 5.2 Cost: OTEL has it per call, but not for sub-agents

OTEL `chat` spans **do** carry `github.copilot.nano_aiu` — per call, as a
delta. This is worth stating plainly because the ticket implies OTEL lacks
vendor cost entirely: it does not.

But the coverage is incomplete:

| | spans/rows | sum nanoAIU |
|---|---:|---:|
| OTEL spans with `github.copilot.nano_aiu` | 40 | 17,249,920,000 |
| `assistant_usage_events` | 45 | 18,913,170,000 |
| **missing from OTEL** | 5 | **1,663,250,000 (8.8%)** |

The 5 missing calls are **exactly** the `initiator = 'sub-agent'` rows. By
initiator the two surfaces agree perfectly where they overlap — user
9,492,945,000 on both, agent 7,756,975,000 on both — so this is not export lag,
it is a structural gap: the 3 sub-agent spans carry `gen_ai.usage.*` but **no
`github.copilot.nano_aiu` attribute at all**.

It is not an error artifact either, which is worth ruling out explicitly: one
of the three sub-agent spans completed cleanly (`finish_reasons: ["stop"]`, no
`error.type`) and still carries no `nano_aiu`, while a main-agent `chat` span
that failed with `SessionDestroyedError` **does** carry it (87,140,000). Cost
tracks the initiator, not the outcome.

The session store has no such gap: its total *is* the checkpoint total (§3).

**`invoke_agent` spans double-count.** They also carry
`github.copilot.nano_aiu`, and their sum is **17,249,920,000 — byte-identical
to the leaf-span total**, with all 40 leaf spans parented to an `invoke_agent`
span. They are per-turn rollups. Summing every span carrying `nano_aiu` yields
34,499,840,000, exactly 2x the truth. Any future cost extraction from OTEL must
take leaf `chat` spans only.

### 5.3 Activity and attribution: session-state wins outright

CLAUDE.md currently records that the Copilot export *"names no skill and no
hook"* and that copilot *"never attributes"*. Both remain true of OTEL and both
are **false of session-state**.

| fact | OTEL | events.jsonl |
|---|---|---|
| tool call, one record per call | `execute_tool <name>` spans (37 measured) | `tool.execution_start` (37) / `_complete` (34) |
| tool name | in the span *name* only; `gen_ai.tool.name` attribute **absent on every span** | `data.toolName`, explicit |
| tool call id | `gen_ai.tool.call.id` | `data.toolCallId` |
| tool outcome | span status | `data.success`, `toolTelemetry.metrics.exit_code`, `elapsed_seconds` |
| **skill name** | only `github.copilot.context.skills` = what was *available* | **`skill.invoked.data.name`** (measured: `context7-docs`, with `source: inherited`, `trigger: user-invoked`) |
| **hook** | nothing | **`hook.start`/`hook.end` with `data.hookType`** (measured: 21 `preToolUse`) |
| **sub-agent** | `invoke_agent task` spans | `subagent.started`/`completed` with `agentName`, `model`, `toolCallId`, `totalTokens`, `totalToolCalls` |
| model switch | — | `session.model_change`, `session.auto_mode_resolved` |
| permission prompts | `permission` spans (5) | `permission.requested`/`completed` |

Measured tool names from `tool.execution_start`: bash 18, read_agent 6, sql 4,
create 3, task 3, view 2, ask_user 1.

**Sub-agent cost is attributable, for the first time.**
`assistant_usage_events.parent_tool_call_id` is non-null on exactly the 5
sub-agent rows, and each value is the `toolCallId` of a `task` tool call in
`events.jsonl` — **3 of 3 matched** (`call_9Pku0…`, `call_SzRmW…`,
`call_rb11i…`, all `toolName: task`), and `subagent.started` names the same
ids. That is a real join key from a cost row to the activity row that caused
it, of the kind claude-code has and copilot was documented as lacking.

Note the shape: 5 usage rows against 3 `task` calls, so this is a
many-usage-rows-to-one-call join. It is the mirror of the `activity_events`
case (one usage row, many calls) and wants summation on the usage side, not a
divisor.

### 5.4 What OTEL alone still has

- A single normalized stream across the whole machine, already parsed.
- `gen_ai.response.id` and `github.copilot.service_request_id` per call —
  provider-side identity that `assistant_usage_events` has no column for.
  (`events.jsonl`'s `assistant.message` does carry `serviceRequestId`,
  `requestId`, `clientRequestId`, `apiCallId`, `messageId`.)
- Latency/duration metrics under standard GenAI semantic conventions.

None of these are cost or activity facts. There is nothing OTEL knows about
*what a session spent or did* that the two session-state surfaces do not.

### 5.5 OTEL is opt-in; session-state is not

Confirmed on this machine. `~/.copilot/otel` exists only because `.bashrc:357-359`
exports `COPILOT_OTEL_ENABLED=true`, `COPILOT_OTEL_EXPORTER_TYPE=file` and
`COPILOT_OTEL_FILE_EXPORTER_PATH`. `~/.copilot/config.json` contains no
telemetry setting. The survey's premise holds: **OTEL is a surface this user
configured, and would be absent on any other install.** `session-state/` and
`session-store.db` are written unconditionally.

---

## 6. Backfill reach — measurable answer is "one day", and that is the honest one

This machine cannot answer the retention question. Stated plainly rather than
extrapolated:

- **3 session-state directories, all created 2026-08-15**, between 08:52:55 and
  15:15:33. Date range: **a single day**.
- `assistant_usage_events` holds 45 rows spanning
  `2026-08-15T08:59:22.190Z` .. `2026-08-15T17:15:47.329Z`.
- `~/.copilot/config.json` records `firstLaunchAt: 2026-08-03T06:19:13.630Z` —
  **12 days before the oldest surviving session directory.**

Whether that 12-day gap means "no sessions were run" or "sessions were pruned"
is **not determinable from this machine.** Two pieces of evidence lean toward
no pruning, neither conclusive:

- `sqlite_sequence` gives `assistant_usage_events = 45` against 45 live rows,
  `turns = 11` against 11, `session_files = 3` against 3. **No row has ever
  been deleted from those tables** since the database was created. With
  AUTOINCREMENT this is a reliable statement about the table's whole lifetime —
  but that lifetime may be short (see below).
- No retention or cleanup setting appears in `~/.copilot/config.json`, and the
  vendor changelog has **0 occurrences** of "retention" or "session-state".

Two hard bounds on any backfill regardless:

1. **`assistant_usage_events` is new.** Changelog 1.0.69: *"Show exact local
   assistant usage in Chronicle and session SQL"*; installed version is 1.0.80.
   Sessions predating that upgrade have no per-call usage rows, whatever
   happened to their directories.
2. `session_store.db` `schema_version = 6`; a future vendor migration could
   rewrite or drop the table.

**Unverified, and it needs a longer-lived install to settle:** whether Copilot
CLI prunes `session-state/` directories or `assistant_usage_events` rows on any
schedule. Do not promise users historical backfill on the strength of this
machine.

---

## 7. Recommendation: augment, with one surface authoritative per fact

**Do not replace the OTEL adapter.** Its token numbers are exactly right (§5.1),
its activity extraction is already correct and hard-won (the 226x dataPoint
trap in `activity.go`), and rewriting a correct thing to reach facts that can
be *added* beside it is cost without benefit.

**Do not make `events.jsonl` the usage surface either.** It carries a
cumulative counter with no model dimension — strictly worse than what we have.

Proposed split, each line backed by a measurement above:

| fact | authoritative surface | why |
|---|---|---|
| per-call tokens | either; prefer **`session-store.db`** | identical totals (§5.1), but the DB is always present (§5.5) and cheaper to read incrementally |
| **per-call cost (nanoAIU → USD)** | **`session-store.db.assistant_usage_events`** | only surface with 100% coverage; OTEL misses 8.8% (§5.2), checkpoints have no model dimension (§3) |
| model price table | **`token_details_json`** | reconstructs `total_nano_aiu` on 45/45 rows (§2.1); removes Copilot from the LiteLLM table entirely |
| tool calls | **`events.jsonl`** (`tool.execution_start`) | explicit `toolName`; OTEL has no `gen_ai.tool.name` attribute (§5.3) |
| **skills and hooks** | **`events.jsonl`** | `skill.invoked.data.name`, `hook.start.data.hookType`; OTEL has neither (§5.3) |
| sub-agent cost attribution | **`parent_tool_call_id`** joined to `events.jsonl` `toolCallId` | 3/3 matched (§5.3) |
| session identity | `session_id` / `gen_ai.conversation.id` | same UUID on all three surfaces |
| premium-request count | `request_multiplier` over user-initiated rows | matches `totalPremiumRequests` exactly (§2.3) |

Nothing should read `session.usage_checkpoint` or `session.shutdown` as usage.
Their one legitimate use is **verification** — a cheap invariant check that our
per-call sum equals the vendor's own running total, which is how §3 was proved
and which would catch a future schema change immediately.

### 7.1 Mechanics that fall out of the doctrine already in CLAUDE.md

- **Read-only, `mode=ro`, no `immutable=1`.** `session-store.db` is written
  concurrently by the live CLI — its WAL was 1.8MB during this research — so it
  is the hermes case, not the immutable one.
- **Incremental reads are trivial here.** `assistant_usage_events.id` is
  AUTOINCREMENT and monotonic, so `WHERE id > ?` against a `source_checkpoints`
  watermark replaces re-parsing 1.9MB of OTEL every poll. Guard against rowid
  reset by also checking that the stored high-water row still exists.
- **Raw stays an allow-list.** The usable fields are already exactly the
  allow-list shape: model, five token counts, `total_nano_aiu`,
  `request_multiplier`, `initiator`, `api_endpoint`, `finish_reason`,
  `reasoning_effort`. `token_details_json` is a price table, not content.
  Nothing in this table holds prompt text.
- **`events.jsonl` needs a decode allow-list, and it is not optional.** Unlike
  the OTEL span map, this file **does** carry prompt and command text:
  `user.message.data.content`, `assistant.message.data.content`,
  `tool.execution_start.data.arguments.{command,prompt,file_text,query,...}`,
  `permission.requested.data.permissionRequest.toolArgs`,
  `hook.start.data.input.toolCalls[].args`, `skill.invoked.data.content`. An
  adapter must decode into structs with fields only for `type`, `id`,
  `parentId`, `timestamp`, `agentId`, `data.toolName`, `data.toolCallId`,
  `data.turnId`, `data.hookType`, `data.name`, `data.success` — so
  `encoding/json` discards the rest while parsing, the claude-code
  `contentBlock.Input` pattern. This is the single largest privacy delta
  between the current adapter and the proposed one and should be treated as a
  merge gate.
- **Discovery must not assume `events.jsonl` exists** (§1) and must keep
  honouring `COPILOT_OTEL_FILE_EXPORTER_PATH` in `cmd.discoveryEnv`. A
  session-state root override, if one exists, would need adding there too —
  **unverified:** no such environment variable was found in the CLI package.

### 7.2 Sequencing

1. Read `assistant_usage_events` for cost only; keep OTEL as the usage source.
   Verify per session against the last `session.usage_checkpoint`. Smallest
   change that closes the 8.8% sub-agent cost hole and drops the Copilot price
   table.
2. Add `events.jsonl` activity with the decode allow-list — gains skill and
   hook rows, which Copilot has never had.
3. Only then consider whether OTEL still earns its place. On this evidence it
   would be kept for installs already exporting it and as a cross-check, not as
   the primary surface.

---

## Sources

All local paths read read-only on 2026-08-15.

- `/home/dev/.copilot/session-state/71b459ce-5312-4161-80d1-a22ee666fbad/events.jsonl` — 302 records, 1,021,528 bytes
- `/home/dev/.copilot/session-state/{6a8b2fa9…,e50c5dbc…}/` — no `events.jsonl`
- `/home/dev/.copilot/session-store.db` — `assistant_usage_events` (45 rows), `sessions` (3), `turns` (11), `sqlite_sequence`
- `/home/dev/.copilot/session-state/71b459ce…/session.db` — todos/inbox only
- `/home/dev/.copilot/otel/copilot-otel-20260815-084418.jsonl`, `…-124122.jsonl`
- `/home/dev/.copilot/config.json` — `firstLaunchAt`
- `/home/dev/.bashrc:353-359` — OTEL exporter environment
- `…/node_modules/@github/copilot-linux-x64/schemas/session-events.schema.json` — vendor schema, 104 event types
- `…/node_modules/@github/copilot-linux-x64/package.json` — version 1.0.80
- `…/node_modules/@github/copilot-linux-x64/changelog.json` — 1.0.69 usage-persistence entry
- `internal/adapter/copilot/copilot.go`, `internal/adapter/copilot/activity.go`
- GitHub Docs, *Usage-based billing for individuals* — https://docs.github.com/en/copilot/concepts/billing/usage-based-billing-for-individuals
