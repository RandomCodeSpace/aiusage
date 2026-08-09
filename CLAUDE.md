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
kind='adjustment'. aggregate_state and source_checkpoints are the only mutable
data tables (schema_meta holds the mutable version stamp) — working state, not
history.

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

## Time buckets

Timestamps are stored as UTC unix seconds. Grouping keys are formatted by
SQLite — `strftime(layout, event_time_unix, 'unixepoch', 'localtime')` in
internal/store/query.go — as lexically sortable local wall-clock strings
('%Y-%m-%d', '%Y-%m-%d %H', ...). The trap: SQLite's 'localtime' follows the
system timezone, not Go's time.Local, so mutating time.Local in a test moves
Go's formatting while SQLite's buckets stay put and the keys silently
disagree. Produce both sides of any bucket-key comparison through the same
store query (see TestScrubCompositionBracketsLocalDay in internal/tui).

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

Charts are ntcharts (linear scale only).

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
