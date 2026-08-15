# Long-context pricing: whole-request tiers vs marginal above-200k

Research resolution for issue #71 (map: #68). No production code changed.

Measured 2026-08-15 against a read-only snapshot of this machine's live ledger
(`~/.local/share/aiusage/usage.db`, 368,515 events, 2026-04-25 -> 2026-08-15,
$24,714.65 stored cost) and the LiteLLM price table cached that same day
(`~/.local/share/aiusage/prices-litellm.json`, `_meta.fetched` = 2026-08-15,
3,020 models).

## Verdict

**Yes, aiusage misprices long-context turns — but not in the way the ticket
guessed.** aiusage does not treat the `*_above_200k_tokens` family as marginal.
It does not read those fields at all. `litellm.go`'s decoder is a seven-field
struct that names none of them, `Rates` has no tier fields, and `Charge` has no
context-size argument the price function could branch on. Every long-context
turn is billed at the base rate.

**Whole-request is the correct semantics, and no provider bills marginally.**
OpenAI: "Prompts with >272K input tokens are priced at 2x input and 1.5x output
for **the full request**." Google: "all tokens (input and output) are charged at
long context rates." Anthropic's retired 1M beta was a two-row table keyed on
prompt size. LiteLLM's `*_above_200k_tokens` naming describes which rate card a
request lands on, not a band of tokens priced separately — the marginal reading
is an artifact of the field name and matches no documented provider behaviour.

Exposure on the real ledger: **551 rows** (0.15%) cross a published threshold.
They were stored at **$77.59**; the correct figure is **$152.12**. **aiusage
under-billed them by $74.53** — 96% of what those rows should have cost, and
**0.30% of the $24,714.65 ledger**. Had the marginal reading been right it would
have been $7.09, so the two interpretations differ by a factor of ten on the
same rows; the ticket was right that the distinction matters, and right about
which way ccusage resolves it.

**The Anthropic scare is dead.** All five tiered models are OpenAI at 272K. The
entire Claude 5 generation is flat-priced across its full 1M window, per
Anthropic's own docs, corroborated independently by LiteLLM and models.dev. Had
a >200K premium survived into that generation the exposure on this ledger would
have been **$3,070**, forty times the real number — which is why that was the
question worth checking against primary sources first.

## 1. What internal/pricing does with `*_above_200k_tokens` today

**It ignores them.** The LiteLLM decoder is an explicit seven-field struct and
no tier field is among them
([internal/pricing/litellm.go:23-31](../../internal/pricing/litellm.go)):

```go
type litellmEntry struct {
	Input        float64 `json:"input_cost_per_token"`
	Output       float64 `json:"output_cost_per_token"`
	CacheRead    float64 `json:"cache_read_input_token_cost"`
	CacheWrite   float64 `json:"cache_creation_input_token_cost"`
	CacheWrite1h float64 `json:"cache_creation_input_token_cost_above_1hr"`
	InputBatch   float64 `json:"input_cost_per_token_batches"`
	OutputBatch  float64 `json:"output_cost_per_token_batches"`
}
```

`cache_creation_input_token_cost_above_1hr` is the only `above_` field read,
and it is a cache-lifetime tier, not a context tier. `encoding/json` discards
every unlisted key silently, so `input_cost_per_token_above_272k_tokens` is
parsed and thrown away on every refresh.

`Rates` has no tier fields either
([internal/pricing/pricing.go:33-41](../../internal/pricing/pricing.go)), and
`Rates.Cost` is a flat five-term dot product with no threshold anywhere in it
([internal/pricing/pricing.go:135-167](../../internal/pricing/pricing.go)):

```go
	usd := float64(c.Input)*in +
		float64(c.Output)*out +
		float64(c.CacheRead)*cacheRead +
		float64(c.CacheWrite5m)*write5m +
		float64(c.CacheWrite1h)*write1h
```

`Charge` carries `Model`, `Provider`, `ServiceTier` and five token counts
([internal/pricing/pricing.go:81-101](../../internal/pricing/pricing.go)). The
only rate-selecting fact it holds is the service tier, routed through
`isBatchTier`. There is no context-size input to the price function at all, so
tiering is not merely unimplemented — the function has no argument that could
express it.

