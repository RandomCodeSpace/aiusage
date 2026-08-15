# The harness matrix: ccusage ∪ tokscale ∪ aiusage, tiered

Research output for issue #70. Map: #68. Compiled 2026-08-15.

**Status: research. No production code changed.** Every claim below carries a
citation — an absolute path into a source tree read on this machine, or a URL.
Anything not verified this session is marked `unverified:`.

---

## 0. How to read this

One row per harness in the union `ccusage ∪ tokscale ∪ aiusage`, minus the two
the ticket excludes. Each row is meant to be enough to cut a build ticket from
without re-research: the exact path, the format, which surface wins, what makes
a record unique, whether the surface can be read without disturbing the writer,
which environment variables move it, and the traps a parser has to survive.

Vocabulary is CONTEXT.md's: **harness**, **surface**, **adapter**, **ledger**,
**live-tier**, **fixture-tier**, **unsupportable**, and the named bug classes
(**split-identity records**, **cumulative-vs-event counting**,
**assigned-not-accumulated columns**).

### Trust order

Sources, most trusted first. This ordering *changed answers* — see §2.

1. **The harness's own source.** What the writer writes is the fact. Cloned
   shallow into `/tmp/hm/` for this research: `charmbracelet/crush`,
   `block/goose`, `openai/codex`, `QwenLM/qwen-code`, `Aider-AI/aider`,
   `continuedev/continue`.
2. **This machine's aiusage adapters** — `internal/adapter/*`.
3. **Third-party parsers**: ccusage (`/tmp/hm/ccusage`, MIT) and tokscale
   (`/tmp/hm/tokscale`, MIT). These describe what *they read*, which is not the
   same claim as what the harness *writes*. Where they disagree with the
   harness's own source, the harness wins and the gap is recorded.
4. **agentsview** (`/tmp/hm/agentsview`, MIT) — schema ideas only. Its parsers
   are not trusted: see §7.3 for the measured reason.
5. Web (docs, release pages) for liveness, install and endpoint support.

### Source repo liveness, checked 2026-08-15 via `gh api`

| Repo | Archived | Last push | License | Stars |
|---|---|---|---|---|
| `ccusage/ccusage` | no | 2026-08-15T17:47:24Z | MIT (`/tmp/hm/ccusage/apps/ccusage/LICENSE:1`) | 17,951 |
| `junhoyeo/tokscale` | no | 2026-08-15T16:56:28Z | MIT | 4,968 |
| `kenn-io/agentsview` | no | 2026-08-15T17:36:48Z | MIT | 5,015 |

Clone heads: ccusage `852aebd`, tokscale `25df658`, agentsview `a5cb40f`.

### Union arithmetic

- tokscale ships **47** clients (`ClientId` 0..46,
  `/tmp/hm/tokscale/crates/tokscale-core/src/clients.rs:324-957`).
- ccusage ships **16** adapters (`/tmp/hm/ccusage/rust/adapters/`): amp, claude,
  codebuff, codex, copilot, droid, gemini, goose, grok, hermes, kilo, kimi,
  openclaw, opencode, pi, qwen. **All 16 are a strict subset of tokscale's 47.**
- aiusage ships **6** adapters (`internal/cmd/root.go:302-308`): claudecode,
  codex, copilot, opencode, hermes, agy. `agy` is Antigravity CLI; all 6 are in
  tokscale's 47.

So the union **is** tokscale's 47. Minus Roo Code and Amp (§6): **45 rows.**

---

## 1. Verdict on the ticket's trap catalog

The catalog was handed over as "measured/sourced 2026-08-15 surveys — verify
against parser source where cheap". Verifying against *parser* source alone
would have refuted four of the nine items. Verifying against the *harness's*
source confirmed them and exposed that both third-party parsers are behind the
harness. That is the single most consequential methodological finding here.

| # | Catalog claim | Verdict | Decided by |
|---|---|---|---|
| 1 | Crush token columns assigned-not-accumulated; cost-only is honest | ✅ **Confirmed, and worse than stated** | crush source |
| 2 | Continue writes tokenizer estimates, real usage deliberately unwritten | ✅ **Confirmed exactly** | continue source |
| 3 | Aider needs `--analytics-log` JSONL; history counts k-abbreviated; `--no-analytics` still writes it | ✅ **Confirmed, all three** | aider source |
| 4 | Droid sidecar cumulative, no per-turn, no dedup key | ✅ **Confirmed ×3** | tokscale + ccusage agree |
| 5 | Codex `.jsonl.zst`, off by default, nobody reads it | ✅ **Confirmed ×3** | codex source |
| 5b | Codex "Ultra-fork" interleaved cumulative lineages | ⚠️ **Real mechanism, wrong name** | codex + ccusage |
| 6 | Qwen ships `~/.qwen/usage/token-usage-YYYY-MM.jsonl`, writer-local buckets | ✅ **Confirmed verbatim in the harness's own doc comment** | qwen-code source |
| 7 | Goose `usage_ledger` with `cost_source` and `carried_forward` rows | ✅ **Confirmed**, but the double-count rule is inverted | goose source |
| 8 | opencode SQLite authoritative, JSON tree frozen | ⚠️ **Half right** — SQLite is primary, JSON tree is live fallback | ccusage + aiusage |
| 9 | Phone-home defaults per tool | see the Privacy column and §9 | mixed |

Detail, in the order that matters:

### 1.1 Crush — confirmed, and the mechanism is uglier than the claim

From the harness, not a parser.
`/tmp/hm/crush/internal/agent/agent.go:1938-1945`:

```go
func updateSessionTokenCounters(session *session.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	if promptTokens := usage.InputTokens + usage.CacheReadTokens; promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}
```

`=`, not `+=`. Eleven lines above it, `/tmp/hm/crush/internal/agent/agent.go:1934`
is `session.Cost += cost` — cost **is** accumulated. The catalog's "cost-only is
honest" is exactly right, from the writer's own code.

Two aggravations the catalog did not have:

- **Summarization zeroes the column.** `/tmp/hm/crush/internal/agent/agent.go:1456-1457`
  sets `currentSession.CompletionTokens = summaryCompletionTokens(...)` and
  `currentSession.PromptTokens = 0` before saving. A compacted session reports a
  prompt total of zero.
- **A second writer path *does* accumulate.** The title-generation call goes
  through `UpdateTitleAndUsage`
  (`/tmp/hm/crush/internal/agent/agent.go:1866`), whose SQL is
  `prompt_tokens = prompt_tokens + ?`
  (`/tmp/hm/crush/internal/db/sessions.sql.go:248-252`). So the column is
  neither a running total nor a last value consistently — it is a last value
  that a side channel occasionally increments. It cannot be read as either.

There is **no per-message token store to fall back to**: `messages.parts` is a
JSON blob whose part types are Text / ToolCall / ToolResult / Finish /
ShellCommand, and `Finish` carries `Reason, Time, Message, Details` and nothing
numeric (`/tmp/hm/crush/internal/message/content.go:129-134`). Verified against
this machine's own `~/.crush/crush.db` schema read read-only: `sessions` has
`prompt_tokens`, `completion_tokens`, `cost`; `messages` has no token column.

tokscale reached the same operational conclusion by a different route and says
so out loud — *"Crush is COST-ONLY... a token-count report showing 0 tokens for
crush is EXPECTED behavior, NOT a bug"*
(`/tmp/hm/tokscale/crates/tokscale-core/src/sessions/crush.rs:3-12`) — and never
`SELECT`s a token column
(`/tmp/hm/tokscale/crates/tokscale-core/src/sessions/crush.rs:143-149`).

### 1.2 Goose — the ledger exists; the double-count rule is the other way round

`usage_ledger` is real, and so are both named columns. From
`/tmp/hm/goose/crates/goose/src/session/session_manager.rs:1036-1050`:

```sql
CREATE TABLE IF NOT EXISTS usage_ledger (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    created_timestamp INTEGER NOT NULL,
    model TEXT,
    input_tokens INTEGER, output_tokens INTEGER, total_tokens INTEGER,
    cache_read_tokens INTEGER, cache_write_tokens INTEGER,
    cost REAL, cost_source TEXT, is_compaction INTEGER DEFAULT 0
)
```

`cost_source` takes `'provider_reported'` / `'estimated'`
(`.../session_manager.rs:865-868`) and `'carried_forward'`
(`.../session_manager.rs:2183`). `carried_forward` rows are inserted by
`record_usage_metrics` (`.../session_manager.rs:2163-2208`) as
`MAX(sessions.accumulated_* - SUM(usage_ledger.*), 0)`, guarded by a `WHERE`
that only fires when the accumulator is ahead of the ledger.

**They are the gap, not a duplicate.** `SUM(usage_ledger) == sessions.accumulated_*`
by construction. Filtering `carried_forward` out — the naive reading of "do not
double-count" — *undercounts*. The real double-count is reading the ledger
**and** `sessions.accumulated_*`, or treating `sessions.total_tokens` as a total
at all: the same `UPDATE` assigns `total_tokens = ?` from the current turn while
incrementing `accumulated_total_tokens` (`.../session_manager.rs:2213-2226`).
Goose therefore carries the assigned-not-accumulated trap **and** ships the
correct column beside it.

Neither third-party parser reads the ledger. ccusage queries `sessions` and
prefers `accumulated_*` (`/tmp/hm/ccusage/rust/adapters/goose/src/parser.rs:32-40`),
inventing reasoning as `total − (input + output)`
(`.../goose/src/parser.rs:45`). tokscale does the same and labels the invention
honestly: *"INFERRED, not a real field... a best-effort estimate, not a measured
count"* (`/tmp/hm/tokscale/crates/tokscale-core/src/sessions/goose.rs:198-206`).
Both therefore stamp a whole session's lifetime on its `created_at` instant. The
ledger has `created_timestamp` per row and `is_compaction`, i.e. everything
needed to do it properly. **Reading `usage_ledger` would put aiusage ahead of
both.**

Dedup identity in the ledger is the autoincrement `id`; `created_timestamp` is
`strftime('%s','now')` — write time, second resolution — so two turns in one
second are separable only by rowid. Rowid is monotonic, which makes it a clean
checkpoint watermark.

### 1.3 Qwen — the ledger exists, and its doc comment is the trap

`/tmp/hm/qwen/packages/core/src/services/tokenUsageService.ts:17-19,192,199,386`:
path is `Storage.getRuntimeBaseDir()/usage/token-usage-<localMonth>.jsonl`.
The record (`.../tokenUsageService.ts:26-53`) carries `schemaVersion`,
`id` (a `randomUUID()`, `.../tokenUsageService.ts:242`), `timestamp`,
`sessionId`, `model`, `authType`, `source`, `inputTokens`, `outputTokens`,
`cachedTokens`, `thoughtsTokens`, `totalTokens`, `apiDurationMs` — and:

```
  /**
   * Calendar date in the local timezone of the process that wrote this record.
   * Records written from different timezones keep their original local bucket.
   */
  localDate: string;
```

The catalog's "writer-local calendar buckets" is the harness's own words. It
bites twice: the **file** is named for `record.localMonth`
(`.../tokenUsageService.ts:386`), and qwen's own `/stats` filters on
`localDate`/`localMonth` (`.../tokenUsageService.ts:454-455`).

