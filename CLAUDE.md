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

**A stale rollup falls back to the ledger, and says so.** The v4 migration
creates usage_rollup EMPTY and leaves the fill to the collector's next pass, so
a read-only `aiusage serve` on a machine with no daemon would otherwise answer
every rollup-served question with zeros over a full ledger. internal/web asks
store.RollupStale (watermark vs MAX(id), then SUM(events) vs COUNT(*)), caches
the verdict against the database's write time with a short TTL, and routes to
the ledger while it is stale. Every /api/summary and /api/facets response
carries "source": "rollup" | "ledger".

**The web surface is read-only and Host-checked.** `aiusage serve` opens the
store with store.OpenReadOnly and never takes the collection lock. Every
request must be addressed to a Host in the allowlist (localhost, 127.0.0.1,
::1, plus `serve --allowed-hosts`), or it is refused with 421 before a handler
runs, and a WebSocket Origin must name a host in that same list (an absent
Origin is a non-browser client and is allowed). Binding loopback is not the
defence: DNS rebinding makes an attacker's page same-origin with this port, and
the Host header is the one part of that request the page cannot choose. A
reverse proxy preserves the public Host, so a proxied deployment must list its
name. No response ever carries usage_events.raw.

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
call) and `calls_in_turn` (how many calls shared that one usage object); tokens
and cost are derived on READ by joining the ledger and dividing
(store.SummarizeActivity, store.TopActivity). One assistant turn commonly emits
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
`.message.usage` are in the SAME record) and rides the existing deduper, so a
sidechain replay cannot count a call twice; opencode joins exactly via
`part.message_id` -> `message.id`, the id its usage dedup key is already built
from; codex NEVER attributes, because its `token_count` records share no
identity with its `function_call`/`custom_tool_call` records (verified: zero of
261,938 local token_count records carry a turn_id, while every call does), and a
positional guess is not on offer.

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

`internal/service` sits off to the side: it may import stdlib, `config` and
`buildinfo`, and nothing else — never collect/store/report/tui/web. It knows
how a machine supervises processes, not what aiusage collects.

## Supervision

aiusage installs itself. Two systemd **user** units — `aiusage-collect.service`
and `aiusage-web.service` — are written to `$XDG_CONFIG_HOME/systemd/user`
(default `~/.config/systemd/user`), enabled and started. No root anywhere: user
units, `loginctl enable-linger` for survival past logout, and every call
non-interactive (`--no-ask-password`).

**Install is create-if-missing; removal is stamp-gated.** An existing unit file
is never rewritten — the user may have edited it — only its enabled/active state
is corrected. `aiusage setup --force` is the sole path that replaces one, and it
then restarts whatever it rewrote, because the point of a new ExecStart is that
the new one runs. Every generated unit opens with the
`# aiusage-generated-unit` stamp, and `setup --remove` refuses to delete a unit
file that lacks it — naming the file and `--force`, and exiting NON-ZERO, since
`setup --remove && rm -rf ~/.config/systemd/user` must not proceed as though the
directory were clean — since create-if-missing exists precisely because the file
may be the user's own (the hand-written units this feature replaced carry no
stamp, and removal must refuse them). The whole
thing happens on any run that already auto-spawns a daemon (`ensureDaemon`);
`setup` is the explicit, inspectable version of it, and `doctor` prints which of
the three states holds (systemd units, unsupervised background process, none).
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
database, state, config, and the unit directory itself — while the unit NAMES
stay fixed constants, so a sandboxed-HOME run used to query the machine's REAL
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
two long-lived services — one of them a network listener — is not a side effect
to perform in silence behind `aiusage today`, so a run that CHANGED the machine
prints its account to stderr (`service.Result.Changed`, `cmd.reportSupervision`).
A run that changed nothing prints nothing: the steady state is a dozen report
commands a day finding everything in place. Lines that explain something which
did NOT happen — a dashboard skipped for a busy port, a linger refusal — do not
count as a change; they ride along with the install that first produced them and
are silent afterwards. A failure is not routed there either: it keeps the single
warning line the CLI has always printed before falling back.

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
deadline over the WHOLE attempt: an install is a dozen calls, and a manager
answering each of them slowly would otherwise add its latency twelve times to a
report command. When the budget expires supervision is abandoned mid-sequence
and the fallback takes over. Every phase gets one, not just `ensureDaemon`:
`doctor`'s supervision block shares that same 5s (it is five calls, measured at
20s against a manager answering in 4s each) and reports a unit it got no answer
about as state unknown rather than inventing "inactive, not enabled"
(`service.UnitStatus.StateKnown`); the explicit `setup` gets its own, far larger
`cmd.setupBudget` (30s), because the user asked for that one and is watching it,
and abandoning an install between writing a unit and starting it is worse than
waiting. A command that ran out of time SAYS so — `timed out after 5s` for the
per-call bound, `the supervision deadline expired` for the phase — rather than
reporting the signal that killed it (`signal: killed` names the mechanism and
hides the cause).

**The web unit may never cost the machine its collector.** It is installed only
when `buildinfo.HasWebUI` — `serve` exits 1 without it, and a unit that exits 1
under `Restart=always` is a restart loop — and `serve` stays in `daemonSkip` so
serving a page never starts a second process on the same port. Before starting
it, the install probes its address (`Manager.Dial`): something already answering
there is a manual `aiusage serve`, and starting the unit against it would be a
restart loop until StartLimitBurst trips, so the unit is written and then left
alone entirely — not started, not even enabled, with one line saying so.
`Install` returns the collection unit's error and no other; every dashboard
failure is a line in the result. Version sync under systemd is `systemctl --user
restart` of the units that are ACTIVE (the web unit does not self-exec on binary
replacement the way the collector does); an inactive unit is never started by
that path, because a second collector against a single-holder lock is worse than
a stale one.

**Enable and start are two calls, and the second can fail.** An enable that this
install performed is rolled back when the start then fails: a unit left enabled
but not started comes up at the next login against the collection lock the
fallback daemon is by then holding. `is-enabled` is matched exactly, so
`enabled-runtime` (enabled until the next reboot, and no longer) is not enabled
and gets a persistent enable.

Both units carry `NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict`,
`ProtectKernelTunables`, `ProtectControlGroups`, `RestrictSUIDSGID` and explicit
`ReadWritePaths`. Honest caveat, measured on this host: for **user** units on
Ubuntu 24.04 the namespace-building directives (ProtectSystem, PrivateTmp,
ProtectKernelTunables, ProtectControlGroups) are inert, because
`kernel.apparmor_restrict_unprivileged_userns=1` stops `systemd --user` building
the namespace and it silently carries on without it. They are kept because they
are real on hosts without that restriction, and because the ReadWritePaths they
imply document what the daemon is allowed to touch.

Tests never touch a real systemd: `service.Manager` takes an injected `Runner`,
`Dial` and `UnitDir`, and `internal/cmd`'s TestMain pins the supervisor seam to
a refusing fake for the whole package.

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