**The embedded floor is worse: the fields are physically absent.** The shipped
snapshot's own `_meta` records the filter that produced it
(`internal/pricing/data/litellm_snapshot.json`):

> `"filter": "litellm_provider in (anthropic, openai, gemini, github_copilot,
> vertex_ai-anthropic_models, vertex_ai-language-models); mode in (chat,
> responses, completion); nonzero input or output rate"`

Grepping the 243-model snapshot for `above` returns 49 hits, all of them
`cache_creation_input_token_cost_above_1hr`. Zero context-tier fields survive
the snapshot. So teaching the decoder alone would leave an air-gapped install
still flat-pricing — the snapshot regeneration has to keep the fields too.

**Config overrides cannot express a tier either.** `config.ModelRates` is seven
flat per-token floats
([internal/config/config.go:46-54](../../internal/config/config.go)), so a user
who knows the correct semantics has no way to state them. There is no
workaround at any layer.

**Empirical confirmation, not just code reading.** For the 551 ledger rows that
cross a published threshold, the cost aiusage actually stored equals a
base-rates-only recomputation to the cent:

| | |
|---|---|
| stored `SUM(cost_micro_usd)` over the 551 crossing rows | $77.5909 |
| recomputed at base rates only, same table | $77.5909 |
| difference | $0.0000 |

All 551 carry `price_source = "embedded-2026-08-09"`.

## 2. What ccusage does, for contrast

ccusage (`github.com/ccusage/ccusage`, cloned at HEAD 2026-08-15) implements
**both** semantics and picks between them per model on the presence of a
`long_context_threshold`.

`Pricing` carries four `*_above_200k` rates plus an `Option<u64>` threshold
(`rust/crates/ccusage-core/src/pricing.rs:59-82`):

```rust
    // Token count above which the `*_above_200k` rates apply. The field names
    // keep the LiteLLM `_above_200k_tokens` suffix for JSON compatibility, but
    // some providers switch tiers at a different point (OpenAI long-context
    // pricing starts above 272K input tokens), so the threshold is per model.
    pub(crate) long_context_threshold: Option<u64>,
```

`calculate_cost_from_pricing` branches on it
(`rust/crates/ccusage-core/src/cost.rs:114-179`). With a threshold present, the
whole request switches tier, and the tier is chosen by **total context
including cache reads and cache writes**:

```rust
    // Two-stage pricing: a per-model `long_context_threshold` means the
    // request's input size selects the tier and every bucket is billed entirely
    // at that tier's rate. The whole request switches once input exceeds the
    // threshold, so this is not a marginal breakpoint.
    ...
    // `input_tokens` here is the uncached remainder - adapters normalize usage
    // into the Claude shape - but the vendor's tier is chosen by the request's
    // whole context, cached or not: a Grok turn re-reading 8M cached tokens
    // with 10K fresh ones is a long-context request, not a short one.
    if let Some(threshold) = pricing.long_context_threshold {
        let context_tokens = usage
            .input_tokens
            .saturating_add(usage.cache_read_input_tokens)
            .saturating_add(cache_create_5m_tokens)
            .saturating_add(cache_create_1h_tokens);
        let long_context = context_tokens > threshold;
```

Without a threshold it falls back to marginal, per bucket, at 200K
(`cost.rs:151-192`):

```rust
    // LiteLLM `*_above_200k_tokens` data keeps its marginal above-threshold
    // semantics at the default 200K boundary.
```

```rust
pub fn tiered_cost(tokens: u64, base: f64, above: Option<f64>, threshold: u64) -> f64 {
    if let Some(above) = above
        && tokens > threshold
    {
        return (threshold as f64 * base) + ((tokens - threshold) as f64 * above);
    }
    tokens as f64 * base
}
```

Note the marginal branch applies the threshold **per bucket**: 300K input
tokens are billed 272K-at-base plus 28K-at-tier, and a 300K cache read is
independently split the same way. That is what LiteLLM's field naming literally
says, and ccusage treats it as the fallback for models it has no better source
for.

**Where the threshold comes from: models.dev, as a second source.** LiteLLM
entries are loaded with `long_context_threshold: None`
(`pricing.rs:730`) — LiteLLM publishes rates but no machine-readable boundary,
only a boundary baked into a field name. `fill_long_context_rates_from_models_dev`
(`pricing.rs:1296-1328`) then stamps both the rates and the threshold from an
embedded models.dev snapshot, but only onto entries that carry no tier rate of
their own:

```rust
    /// Entries that already carry any tier rate are left untouched so upstream
    /// data wins once it exists. The check is deliberately all-or-nothing
    /// rather than per field: each source's rates assume that source's own
    /// boundary, so mixing fields across sources would price both tiers wrong.
```

models.dev's shape is what makes it useful — the boundary is data, not a field
name (`models-dev-pricing.json`, `gpt-5.6-sol`):

```json
"tiers": [ { "cache_read": 1, "cache_write": 12.5, "input": 10, "output": 45,
             "tier": { "size": 272000, "type": "context" } } ]
```

`"type": "context"` is the load-bearing part: the tier is keyed on the
request's context size, which is a statement that the tier is a property of the
request, not of a token band.

The other provenance ccusage tracks is `cache_read_explicit` /
`cache_create_explicit` (`pricing.rs:65-68`) — whether a cache rate was
published or derived from `input * 1.25` / `input * 0.1`
(`pricing.rs:719-722`) — so later provider-fact patches know which numbers they
are allowed to correct. That is a separate concern from tiering, but it is the
same discipline: record whether a number was stated or inferred.

The tests pin the difference exactly (`cost.rs:262-349`): `gpt-5.6-sol` with
300K input and 1K output bills the output at the long rate too (3.0451, not
0.5451+), and `grok-4.5` with 10K input and 500K **cache reads** selects the
long tier (0.352 vs 0.056 — a 6.3x swing decided entirely by cached tokens).

## 3. Ground truth per model in this ledger

19 distinct models appear in `usage_events` (the ticket's "17" is stale; two
more arrived since). Resolved through aiusage's own `lookupKeys` /
`providerPrefixes` logic against the 2026-08-15 LiteLLM cache:

| model | provider | LiteLLM key | `max_input_tokens` | context tier in LiteLLM | in models.dev |
|---|---|---|---|---|---|
| gpt-5.6-sol | openai | `gpt-5.6-sol` | 1,050,000 | **272K** (input/output/cache_read/cache_write, + `_flex`) | tier `{size: 272000, type: context}` |
| gpt-5.6-terra | openai | `gpt-5.6-terra` | 1,050,000 | **272K** (same four, + `_flex`) | tier `{size: 272000, type: context}` |
| gpt-5.6-luna | openai | `gpt-5.6-luna` | 1,050,000 | **272K** (same four, + `_flex`) | tier `{size: 272000, type: context}` |
| gpt-5.5 | openai | `gpt-5.5` | — | **272K** (input/output/cache_read) | tier `{size: 272000, type: context}` |
| gpt-5.4 | openai | `gpt-5.4` | — | **272K** (input/output/cache_read) | tier `{size: 272000, type: context}` |
| gpt-5 | openai | `gpt-5` | 272,000 | none | none (context 400,000) |
| gpt-5-mini | github | `gpt-5-mini` | — | none | — |
| claude-opus-5 | anthropic | `claude-opus-5` | 1,000,000 | none | none, context 1,000,000 |
| claude-sonnet-5 | anthropic | `claude-sonnet-5` | 1,000,000 | none | none, context 1,000,000 |
| claude-fable-5 | anthropic | `claude-fable-5` | 1,000,000 | none | none, context 1,000,000 |
| claude-opus-4-8 | anthropic | `claude-opus-4-8` | 1,000,000 | none | none, context 1,000,000 |
| claude-opus-4-7 | anthropic | `claude-opus-4-7` | — | none | — |
| claude-haiku-4-5-20251001 | anthropic | `claude-haiku-4-5-20251001` | — | none | — |
| claude-haiku-4.5 | github | *(unpriced)* | — | — | — |
| gpt-5.3-codex-spark | openai | *(unpriced)* | — | — | — |
| gpt-5.4-mini-fast | openai | *(unpriced)* | — | — | — |
| gpt-5.6-luna-fast | openai | *(unpriced)* | — | — | — |
| hy3-free | opencode | *(unpriced)* | — | — | — |
| big-pickle | opencode | *(unpriced)* | — | — | — |