**Survivable, and the escape hatch is already there.** `timestamp` is an ISO
8601 instant (`.../tokenUsageService.ts:235`), so a reader that ignores
`localDate`/`localMonth` and buckets from `timestamp` gets it right —
exactly aiusage's rule that grouping keys are derived on read. And `id` is a
UUID, i.e. a genuine dedup key, which almost nothing else in this matrix has.

**Neither parser opens it.** ccusage reads
`${QWEN_DATA_DIR:-~/.qwen}/projects/<project>/chats/*.jsonl`
(`/tmp/hm/ccusage/rust/adapters/qwen/src/paths.rs:32-60`); tokscale reads
`~/.qwen/projects/**/*.jsonl` (`/tmp/hm/tokscale/crates/tokscale-core/src/clients.rs:513-522`).
Both build content-tuple dedup keys because the transcript has no id, while the
authoritative ledger next door hands out UUIDs.

Two Qwen discovery variables exist that **tokscale's registry does not know
about at all**: `QWEN_HOME` (`/tmp/hm/qwen/packages/core/src/config/storage.ts:183-192`)
and `QWEN_RUNTIME_DIR` (`.../storage.ts:162-181`, which takes precedence).
tokscale's Qwen entry is `PathRoot::Home` with no env var.

### 1.4 Codex — compression confirmed, "Ultra" is a misnomer

`.jsonl.zst` is real: a whole module,
`/tmp/hm/codex/codex-rs/rollout/src/compression.rs`, with
*"Opens a rollout line reader that transparently handles plain `.jsonl` and
`.jsonl.zst` files"* (`:41`) and a background worker that compresses **cold**
rollouts (`:24-29`).

Off by default, verifiably:
`/tmp/hm/codex/codex-rs/features/src/lib.rs:975-979` —

```rust
FeatureSpec {
    id: Feature::LocalThreadStoreCompression,
    key: "local_thread_store_compression",
    stage: Stage::UnderDevelopment,
    default_enabled: false,
},
```

gated at `/tmp/hm/codex/codex-rs/core/src/thread_manager.rs:371-373`.

"Nobody reads it" is also confirmed: `grep -rni zst` over
`/tmp/hm/ccusage/rust` returns zero hits and `zstd` is absent from ccusage's
`Cargo.lock`; aiusage's codex adapter matches `.jsonl` by exact suffix
(`internal/adapter/codex/codex.go:121`). **When that feature graduates, both
tools silently stop seeing old sessions** — not an error, a shrinking history.

**"Ultra-fork" is the wrong name for a real thing.** `Ultra` in codex is
`ReasoningEffort::Ultra`, a reasoning level
(`/tmp/hm/codex/codex-rs/state/src/extract.rs:601`). The real mechanism is
*forked / subagent* threads: a fork replays the parent's history verbatim, so
its cumulative lineage interleaves with the parent's.
`session_meta.payload.forked_from_id` and
`/source/subagent/thread_spawn/parent_thread_id` are the handles
(`/tmp/hm/ccusage/rust/adapters/codex/src/replay.rs:248-259`); ccusage builds a
whole `CodexReplayPlan` around it (`.../replay.rs:31-112`), truncating the
parent prefix at the fork instant because *"Usage the parent recorded after the
fork was never replayed, so it must not mask the child's own events"*
(`.../replay.rs:101-109`).

aiusage is accidentally immune to the *exact-duplicate* half: its codex dedup key
is `sha1(ts|model|input|cached|output|reasoning|total)` with session id
deliberately excluded (`internal/adapter/codex/codex.go:526-540`), so a
replayed `token_count` record collapses onto the original. It is **not** immune
to the derived-delta half: `internal/adapter/codex/codex.go:1-6` derives deltas
"against a per-file running previous total", and a fork's file starts its
cumulative lineage mid-history.

### 1.5 Aider — confirmed on all three sub-claims

`--analytics-log ANALYTICS_LOG_FILE` exists
(`/tmp/hm/aider/aider/args.py:574-578`) and writes newline-delimited JSON
objects (`/tmp/hm/aider/aider/analytics.py:242-254`).

`--no-analytics` and `--analytics-disable` do **not** stop it, and the code
shows why plainly. `Analytics.__init__` assigns `self.logfile = logfile` at
`/tmp/hm/aider/aider/analytics.py:80`, *then* calls `self.disable(...)` at `:86`;
`disable()` clears `self.mp` and `self.ph` and nothing else
(`.../analytics.py:110-118`). `event()` returns early only when all three sinks
are falsy (`.../analytics.py:214`). So the local file keeps being written while
the network sinks are dead. That is the catalog's claim, confirmed.

The payload is good: `message_send` carries `main_model`, `edit_format`,
`prompt_tokens`, `completion_tokens`, `total_tokens`, `cost`, `total_cost`
(`/tmp/hm/aider/aider/coders/base_coder.py:2112-2122`), plus `user_id` and
`"time": int(time.time())` (`.../analytics.py:245-249`).

Two caveats the catalog does not carry:

- **The counts are silently mixed.** `calculate_and_show_tokens_and_cost` uses
  `completion.usage.prompt_tokens` when the provider returned usage and
  `self.main_model.token_count(messages)` — a tokenizer estimate — when it did
  not (`/tmp/hm/aider/aider/coders/base_coder.py:2000-2018`). **No field in the
  emitted event distinguishes the two.**
- **The cache split is computed and then dropped.** `cache_hit_tokens` and
  `cache_write_tokens` are read at `.../base_coder.py:2003-2006` and are not
  among the fields sent to `event()`.

**The k-abbreviation half is also confirmed, and it is exactly why the history
file is not a substitute.** `aider/utils.py::format_tokens` renders `12345` as
`"12k"`:

```python
def format_tokens(count):
    if count < 1000:   return f"{count}"
    elif count < 10000: return f"{count / 1000:.1f}k"
    else:               return f"{round(count / 1000)}k"
```

`calculate_and_show_tokens_and_cost` builds `f"Tokens: {format_tokens(...)} sent"`,
`show_usage_report` hands it to `io.tool_output()`, and `io.tool_output()` calls
`append_chat_history(..., blockquote=True)` onto `.aider.chat.history.md`. Below
1,000 it is exact; 1,000–9,999 keeps one decimal; **at or above 10,000 it is
rounded to the nearest thousand** — worthless as a ledger. That is the surface
agentsview reads (§7.3), and it is why `--analytics-log` is the only honest
Aider surface.

### 1.6 opencode — SQLite is primary, the JSON tree is not frozen

ccusage: *"SQLite databases are the primary source. Legacy JSON messages under
`storage/message/` are loaded as a fallback and deduplicated behind database
rows."* (`/tmp/hm/ccusage/rust/adapters/opencode/src/README.md:10`) — and a
JSON-only install with no database is still a valid source
(`.../opencode/src/loader.rs:129-134`).

aiusage already reads both and collapses them on `opencode|<message id>`, the DB
winning because it is discovered first (`internal/adapter/opencode/opencode.go:1-30`).
That is the correct handling of a "frozen" tree that is not actually frozen.

---

## 2. Tier policy

Under the **no-new-paid-signups** policy:

- **live** — the harness installs on Linux and can produce a real session
  without a new paid account: BYO existing key, a free tier, or (the big lever)
  an OpenAI-compatible `base_url` pointed at Ollama. An adapter reaches
  live-tier only once verified against sessions actually run on such an install.
- **fixture** — the surface format is derivable from trusted source (ccusage /
  tokscale / the harness itself) but the harness cannot be exercised here:
  vendor-only auth, paid subscription, not on Linux, or simply unobtainable.
  Ships labelled unverified-against-live until a real log promotes it.
- **unsupportable** — no locally readable *usage* surface. Three distinct
  shapes qualify, and they are worth naming because the matrix has all three:
  1. **Cloud-only** — the "local file" a third-party tool reads is that tool's
     own cache, filled from the vendor's authenticated API. Reading it requires
     being that tool. (cursor, warp, trae)
  2. **RPC-only** — data exists locally but only inside a running process.
     (antigravity IDE)
  3. **Estimates-only** — a local transcript exists and carries no usage; the
     numbers a parser reports are `chars / 4`. Under CONTEXT.md's *unpriced ≠ $0*
     and *unattributed ≠ free* doctrine, an estimate is not usage.
     (commandcode, freebuff, kiro)

`live` is a *ceiling* claim about feasibility, not a claim that verification has
happened. Nothing in this document has been verified against a live session
except the six aiusage already ships and the local surfaces noted in §4.

### Tallies

| Tier | Count | Rows |
|---|---|---|
| **live** | **19** | 1–9, 11, 13–16, 18, 19, 37, 40, 41 |
| **fixture** | **19** | 10, 12, 17, 20–23, 25, 27–36, 38 |
| **unsupportable** | **7** | 24, 26, 39, 42, 43, 44, 45 |
| **total rows** | **45** | |

Excluded, with evidence: 2 (§5).

The unsupportable seven split by shape: cloud-only 3 (cursor, warp, trae),
RPC-only 1 (antigravity IDE), estimates-only 3 (commandcode, freebuff, kiro).

---

## 3. The matrix

Columns are abbreviated for width; every row has a full block in §4.

- **Fmt**: JSONL / JSON / SQLite / NDJSON / CSV / zstd-JSONL / OTEL.
- **Dedup**: `id` = a provider-supplied stable identifier; `pos` = position
  (row id, line index, array ordinal); `hash` = content tuple; `—` = none.
- **RO**: read-only readable without disturbing the writer.
- **Endpoint**: accepts an OpenAI-compatible base URL → can be driven by Ollama.
- **P/H**: phone-home default of the *harness*.

