# Machine output contract

This document is for scripts and programs that parse aiusage output. After
reading it, a consumer should be able to choose a format, handle missing cost
honestly, and reject an incompatible payload instead of silently reading the
wrong field.

`v0.5.0` fixes the formats below as version 1. The current command spellings
continue to emit version 1. A future incompatible shape needs an explicit new
format or version. It does not replace version 1 in place.

## Command behavior

Machine output goes to stdout. Notices and errors go to stderr. A failed
command returns nonzero. Human output is not a serialization format.

Timestamps use RFC 3339 in UTC. Integer token and micro-dollar values remain
integers. Vocabulary fields such as tool, model, provider, service tier, and
price source may gain new values without changing the schema.

## Summary JSON version 1

`today --json`, `last --json`, and `summary --json` emit
`summary-json/v1`.

The top-level object contains exactly these keys:

```text
GroupBy
Buckets
Totals
```

Every bucket and `Totals` object contains exactly these keys:

```text
CacheCreation
CacheRead
ComputedCostEvents
CostApproximate
CostKnown
CostMicroUSD
DisplayCostMicroUSD
Events
Input
Keys
OrderedKeys
Output
Reasoning
Sessions
Total
UnpricedEvents
```

When `GroupBy` includes `provider`, each bucket also contains the conditional
key `provider_label`. It is absent from other groupings and from `Totals`.
`Keys.provider` keeps the raw stored value and remains an empty string when the
source did not name a provider. `provider_label` contains the human label
`unknown` for that case.

`CostMicroUSD` is the exact sum stamped on events. `DisplayCostMicroUSD` may
include current-table estimates for unstamped events. Consumers must read
`CostKnown` and `CostApproximate` before presenting either number as a cost.
`UnpricedEvents` states how many events lack a stamped cost.

An empty ledger still emits the complete top-level object and a zero-valued
`Totals` object. A grouped query has an empty `Buckets` array. An ungrouped
query has one zero-valued bucket because that bucket is the aggregate result.

## Event JSON version 1

`export --format json` emits `events-json/v1`. Each event contains exactly
these keys:

```text
CacheCreationTokens
CacheReadTokens
CostMicroUSD
DedupKey
EventTime
InputTokens
Kind
MessageID
Model
ObservedTime
OutputTokens
PriceSource
Project
Provider
ReasoningTokens
RequestID
ServiceTier
SessionID
SourcePath
Tool
TotalTokens
```

An unpriced event has `CostMicroUSD: null`. Zero means the source reported or
the pricing owner calculated a real zero cost. An empty export is `[]`.

Raw provider data is absent by default. `--include-raw` adds exactly one key,
`Raw`, to every event. This opt-in can expose content stored by older aiusage
versions, so a consumer must request it deliberately.

## Event CSV version 1

`export --format csv` and `summary --csv` emit `events-csv/v1`. The header order
is fixed:

```text
tool,model,session,project,event_time,observed_time,input,output,cache_creation,cache_read,reasoning,total,request_id,message_id,source_path,kind,provider,service_tier,cost_micro_usd,cost_usd,price_source
```

Unpriced cost cells are empty. They are never `0`. An empty export contains the
header and no data rows.

`export --include-raw` appends one trailing `raw` column. It does not move any
version 1 column. `summary --csv` has no raw-data flag.

## Change rules

Version 1 freezes field names, field presence, value types, nullability,
timestamp and numeric meaning, CSV order, empty output, and raw-data opt-in.
It does not gain an extra JSON key or a reordered CSV column.

A new vocabulary value is compatible. A value correction is compatible only
when the old value broke the documented accounting rules. Such a fix ships
with a regression fixture and release note.