Five models carry a context tier, all OpenAI, all at **272K**, all corroborated
by models.dev with identical rates. Not one model in this ledger carries a
`*_above_200k_tokens` field. The 200K family exists in LiteLLM (59 models) but
covers claude-sonnet-4/4.5/4.6, gemini-2.5/3.x-pro and the xai/grok line —
none of which this machine runs.

**The Claude 5 generation is flat-priced at 1M context, per both sources.**
`claude-opus-5`, `claude-sonnet-5`, `claude-fable-5` and `claude-opus-4-8` all
publish `max_input_tokens: 1000000` with no tier fields in LiteLLM, and
models.dev independently lists them at `limit.context: 1000000` with no
`tiers`. The predecessor generation did tier: `claude-sonnet-4-5` in LiteLLM
carries `input_cost_per_token_above_200k_tokens: 6e-06` against a base
`3e-06`, `output ... 2.25e-05` against `1.5e-05`, `cache_read ... 6e-07`
against `3e-07`, and `max_input_tokens: 200000`. So the two sources agree that
the >200K premium did not carry forward to the 5 generation.

### What the providers themselves say

Provider docs are the authority; LiteLLM and models.dev are interpretations of
them. All URLs fetched 2026-08-15.

**Every provider bills the whole request. None bills marginally.**