| # | Harness | Authoritative surface | Fmt | Dedup | RO | Endpoint | Tier | Discovery env |
|---|---|---|---|---|---|---|---|---|
| 1 | Claude Code | `${CLAUDE_CONFIG_DIR:-~/.claude}/projects/**/*.jsonl` | JSONL | `id` (message.id + requestId) | yes | proxy only (`ANTHROPIC_BASE_URL`, Messages wire) | **live** | `CLAUDE_CONFIG_DIR`, `XDG_CONFIG_HOME` |
| 2 | Codex CLI | `${CODEX_HOME:-~/.codex}/{sessions,archived_sessions}/**/*.jsonl` | JSONL (+`.zst`) | `hash` | yes | **yes** (`[model_providers.*].base_url`) | **live** | `CODEX_HOME` |
| 3 | GitHub Copilot CLI | `~/.copilot/otel/**/*.jsonl` ∪ `$COPILOT_OTEL_FILE_EXPORTER_PATH` | OTEL JSONL | `id` (traceId+spanId) → `pos` | yes | **yes** (`COPILOT_PROVIDER_BASE_URL`) | **live** | `COPILOT_OTEL_FILE_EXPORTER_PATH` |
| 4 | opencode | `${OPENCODE_DATA_DIR:-~/.local/share/opencode}/opencode.db` | SQLite | `id` (message.id) | yes (`mode=ro`, no `immutable`) | yes | **live** | `OPENCODE_DATA_DIR`, `XDG_DATA_HOME`, `OPENCODE_CONFIG*` |
| 5 | Hermes | `${HERMES_HOME:-~/.hermes}/state.db` → `sessions` | SQLite | `id` (session id) | yes (`mode=ro`, no `immutable`) | **yes** (`provider: custom`, `CUSTOM_BASE_URL`, `OLLAMA_BASE_URL`) | **live** | `HERMES_HOME`, `LOCALAPPDATA` |
| 6 | Antigravity CLI | `${GEMINI_CLI_HOME:-~/.gemini}/antigravity-cli/conversations/*.db` | SQLite + protobuf | `id` (responseId) | yes | no | **live** | `GEMINI_CLI_HOME` |
| 7 | Gemini CLI | `${GEMINI_CLI_HOME:-~/.gemini}/tmp/**/*.{json,jsonl}` | JSON/JSONL | `id` → none | yes | no | **live** | `GEMINI_CLI_HOME`, `GEMINI_DATA_DIR` |
| 8 | Crush | `<project>/.crush/crush.db` → `sessions.cost` (cost only) | SQLite | — | yes | **yes** (`base_url`) | **live** (cost-only) | `CRUSH_GLOBAL_DATA`, `XDG_DATA_HOME`, `LOCALAPPDATA` |
| 9 | Goose | `${GOOSE_PATH_ROOT:-<xdg-data>/goose}/sessions/sessions.db` → `usage_ledger` | SQLite | `pos` (rowid) | yes | **yes** | **live** | `GOOSE_PATH_ROOT`, `XDG_DATA_HOME` |
| 10 | Droid (Factory) | `${DROID_SESSIONS_DIR:-~/.factory/sessions}/**/*.settings.json` | JSON | `pos` (file stem) | yes | **yes** (`customModels[].baseUrl`) | fixture (paid-only vendor) | `DROID_SESSIONS_DIR` |
| 11 | Zed | `<xdg-data>/zed/threads/threads.db` → `threads.data` | SQLite + zstd JSON | `id` (thread id) | yes | **yes** | **live** | `XDG_DATA_HOME` |
| 12 | Kilo CLI | `<xdg-data>/kilo/kilo.db` → `message` | SQLite + JSON | `id` → `pos` | yes | unverified | fixture | `XDG_DATA_HOME` |
| 13 | Kilo Code (VS Code) | `~/.config/Code/User/globalStorage/kilocode.kilo-code/tasks/*/ui_messages.json` | JSON | — | yes | **yes** | **live** | none (hardcoded path) |
| 14 | Cline | VS Code `.../saoudrizwan.claude-dev/tasks/*/ui_messages.json`; CLI `${CLINE_*}/sessions/*/*.messages.json` | JSON | CLI `id`→`pos`; VS Code — | yes | **yes** | **live** | `CLINE_SESSION_DATA_DIR`, `CLINE_DATA_DIR`, `CLINE_DIR`, `APPDATA` |
| 15 | Qwen Code | `${QWEN_RUNTIME_DIR:-${QWEN_HOME:-~/.qwen}}/usage/token-usage-<localMonth>.jsonl` | JSONL | `id` (UUID) | yes | **yes** | **live** | `QWEN_HOME`, `QWEN_RUNTIME_DIR` |
| 16 | Kimi Code / Kimi CLI | `~/.kimi/sessions/**/wire.jsonl`; `${KIMI_CODE_HOME:-~/.kimi-code}/sessions/**/wire.jsonl` | JSONL | `id` (message_id) → none | yes | **yes** (`type = openai_legacy`, `KIMI_MODEL_BASE_URL`) | **live** | `KIMI_DATA_DIR` (ccusage), `KIMI_CODE_HOME` (tokscale) |
| 17 | Grok CLI (Grok Build) | `${GROK_HOME:-~/.grok}/sessions/**/updates.jsonl` + `logs/unified.jsonl` | JSONL | `id` (eventId, **not unique**) → `hash` | yes | **yes** (`base_url`, `api_backend = chat_completions`) | fixture (vendor auth gate unverified) | `GROK_HOME` |
| 18 | Pi | `~/.pi/agent/sessions/**/*.jsonl` (+ `~/.omp/...`) | JSONL | — | yes | **yes** (`providers.*.baseUrl`; `/login llama.cpp`) | **live** | `PI_AGENT_DIR` (ccusage) |
| 19 | OpenClaw | `~/.openclaw/agents/**/*.jsonl*` (+3 legacy roots) | JSONL | — (ccusage: `hash`) | yes | **yes** (`providers.*.baseUrl`, `api: ollama`) | **live** | `OPENCLAW_DIR` (ccusage) |
| 20 | Codebuff | `${CODEBUFF_DATA_DIR:-~/.config/manicode}/projects/**/chat-messages.json` | JSON | `id` → `hash`+`pos` | yes | no | fixture | `CODEBUFF_DATA_DIR` |
| 21 | Mux | `~/.mux/sessions/*/session-usage.json` | JSON | `hash` (workspace+model) | yes | unverified | fixture | none |
| 22 | GJC | `${GJC_CODING_AGENT_DIR:-~/.gjc/agent}/sessions/**/*.jsonl` | JSONL | `id` → `hash` (sha256 of line) | yes | unverified | fixture | `GJC_CODING_AGENT_DIR`, `GJC_CONFIG_DIR`, `PI_CONFIG_DIR`, `XDG_DATA_HOME` |
| 23 | JCode | `${JCODE_HOME:-~/.jcode}/sessions/session_*.json` + `.journal.jsonl` | JSON + JSONL | `id` → `pos` | yes | unverified | fixture | `JCODE_HOME` |
| 24 | CommandCode | `~/.commandcode/projects/**/*.jsonl` (**no usage in it**) | JSONL | `pos` | yes | no | **unsupportable** | none |
| 25 | MiMo Code | `<xdg-data>/mimocode/*.db` → `message` | SQLite + JSON | `id` → `pos` | yes | unverified | fixture | `XDG_DATA_HOME` |
| 26 | Antigravity (IDE) | none on disk — RPC to a running language server | — | `id` (responseId) | n/a | no | **unsupportable** | n/a |
| 27 | Junie (JetBrains) | `~/.junie/sessions/**/events.jsonl` | JSONL | `hash` | yes | no | fixture | none |
| 28 | ZCode | `~/.zcode/cli/db/db.sqlite` → `model_usage` | SQLite | `pos` (rowid) | yes | no | fixture | none |
| 29 | OpenCodeReview | `~/.opencodereview/sessions/**/*.jsonl` | JSONL | `hash` → `pos` (line) | yes | unverified | fixture | none |
| 30 | CodeBuddy (Tencent) | `~/.codebuddy/projects/**/*.jsonl` (+ extension `.log`) | JSONL + text log | `id` → **none** | yes | unverified | fixture | none |
| 31 | WorkBuddy | `~/.workbuddy/projects/**/*.jsonl` (SQLite is the fallback) | JSONL + SQLite | `id`; SQLite `hash` | yes | unverified | fixture | none |
| 32 | Devin CLI | `<xdg-data>/devin/cli/sessions.db` → `message_nodes` | SQLite + JSON | `pos` (rowid) | yes | no | fixture | `XDG_DATA_HOME` |
| 33 | Devin Desktop | `~/Library/.../Devin/User/acp-events/*.ndjson` | NDJSON | `pos` (path+line) | yes | no | fixture | `APPDATA`, `XDG_DATA_HOME` (lookup DB) |
| 34 | Senpi (OmO) | `${SENPI_CODING_AGENT_DIR:-~/.senpi/agent}/sessions/**/*.jsonl` | JSONL | — | yes | unverified | fixture | `SENPI_CODING_AGENT_DIR`, `SENPI_CODING_AGENT_SESSION_DIR` |
| 35 | Augment / Auggie | `~/.augment/sessions/*.json` | JSON | `id` → `pos` | yes | no | fixture | none |
| 36 | Kimchi | `${KIMCHI_CODING_AGENT_DIR:-~/.config/kimchi/harness}/sessions/**/*.jsonl` | JSONL | `id` | yes | unverified | fixture | `KIMCHI_CODING_AGENT_DIR` |
| 37 | Reasonix | `${REASONIX_STATE_HOME:-${REASONIX_HOME:-~/.reasonix}}/stats/YYYY-MM-DD.jsonl` | JSONL | `pos` (path+line) | yes | **yes** | **live** | `REASONIX_STATE_HOME`, `REASONIX_HOME` |
| 38 | Prime Agent | `${PRIME_AGENT_*:-~/.prime/agent}/sessions/**/*.jsonl` + `session-artifacts/` | JSONL | `id` (responseId) → `hash` | yes | unverified | fixture | `PRIME_AGENT_SESSION_DIR`, `PRIME_AGENT_CODING_AGENT_SESSION_DIR`, `PRIME_AGENT_CODING_AGENT_DIR` |
| 39 | Freebuff | `${FREEBUFF_DATA_DIR:-~/.config/manicode}/projects/**/chat-messages.json` (**no usage**) | JSON | `pos` | yes | no | **unsupportable** | `FREEBUFF_DATA_DIR`, `CODEBUFF_DATA_DIR` |
| 40 | Cherry Studio | `<app-data>/CherryStudio/Data/Agents/.claude/projects/**/*.jsonl` | JSONL | union-find over `requestId`/`message.id`/`uuid` | yes | **yes** | **live** | `APPDATA` (Windows only) |
| 41 | DSH (DeepSeek Harness) | `${DSH_HOME:-~/.dsh}/sessions/**/session.jsonl[.zstd]` | zstd-framed JSONL | `id` (message.id) + `hash` | yes | **yes** | **live** | `DSH_HOME` |
| 42 | Cursor | none — tokscale's own cache of an authenticated cloud API | CSV | — | n/a | n/a | **unsupportable** | n/a |
| 43 | Warp | none — tokscale's own cache; **no tokens at all**, requests+spend only | JSON | — | n/a | n/a | **unsupportable** | n/a |
| 44 | Trae | none — tokscale's own dump of Trae's usage API | JSON | `id` (session+usage_time) | n/a | n/a | **unsupportable** | n/a |
| 45 | Kiro (AWS) | 4 surfaces, **all token counts are estimates** | JSON/JSONL/SQLite | `pos` | yes | no | **unsupportable** | `APPDATA` |

Live-tier rows are exactly 1–9, 11, 13–16, 18, 19, 37, 40, 41 (19 of them). Every
other supportable row is fixture-tier: the format is derivable from trusted
source, but the harness cannot be exercised here without a new paid signup, is
not installable on Linux, or could not be obtained at all.

**Rows 12 and 13 are one project, two surfaces.** Kilo Code
(`Kilo-Org/kilocode`, not archived, pushed 2026-08-15T17:58:12Z per `gh api`)
ships both the VS Code extension (row 13) and a CLI whose store is
`<xdg-data>/kilo/kilo.db` (row 12). tokscale registers them as two client ids —
`kilo` at index 14 and `kilocode` at index 12
(`/tmp/hm/tokscale/crates/tokscale-core/src/clients.rs`) — because the surfaces
are genuinely different files with different formats. They are kept as two rows
for that reason, and a build ticket for either must know it is touching one
vendor.

---

## 4. Per-harness notes

Grouped by tier. Each block carries what the table cannot: the token field
mapping, why one surface beats another, and the traps a parser must survive.

