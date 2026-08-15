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
positional guess is not on offer; copilot never attributes either, for the same
reason from a different shape — its `execute_tool` span's parent is the
`invoke_agent` span, which makes it a SIBLING of the `chat` spans the usage rows
are built from, its `gen_ai.tool.call.id` occurs exactly once in the whole
export, and the only handle it shares with usage is the traceId, which covers
every turn of a conversation rather than one of them.

**Copilot activity comes from SPANS, never from metric dataPoints.** One OTEL
file carries both shapes and only one of them counts anything. A span is written
once per operation, so one `execute_tool` span is one tool call.
`github.copilot.tool.call.count` (and `agent.turn.count`,
`invoke_agent.tool_calls`, ...) is a CUMULATIVE counter re-exported on the
exporter's timer: measured on a live export, a session that made exactly ONE
tool call had produced 226 dataPoints, every one of them value 1 under an
identical attribute set. Counting or summing dataPoints reports that call once
per export interval — a 226x inflation of a single `view`. The export names no
skill and no hook: the skill list on `invoke_agent` is what was AVAILABLE, and an
invoked skill arrives as a tool call named `skill` whose skill name lives in
arguments the exporter does not write. So copilot emits kind='tool' rows and
nothing else.

**Skill cost is a property of the TURN, and lives in its own table** (schema v6,
usage_skill_context). An activity row of kind='skill' records the turn that
INVOKED a skill — one call — not the thousands of turns the skill then spends;
locally that is 44 invocation rows against 8,039 records of real work. The
missing fact is Claude Code's top-level `attributionSkill`, a scalar string on
every assistant record emitted while operating inside a skill. Because it is
scalar, a usage row carries AT MOST ONE skill, and the table makes that a
constraint rather than a hope: usage_dedup_key is its PRIMARY KEY, usage_events'
dedup_key is UNIQUE, so the join is 1:1 and per-skill cost is a plain SUM with NO
divisor — over-attribution is unrepresentable, not merely avoided.

It is deliberately NOT another activity_events kind. Tool-call attribution splits
a turn's cost among the calls that shared it; skill-context attribution assigns
the whole turn to one skill. They are two PARTITIONS of the same dollars —
"which tool was called" and "which skill was running" — each honest alone and
meaningless added together, and sharing a table would have put that mistake one
forgotten WHERE clause away (SummarizeActivity grouped by tool, unfiltered by
kind, would have counted every skill turn twice). Nor is it a column on
activity_events: 41.8% of skill records (3,361 of 8,039) call no tool at all and
emit no activity row to hang a column on. Keying on the usage row captures those
for free. SummarizeSkillCost REFUSES ActivityFilter's Kinds/Names rather than
ignoring them, because honouring them means joining activity_events, where a turn
with two matching calls joins twice.

NESTING is recorded shallowly because that is all the source offers: a skill may
invoke another, and once the inner one runs the field names only the inner skill,
so cost lands on the innermost active skill. **v6 cannot be backfilled.** The
fact exists only on the source transcript — usage_events never carried it — and
the no-UPDATE trigger forbids adding it to a stored row regardless. Usage already
collected stays permanently unattributed to any skill; only the CONTEXT is lost,
never the cost, and every total that does not group by skill is unaffected.

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