**OpenAI** settles it in one sentence, on the model page rather than the
pricing page —
[developers.openai.com/api/docs/models/gpt-5.6-sol](https://developers.openai.com/api/docs/models/gpt-5.6-sol):

> "Prompts with >272K input tokens are priced at 2x input and 1.5x output for
> **the full request**. Cache writes are billed at 1.25x the uncached input
> token rate."

The pricing table at
[developers.openai.com/api/docs/pricing](https://developers.openai.com/api/docs/pricing)
is laid out as two rate cards, not two bands — its columns are "Short context
input / Short context cached input / Short context cache writes / Short context
output / Long context input / ..." — and gives `gpt-5.6-sol` as
`$5.00 | $0.50 | $6.25 | $30.00 | $10.00 | $1.00 | $12.50 | $45.00`, matching
LiteLLM's base and `_above_272k_tokens` rates exactly. `gpt-5.5` and `gpt-5.4`
are listed with a hard cap instead (`gpt-5.5 (<272K context length)`), which is
consistent with them having no window above the boundary to price.

Two details the sentence pins down: the threshold is measured on **input**
tokens (output does not push a request over the line, it is merely billed at
1.5x once the request is over), and the switch is `>`, not `>=`.

**Google** states it as a footnote under both the Gemini 3 and Gemini 2.5
tables at
[cloud.google.com/vertex-ai/generative-ai/pricing](https://cloud.google.com/vertex-ai/generative-ai/pricing):

> "If a query input context is longer than 200K tokens, **all tokens (input and
> output) are charged at long context rates**."

Its column headers are `Price (/1M tokens) <= 200K input tokens` /
`> 200K input tokens`, with a parallel pair for cached input. The
[Gemini API pricing page](https://ai.google.dev/gemini-api/docs/pricing) carries
the same rates ("prompts <= 200k tokens" / "prompts > 200k tokens") with no
explanatory note, so the Vertex footnote is the settling text.

**Anthropic historically tiered and has since removed it.** The Claude Sonnet 4
1M-context announcement,
[claude.com/blog/1m-context](https://claude.com/blog/1m-context), is a
two-row table keyed on prompt size — "Prompts ≤ 200K | $3 / MTok | $15 / MTok"
against "Prompts > 200K | $6 / MTok | $22.50 / MTok" — which is whole-request by
construction. The release notes date the mechanism precisely
([platform.claude.com/docs/en/release-notes/overview](https://platform.claude.com/docs/en/release-notes/overview)):
2026-02-05, "Long context pricing applies to requests exceeding 200k input
tokens"; 2026-03-13, the 1M window went GA for Opus 4.6 and Sonnet 4.6 "at
standard pricing"; 2026-04-30, the Sonnet 4/4.5 1M beta was retired outright.

**This resolves §3's open question, and it resolves it in LiteLLM's favour.**
[platform.claude.com/docs/en/about-claude/pricing](https://platform.claude.com/docs/en/about-claude/pricing):

> "Claude 4.6 and later models and Claude Mythos Preview include the full 1M
> token context window at standard pricing. (A 900k-token request is billed at
> the same per-token rate as a 9k-token request.) Prompt caching and batch
> processing discounts apply at standard rates across the full context window."

and
[platform.claude.com/docs/en/build-with-claude/context-windows](https://platform.claude.com/docs/en/build-with-claude/context-windows):

> "For every model with a 1M-token context window, 1M is the default: you don't
> need a beta header, and long-context requests are billed at standard
> pricing."

**There is no long-context tier on any Claude model this machine runs.** Both
data sources were right and the $3,070 hypothetical in §4 is dead. The tier
survives only on Vertex for Sonnet 4.5, whose footnote is also whole-request
and, unusually, uses `>=`: "If a query input context is longer than or equal to
200K tokens, all tokens (input and output) are charged at long context rates."
Sonnet 4.6 on Vertex is flat.

**Nobody documents marginal billing.** The provider-docs sweep found the
opposite stated in words twice ("for the full request", "all tokens (input and
output) are charged at long context rates") and by table structure the third
time. **LiteLLM's `*_above_200k_tokens` field naming describes which rate card
a request lands on, not a band of tokens priced separately.** ccusage's
fallback branch — marginal at 200K for any model with no models.dev threshold —
is therefore wrong for every model whose semantics are actually documented; it
is a conservative guess for models it has no boundary for, not a claim about
any provider.

**Unverified, and it matters:** no provider states whether cached input counts
toward the threshold. Anthropic's context-windows doc says all three of
`input_tokens`, `cache_read_input_tokens` and `cache_creation_input_tokens`
"count toward the window", but that is the *window*, not the price tier, and
Anthropic no longer has a tier. For OpenAI the reconciliation is structural
rather than documented: the Responses API reports `cached_tokens` as a
**subset** of `input_tokens`, and the codex adapter subtracts it out
(`input := t.input - cached`,
[codex.go:497](../../internal/adapter/codex/codex.go)), so `input_tokens +
cache_read_tokens` in this ledger *is* OpenAI's `input_tokens` — the number its
">272K input tokens" sentence is about. Including cache reads in the threshold
is therefore not an extrapolation from ccusage's Grok reasoning; it is undoing
the adapter's own normalization. The only genuinely open case is a provider
that reports cache reads outside its input count, and none of the five affected
models is one. The single third-party source found (an OpenAI community thread,
no staff response) asserts input-only; it is not evidence.


## 4. Exposure on the real ledger

Read-only snapshot taken with
`sqlite3 "file:$HOME/.local/share/aiusage/usage.db?mode=ro" ".backup /tmp/usage-ro.db"`;
the live database was never opened for writing.

Context size is reconstructed as `input_tokens + cache_read_tokens +
cache_creation_tokens`, which is the provider's prompt size: the codex adapter
stores `input = raw_input - cached`
([internal/adapter/codex/codex.go:495-520](../../internal/adapter/codex/codex.go)),
so the buckets are disjoint and their sum is what OpenAI counted.

### Rows crossing 272K, by model and tool

| model | tool | events | crossers | max context |
|---|---|---|---|---|
| gpt-5.6-terra | opencode | 1,502 | 315 | 370,578 |
| gpt-5.6-sol | opencode | 1,789 | 189 | 356,104 |
| gpt-5.4 | opencode | 2,351 | 32 | 309,308 |
| gpt-5.6-luna | opencode | 153 | 15 | 288,911 |
| gpt-5.5 | opencode | 8,574 | 0 | 263,826 |
| gpt-5.6-sol | codex | 142,205 | 0 | 249,572 |
| gpt-5.6-luna | codex | 9,250 | 0 | 244,469 |
| gpt-5.6-terra | codex | 5,718 | 0 | 243,176 |

**Every crossing row came from opencode. Codex never crosses**, on 157,173
events, because it compacts below the boundary — its highest observed context
is 249,572, some 22K short. All 551 crossers landed in **2026-07**; June and
August have zero.

Threshold input matters, and cache reads are what push rows over: counting
uncached `input_tokens` alone, only **17** rows cross 272K. Counting
`input + cache_read`, **551** do. Cache writes add none on top. This is exactly
the case ccusage's `cached_context_selects_the_long_context_tier` test was
written for.

### Dollar delta, three interpretations

All three recomputed from the same 2026-08-15 LiteLLM table, so the deltas are
purely semantic. A = base rates only (what aiusage does today); B = marginal,
per bucket, threshold 272K; C = whole-request, tier chosen by total context.

| model | events | crossers | A base | B marginal | C whole-request | C − A | C − A % |
|---|---|---|---|---|---|---|---|
| gpt-5.6-sol | 143,994 | 189 | $12,632.28 | $12,636.30 | $12,676.37 | +$44.09 | +0.35% |
| gpt-5.5 | 8,574 | 0 | $801.48 | $801.48 | $801.48 | $0.00 | 0.00% |
| gpt-5.6-terra | 7,220 | 315 | $285.63 | $288.58 | $313.28 | +$27.65 | +9.68% |
| gpt-5.4 | 2,351 | 32 | $84.23 | $84.35 | $86.83 | +$2.60 | +3.08% |
| gpt-5.6-luna | 9,403 | 15 | $26.80 | $26.80 | $26.98 | +$0.19 | +0.70% |
| **total** | **171,542** | **551** | **$13,830.42** | **$13,837.51** | **$13,904.94** | **+$74.53** | **+0.54%** |

Restricted to the 551 crossing rows alone:

| | base (stored) | marginal | whole-request |
|---|---|---|---|
| gpt-5.6-sol (189) | $46.05 | $50.07 | $90.14 |
| gpt-5.6-terra (315) | $28.71 | $31.67 | $56.36 |
| gpt-5.4 (32) | $2.64 | $2.76 | $5.24 |
| gpt-5.6-luna (15) | $0.19 | $0.19 | $0.38 |
| **551 rows** | **$77.59** | **$84.69** | **$152.12** |

So: under-billed by **$74.53 (96% of the correct figure)** if whole-request is
right, or **$7.09 (9%)** if marginal is right. Against the full $24,714.65
ledger that is **0.30%** or **0.029%**.

The marginal number is small for a structural reason worth stating: marginal
tiering only bills the excess of each individual bucket above 272K, and these
turns are *broad* rather than *deep* — a 356K context is a 300K cache read plus
a 50K input, and neither bucket alone clears 272K by much. Whole-request
tiering doubles the rate on all 356K. The two interpretations differ by a
factor of ten on the same rows.

### The number that dwarfed it, before §3 killed it

**Hypothetical, and now refuted — kept because it is the reason the Anthropic
question was checked against primary sources instead of assumed.** If the
claude-5 line did carry
a >200K premium at the previous generation's multipliers (2x input, 1.5x
output, 2x cache read, 2x cache write, taken from `claude-sonnet-4-5`'s own
published tier fields), whole-request tiering on this ledger would cost:

| model | events | ctx > 200K | base | tiered | delta |
|---|---|---|---|---|---|
| claude-opus-5 | 40,205 | 7,441 | $4,224.14 | $5,611.87 | +$1,387.73 (+32.9%) |
| claude-fable-5 | 9,497 | 3,354 | $2,778.86 | $4,227.96 | +$1,449.10 (+52.1%) |
| claude-opus-4-8 | 541 | 239 | $315.19 | $546.77 | +$231.58 (+73.5%) |
| claude-sonnet-5 | 157 | 15 | $6.08 | $8.14 | +$2.05 (+33.8%) |
| claude-opus-4-7 | 341 | 0 | $38.10 | $38.10 | $0.00 |
| claude-haiku-4-5 | 2,646 | 0 | $22.32 | $22.32 | $0.00 |
| **total** | | **11,049** | **$7,384.69** | **$10,455.15** | **+$3,070.46** |

**$3,070 versus $74.** The sensitivity of this ledger to *whether an Anthropic
tier exists* was forty times its sensitivity to *how the OpenAI tier is
applied* — so that was the question worth answering first. Anthropic's own docs
answer it: the 1M window is standard-priced, "a 900k-token request is billed at
the same per-token rate as a 9k-token request". **This $3,070 is not owed.** Both
data sources were correct and aiusage prices those 11,049 rows right today.

## 5. Recommendation

### Correct semantics, per affected model

| model | threshold | measured on | applies to | source |
|---|---|---|---|---|
| gpt-5.6-sol | 272,000, strict `>` | input tokens (uncached + cached + cache writes) | the full request, every bucket | [OpenAI model page](https://developers.openai.com/api/docs/models/gpt-5.6-sol) |
| gpt-5.6-terra | 272,000, `>` | same | same | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) |
| gpt-5.6-luna | 272,000, `>` | same | same | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) |
| gpt-5.5 | 272,000, `>` | same | same | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) |
| gpt-5.4 | 272,000, `>` | same | same | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) |
| every claude-* in this ledger | none | — | flat to 1M | [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing) |
| gpt-5, gpt-5-mini | none | — | flat within the window | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) |

Not in this ledger but in the same table and worth encoding correctly when it
appears: Gemini 2.5/3.x Pro at 200,000 strict `>` (whole request, "all tokens
(input and output)"), Claude Sonnet 4.5 **on Vertex only** at 200,000 with
`>=`, and the xai/grok line at 200,000.

### Does aiusage need a change?

**Yes, but it is a correctness fix, not an incident.** Severity: ⚠️ low on this
ledger, structural in general.

- The under-bill is **$74.53 on $24,714.65 (0.30%)**, concentrated in 551 rows
  in one month. Per model it reaches **9.68%** (gpt-5.6-terra), so the ledger
  total understates how wrong a single answer can be.
- It is unbounded in the wrong direction: the exposure is a function of how
  long the user's contexts get, and every crossing row here came from opencode,
  which does not compact. Codex compacts below 272K and produced zero crossers
  on 157,173 events. A user who runs a non-compacting harness against a 1M
  window is exposed far more than this machine is.
- It fails silently. There is no "unpriced" signal, no warning: a long-context
  turn gets a plausible number that is simply half of the real one, and the
  `price_source` stamp names a table that did publish the right rates.

**What the fix has to be.**

1. **The threshold is per-model data, and LiteLLM already carries it — in the
   field name.** The live table has five distinct boundaries: 128K, 200K, 256K,
   272K, 512K, across 120 models. A hardcoded 200K constant is wrong for four
   of the five. Parsing `^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost)_above_(\d+)k_tokens$`
   recovers the boundary from LiteLLM alone, and **no model in the table carries
   more than one threshold**, so a single `Option[int64]` per model is a
   sufficient data model — no multi-band ladder needed.
2. **The grammar composes, and the composites must be handled or deliberately
   ignored.** The full shape is
   `<bucket>[_above_1hr]_above_<N>k_tokens[_priority|_flex]`:
   `cache_creation_input_token_cost_above_1hr_above_200k_tokens` (10 models),
   `input_cost_per_token_above_272k_tokens_priority` (14),
   `..._above_272k_tokens_flex` (4). aiusage already models the 1h cache write
   and the batch tier, so the 1h × long-context cross-product is the one that
   actually needs a field. `_priority` / `_flex` are service tiers aiusage does
   not model at all today — this ledger has only `''` and `'standard'`, so they
   can be skipped, but skipping should be a decision in a comment rather than an
   accident of the parser.
3. **`Charge` needs a context-size input.** Today the price function has no
   argument that could express the tier
   ([pricing.go:81-101](../../internal/pricing/pricing.go)); the tier is a
   property of the request, so `Rates.Cost` has to see
   `Input + CacheRead + CacheWrite5m + CacheWrite1h`. Not `Output` — every
   provider measures the boundary on the prompt.
4. **The embedded snapshot must stop stripping the fields**, or an air-gapped
   install keeps the bug with no way to notice. This is the rung that priced all
   551 rows (`price_source = "embedded-2026-08-09"`).
5. **A test with a cached-heavy charge is the one that matters.** Only 17 rows
   cross on uncached input; 551 cross once cache reads are counted. An
   implementation that reads the tier fields but measures the threshold on
   `Input` alone would fix 3% of the exposure and look correct.

**What the fix does NOT do: repair history.** `cost_micro_usd` is stamped at
insert ([collect.stampCost](../../internal/collect/collector.go)) into a table
whose BEFORE UPDATE trigger aborts. The 551 stored rows keep $77.59 forever.
Correcting them means 551 `kind='adjustment'` rows totalling +$74.53.
**Recommendation: don't.** $74.53 of adjustment rows to correct a month-old
0.3% error is more ledger noise than it is worth, and the append-only invariant
is better served by a documented known-underbill window than by a batch of
synthetic corrections. Fix forward; note the window here.

### Is models.dev worth adding as a second source?

**No, not for this.** ⚠️ It is the more expensive answer to a question LiteLLM
already answers.

What models.dev genuinely adds is `"tier": {"size": 272000, "type": "context"}`
— the boundary as data with an explicit statement that it is a *context* tier.
That is cleaner than parsing a field name. But:

- **LiteLLM's field names are unambiguous and complete for every affected
  model.** All five carry `_above_272k_tokens`; the regex above recovers 120
  models' boundaries with zero multi-threshold ambiguity. The parse is a dozen
  lines against a second embedded snapshot, a second fetch URL, a second
  provenance stamp and a merge policy.
- **The two sources agree on every model in this ledger** — identical rates for
  all five tiered models, identical 272K boundary, identical "no tier" verdict
  for the whole Claude 5 generation. A second source that never disagrees is not
  resolving ambiguity; it is confirming a reading.
- **Where they do disagree, a second source makes it worse, not better.** On
  `claude-sonnet-4-5`, LiteLLM says `max_input_tokens: 200000` with a >200K
  premium; models.dev says `limit.context: 1000000` with no tiers. Both are
  defensible readings of a model whose 1M beta was retired on 2026-04-30, and
  ccusage's own merge rule
  ([pricing.rs:1292-1295](https://github.com/ccusage/ccusage)) is explicitly
  all-or-nothing per source precisely because mixing them prices both tiers
  wrong. Adopting models.dev means adopting that conflict-resolution problem.
- **It solves a different problem well.** Five models in this ledger are
  unpriced (`gpt-5.3-codex-spark`, `gpt-5.4-mini-fast`, `gpt-5.6-luna-fast`,
  `claude-haiku-4.5` via github, plus the opencode locals `hy3-free` and
  `big-pickle`). ccusage's `enable_embedded_models_dev_fallback` exists for
  exactly that. If models.dev is ever added, **coverage is the reason** — not
  tier semantics.

What is worth borrowing from ccusage regardless is the **provenance discipline**
rather than the second source: its `cache_read_explicit` / `cache_create_explicit`
flags record whether a rate was published or derived from `input * 1.25`, so a
later correction knows which numbers it is allowed to touch. aiusage has the
same derivation (`cacheRead == 0 -> in`,
[pricing.go:145-156](../../internal/pricing/pricing.go)) and no record that it
happened.

### Out of scope, flagged

⚠️ Pre-existing, found incidentally, not researched here:
`regional_processing_uplift_multiplier_us: 1.1` and
`provider_specific_entry: {"us": 1.1}` appear on the same LiteLLM entries and
are also unread — a 10% regional uplift aiusage cannot see, since nothing in the
ledger records the processing region. Severity low, and arguably unfixable
without a signal the sources do not emit. The related `fast` multiplier is
**not** a gap: the claude-code adapter appends `-fast` to the model id
([claudecode.go:571](../../internal/adapter/claudecode/claudecode.go)), so a
fast turn either resolves to a fast-specific key or lands unpriced, which is the
honest outcome.

## Appendix: method

- Ledger snapshot: `sqlite3 "file:$HOME/.local/share/aiusage/usage.db?mode=ro"
  ".backup /tmp/usage-ro.db"`. Read-only handle; the live collector's lock was
  never contended and the database was never opened for writing.
- Price table: `~/.local/share/aiusage/prices-litellm.json`,
  `_meta.fetched = 2026-08-15`, 3,020 models, fetched by aiusage's own refresh
  from `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json`.
- Model -> key resolution reimplements `pricing.lookupKeys` and
  `pricing.providerPrefixes` exactly, so the rates quoted are the rates aiusage
  would have used.
- Cost recomputation reimplements `Rates.Cost`, including the zero-rate
  fallbacks (cache read -> input, cache write -> input) and
  `model.ReasoningModeFor` (opencode additive, codex/claude-code subset). It
  reproduces the stored cost of the crossing rows to $0.0000, which is the
  check that the reimplementation is faithful.
- ccusage: `github.com/ccusage/ccusage`, shallow clone at HEAD on 2026-08-15.
- Service tiers present in the ledger: `''` (315,128 rows) and `'standard'`
  (53,387). No `flex` or `priority` rows, so LiteLLM's `_flex` / `_priority`
  tier variants are not in play here.