Where a claim rests only on a third-party parser rather than on the harness's
own source, the citation says which parser. Line numbers are given where they
were read directly this session; a bare file path means the file was read but
the claim is a summary of it rather than a quotation.

### 4.1 The six aiusage already ships

**1. Claude Code** — `internal/adapter/claudecode/`.
Surface `${CLAUDE_CONFIG_DIR:-~/.claude}/projects/**/*.jsonl`, with
`~/.config/claude` probed ahead of `~/.claude`
(`internal/adapter/claudecode/claudecode.go:98-105`). Identity is
`(message.id, requestId)`; the deduper collapses usage keep-best and **unions**
activity and turn context across the records of one message
(`internal/adapter/claudecode/dedup.go:5-40`). This is the reference
implementation of the split-identity bug class: taking only the winner's blocks
dropped 19,682 of 60,832 local tool calls, non-uniformly (Read −45%, Bash −29%).
Traps: streamed responses repeating one usage object; sidechain replays sharing
a message id under a different requestId; compaction upstream. Telemetry is on
by default (`DISABLE_TELEMETRY=1`), which is a property of the harness, not of
the transcript.

**2. Codex CLI** — `internal/adapter/codex/`.
`${CODEX_HOME:-~/.codex}/sessions` plus `archived_sessions`
(`internal/adapter/codex/codex.go:31-34,89-96`). Prefers the per-turn
`info.last_token_usage` and otherwise derives a delta by saturating subtraction
against a per-file running total (`.../codex.go:1-10`). OpenAI semantics: cached
is a **subset** of input, so `Input = input − cached`, `CacheRead = cached`.
**No stable id anywhere in the surface** — the dedup key is
`sha1(ts|model|input|cached|output|reasoning|total)` with session id
deliberately excluded so branch-copied histories count once
(`.../codex.go:526-540`). Two open gaps, both real: `.jsonl.zst` is not matched
(`.../codex.go:121`, §1.4), and a timestamp-less `token_count` line falls back
to file mtime, which can move between polls and overcount
(`.../codex.go:544-550`, TODO in source). Activity is never cost-attributed:
zero of 261,938 local `token_count` records carry a `turn_id` while every
`function_call` does (`.../codex.go:360-375`).

**3. GitHub Copilot CLI** — `internal/adapter/copilot/`.
Usage exists **only** in the opt-in OTEL file export:
`~/.copilot/otel/**/*.jsonl` recursively, plus the single file named by
`COPILOT_OTEL_FILE_EXPORTER_PATH` (`internal/adapter/copilot/copilot.go:1-8,116,137`).
The session store and `events.jsonl` carry context-window gauges, not billing.
One model call appears from four vantage points (chat span, inference log,
agent-turn log, agent-summary span); the adapter keeps the highest-priority
record per shared `traceId` / `gen_ai.response.id`
(`.../copilot.go:10-13`). **Cumulative-vs-event counting, measured**: activity
must come from `execute_tool` SPANS, never from the metric dataPoints in the
same file — one tool call had produced 226 identical dataPoints, a 226×
inflation. Now that BYOK exists (`COPILOT_PROVIDER_BASE_URL`, 2026-04-07), the
same OTEL export can be produced against a local model, which is what promotes
this row to live without a Copilot seat beyond the free plan.

**4. opencode** — `internal/adapter/opencode/`.
Reads **both** `${OPENCODE_DATA_DIR:-~/.local/share/opencode}/opencode.db`
(table `message(id, session_id, data)`) and `storage/message/**/*.json`, and
collapses them on `opencode|<message id>`, the DB winning because it is
discovered first (`internal/adapter/opencode/opencode.go:1-30`). Reasoning is
**additive** here, not a subset of output: every local row satisfies
`total = input + output + reasoning + cache.read + cache.write`, and rows with
`reasoning > output` exist (`.../opencode.go:20-26`). Read-only discipline is
explicit and load-bearing: `mode=ro` plus `query_only(1)` and **never**
`immutable=1`, because opencode writes this database live and keeps a large WAL
an immutable reader cannot see (`.../opencode.go:30-35`). Upstream org renamed
`sst/opencode` → `anomalyco/opencode`; old URLs redirect.

**5. Hermes** — `internal/adapter/hermes/`.
`${HERMES_HOME:-~/.hermes}/state.db`; `HERMES_HOME` may hold a
**comma-separated list** of homes (`internal/adapter/hermes/hermes.go:34-35,66`).
This is an **aggregate** adapter, not event-level: a session row's counters grow
across polls, so it emits one `AggregateSnapshot` per session keyed by session
id and the collector appends the positive delta
(`.../hermes.go:1-12`). `immutable=1` is deliberately omitted — the note in
CLAUDE.md points here. Identified as `NousResearch/hermes-agent`, MIT, whose
docs name `~/.hermes/state.db` exactly; custom endpoints are first-class
(`provider: custom`, `CUSTOM_BASE_URL`, `OLLAMA_BASE_URL`), and the project
states it collects no telemetry.

**6. Antigravity CLI (`agy`)** — `internal/adapter/agy/`.
Scans three roots — `~/.gemini/antigravity-cli`, `~/.antigravitycli`,
`~/.cache/antigravity` — for usage-bearing `*.json`/`*.jsonl` and parses them
through the shared `geminishape` cumulative parser, tagged `tool="agy"`
(`internal/adapter/agy/agy.go:1-10,43-48`). **The adapter's own header records
that the live install emits no token usage at all**: the `.pb` conversation
blobs carry content only, so `Discover` finds nothing and returns an empty
source list with no error (`.../agy.go:10-17`). tokscale, by contrast, registers
`antigravity-cli` against `${GEMINI_CLI_HOME:-~/.gemini}/antigravity-cli/conversations/*.db`
— per-conversation **SQLite**, read directly with no RPC
(`/tmp/hm/tokscale/crates/tokscale-core/src/clients.rs`, client 31). **That is a
concrete, actionable disagreement**: tokscale names a surface aiusage's adapter
does not probe. See §10.

`geminishape` itself (`internal/adapter/geminishape/geminishape.go:1-32`) is the
cumulative-record parser: records for one turn re-emit growing running totals,
so it groups by id within a file and keeps the max snapshot per id — the
correct handling of the cumulative-vs-event class. `model.ToolGemini` is retired
(`internal/model/model.go:20-34`): Antigravity replaced the Gemini CLI, no
adapter collects it, and the identifier survives only because `usage_events` is
append-only.

### 4.2 Other live-tier rows

**7. Gemini CLI** — `${GEMINI_CLI_HOME:-~/.gemini}/tmp/**/*.{json,jsonl}`
(`clients.rs`, client 4). Apache-2.0, `v0.55.1` 2026-08-11. ccusage additionally
honours `GEMINI_DATA_DIR`. **No custom-endpoint support upstream** — the
documented auth is `GEMINI_API_KEY` / Vertex / ADC and no base-URL key exists;
the Qwen fork added one, upstream did not. Live only because a free OAuth tier
is still advertised, and that is contested: gemini-cli discussion #27274
announces free/Pro/Ultra service ending 2026-06-18 with Antigravity CLI as the
replacement, a date now past, while the README still advertises 1,000 req/day.
**unverified: which of the two is currently true.** Telemetry:
`privacy.usageStatisticsEnabled` defaults **true**; the separate OTel block
defaults false. `ToolGemini` is retired in aiusage, so a build ticket here is a
re-introduction, not a repair.

**8. Crush** — see §1.1 for the full mechanism. Surface is per-project
`<project>/.crush/crush.db`, located through
`<xdg-data>/crush/projects.json`, which is an index of `{path, data_dir,
last_accessed}` — verified on this machine, which has exactly one entry pointing
at `/home/dev/.crush`. **Cost only.** `sessions.cost` accumulates
(`/tmp/hm/crush/internal/agent/agent.go:1934`); the token columns do not. There
is no per-message token store to fall back to. FSL-1.1-MIT (source-available,
not OSI, converts to MIT after two years) — worth knowing before vendoring any
of it. Telemetry on by default; `CRUSH_DISABLE_METRICS=1`, honours
`DO_NOT_TRACK=1`.

**9. Goose** — see §1.2. `<data-dir>/sessions/sessions.db`, where the data dir
is `<xdg-data>/goose` (`/tmp/hm/goose/crates/goose/src/session/session_manager.rs:28-29,912-914`).
**Read `usage_ledger`, not `sessions`.** It has per-row `created_timestamp`,
`model`, the full five-way token split, `cost`, `cost_source` and
`is_compaction`; `sessions` has only lifetime accumulators stamped at
`created_at`. Watermark on the autoincrement `id`. Never sum the ledger *and*
`sessions.accumulated_*` — they are equal by construction. Never read
`sessions.total_tokens` as a total — it is the last turn only. Project moved
`block/goose` → **`aaif-goose/goose`** (Agentic AI Foundation), Apache-2.0,
v1.46.0 2026-08-12. Telemetry **off by default** (`GOOSE_TELEMETRY_ENABLED`),
one of the few. Custom endpoints first-class (`OPENAI_HOST` + `OPENAI_BASE_PATH`,
`OLLAMA_HOST`, `GOOSE_PROVIDER__HOST`).

