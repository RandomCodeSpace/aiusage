# aiusage

Go daemon + Bubble Tea v2 TUI that records AI agent CLI token usage into an
append-only SQLite ledger and reports on it. Pure Go, CGO disabled everywhere
except the race gate.

## Invariants

**usage_events is append-only.** It is the immutable source of truth; BEFORE
UPDATE / BEFORE DELETE triggers in internal/store/schema.sql abort any
mutation. Insert new rows only, deduplicated via
`ON CONFLICT(dedup_key) DO NOTHING` (deliberately not INSERT OR IGNORE, which
would also swallow CHECK violations). Corrections are new rows with
kind='adjustment'. aggregate_state, source_checkpoints and usage_rollup are the
only mutable data tables (schema_meta holds mutable bookkeeping: the version
stamp and the rollup watermark) — working state, not history.

**usage_rollup is derived, never authoritative** (issue #59, resolved). It is a
summary of usage_events keyed by (UTC 15-minute bucket, tool, model, project),
and nothing may read it as history: every row is reproducible from the ledger
by store.RebuildRollup, and dropping the whole table loses nothing. Its deltas
are written inside the same transaction that appends the events
(store.insertEventsTx), so a crash cannot land events without the matching
delta; the collector re-checks it against the ledger watermark and event count
at the start of every pass (EnsureRollup) and rebuilds when they disagree,
which is the only direction a disagreement is ever resolved in. The bucket key
is UTC — rolling up by local time would bake the writing machine's calendar
into stored data — and the local fold happens on read, in SQL, exactly the way
query.go folds event_time_unix. The width is 15 minutes because every
real-world UTC offset is a whole number of quarter hours: hourly keys misplace
half-hour zones (Asia/Kolkata at +05:30, this machine's zone), where an hour
bucket straddles two local buckets and the first half hour of every local day
would land in the previous one. The rollup deliberately keeps no session,
provider or service-tier dimension and no resolution below its bucket:
distinct-session counts, per-event listing and finer buckets go to the ledger,
and the read API refuses granularities below an hour outright.

**A stale rollup is DETECTED, never trusted.** The v4 migration creates
usage_rollup EMPTY and leaves the fill to the collector's next pass, so between
the two there is a window where a full ledger has a summary of zero rows behind
it — and a reader that took the rollup's word for it would answer every question
with zeros. `store.RollupStale` is the test, and it is two comparisons against
the ledger itself: the watermark in schema_meta vs MAX(id), then SUM(events) vs
COUNT(*). `store.EnsureRollup` runs it at the start of every collection pass and
REBUILDS from usage_events when they disagree, which is the only direction a
disagreement is ever resolved in — the ledger is never corrected to match the
summary. Any future reader of usage_rollup owes the same check before it reads:
the table is derived, and a derived table that has fallen behind is wrong rather
than merely old.

**Raw is usage-object-only** (issues #17 and #42, resolved). Every adapter
builds usage_events.raw / aggregate_state.raw from an explicit ALLOW-LIST of
usage/model/identity fields — never by stripping content out of a whole
record, which rots the moment the provider adds a field. Raw is an audit
payload, never a backfill source: the schema columns carry everything cost and
reporting need. `privacy.no_raw` drops it everywhere; the collector enforces
that centrally (collect.WithoutRaw), so adapters stay unaware of it. Rows
already in usage_events are never rewritten — history predating the allow-list
still holds whole transcript lines, which is why export gates raw behind
--include-raw. aggregate_state is mutable, so its rows shrink on the next
snapshot cycle.

**activity_events is a second ledger, and it stores no tokens** (schema v5). It
records agent ACTIVITY — which tool was called, which skill was invoked, which
hook fired — one append-only row per invocation, on the same terms as
usage_events (UNIQUE dedup_key, no-UPDATE/no-DELETE triggers, ON
CONFLICT(dedup_key) DO NOTHING). It is deliberately a SEPARATE table: activity
is not token accounting, and a call that cost nothing has no business in the
ledger that answers "what did this cost". It is also NOT in usage_rollup, which
stays a token summary.

Cost is attributed by REFERENCE, never by copy. A row carries
`usage_dedup_key` (the usage_events row whose provider record contained the
call); tokens and cost are derived on READ by joining the ledger and dividing by
the number of rows that name that key (store.activityDivisorSQL, used by
SummarizeActivity and TopActivity). One assistant turn commonly emits
several tool_use blocks against a SINGLE usage object, so copying the turn's
tokens onto each row would multiply the real cost by the number of calls in it —
measured on this machine's corpus, the undivided query overstates by 25% (8.56bn
vs 6.85bn tokens), with turns of up to 25 calls. Keeping the number where it
already lives makes that inflation structurally impossible rather than a rule
someone has to keep remembering, and integer division means the split can only
ever UNDERSTATE. `usage_dedup_key` is NOT a foreign key: a call is an observed
fact even when its usage row was skipped or predates activity collection, and
the read path left-joins so a missing partner contributes no cost instead of
losing the call. Rows with no join are reported as `UnattributedCalls`, never as
free.

**The cost divisor is COUNTED in the table, never read from `calls_in_turn`.**
The column is what an adapter believed when it wrote the row, and an append-only
table cannot correct a belief that later turns out to be low. Claude Code
streams ONE API response across SEVERAL transcript records sharing a message id
(102,793 assistant records with usage collapse to 50,729 message ids locally,
7,957 ids spanning more than one call-carrying record), and until the deduper
unioned them the adapter emitted one row stamped `calls_in_turn=1` for turns
that had three. The missing rows append cleanly — their dedup keys never
collided — but the row already stored keeps its 1 forever, so dividing by the
STAMP hands one turn's tokens out as 1/1 + 1/3 + 1/3. Measured on the recovered
ledger: 6,602 usage keys carry rows that disagree about `calls_in_turn`, and the
stamp attributes 6.862bn tokens against a ceiling of 6.607bn — a 255M-token
OVERSTATEMENT, the one direction this table promises never to go. Counting
`(SELECT COUNT(*) ... WHERE s.usage_dedup_key = a.usage_dedup_key)` makes the
divisor exactly the number of rows sharing the row being divided, so integer
division bounds the shares by the turn's real total by construction — whatever
any adapter stamped, in whatever order the rows landed, however many passes it
took. Same rule as usage_rollup: derived on read, never authoritative on disk.
`calls_in_turn` stays as what the source reported, which is a different and
still-worth-keeping claim. Cost measured on 63k rows: +37ms over the whole
history, 7ms over a day.

**One usage identity can span several source RECORDS, and the activity key must
survive that.** It is the bug class this ledger is most exposed to, and it is
per adapter: claude-code streams a response across records that share a
message.id and PARTITION its tool_use blocks between them (the usage rows
collapse keep-best, the calls union — see `deduper.addCalls`), so both the key
and the divisor have to be settled per MESSAGE, never per record. The key is the
`tool_use` block's own `id`, which is the provider's identity for the call and
globally unique (60,869 blocks, 60,869 distinct ids, none repeated, none
absent); an id-less block falls back to a hash of its record's CONTENT plus its
position among that record's blocks, never to a read position — the adapter
re-reads every transcript in full whenever any file under the root changes, so a
key minted from a file offset or line number would recount the call on every
poll. opencode is already per-message (`opencode|part|<part.id>`, divisor from
one query over the whole message range); codex and copilot mint one row per
call from a span/call id and attribute nothing at all, so they have no divisor
to get wrong.

**Activity is names and counts ONLY.** There is no column for a tool's input and
no raw column, so a command string, a file path or a prompt has nowhere to land;
`privacy.no_raw` is satisfied by construction rather than by a switch. The
adapters enforce it at the decode: claude-code's `contentBlock.Input` is a
struct with a single `Skill` field, so encoding/json DISCARDS every other input
key as it parses — the content never becomes a value in the process. MCP tool
names are names, not content, and are kept verbatim. Claude Code's hook records
carry NO hook name — each `hookInfos` element identifies its hook only by the
raw shell command it ran — so hook rows are named for the EVENT (`Stop`) and
`hookInfos` is decoded as `[]struct{}` to take its length and nothing else.

What each adapter captures: claude-code joins exactly (tool_use blocks and
`.message.usage` are in the SAME message) and rides the existing deduper, so a
sidechain replay cannot count a call twice; opencode joins exactly via
`part.message_id` -> `message.id`, the id its usage dedup key is already built
from; codex NEVER attributes, because its `token_count` records share no
identity with its `function_call`/`custom_tool_call` records (verified: zero of
261,938 local token_count records carry a turn_id, while every call does), and a
positional guess is not on offer; copilot never attributes a CALL either, for
the same reason from a different shape — its `execute_tool` span's parent is the
`invoke_agent` span, which makes it a SIBLING of the `chat` spans the usage rows
are built from, its `gen_ai.tool.call.id` occurs exactly once in the whole
export, and the only handle it shares with usage is the traceId, which covers
every turn of a conversation rather than one of them. That id is not useless,
though: a DIFFERENT surface repeats it, and a turn's whole cost joins through it
even where a call's share cannot — see the three-surface paragraph below.

The harnesses added since: pi and openclaw are ONE package over one
byte-identical session tree and TWO tools whose rows are never summed, taking
usage from three entry kinds (assistant `message`, plus `compaction` and
`branch_summary`, which carry the usage of the LLM call that wrote the summary
and name no model of their own) and joining activity exactly, since a `toolCall`
block sits in the SAME record as the usage object it cost; crush is cost-only —
one event per growth of `sessions.cost`, zero tokens, 0.0 refused as unmeasured
rather than free, sub-agent sessions excluded because the parent's cost already
contains them, and the assigned token columns never read as a total; kimi-code
reads the per-agent append-only wire log (subagents included: each agent owns its
recorder) and emits usage alone, carrying the model forward from the nearest
prior `llm.request` in the checkpoint, since a tail read can start after it;
reasonix reads the harness's own stats ledger and emits usage alone, unpriced,
keyed by a content hash rather than a byte offset; dsh emits usage, tool activity
and agent turn context, and deliberately records NO context for a fork seed — a
child log opens with the parent's leading events copied verbatim, both logs
derive the same usage dedup key, and their headers can disagree about the records
they share, so the first walked would otherwise re-label the ancestor's turns;
qwen-code reads the harness's OWN usage ledger rather than the transcripts every
third-party parser reads (the ledger hands out a per-record randomUUID where a
transcript forces a content tuple), buckets on `timestamp` because both
`localMonth` and the FILE NAME are the writing machine's calendar, and emits
usage plus agent turn context from `source` — the "main" sentinel producing none,
since storing it would invent an agent and collide with a real subagent of that
name — while its Gemini-shaped counters are filled by whichever wire answered, so
`thoughtsTokens` is billed as a SUBSET (exact on openai, qwen-oauth and
anthropic, under-billing only on the native Gemini wire) and is kept out of the
total floor for the same reason cache read always has been; goose reads the
purpose-built `usage_ledger` and NEVER a token column of `sessions`, whose
`total_tokens` is assigned rather than accumulated, keeps `carried_forward` rows
because they are the GAP between accumulator and ledger rather than a duplicate
(and carry no model, so a model-is-required guard would delete exactly the
reconciliation), subtracts cache out of goose's cache-INCLUSIVE input so a cached
token is not billed at both rates, leaves a NULL cost unpriced rather than $0,
and never attributes its tool calls — `usage_ledger` rows carry no message id and
two of them commonly share one second, so a timestamp match would be a positional
guess; cline is split identity INVERTED, since the message document is rewritten
whole on every save (no byte offset survives, the gate is size+mtime) while the
message ids inside it do not change, so re-reads collapse on a
`cline|<session>|<agent>|<message id>` key scoped against locally-minted ids, it
sums the per-message `metrics` and never the running `metadata_json`
accumulator, joins activity exactly because the `tool_use` block sits in the SAME
message as the metrics it was billed under, and opens the WAL-backed sessions.db
index without immutable=1 — measured on this install the main file is 4096 bytes
holding no sessions table at all while every row lives in a 181KB WAL.

**Copilot activity comes from SPANS, never from metric dataPoints.** One OTEL
file carries both shapes and only one of them counts anything. A span is written
once per operation, so one `execute_tool` span is one tool call.
`github.copilot.tool.call.count` (and `agent.turn.count`,
`invoke_agent.tool_calls`, ...) is a CUMULATIVE counter re-exported on the
exporter's timer: measured on a live export, a session that made exactly ONE
tool call had produced 226 dataPoints, every one of them value 1 under an
identical attribute set. Counting or summing dataPoints reports that call once
per export interval — a 226x inflation of a single `view`. That rule governs the
OTEL surface and is unchanged by everything below it.

**Copilot is THREE SURFACES, each authoritative for exactly ONE fact** (issue
#69). The OTEL export is not the vendor's ledger; it is one of three artefacts
the CLI writes, and it is also the only one that is OPT-IN — `~/.copilot/otel`
exists on this machine solely because a shell profile exports
`COPILOT_OTEL_ENABLED`, while `session-store.db` and `session-state/` are
written unconditionally. Reading any of the three for what another owns is how
this adapter would double count, so the split is measured rather than tidy:

TOKENS stay OTEL's, because its numbers are already exact — reconciled against
the vendor's own per-call ledger over one session, input 975,612, output 15,237,
cache_read 811,374 and cache_write 56,061 agree TO THE UNIT on both surfaces. It
also remains the only surface tool activity is keyed from.

COST is VENDOR-PRICED and never touches the LiteLLM ladder. Copilot meters in
nano-AI-units, 1 AIC = $0.01 by GitHub's own billing documentation, and the
vendor ships the arithmetic inline: `assistant_usage_events.token_details_json`
is a `{tokenType, tokenCount, batchSize, costPerBatch}` array whose
`sum(tokenCount * costPerBatch / batchSize)` reproduces `total_nano_aiu` on 45 of
45 local rows, and the eight implied per-token rates land on two models'
published list prices to the cent. So nanoAIU -> micro-USD is a fixed division by
100,000, TRUNCATED, because rounding up per row could push a window's summed cost
past the vendor's own total for it and integer division can only ever understate
— the activity divisor's rule, applied to a different number.

The number reaches the ledger by TWO routes, and which route is decided by which
one is an IDENTITY, never by which surface reads nicer. A main-agent call carries
`github.copilot.nano_aiu` on the very `chat` span its usage row is built from, so
there is no join to get wrong; verified against the store, the attribute equals
`total_nano_aiu` on all 40 overlapping calls, zero mismatches. A SUBAGENT call
carries no such attribute at all — 3 of 43 spans, 8.8% of the session's whole
cost (1,663,250,000 of 18,913,170,000 nanoAIU), and it tracks the INITIATOR
rather than the outcome, since a span that died with SessionDestroyedError still
carries its cost while a subagent span that finished cleanly does not. That cost
exists only in `assistant_usage_events`, and it joins by walking the span's own
parents: chat -> `invoke_agent task` -> `execute_tool task`, whose
`gen_ai.tool.call.id` is the string the store records as `parent_tool_call_id`
(3 of 3 matched, covering all 5 subagent rows). The shape is MANY store rows to
ONE span — the export writes one span per spawned task however many API calls the
subagent then made — so this SUMS on the usage side, the mirror of the activity
ledger's one-row-to-many-calls case, with no divisor. More than one unvalued span
under one spawn is REFUSED rather than copied or divided.

The other 40 store rows are deliberately NOT joined back, and that is the codex
rule rather than an omission: the table carries no provider-side call identifier
at all, and the only per-row handle both surfaces share is the token tuple
(session, model, input, output, cache_read, cache_write) — a CONTENT FINGERPRINT
that happens to be unique here (45 of 45 and 43 of 43 distinct, 42 exact matches)
and would silently misprice the day two calls of a session billed the same
counts. It costs nothing to refuse, because the number those rows would supply is
already on the span, proven equal. Measured end to end on this machine: 43 rows
stored at 189,113 micro-USD against the vendor's own 189,131 — under by 18, one
per truncated row, and never over.

**The cumulative trap is live in this surface too.**
`session.usage_checkpoint.totalNanoAiu` and `session.shutdown.totalNanoAiu` are
session-wide ACCUMULATED counters, exact to the nano-AIU against the running sum
of `assistant_usage_events` over 12 consecutive local checkpoints. Summing them
yields 124,893,605,000 against a real 18,913,170,000 — a 6.6x overstatement — and
shutdown is a snapshot of the same counter rather than a further increment.
Nothing in the package has a FIELD for either, so they cannot reach the ledger by
accident; the test that proves they are cumulative decodes them itself.

SKILLS AND HOOKS come from `session-state/<id>/events.jsonl`, which is what the
export does not name: `github.copilot.context.skills` on `invoke_agent` is what
was AVAILABLE, not what ran, and OTEL has nothing for hooks at all, while this
file carries `skill.invoked.data.name` and `hook.start.data.hookType` explicitly.
`hook.start` and not `hook.end`: the invocation is the fact, and a hook that
never returned still fired (21 starts against 18 ends). TOOL CALLS ARE NOT READ
THERE — the same 37 calls appear on both surfaces under different ids, the OTEL
key is the one already in an append-only table, and emitting both would report
every Copilot tool call twice. The honest consequence: an install with the export
switched off records skills and hooks and no tool calls, the same way it records
no tokens.

SUBAGENT TURN CONTEXT (dimension 'agent') comes from the OTEL span tree alone and
is exact by structure: every `chat` span's direct parent is an `invoke_agent`
span (43 of 43, zero hops), and that span carries `gen_ai.agent.name` when and
only when the turn ran as a subagent — the session's own agent is
`gen_ai.agent.id = github.copilot.default` with NO name attribute. So the
emptiness test IS the sentinel test, with no list of default ids to keep in step;
naming the default would invent an agent and collide with a real subagent of that
name, the qwen-code "main" rule. A span has one parent, so a second value per
dimension is unreachable rather than merely unlikely.

**events.jsonl is the one Copilot surface that carries prompt text, and the
decode is TWO-STAGE.** Unlike the OTEL attribute map it holds content in at least
six named paths — `user.message.data.content`, `assistant.message.data.content`,
`tool.execution_start.data.arguments.{command,prompt,file_text,query,...}`,
`permission.requested.data.permissionRequest.toolArgs`,
`hook.start.data.input.toolCalls[].args` and `skill.invoked.data.content`. The
outer struct names only type/id/timestamp and keeps `data` as raw BYTES; those
bytes are decoded only for the two record types this reads, into structs carrying
one string each. So `data.content` is never a value in this process for any
record, and even `data.name` is never one for a record that is not a skill
invocation — a record type the package has not been taught about contributes
nothing however much it carries. The session store is read for exactly one
statement, a `parent_tool_call_id` -> `SUM(total_nano_aiu)` group-by; its `turns`
table holds whole prompts and replies and nothing has a field for one. Both are
covered by a plant-a-secret test over fixtures scrubbed from the real files, with
the canary in every path named here plus `sessions.summary`,
`turns.user_message`, `turns.assistant_response`, `gen_ai.tool.definitions`,
`gen_ai.agent.description` and the span `events` array.

`session-store.db` is opened mode=ro + query_only(1) and never immutable=1 — the
CLI holds it open and writes it live, and on this machine the main file was 4,096
bytes against a 1.8 MiB WAL, so an immutable reader would have seen an empty
table. It is NOT a source of its own: cost is a FIELD of a usage row, so a
separate source could only report it by minting a second usage event for a call
the OTEL source already reported. It rides on the OTEL sources as
`Meta["cost_db"]` and is opened at most once per file, only when that file holds
an unvalued call.

**A cost the adapter stamped is never overwritten by the price ladder.**
`collect.stampCost` prices only events that arrive UNPRICED, which is what its
doc comment always claimed and is now enforced. An adapter-set cost came from the
harness's own accounting — copilot's vendor valuation, crush's session cost,
goose's provider-reported figure — and the ladder is a public-rate-card ESTIMATE
of the same charge; letting the estimate win is a strict loss of fidelity, and it
happened silently whenever the table knew the model id. Copilot proxies
`gpt-5-mini`, which the embedded LiteLLM snapshot prices, so every Copilot call
on that model would have had its exact vendor cost replaced by an approximation.

**Turn context is a property of the TURN, and five axes share ONE table**
(schema v7, usage_turn_context). An activity row of kind='skill' records the turn
that INVOKED a skill — one call — not the thousands of turns the skill then
spends; locally that is 44 invocation rows against 8,039 records of real work.
For subagents the gap is a chasm: 81,231 usage-bearing assistant records ran
under `attributionAgent`, 77.9% of every token-bearing turn on this machine,
described by 129 `Agent` call rows.

The missing facts are Claude Code's FIVE top-level attribution strings —
`attributionAgent`, `attributionSkill`, `attributionMcpTool`,
`attributionMcpServer`, `attributionPlugin`. Each is a scalar string (verified:
101,309 occurrences across the five, 100% of them JSON strings, none an array or
object), so a usage row carries AT MOST ONE VALUE PER AXIS, and the table makes
that a constraint rather than a hope: **(usage_dedup_key, dimension) is the
PRIMARY KEY**, usage_events' dedup_key is UNIQUE, so once a query is pinned to
one dimension the join is 1:1 and per-value cost is a plain SUM with NO divisor —
over-attribution within a dimension is unrepresentable, not merely avoided.
`mcp_tool` and `mcp_server` always co-occur (7,084 records each, over the same
3,265 message ids, neither ever without the other) and are still two dimensions,
because "what did the ruflo server cost" and "what did browser_eval cost" are
different questions a composite string would answer neither of.

ONE TABLE, NOT FIVE. The giveaway that a table per axis was wrong is that the
second one would have been a verbatim copy of the first with a column renamed:
same key, same 1:1 join, same absent divisor, same triggers, same indexes. The
axis is a COLUMN, so a sixth attribution field is a new CHECK value rather than a
migration plus a fifth query builder that can drift.

**The six partitions, and the refusal that enforces them.** These five plus
activity_events' tool-call attribution are SIX PARTITIONS OF THE SAME DOLLARS —
each honest alone, meaningless summed, the way cost-by-region and
cost-by-product are two views of one budget. A turn commonly carries several at
once (locally 3,184 turns carry three, 19 carry four, 5 carry all five), and
EVERY row names the turn's FULL cost because each answers a different question,
so a dimension-blind join counts a turn once per context it holds. Measured on
this machine's ledger: the blind query reports 6.213bn tokens and $6,023.52 for
turns that actually cost 4.984bn and $4,700.90 — a **28.1% overstatement** from
one missing predicate.

So the dimension is a REQUIRED ARGUMENT of `SummarizeTurnContext` /
`TopTurnContext`, never a filter field a caller could leave unset: it is
validated against the closed vocabulary before any SQL is built, and
`buildTurnContextWhere` writes `c.dimension = ?` unconditionally, so no argument
and no combination of empty filters produces a statement without it. Grouping by
`"dimension"` is refused by name — it is exactly the operation that
concatenates two partitions — and so is grouping by any dimension OTHER than the
one queried (`GroupBy: ["agent"]` on a skill query would label skill values as
agents). `Kinds`/`Names` stay refused as before, since honouring them means
joining activity_events where a turn with two matching calls joins twice.
`Skills` is the pre-generalisation spelling of `Values` and is refused on any
dimension but skill, rather than filtering agent names against a list of skills
and returning an empty result that reads as "that agent cost nothing". No query
in internal/store reads two dimensions, or both this table and activity_events,
in one statement.

It is deliberately NOT another activity_events kind, and not a column on it:
41.8% of skill records (3,361 of 8,039) call no tool at all and emit no activity
row to hang a column on, and call-less subagent turns are far more common still.
Keying on the usage row captures those for free.

**usage_skill_context was FOLDED IN AND DROPPED, because it was derived.** Its
rows are exactly this table's `dimension='skill'` partition, and two tables
answering one question with nothing keeping them in step is the mistake the whole
change exists to avoid. The drop is safe on the same test usage_rollup passes:
every row is re-derived from the source transcript on the next pass, nothing
originates there, unlike usage_events whose rows are the only copy of a fact the
provider will never repeat. Proof on the production copy — the 3,009 rows dropped
came back as exactly 3,009 skill rows in one pass, alongside 41,055 agent, 3,265
mcp_tool, 3,265 mcp_server and 1,317 plugin rows.

v7 also **DELETEs the claude-code source_checkpoints row**, and that is what makes
"re-derived on the next pass" true rather than aspirational: the adapter gates a
root on a size+mtime manifest and opens no file when nothing changed, so an idle
machine would otherwise keep an empty table and never see the four new dimensions
at all. source_checkpoints is mutable working state (no append-only triggers) and
this is the one table where a DELETE costs a re-read rather than a fact; the
re-read is idempotent, since usage and activity rows re-derive their existing
dedup keys and conflict-skip. Measured v6→v7 on a 273MB production copy: the
migration is imperceptible against a 0.284s command, usage_events 366,946 →
366,946, activity_events 63,931 → 63,931, usage_rollup 3,067 → 3,067,
source_checkpoints 1,474 → 1,473 (exactly the one claude-code row), and the
following collection landed 51,911 context rows over 42,780 turns with ZERO
unjoined. The v6 step stays FROZEN in migrate.go and still creates the table v7
drops one transaction later: a migration step defines what a version MEANS, and a
database stopped at v6 must be what v6 produced.

The table is created EMPTY and **cannot be backfilled**. The facts exist only on
the source transcripts — usage_events never carried them — and the no-UPDATE
trigger forbids adding them to a stored row regardless. Usage whose transcript is
gone stays permanently unattributed; only the CONTEXT is lost, never the cost, and
every total that does not group by a dimension is unaffected.

**Turn context unions across a message's records; it never reads the winner's
copy.** This is the named bug class — one usage identity spans several transcript
records — applied to a third fact type. Claude Code streams one response across
records sharing a message id, exactly one of which becomes the usage row, and
`better` picks it on token total, cost and the presence of a speed field: metrics
with NO relationship to attribution. A winner that happens not to carry the field
would silently discard what its losing siblings recorded, which is precisely how
19,682 of 60,832 tool calls went missing. `deduper.addContexts` therefore unions
across every record of the message, first non-empty value winning per dimension,
so the outcome is independent of which record wins. Measured over 1,275
transcripts and 104,311 usage-bearing records, the source never disagrees and
never partially agrees — for every dimension, every record of a message carries
the same value or none carries the field, zero exceptions on all five — so today
the union and the winner's copy give the same answer. That is a reason the rule
is cheap, not a reason to skip it: it describes what one version of one CLI
happens to write, and the failure mode if it changes is silent under-attribution
that looks exactly like "that skill was cheap". The tie-break, should the source
ever disagree, is FIRST SEEN, deterministic across passes because files are
walked in lexical order and lines in file order.

**Turn context is NAMES only.** The five fields are typed as strings in
`rawLine`, so encoding/json can only ever put a name in them: a nested object of
arguments, a prompt, a result has no field to land in and never becomes a value
in this process. It is an ALLOW-LIST, not a prefix match — `recordContexts` reads
exactly five named fields, so an `attributionSomethingElse` carrying content
contributes nothing until this package is taught about it on purpose
(TestTurnContextDecodeIsAnAllowList). model.TurnContext has no field for inputs
and no raw column, so `privacy.no_raw` is satisfied by construction. The audit
payload did not grow an attribution field either.

**A long-context request switches RATE CARDS, for the whole request** (issue
#71, resolved). LiteLLM's `*_above_<N>k_tokens` family is a second, dearer rate
card, not a band of tokens priced separately: OpenAI, "Prompts with >272K input
tokens are priced at 2x input and 1.5x output for **the full request**";
Google, "all tokens (input and output) are charged at long context rates". The
marginal reading is an artifact of the field NAME and matches no documented
provider; on this ledger the two differ by a factor of ten (+$74.53
whole-request against +$7.09 marginal over the same 551 rows), because a
crossing turn is broad rather than deep — a 356K context is a 300K cache read
plus a 56K input, and neither bucket alone clears the line by much.

The threshold is PER MODEL and lives in the field name — the live table
publishes five boundaries (128K, 200K, 256K, 272K, 512K) across 120 models, so a
hardcoded 200K would be wrong for four of them — and no model carries two, which
is why `Rates.Long` is one card rather than a ladder. The tier is chosen by the
request's WHOLE prompt: `Charge.ContextTokens` is `Input + CacheRead +
CacheWrite5m + CacheWrite1h`, cached tokens included, because the adapters
normalize a provider's prompt into disjoint buckets (codex stores
`input = raw_input - cached`) and `Input` alone is that normalization's
leftovers, not a prompt size. Measured: 17 rows cross 272K on uncached input,
**551** cross on input + cache read — an implementation that thresholds on
`Input` looks right and fixes 3%. Output never counts; every provider measures
the boundary on the prompt, and the comparison is strict `>`.
`price_source` gains a `+long-context` suffix, which the open vocabulary already
allows, so a doubled row says why.

Two seams keep it honest. The embedded snapshot is regenerated by
`internal/pricing/gensnapshot.go` through `pricing.SnapshotField` — the
DECODER's own predicate — because the old filter kept seven hardcoded field
names and stripped every tier rate, which is how an air-gapped install
flat-priced a request the table did publish a second card for. And a Charge that
sums SEVERAL requests (report's display-time valuation of unpriced groups) sets
`Aggregate`, so a thousand short turns can never add up past a threshold and
bill the group off the long card. History is untouched: cost is stamped at
insert into an append-only table, the 551 rows keep their $77.59, and correcting
them would mean $74.53 of synthetic `kind='adjustment'` noise against a 0.30%
month-old error. Fix forward.

**Who priced a row is a CLOSED vocabulary of vendors against an open ladder.**
`price_source` stays free-form (a new rung must not need a schema change), but
exactly one place classifies it: `model.PriceProvenance`. Empty is `CostAbsent`
— unpriced, never free. The VENDOR set is an explicit allow-list of four stamps
(`copilot-nano-aiu`, `crush-session-cost`, `pi-reported`, `openclaw-reported`)
plus one family (`goose`, whose tail is goose's own provenance word), because
those are the four adapters that call `SetCost` and the ones `collect.stampCost`
is forbidden to overwrite. Everything else is `CostComputed`, and the default
runs that way on purpose: the ladder's rungs are open-ended
(`litellm-<date>`, `embedded-<date>`, `override`, their `+` composites, the
`+long-context` suffix), so enumerating them means chasing every new one, while
an unrecognised source read as vendor would overstate confidence in a number
this project guessed. It is an allow-list rather than a prefix match on tool ids
for the raw-payload reason: "starts with a tool name" silently admits whatever a
future stamp is called. Family names carry no SQL LIKE metacharacter, since
store builds a LIKE pattern from them. The UI spends this distinction as a
glyph: plain `$` is the bill, `~$` is our estimate, `-` is absent — and `≥` is a
SEPARATE mark meaning some row in the window had no price at all, so a total can
be approximate, bounded, both or neither.

**The partition invariant is UI, not just SQL.** `store.SummarizeTurnContext`
makes the dimension a required argument because a dimension-blind join
overstates by 28.1%; the Activity tab is the other half of the same rule.
`tui.ActivityPivot` is ONE SCALAR — the activity ledger's per-call reading plus
the five turn-context dimensions, cycled by `p`, never combined — so "show me
agents and skills together" is unrepresentable rather than discouraged. Its
cycle is built at init from `model.TurnDimensions()` rather than restated, so a
sixth attribution axis reaches the tab by being added to the store's vocabulary
and nowhere else. The kind column is DROPPED in a turn-context pivot: five cells
truncate `mcp_tool` and `mcp_server` to the same `mcp_s…`, which would make two
partitions indistinguishable in the one column meant to separate them, so the
panel title carries the dimension instead.

**Adapters are strictly observational.** They read files/DBs the agent CLIs
have already produced, nothing more: no writes, no write locks, no rotation.
SQLite sources open read-only (mode=ro; immutable=1 only when the source is
never concurrently written — hermes deliberately omits it, see
internal/adapter/hermes/hermes.go).

## Layering

model < adapter, store < collect, report, tui < cmd

Imports point strictly downward: `model` imports nothing internal; adapters
and the store depend only on `model`; collect/report/tui compose adapters and
the store; `cmd` wires everything. Shared types belong in `model`, not in a
sideways import.

`internal/service` sits off to the side: it may import stdlib and nothing else —
never collect/store/report/tui. It knows how a machine supervises processes, not
what aiusage collects.

## Supervision

aiusage installs itself. One systemd **user** unit — `aiusage-collect.service` —
is written to `$XDG_CONFIG_HOME/systemd/user` (default
`~/.config/systemd/user`), enabled and started. No root anywhere: a user unit,
`loginctl enable-linger` for survival past logout, and every call
non-interactive (`--no-ask-password`).

**Install is create-if-missing; removal is stamp-gated.** An existing unit file
is never rewritten — the user may have edited it — only its enabled/active state
is corrected. `aiusage setup --force` is the sole path that replaces it, and it
then restarts what it rewrote, because the point of a new ExecStart is that
the new one runs. The generated unit opens with the
`# aiusage-generated-unit` stamp, and `setup --remove` refuses to delete a unit
file that lacks it — naming the file and `--force`, and exiting NON-ZERO, since
`setup --remove && rm -rf ~/.config/systemd/user` must not proceed as though the
directory were clean — since create-if-missing exists precisely because the file
may be the user's own (the hand-written unit this feature replaced carries no
stamp, and removal must refuse it). The whole
thing happens on any run that already auto-spawns a daemon (`ensureDaemon`);
`setup` is the explicit, inspectable version of it, and `doctor` prints which of
the three states holds (the systemd unit, an unsupervised background process,
none).
`--no-daemon` suppresses the AUTOMATIC daemon start and the automatic install;
it does not suppress `setup`, which is the explicit ask.

**Path overrides suppress the automatic install — flags and environment alike.**
`aiusage --db /tmp/scratch.db report` must never bake `/tmp/scratch.db` into a
unit that then collects there forever, so any of --db/--config/--home skips
supervision entirely and falls back to the detached spawn (`cmd.autoInstall`).
The environment counts for a sharper reason: `config.PathEnvOverrides` reports
which of AIUSAGE_DB, AIUSAGE_HOME, XDG_DATA_HOME, XDG_STATE_HOME,
XDG_CONFIG_HOME and **HOME** moved a path, and a unit does NOT inherit the
installing shell's environment — an install made under one of them writes
ReadWritePaths for the overridden directories while its ExecStart carries no
flags at all, i.e. a daemon sandboxed for one database and collecting into
another. HOME is the worst of them and the only one that is never absent, so
being set cannot be its test: it is compared against the account's own home
(`os/user`, which the environment cannot move) and only a difference counts, an
unresolvable account home included. It moves every derived path at once —
database, state, config, and the unit directory itself — while the unit NAME
stays a fixed constant, so a sandboxed-HOME run used to query the machine's REAL
`aiusage-collect.service`, find it running, and manage a sandbox file while
believing itself supervised. `cmd.discoveryEnv` is the same trap from the other
side: CLAUDE_CONFIG_DIR, CODEX_HOME, COPILOT_OTEL_FILE_EXPORTER_PATH,
HERMES_HOME and OPENCODE_DATA_DIR move what the adapters READ, have no flag to
forward either, and suppress the install the same way (a test parses the adapter
sources and fails on a variable the list has not been taught about). --interval
and AIUSAGE_INTERVAL are exempt from the refusal — clamped, and they name no
path — but exempt is not the same as permanent: the automatic install bakes NO
flags at all (`cmd.autoArgs`), so `aiusage --interval 61 today` cannot leave a
unit polling at 61 seconds forever. The explicit `setup` command has the
opposite rule — it bakes whatever flags it was given, because being asked is the
difference — and prints one note per environment override it cannot bake (there
is no flag for a state directory, and none for an adapter's discovery root).

**The automatic install reports itself once.** Installing, enabling and starting
a long-lived service is not a side effect to perform in silence behind
`aiusage today`, so a run that CHANGED the machine prints its account to stderr
(`service.Result.Changed`, `cmd.reportSupervision`). A run that changed nothing
prints nothing: the steady state is a dozen report commands a day finding
everything in place. Lines that explain something which did NOT happen — a
linger refusal, a unit that could not be written — do not count as a change;
they ride along with the install that first produced them and are silent
afterwards. A failure is not routed there either: it keeps the single warning
line the CLI has always printed before falling back.

**Everything degrades, and every wait is bounded.** Availability is `systemctl`
on PATH *plus* a successful `systemctl --user show-environment` (what actually
works over ssh — testing XDG_RUNTIME_DIR gets it wrong both ways), detected once
per process. No user manager, a refusal, a timeout: the CLI silently spawns the
detached background daemon it always did, prints at most one warning line, and
runs the command the user asked for. Two separate bounds hold that promise up.
Per call, `service.DefaultTimeout` (5s) kills the command and `WaitDelay` (500ms)
force-closes its pipes — without the second, `CombinedOutput` blocks until every
writer is gone, so a systemctl leaving a grandchild behind hangs the CLI forever
despite the context. Per phase, `cmd.supervisionBudget` (5s) is one parent
deadline over the WHOLE attempt: an install is several calls, and a manager
answering each of them slowly would otherwise add its latency once per call to a
report command. When the budget expires supervision is abandoned mid-sequence
and the fallback takes over. Every phase gets one, not just `ensureDaemon`:
`doctor`'s supervision block shares that same 5s (measured at 20s against a
manager answering in 4s each) and reports a unit it got no answer
about as state unknown rather than inventing "inactive, not enabled"
(`service.UnitStatus.StateKnown`); the explicit `setup` gets its own, far larger
`cmd.setupBudget` (30s), because the user asked for that one and is watching it,
and abandoning an install between writing a unit and starting it is worse than
waiting. A command that ran out of time SAYS so — `timed out after 5s` for the
per-call bound, `the supervision deadline expired` for the phase — rather than
reporting the signal that killed it (`signal: killed` names the mechanism and
hides the cause).

**Version sync restarts only what is ACTIVE.** Under systemd a build mismatch is
resolved with `systemctl --user restart`, not by killing a process the manager
would start again anyway. An INACTIVE unit is never started by that path: it is
either one the user stopped deliberately or one sitting beside a daemon that was
started some other way, and a second collector against a single-holder lock is
worse than a stale one. `Manager.Restart` reports whether the collection unit
was in fact what it restarted, so a caller that gets false knows supervision did
not handle the mismatch and it still owns the stop-and-respawn.

**Enable and start are two calls, and the second can fail.** An enable that this
install performed is rolled back when the start then fails: a unit left enabled
but not started comes up at the next login against the collection lock the
fallback daemon is by then holding. `is-enabled` is matched exactly, so
`enabled-runtime` (enabled until the next reboot, and no longer) is not enabled
and gets a persistent enable.

The unit carries `NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict`,
`ProtectKernelTunables`, `ProtectControlGroups`, `RestrictSUIDSGID` and explicit
`ReadWritePaths`. Honest caveat, measured on this host: for **user** units on
Ubuntu 24.04 the namespace-building directives (ProtectSystem, PrivateTmp,
ProtectKernelTunables, ProtectControlGroups) are inert, because
`kernel.apparmor_restrict_unprivileged_userns=1` stops `systemd --user` building
the namespace and it silently carries on without it. They are kept because they
are real on hosts without that restriction, and because the ReadWritePaths they
imply document what the daemon is allowed to touch.

Tests never touch a real systemd: `service.Manager` takes an injected `Runner`
and `UnitDir`, and `internal/cmd`'s TestMain pins the supervisor seam to a
refusing fake for the whole package.

## Time buckets

Timestamps are stored as UTC unix seconds. Grouping keys are formatted by
SQLite — `strftime(layout, event_time_unix, 'unixepoch', 'localtime')` in
internal/store/query.go — as lexically sortable local wall-clock strings
('%Y-%m-%d', '%Y-%m-%d %H', ...). The trap: SQLite's 'localtime' follows the
system timezone, not Go's time.Local, so mutating time.Local in a test moves
Go's formatting while SQLite's buckets stay put and the keys silently
disagree. Produce both sides of any bucket-key comparison through the same
store query (see TestScrubCompositionBracketsLocalDay in internal/tui).

usage_rollup folds the same way, from its 15-minute UTC buckets. Since the zone
is fixed when the process starts and cannot be moved from inside a test, the
+05:30 case re-executes the test binary with TZ set
(TestRollupFoldsSubHourEventsInKolkata in internal/store).

## TUI stack

charm.land v2 modules (bubbletea/v2, bubbles/v2, lipgloss/v2) — not the
github.com/charmbracelet v1 paths. Two v2 behaviours upstream does not
document:

- lipgloss v2 Width/Height are border-inclusive: a bordered style with
  Width(w) renders w cells total. Subtract the frame (border + padding) for
  content math.
- bubbles v2 table.SetRows clamps the cursor to -1 while the table is empty
  and never restores it when rows arrive. Reset the cursor after repopulating
  (see Browse.SetData in internal/tui/views/browse.go).

Charts are ntcharts (linear scale only). The hero trend is the exception: its
heat lanes are drawn cell by cell onto an ntcharts canvas
(internal/tui/views/trendrender.go), so ntcharts still supplies the widget, the
axes and the memo/scrub contract while the plot rectangle is ours. Every other
chart surface — the decade band, the leverage pivot, the sub-floor two-pane
fallback — is ntcharts braille as before.

## Migrations

schema.sql always describes the full latest schema; fresh databases are
created directly at store.SchemaVersion. Older databases run the ordered steps
in internal/store/migrate.go: additive statements only (ALTER TABLE ... ADD
COLUMN, CREATE ... IF NOT EXISTS — the no-UPDATE trigger blocks backfills),
one transaction per step with the version stamp as its last statement and the
version re-checked inside the transaction. A database newer than the binary
refuses to open; versions never move backwards. A new step also gets an entry
in migrate.go's version-ledger comment.

## Build & test

```sh
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./... -count=1
CGO_ENABLED=1 go test -race -count=1 ./...        # race gate; CI runs this
go test -run='^$' -bench=. -benchmem ./...        # benchmarks (tui, collect, adapters)
gofmt -l . ; go vet ./... ; staticcheck ./...     # all must be clean; CI gates each
```

gofmt everything. staticcheck runs in CI at a pinned version; U1000 (unused
code) fails the build.