**11. Zed** — `<xdg-data>/zed/threads/threads.db`, thread rows holding
zstd-compressed JSON (`clients.rs`, client 21). GPL-3.0-or-later with Apache-2.0
for marked components; GitHub reports `NOASSERTION`. Custom endpoints are
first-class (`language_models.openai_compatible.<id>.api_url`,
`language_models.ollama.api_url`), and no zed.dev account is needed with your
own key — but the shipped default `"default_model"` points at the hosted
`zed.dev` provider, so a verification run must switch it explicitly.
⚠️ **There is no headless mode**: the `zed` CLI has no agent or prompt flag
(request zed-industries/zed#59146 unanswered), so live verification means
driving the GUI. Two third-party sources disagree on whether the thread store is
LMDB or SQLite — **unverified**; tokscale says SQLite and tokscale has a working
parser, which is the stronger evidence. Telemetry on by default
(`telemetry.diagnostics` / `telemetry.metrics` both `true` in
`assets/settings/default.json`).

**13. Kilo Code (VS Code / JetBrains)** —
`~/.config/Code/User/globalStorage/kilocode.kilo-code/tasks/*/ui_messages.json`
(`clients.rs`, client 12). MIT, v7.4.22 2026-08-13, BYOK across 30+ providers
with an explicit "OpenAI Compatible" provider. **No stable per-message id in
`ui_messages.json`** — dedup falls back to content plus position, which makes
in-place edits of the file indistinguishable from new work. Telemetry on by
default with a hardcoded PostHog key and `let enabled = true`; the only opt-out
left is `KILO_TELEMETRY_LEVEL` set to anything but `all` (the UI checkbox was
removed in v5.6.0, issue #5825 closed as not planned).

**14. Cline** — two surfaces. VS Code:
`~/.config/Code/User/globalStorage/saoudrizwan.claude-dev/tasks/*/ui_messages.json`
(`clients.rs`, client 25). CLI: `${CLINE_SESSION_DATA_DIR}/sessions/*/*.messages.json`,
with `CLINE_DATA_DIR` and `CLINE_DIR` as fallbacks. Apache-2.0, extension
v4.1.10 / cli-v3.0.55, both 2026-08-14. BYOK via
`cline auth --provider openai-native --baseurl ...`. Telemetry is **opt-in**
(`TelemetrySetting` starts `"unset"` behind an Allow/Deny prompt) and defers to
VS Code's `telemetry.telemetryLevel: "off"` — one of the better postures here.
The CLI surface has message ids; the VS Code surface does not.

**15. Qwen Code** — see §1.3. **Read the ledger, not the transcript.**
`${QWEN_RUNTIME_DIR:-${QWEN_HOME:-~/.qwen}}/usage/token-usage-<localMonth>.jsonl`
carries a `randomUUID()` per record, an ISO `timestamp`, `sessionId`, `model`,
`authType`, `source`, and a five-way token split
(`/tmp/hm/qwen/packages/core/src/services/tokenUsageService.ts:26-53,192-199,242,386`).
Bucket from `timestamp`; treat `localDate`/`localMonth` as writer metadata, and
expect the **file name itself** to be writer-local. Apache-2.0, v0.21.12
2026-08-14. The free Qwen OAuth tier was **discontinued 2026-04-15**, so the
live path is `OPENAI_BASE_URL` at a local server. `privacy.usageStatisticsEnabled`
defaults **true** and ships to an Alibaba Cloud RUM endpoint.

**16. Kimi Code / Kimi CLI** — `MoonshotAI/kimi-code` (MIT, v0.36.1 2026-08-14)
supersedes `MoonshotAI/kimi-cli`, which official docs say is being wound down.
Surface `wire.jsonl` per session under `~/.kimi/sessions/**` (tokscale,
`clients.rs` client 9) or `${KIMI_CODE_HOME:-~/.kimi-code}/sessions/**`; ccusage
uses `KIMI_DATA_DIR`. Live via `type = "openai_legacy"` (OpenAI Chat
Completions) plus `KIMI_MODEL_BASE_URL`. Telemetry **on by default**
(`telemetry` boolean defaults true); `KIMI_DISABLE_TELEMETRY=1` wins over
config. ⚠️ Kimi's own Help Center cites the npm package `@kimi-ai/kimi-code`,
which 404s; the real one is `@moonshot-ai/kimi-code`.

**18. Pi** — `earendil-works/pi` (originally `badlogic/pi-mono`), MIT, v0.84.2
2026-08-14. `~/.pi/agent/sessions/**/*.jsonl`, organised by working directory —
confirmed by the project's own docs, which is what settles the identification
against the unrelated withpi.ai. ccusage honours `PI_AGENT_DIR`. **No account
exists for this product**, and llama.cpp has a dedicated code path (`/login
llama.cpp`, `/llama`), which makes it the cheapest live-tier verification in the
whole matrix. Several other rows are Pi descendants sharing this exact session
format: GJC (22), Senpi (34), Kimchi (36), Prime Agent (38) — **one parser can
serve five rows.** Telemetry minimal and opt-out (`PI_OFFLINE=1` kills
everything).

**19. OpenClaw** — `openclaw/openclaw` (formerly Clawdbot/Moltbot), MIT,
v2026.7.1-2 2026-08-04. `~/.openclaw/agents/**/*.jsonl*` — note the trailing
`*`, which catches rotated/suffixed files (`clients.rs`, client 7); ccusage adds
three legacy roots and honours `OPENCLAW_DIR`. No account, providers auto-enable
from `OPENAI_API_KEY`/`ANTHROPIC_API_KEY`/`GEMINI_API_KEY`/`OLLAMA_API_KEY`.
⚠️ For Ollama the docs require the **native** URL with `api: "ollama"`; the
`/v1` OpenAI-compatible path "breaks tool calling and models can emit raw
tool-call JSON as plain text" — which would corrupt an activity ledger, not just
a chat.

**37. Reasonix** — `esengine/DeepSeek-Reasonix`, MIT. Surface is
`<state root>/stats/*.jsonl`, daily append-only records, and tokscale's registry
comment says why plainly: *transcript JSONL is intentionally excluded — it lacks
exact token counters and overlaps these records*
(`clients.rs`, client 42). That is the same authoritative-surface judgement
Goose and Qwen need and neither parser makes there. Root resolution is the most
elaborate in the registry: `REASONIX_STATE_HOME` then `REASONIX_HOME`, each
passed through `${VAR:-default}` expansion, then tilde expansion, then
relative-to-cwd resolution (`clients.rs:214-268`). Any adapter must reproduce
that order or it will read the wrong directory.

**40. Cherry Studio** — an Electron desktop client that writes **standard Claude
Code transcripts** under its own app-data root
(`<app-data>/CherryStudio/.claude/projects/**/*.jsonl`, `clients.rs`, client 45).
Two consequences. First, the Claude Code parser is reusable wholesale. Second,
records are **replayed**, so tokscale ships a dedicated parser that dedupes by
stable request/message ids rather than reusing the plain path — a union-find
over `requestId`, `message.id` and `uuid`. This is the split-identity class in a
form Claude Code itself does not produce. On Windows the root is
`%APPDATA%\CherryStudio\Data\Agents\.claude\projects`, and tokscale's
`app_data_follows_home` exists specifically because this is the one client whose
root is a Win32 known folder no environment variable can redirect
(`clients.rs:30-52`).

**41. DSH (DeepSeek Harness)** — `${DSH_HOME:-~/.dsh}/sessions/<encoded-cwd>/<session-id>/session.jsonl.zstd`,
with a backend configured `compression: none` writing plain `session.jsonl`, so
the scan pattern must accept both spellings (`clients.rs`, client 46). Each
`assistant/message` event carries authoritative per-call usage
(`inputTokens`/`outputTokens`/`cacheReadTokens`) plus the serving model and
provider; the `session` event supplies `cwd` and session id. **This is the
second zstd surface in the matrix** (Codex is the other), and unlike Codex's it
is compressed *by default* — an adapter without a zstd reader sees nothing at
all rather than losing only old data.

### 4.3 Fixture-tier rows

**10. Droid (Factory AI)** — `${DROID_SESSIONS_DIR:-~/.factory/sessions}/**/*.settings.json`
(`clients.rs`, client 6). **The trap catalog is confirmed on all three counts by
both parsers independently**: the sidecar is cumulative, there are no per-turn
rows, and there is no dedup key — the file stem is the only identity, so the
adapter is necessarily aggregate-with-delta (the Hermes shape), never
event-level. Closed source, npm `droid` is `UNLICENSED`. Fixture because
Factory's current pricing docs list **no free tier** and BYOK does not escape it
("BYOK is free up to an allowance on all Individual plans"). Custom endpoints
are nonetheless the most complete of any closed tool here
(`customModels[] {baseUrl, provider: generic-chat-completion-api}`), so this row
flips to live the moment a free tier reappears. OTLP telemetry by default with
**no documented individual opt-out**.

**12. Kilo CLI** — `<xdg-data>/kilo/kilo.db`, table `message` with the payload as
JSON (`clients.rs`, client 14); ccusage honours `KILO_DATA_DIR`. Same vendor as
row 13, different file. Fixture rather than live only because the CLI's own
session store could not be exercised here; the endpoint story is row 13's.

**17. Grok CLI (Grok Build)** — `xai-org/grok-build`, first-party code
Apache-2.0, npm `@xai-official/grok@1.0.4`, **no GitHub releases**. Surface
`${GROK_HOME:-~/.grok}/sessions/**/updates.jsonl`, plus `logs/unified.jsonl`.
⚠️ **`eventId` is present but not unique** — a parser that trusts it as a dedup
key will collapse distinct events; fall back to a content tuple. Custom
endpoints exist (`base_url` + `api_backend = chat_completions`), but the vendor
path needs SuperGrok or X Premium+ and **whether the CLI still gates on xAI auth
when a custom `base_url` is set is unverified** — that single question is what
separates this row from live. xAI documents **no telemetry opt-out**; a
trace-upload pipeline exists in the repo. ⚠️ Namespace collision: the community
`superagent-ai/grok-cli` (MIT, stalled since 2026-05-15) writes to the **same**
`~/.grok/`.

**20. Codebuff** — `${CODEBUFF_DATA_DIR:-~/.config/manicode}/projects/<project>/chats/<chatId>/chat-messages.json`
(`clients.rs`, client 19). The `manicode` path is historical: Codebuff was
formerly Manicode. Fixture: the paid CLI needs a Codebuff login. See row 39 for
the sibling that shares this tree.

**21. Mux** — `~/.mux/sessions/*/session-usage.json` (`clients.rs`, client 13).
A per-session usage sidecar, so cumulative by nature. **No id at all**; tokscale
keys on a workspace+model tuple. No discovery env var.

**22. GJC** — `${GJC_CODING_AGENT_DIR:-~/.gjc/agent}/sessions/**/*.jsonl`
(`clients.rs`, client 26). A Pi-format descendant (see row 18). Notable for
having the largest env surface of the Pi family: `GJC_CODING_AGENT_DIR`,
`GJC_CONFIG_DIR`, and — because of its lineage — `PI_CONFIG_DIR`.

**23. JCode** — `${JCODE_HOME:-~/.jcode}/sessions/session_*.json` plus a sibling
`.journal.jsonl` (`clients.rs`, client 28). Two surfaces, the JSON snapshot
being the primary and the journal the append-only companion.

**25. MiMo Code** — `<xdg-data>/mimocode/*.db`, table `message`, payload JSON
(`clients.rs`, client 30, id `micode`). ⚠️ The client id `micode` and the display
name disagree with the on-disk directory `mimocode`; an adapter keyed on the id
will look in the wrong place.

**27. Junie (JetBrains)** — `~/.junie/sessions/**/events.jsonl`
(`clients.rs`, client 32). An event stream with no record id; dedup is a content
hash. No env var.

**28. ZCode** — `~/.zcode/cli/db/db.sqlite`, table **`model_usage`**
(`clients.rs`, client 33 points at `~/.zcode/projects/*.jsonl`; the SQLite table
is the better surface and is what makes this row supportable). A purpose-built
usage table is rare in this matrix — only Goose, Qwen, Reasonix and this one
have anything comparable.

**29. OpenCodeReview** — `~/.opencodereview/sessions/**/*.jsonl`
(`clients.rs`, client 34). Despite the name, unrelated to opencode (row 4).

**30. CodeBuddy (Tencent)** — `~/.codebuddy/projects/**/*.jsonl` plus a
plain-text extension log (`clients.rs`, client 35; parser
`/tmp/hm/tokscale/crates/tokscale-core/src/sessions/tencent_buddy.rs`).
⚠️ Some records carry an id and some carry **none at all** — a parser must
handle both in the same file rather than assuming one shape.

**31. WorkBuddy** — `~/.workbuddy/workbuddy.db` per the registry
(`clients.rs`, client 36); the parser prefers per-project JSONL and treats the
SQLite file as the fallback. ⚠️ **No official Linux build**, which is why this
is fixture and not live, and the `~/.workbuddy/` layout is
**unverified against a real install**.

**32. Devin CLI** — `<xdg-data>/devin/cli/sessions.db`, table `message_nodes`
(`clients.rs`, client 37). Rowid is the only identity.
**33. Devin Desktop** — `~/Library/Application Support/Devin/User/acp-events/*.ndjson`
(`clients.rs`, client 38) — macOS-only path, so not verifiable on Linux at all.
Both fixture for the same commercial reason: Devin is account-bound with **no
BYOK**, so it cannot be pointed at a local model.

**34. Senpi (OmO Native)** — `${SENPI_CODING_AGENT_DIR:-~/.senpi/agent}/sessions/**/*.jsonl`,
a Pi-mono descendant mirroring the GJC layout (`clients.rs`, client 39).

**35. Augment / Auggie CLI** — `~/.augment/sessions/<sessionId>.json`, with
per-turn `token_usage` on `exchange.response_nodes`
(`clients.rs`, client 40). Fixture because Augment is **paid-only with no free
tier** and no BYO endpoint.

**36. Kimchi Coding** — `${KIMCHI_CODING_AGENT_DIR:-~/.config/kimchi/harness}/sessions/**/*.jsonl`,
the Pi session format again (`clients.rs`, client 41). Note the default is under
`~/.config`, not `~/`, unlike its siblings.

**38. Prime Agent** — `${PRIME_AGENT_CODING_AGENT_DIR:-~/.prime/agent}/sessions`
for root sessions, with **RLM child sessions discovered from a sibling
`session-artifacts` tree** (`clients.rs`, client 43). That second tree is the
interesting part: child sessions are a separate lineage that must be joined to
the parent, the same shape as Codex forks (§1.4). Pi format.

### 4.4 Unsupportable rows, with the evidence

**24. CommandCode** — `~/.commandcode/projects/**/*.jsonl` exists and is
readable, and **contains no usage**. tokscale's parser estimates tokens from
message text at roughly four characters per token. Under CONTEXT.md's
*unpriced ≠ $0* / *unattributed ≠ free* doctrine an estimate is not usage, so
there is nothing here to meet the coverage bar. Document, do not adapt.

**26. Antigravity (IDE)** — no on-disk usage surface. tokscale obtains usage by
RPC to a running language server and caches the result as JSONL under **its
own** config dir; the cache is tokscale's artifact, not Antigravity's
(`clients.rs`, client 20, contrasted in the source comment with client 31, the
CLI, which *does* sit on disk). Proprietary, free Google account, no BYO key, no
custom endpoint. An adapter would have to become an RPC client of a running IDE,
which is not an observational read. Note the CLI (row 6) **is** supportable and
is a different product.

**39. Freebuff** — shares `~/.config/manicode*` and the
`projects/<project>/chats/<chatId>/chat-messages.json` layout with Codebuff, the
two told apart per chat by the persisted root agent id rather than by location
(`clients.rs`, client 44). The repo is now the same repo: `CodebuffAI/codebuff`
resolves to **`CodebuffAI/freebuff`**. Unsupportable because the free product
**writes no usage field** — only `credits`, which is 0 in free mode — and usage
is fetched from the server. No BYOK, no base URL. ⚠️ License is unresolved:
GitHub reports Apache-2.0, npm reports MIT.

**42. Cursor** — no local usage surface exists. tokscale's "surface" is
`~/.config/tokscale/cursor-cache/usage*.csv`, its **own** cache, and it is the
one client in the registry with `parse_local: false`
(`clients.rs`, client 3). The data comes from the authenticated Cursor dashboard;
the Admin/Analytics APIs are Enterprise-only and individual users have no
supported API (the PyPI `cursor-usage` tool scrapes a session token out of
`state.vscdb`). Cursor staff confirm the **CLI has no BYOK**. The manual export
path is real — `cursor.com/dashboard/usage` → Export CSV, columns
`Date, Kind, Model, Max Mode, Input (w/ Cache Write), Input (w/o Cache Write),
Cache Read, Output Tokens, Total Tokens, Cost` — which makes Cursor a candidate
for an **import** command, not an adapter. ⚠️ A Cursor employee has stated the
Cost column was broken in that export; **whether it is fixed is unverified**.

**43. Warp** — same cloud-only shape, and worse: tokscale's cache records
**no tokens at all**, only request counts and spend (`clients.rs`, client 24,
`submit_default: false`). Warp open-sourced its client in 2026-04 (MIT for
`warpui_core`/`warpui`, AGPL-3.0 for the rest) but the orchestration stays
proprietary. 🔒 The decisive detail for a local-model workaround: Warp supports
arbitrary OpenAI-compatible endpoints **but routes inference through its own
servers, so "localhost, 127.0.0.1, and other private or local network URLs are
rejected"**. There is no local-only path. Telemetry must additionally stay
enabled to use AI at all on the free plan.

**44. Trae (ByteDance)** — tokscale dumps Trae's official usage API into
`~/.config/tokscale/trae-cache` (`clients.rs`, client 23, `submit_default: false`).
No local harness surface. Three products share the name and only one is open:
the **Trae IDE** (proprietary, `.deb` x64 only, no base-URL field — the existence
of `arch3rPro/Trae-Proxy` is the evidence), the **TraeCode CLI** (documented only
on `docs.trae.cn`, enterprise login), and **`bytedance/trae-agent`** (MIT, open,
`base_url` in YAML, Ollama listed) — which shares only the brand and **is not in
the union**. ⚠️ Trae has the worst telemetry record in this document:
independent 2025 research found telemetry continued and increased after every
toggle was disabled.

**45. Kiro (AWS)** — four local surfaces exist and **every token count in them is
an estimate**; tokscale's own notes record that turn-level counts are currently
zero in both primary sources. Proprietary, VS Code-derived, mandatory account
(AWS Builder ID is free, 50 credits/month), **no custom endpoint** — the
still-open requests are the evidence (kirodotdev/Kiro #9367, #3115, #1952,
#1067). Telemetry is on by default and **includes content**, with a GUI-only
opt-out. Readable surface, no usable usage: document, do not adapt.

---

## 5. Excluded, with evidence

### 5.1 Roo Code — excluded, and the stated reason holds

`gh api repos/RooCodeInc/Roo-Code` returns `"archived": true`, last push
**2026-05-15T18:08:47Z**, 24,340 stars. (`archived_at` is null, which GitHub
leaves unset for older archivals; `archived: true` is the authoritative field.)

tokscale still carries it as client 11 against
`~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks/*/ui_messages.json`
— the same `ui_messages.json` shape as Cline (row 14) and Kilo Code (row 13),
which is the shared Cline-fork lineage. Nothing in the parser marks the project
dead; the archival is only visible from GitHub. **Correctly excluded.**

### 5.2 Amp — excluded by the ticket, and the stated reason is FALSE

The ticket excludes Amp as "no local ledger". The parser source says otherwise,
unambiguously, and this is the single most consequential correction in this
document.

`/tmp/hm/ccusage/rust/adapters/amp/src/README.md:5-20`:

```text
${AMP_DATA_DIR:-~/.local/share/amp}/threads/
```

> Each thread is a JSON file named `T-{uuid}.json`. Usage comes from:
> - `usageLedger.events[]` for token usage and credits, with `messages[].usage`
>   supplying the cache creation/read breakdown per `toMessageId`...
> - `messages[].usage` directly when `usageLedger.events` is not present
>   (current Amp schema). Each assistant message's `usage` object carries
>   `model`, `timestamp`, and the `inputTokens`, `outputTokens`,
>   `cacheCreationInputTokens`, `cacheReadInputTokens`, and `totalTokens` fields.

The upstream field is literally called **`usageLedger`**. `read_thread_file`
prefers it and falls back to `messages[].usage`
(`/tmp/hm/ccusage/rust/adapters/amp/src/parser.rs:104-118`), and the fixtures
carry real Anthropic-shaped numbers — `inputTokens: 10, outputTokens: 178,
cacheCreationInputTokens: 986, cacheReadInputTokens: 11372`
(`.../parser.rs:376-420`). Amp also reports **credits** alongside tokens
(`.../parser.rs:212`), which is more than most rows here manage.

tokscale independently registers the same path — `PathRoot::XdgData` +
`amp/threads`, pattern `T-*.json` (`clients.rs`, client 5) — and ships a
20.3 KB parser. Two independent implementations against one surface is the
strongest evidence in this document.

**Assessment if promoted to a row:** authoritative surface
`${AMP_DATA_DIR:-~/.local/share/amp}/threads/T-*.json`, JSON, per-message
`usage` objects with the full four-way Anthropic split plus `totalTokens` as a
fallback; dedup identity is `messageId` (ledger events additionally carry their
own `id` and a `toMessageId` join to the cache split); read-only trivially;
discovery env `AMP_DATA_DIR`, **comma-separated**, defaulting to
`~/.local/share/amp` (`/tmp/hm/ccusage/rust/adapters/amp/src/paths.rs:5-30`).
Traps: two schema generations in one file format (ledger vs message usage,
ledger winning); `totalTokens` used only when the split fields are absent, which
is the same `ApplyTotalFallback` shape aiusage already has; and a **split
identity between the two halves** — ledger events carry tokens while the cache
breakdown lives on the messages, joined by `toMessageId`
(`.../parser.rs:143-148`).

It is kept out of the matrix only because the ticket named it. **The exclusion
should be reversed**, and the row above is complete enough to cut a build ticket
from as written.

⚠️ One asymmetry worth recording: tokscale does **not** honour `AMP_DATA_DIR` —
its Amp root is `XdgData` only. ccusage's path discovery is the better one here.

---

## 6. Trap-catalog members that are NOT in the union

The ticket's trap catalog names **Aider** and **Continue**. Neither is a member
of `ccusage ∪ tokscale ∪ aiusage`, so neither gets a matrix row:

- `grep -rli "aider" /tmp/hm/ccusage/rust /tmp/hm/tokscale/crates /tmp/hm/tokscale/packages`
  returns nothing.
- Neither `"continue"` as a client id nor a `Continue` variant appears in
  tokscale's `ClientId` enum, and ccusage has no such adapter.
- Aider is an **agentsview**-only parser (§7.3), and agentsview is not one of
  the three sets the union is defined over.

They are documented here at full row depth anyway, because the catalog attached
them and because both were verified from the harness's own source this session.
If the union is ever redefined to include agentsview, these two drop straight
in.

### 6.1 Aider

Repo `Aider-AI/aider`, Apache-2.0. Full mechanism in §1.5.

| Field | Value |
|---|---|
| Authoritative surface | the file named by `--analytics-log ANALYTICS_LOG_FILE` — **no default path** |
| Format | JSONL (`json.dump` + `"\n"`, append mode, `analytics.py:250-252`) |
| Record | `{event, properties, user_id, time}`; `message_send` properties carry `main_model`, `edit_format`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `cost`, `total_cost` (`base_coder.py:2112-2122`) |
| Dedup identity | **none** — no per-event id; `time` is unix **seconds**. Falls back to file path + line offset |
| Read-only | yes, plain append-only text file |
| Custom endpoint | yes — aider is LiteLLM-backed, so any OpenAI-compatible base URL works |
| Tier | **fixture**, and unusually so: the surface does not exist unless the user opts in with a flag that has no default |
| Discovery env | none for the log path; `AIDER_ANALYTICS_LOG` is the standard aider env spelling of the flag — **unverified** |
| Phone-home | PostHog when opted in; `--analytics-disable` is permanent and persists to disk |

Traps, both new to the catalog:
- **Provider-reported and tokenizer-estimated counts are mixed with no
  discriminator** (`base_coder.py:2000-2018`). Every other estimate-carrying
  surface in this document at least segregates them; Goose has an explicit
  `cost_source` column for exactly this.
- **The cache split is computed then dropped** — `cache_hit_tokens` and
  `cache_write_tokens` exist at `base_coder.py:2003-2006` and are not among the
  fields passed to `event()`. Cache read/write is unrecoverable from this
  surface.
- `.aider.chat.history.md` is **not** a fallback: `format_tokens` rounds to the
  nearest thousand at ≥10,000 (§1.5).

### 6.2 Continue

Repo `continuedev/continue`. **The catalog's claim is confirmed exactly**, and
the code makes the intent visible rather than accidental.

`/tmp/hm/continue/core/llm/index.ts:340-368` — `_logEnd` receives the real
provider usage as a parameter (`usage: Usage | undefined`), and then:

```ts
let promptTokens = this.countTokens(prompt);
let generatedTokens = this.countTokens(completion);
...
void DevDataSqliteDb.logTokensGenerated(model, this.providerName, promptTokens, generatedTokens);
void DataLogger.getInstance().logDevData({ name: "tokensGenerated", data: { ... promptTokens, generatedTokens } });
...
interaction?.logItem({ kind: "success", promptTokens, generatedTokens, thinkingTokens, usage });
```

`countTokens` is a local tokenizer (`core/llm/index.ts:1450-1451`). The real
`usage` object is passed **only** to the in-memory `interaction` log — never to
the SQLite table and never to the dev-data JSONL. Both persistent surfaces get
estimates.

| Field | Value |
|---|---|
| Surfaces | `${CONTINUE_GLOBAL_DIR:-~/.continue}/dev_data/devdata.sqlite` → table `tokens_generated`; and `${CONTINUE_GLOBAL_DIR:-~/.continue}/dev_data/<schema>/tokensGenerated.jsonl` |
| Schema | `tokens_generated(id INTEGER PK AUTOINCREMENT, model TEXT, provider TEXT, tokens_generated INTEGER, tokens_prompt INTEGER DEFAULT 0, timestamp DATETIME DEFAULT CURRENT_TIMESTAMP)` (`core/data/devdataSqlite.ts:16-24`) |
| Authoritative | neither — both hold the same estimates |
| Dedup identity | autoincrement `id` only. **No session id, no message id, no request id** |
| Read-only | yes |
| Custom endpoint | yes — Ollama and OpenAI-compatible are first-class providers |
| Tier | **unsupportable** under the coverage bar: the only persistent numbers are tokenizer estimates, and *unpriced ≠ $0* applies |
| Discovery env | **`CONTINUE_GLOBAL_DIR`** (`core/util/paths.ts:27-36`); relative values are resolved against cwd |
| Phone-home | dev-data telemetry, opt-out; not characterised further this session — **unverified** |

Also missing from the surface: no cache buckets, no reasoning tokens, no cost.
`timestamp` is SQLite `CURRENT_TIMESTAMP` (UTC, second resolution).

---

## 7. agentsview — what to steal, and what not to trust

`kenn-io/agentsview`, MIT, Go, ~50 providers under `internal/parser/`. Read for
schema ideas per the ticket; its parsers are explicitly lower-trust.

### 7.1 The good idea: a machine-readable capability declaration

agentsview makes each provider **declare** what it can extract, in code, as data
— `internal/parser/capabilities.go` plus a generated
`capabilitysupport_enumer.go`, with `provider_capabilities.go` binding the
declaration to the provider. A provider is a value that answers "what do I
support" rather than a function whose behaviour you discover by running it.

That is exactly CONTEXT.md's **capability declaration** ("the adapter's
machine-readable statement of what it captures (usage / activity /
attribution) and its verification tier") — and aiusage **does not have one
today**. `internal/adapter/adapter.go` has no `Capability` type; the word does
not appear in the package. The vocabulary exists in CONTEXT.md and the
implementation does not.

This matters more with 45 candidate rows than with 6: without a declaration,
"does the copilot adapter capture activity?" is answered by reading
`activity.go`, and "is this row live or fixture" is answered by asking a person.
With one, `doctor` can print it and a test can assert the coverage bar per
adapter. **Recommended follow-up ticket**, and the single most reusable thing in
agentsview.

Adjacent ideas worth a look in the same sweep: `internal/parser/taxonomy.go`
(a shared vocabulary for record kinds), `source_set.go` /
`jsonl_source_set.go` / `directory_jsonl_source_set.go` (a reusable
"set of JSONL files under a directory" abstraction — aiusage re-implements this
per adapter), and `db_backed_provider.go` (the SQLite-backed provider shape,
which would serve Goose, Zed, ZCode, Devin, Kilo, MiMo and WorkBuddy at once).

### 7.2 Coverage agentsview adds beyond the union

Names only, since none are union members: `chatgpt`, `claude_ai`,
`cortex`, `cowork`, `deepseek_tui`, `forge`, `gptme`, `icodemate`, `iflow`,
`kiro_ide`, `omnigent`, `openhands`, `piebald`, `poolside`, `posit_assistant`,
`positron`, `qclaw`, `qoder`, `qwenpaw`, `shelley`, `vibe`,
`visualstudio_copilot`, `vscode_copilot`, `windsurf`, `zencoder`, plus `aider`
(§7.3). Several are IDE-side counterparts of union members
(`vscode_copilot`/`visualstudio_copilot` vs row 3, `kiro_ide` vs row 45).
**If the union is ever widened, this is the candidate list.**

### 7.3 Why its parsers are not trusted: the aider case

The ticket's framing — "it ships an aider parser that extracts zero tokens" — is
the right conclusion, and the reason is now precisely locatable. Aider's only
honest local surface is the `--analytics-log` JSONL, which **has no default path
and does not exist unless the user passes the flag** (§6.1). The surface a
parser would naturally reach for, `.aider.chat.history.md`, contains counts that
`format_tokens` has already rounded to the nearest thousand above 10,000
(§1.5) — so a parser built on it yields either nothing or numbers that are wrong
by up to 500 tokens per turn by construction.

That is the general warning, not an aider-specific one: **a parser can be
syntactically correct and semantically empty**, and only reading the writer
tells you which. Every third-party parser in this document was checked against
the harness where the harness was open; the four cases where that changed the
answer are §1.1, §1.2, §1.3 and §1.4.

---

## 8. Discovery environment registry

Everything that moves **what a harness wrote**. `cmd.discoveryEnv` must learn
each of these as its adapter lands — a systemd unit does not inherit the
installing shell's environment, so an install made under any of them supervises
a daemon reading a different directory than the CLI that installed it
(CLAUDE.md, "Path overrides suppress the automatic install").

### 8.1 Already known to `discoveryEnv`

`internal/cmd/root.go:321-329` names five:
`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `COPILOT_OTEL_FILE_EXPORTER_PATH`,
`HERMES_HOME`, `OPENCODE_DATA_DIR`.

`TestDiscoveryEnvCoversEveryAdapterVariable` parses the adapter sources and
fails on a variable the list has not been taught about, so the list cannot drift
below the adapters. It **can** drift below reality — an adapter that never
learned a variable is invisible to that test. Five of the gaps below are exactly
that shape.

### 8.2 Harness-discovery variables, by row

Provenance: **[H]** read from the harness's own source this session,
**[C]** from ccusage's path discovery, **[T]** from tokscale's `define_clients!`
registry (`/tmp/hm/tokscale/crates/tokscale-core/src/clients.rs`).

| Variable | Row | Moves | Fallback | Src |
|---|---|---|---|---|
| `CLAUDE_CONFIG_DIR` | 1, 40 | Claude Code config root | `~/.config/claude`, then `~/.claude` | [T][aiusage] |
| `CODEX_HOME` | 2 | Codex home → sessions + archived | `~/.codex` | [T][aiusage] |
| `COPILOT_OTEL_FILE_EXPORTER_PATH` | 3 | a single OTEL export **file** | none (dir scan only) | [aiusage] |
| `OPENCODE_DATA_DIR` | 4 | opencode data dir → db + JSON tree | `<xdg-data>/opencode` | [T][aiusage] |
| `HERMES_HOME` | 5 | Hermes home → `state.db`. **Comma-separated list** | `~/.hermes` | [T][aiusage] |
| `GEMINI_CLI_HOME` | 6, 7 | Gemini home → both `tmp/` and `antigravity-cli/conversations` | `~/.gemini` | [T] |
| `GEMINI_DATA_DIR` | 7 | Gemini data root | — | [C] |
| `CRUSH_GLOBAL_DATA` | 8 | Crush global data dir → `projects.json` | `<xdg-data>/crush` | [C] |
| `GOOSE_PATH_ROOT` | 9 | Goose data root → `sessions/sessions.db` | `<xdg-data>/goose` | [C] |
| `DROID_SESSIONS_DIR` | 10 | Droid sessions dir | `~/.factory/sessions` | [C] |
| `KILO_DATA_DIR` | 12 | Kilo CLI data dir → `kilo.db` | `<xdg-data>/kilo` | [C] |
| `CLINE_SESSION_DATA_DIR` | 14 | Cline CLI session dir | — | [T] |
| `CLINE_DATA_DIR` | 14 | Cline CLI data dir | — | [T] |
| `CLINE_DIR` | 14 | Cline CLI root | — | [T] |
| **`QWEN_HOME`** | 15 | Qwen global dir → `usage/` ledger | `~/.qwen` | **[H]** |
| **`QWEN_RUNTIME_DIR`** | 15 | Qwen runtime base — **takes precedence over `QWEN_HOME`** | falls through | **[H]** |
| `QWEN_DATA_DIR` | 15 | Qwen data root (transcripts) | `~/.qwen` | [C] |
| `KIMI_DATA_DIR` | 16 | Kimi data root | `~/.kimi` | [C] |
| `KIMI_CODE_HOME` | 16 | Kimi **Code** home | `~/.kimi-code` | [T] |
| `GROK_HOME` | 17 | Grok home → sessions + logs | `~/.grok` | [T] |
| `PI_AGENT_DIR` | 18 | Pi agent dir → sessions | `~/.pi/agent` | [C] |
| `OPENCLAW_DIR` | 19 | OpenClaw root | `~/.openclaw` | [C] |
| `CODEBUFF_DATA_DIR` | 20, 39 | Codebuff data dir | `~/.config/manicode` | [T] |
| `GJC_CODING_AGENT_DIR` | 22 | GJC agent dir | `~/.gjc/agent` | [T] |
| `GJC_CONFIG_DIR` | 22 | GJC config dir | — | [T] |
| `PI_CONFIG_DIR` | 22 | GJC's Pi-lineage config dir | — | [T] |
| `JCODE_HOME` | 23 | JCode home | `~/.jcode` | [T] |
| `SENPI_CODING_AGENT_DIR` | 34 | Senpi agent dir | `~/.senpi/agent` | [T] |
| `SENPI_CODING_AGENT_SESSION_DIR` | 34 | Senpi session dir | — | [T] |
| `KIMCHI_CODING_AGENT_DIR` | 36 | Kimchi agent dir | `~/.config/kimchi/harness` | [T] |
| `REASONIX_STATE_HOME` | 37 | Reasonix state root → `stats/`. **Checked first** | falls through | [T] |
| `REASONIX_HOME` | 37 | Reasonix home | `~/.reasonix` | [T] |
| `PRIME_AGENT_SESSION_DIR` | 38 | Prime Agent session dir | — | [T] |
| `PRIME_AGENT_CODING_AGENT_SESSION_DIR` | 38 | Prime Agent session dir (alt) | — | [T] |
| `PRIME_AGENT_CODING_AGENT_DIR` | 38 | Prime Agent agent dir | `~/.prime/agent` | [T] |
| `FREEBUFF_DATA_DIR` | 39 | Freebuff data dir | `~/.config/manicode` | [T] |
| `DSH_HOME` | 41 | DSH home → sessions | `~/.dsh` | [T] |
| **`AMP_DATA_DIR`** | §5.2 | Amp data dir. **Comma-separated list** | `~/.local/share/amp` | **[C]** |
| **`CONTINUE_GLOBAL_DIR`** | §6.2 | Continue global dir → `dev_data/` | `~/.continue` | **[H]** |

### 8.3 Platform roots that move harness surfaces

These are **not** harness-specific and are already partly covered by
`config.PathEnvNames`, but they move third-party surfaces too, which is a
different claim:

| Variable | Moves | Rows affected |
|---|---|---|
| `HOME` | everything home-relative | all |
| `XDG_DATA_HOME` | `PathRoot::XdgData` | 4, 8, 9, 11, 12, 25, 32, 33, §5.2 |
| `XDG_CONFIG_HOME` | `PathRoot::AppData` on Linux; VS Code `globalStorage` roots | 1, 13, 14, 22, 36, 40 |
| `APPDATA` / `LOCALAPPDATA` | `PathRoot::AppData` on Windows | 5, 33, 40, 45 |

⚠️ `PathRoot::AppData` is the one root that can disagree with `HOME`, and only on
Windows: `dirs::config_dir()` is a Win32 known folder no environment variable can
redirect. tokscale's `app_data_follows_home` exists solely for that
(`clients.rs:30-70`), and Cherry Studio (row 40) is the client it was written
for.

### 8.4 Special read semantics to reproduce

- **Comma-separated lists**: `HERMES_HOME` (aiusage already), `AMP_DATA_DIR`
  (ccusage, `paths.rs:10-22`).
- **`${VAR:-default}` expansion, then `~`, then relative-to-cwd**: Reasonix only
  (`clients.rs:214-268`). Three steps, in that order.
- **Trimmed-empty means unset**: aiusage's own rule
  (`discoveryEnvOverrides`, `internal/cmd/root.go:334-342`); tokscale does the
  same (`val.trim().is_empty()`).
- **Precedence chains**: `QWEN_RUNTIME_DIR` > `QWEN_HOME` > `~/.qwen`;
  `REASONIX_STATE_HOME` > `REASONIX_HOME` > `~/.reasonix`;
  `CLINE_SESSION_DATA_DIR` > `CLINE_DATA_DIR` > `CLINE_DIR`.
- **Relative values resolved against cwd**: Continue
  (`core/util/paths.ts:29-33`), Reasonix.

### 8.5 What `parse_local` and `submit_default` mean in tokscale

Two flags on every `ClientDef` (`clients.rs:285-293`) that a reader will assume
things about:

- **`parse_local`** — whether the client's data is parsed from local files at
  all. Exactly one client has it `false`: **cursor** (client 3), which is the
  mechanical statement of §4.4's cloud-only finding.
- **`submit_default`** — whether the client is included by default when tokscale
  **submits** to its own service. Three are `false`: **crush** (15), **trae**
  (23), **warp** (24). This is tokscale's phone-home switch, not the harness's,
  and it is listed here only so it is not mistaken for one. The Privacy column
  in §3 is about the *harness*.

---

## 9. New traps, not in the ticket's catalog

Numbered for citation from build tickets.

1. **The zstd cliff is coming for Codex and is already here for DSH.** Codex's
   `local_thread_store_compression` is `default_enabled: false` **today**
   (§1.4). When it flips, every reader without a zstd path silently loses old
   sessions — no error, a shrinking history. DSH ships compressed *by default*
   (row 41), so the same gap there means seeing nothing at all. One zstd reader
   serves both.
2. **Assigned-vs-accumulated is a two-column pattern, not a one-column bug.**
   Crush has only the assigned column (§1.1). Goose ships **both**, adjacent, in
   the same `UPDATE`: `total_tokens = ?` beside
   `accumulated_total_tokens = COALESCE(...) + ?` (§1.2). The trap is picking the
   wrong one, and the column names do not warn you.
3. **A back-fill row can be the gap rather than a duplicate.** Goose's
   `cost_source = 'carried_forward'` rows are computed as
   `accumulated − SUM(ledger)`. Filtering them out **undercounts** (§1.2). Before
   excluding any reconciliation row, check which direction it was computed in.
4. **Estimates masquerading as usage, ungated.** Aider mixes provider-reported
   and tokenizer counts in one field with no discriminator (§6.1); Continue
   persists only estimates (§6.2); CommandCode, Freebuff and Kiro report
   `chars/4` (§4.4). Goose's `cost_source` column is the shape that gets this
   right, and it is the exception.
5. **`eventId` present but not unique** (Grok, row 17). An id field is not a
   dedup key until it has been counted. aiusage's Claude Code adapter earned
   that right the hard way: 60,869 `tool_use` blocks, 60,869 distinct ids, none
   repeated, none absent — that is the standard of evidence.
6. **One vendor, two client ids, two formats** (Kilo/Kilo Code, rows 12–13;
   Codebuff/Freebuff, rows 20/39; Kimi CLI/Kimi Code, row 16; Devin CLI/Desktop,
   rows 32–33). Telling them apart by *location* fails for Codebuff/Freebuff,
   which share one tree and are distinguished per chat by the persisted root
   agent id (`clients.rs`, client 44).
7. **A namespace collision between an official and a community CLI.** Both
   `xai-org/grok-build` and `superagent-ai/grok-cli` write to `~/.grok/`
   (row 17). Path presence does not identify the writer.
8. **A "local" file that belongs to another usage tool.** Four registry entries
   point into `~/.config/tokscale/` (cursor, warp, trae, and the antigravity IDE
   cache). Reading one would make aiusage a consumer of tokscale's cloud
   credentials, not an observer of a harness. §4.4 excludes all four.
9. **Sub-agent / fork lineages beyond Codex.** Prime Agent's RLM children live in
   a sibling `session-artifacts` tree that must be joined to the parent (row 38)
   — the same shape as Codex's `forked_from_id` (§1.4), in a different file
   layout.
10. **The `/v1` compatibility shim can corrupt the ledger, not just the chat.**
    OpenClaw's docs warn that pointing it at Ollama's OpenAI-compatible `/v1`
    path "breaks tool calling and models can emit raw tool-call JSON as plain
    text" (row 19). A verification run set up that way would produce a transcript
    whose activity rows are fiction.
11. **A directory that exists proves nothing.** This machine has `~/.gemini`
    (config only, no `tmp/`) and a fully-installed Crush whose `crush.db` has
    zero sessions and zero messages — verified read-only this session. Discovery
    must gate on usage-bearing content, which is exactly what aiusage's `agy`
    adapter already does (`internal/adapter/agy/agy.go:78-86`).

---

## 10. Where ccusage and tokscale disagree

Only disagreements that change what an adapter would do.

| Subject | ccusage | tokscale | Trust, and why |
|---|---|---|---|
| **Antigravity CLI surface** | no adapter | `${GEMINI_CLI_HOME:-~/.gemini}/antigravity-cli/conversations/*.db`, per-conversation SQLite, read directly | **tokscale.** It names a concrete file; aiusage's `agy` adapter probes `*.json`/`*.jsonl` in three roots and its own header records finding no usage. This is an actionable gap in aiusage today. |
| **Amp discovery** | `AMP_DATA_DIR`, comma-separated, → `~/.local/share/amp` | `XdgData` + `amp/threads`, **no env var** | **ccusage.** It honours a variable that exists; tokscale's registry silently misses it. |
| **Qwen surface** | `${QWEN_DATA_DIR:-~/.qwen}/projects/<p>/chats/*.jsonl` | `~/.qwen/projects/**/*.jsonl` | **Neither.** Both read the transcript; the harness ships an authoritative ledger with UUIDs at `usage/token-usage-*.jsonl` (§1.3). |
| **Qwen env** | `QWEN_DATA_DIR` | none | **Neither is complete.** The harness reads `QWEN_RUNTIME_DIR` and `QWEN_HOME` (§1.3), which neither tool knows. |
| **Goose surface** | `sessions` table, prefers `accumulated_*` | `sessions` table, same | **Neither.** `usage_ledger` exists with per-row timestamps, model, cost and `cost_source` (§1.2). Both stamp a session's whole lifetime on `created_at`. |
| **Goose reasoning** | computes `total − (input + output)` silently | computes the same and labels it *"INFERRED, not a real field... a best-effort estimate, not a measured count"* | **tokscale**, on honesty. The number is identical; only tokscale says it is invented. Under *unpriced ≠ $0* the label is the part that matters. |
| **opencode JSON tree** | fallback behind the DB, still valid alone | primary path is the JSON tree (`<xdg-data>/opencode/storage/message/*.json`) | **ccusage**, and aiusage already does better than both by reading both and collapsing on message id. |
| **Crush** | no adapter | cost-only, and says so in a header comment | **tokscale**, confirmed against crush's own source (§1.1). |
| **Kimi env** | `KIMI_DATA_DIR` | `KIMI_CODE_HOME` | **Both.** They are two different products sharing a lineage (row 16); an adapter needs both. |

The pattern across the first five rows is one finding, not five: **both parsers
read the surface that is easiest to find rather than the one the harness
designates as authoritative.** Where a harness ships a purpose-built usage
ledger — Goose's `usage_ledger`, Qwen's `usage/`, Reasonix's `stats/` — only
Reasonix's is actually read, and only because its registry entry happens to
point there. That is the largest single opportunity in this document.

---

## 11. Suggested build order

Not part of the ticket; recorded because the matrix implies it and re-deriving
it would cost another pass.

1. **Goose** (row 9) — Apache-2.0, free, telemetry off, custom endpoints, and
   reading `usage_ledger` puts aiusage ahead of both reference tools on day one.
2. **Qwen** (row 15) — the ledger hands out UUIDs, which is the only real dedup
   key in the matrix outside Claude Code.
3. **Amp** (§5.2) — two independent parsers already agree on the format; the
   exclusion is the only thing in the way.
4. **The Pi family** (rows 18, 22, 34, 36, 38) — one session format, five rows.
5. **The `ui_messages.json` family** (rows 13, 14, and Roo Code's corpse) — one
   format, two live rows.
6. **A zstd reader** — unblocks DSH (row 41) now and Codex (row 2) later.
7. **The `agy` surface correction** (row 6 / §10) — tokscale names a SQLite file
   the current adapter never opens.

Before any of them: the **capability declaration** (§7.1). It is cheap now and
gets expensive once there are twenty adapters whose tier lives in a person's
head.

