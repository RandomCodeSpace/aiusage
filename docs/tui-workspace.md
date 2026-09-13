# TUI usage workspace

The Usage workspace opens as a model-first view of the selected time range. It
shows Input, Output, Cache, and Cost at the top, current machine readings below
them, then a comparison table and details for the selected row. Provider is an
available group and filter scope alongside the existing usage dimensions.

The workspace fills the terminal and inherits its native background. A short or
narrow terminal keeps the four summary values, stacks machine readings, and
keeps the comparison available through More → Compare rows when the list cannot fit. Selected details use the space below
the list when it fits and remain available through the Details action. A wide
terminal adds the current-scope plot and a selected suggestion on the right.
The model list stays sized to its contents instead of filling spare rows.

## Run from this checkout

Run `go run .` from the repository root. This launches the production TUI with
your normal aiusage configuration and opens its existing database read-only.
Opening or using the TUI does not start, restart, or request a cycle from the
collector daemon. An independently running daemon continues collecting on its
own schedule. The TUI observes its database updates through the existing refresh
poll. A missing or incompatible database requires an explicit collection or
migration command before opening the dashboard.

The standalone `examples/tui-demo` remains a
synthetic demonstration and is not the production entry point.

## Controls

Group, Sort, Chart, and Inspector are independent:

- Group changes the comparison dimension. The default is Model.
- Sort changes row order only. It does not change the plot or selected metric.
- Chart changes the plotted metric only. It does not reorder the table.
- Inspector explains one metric, its scope, accounting, coverage, and cost
  provenance. Opening it does not change grouping, sorting, or chart state.

The direct range keys replace the current range immediately. They do not cycle
through an intermediate range. `[` steps to the previous window and `]` steps
forward when a later window is available.

Group, Sort, and Chart expand as inline button groups above the workspace. The
current value has brackets; an arrow marks keyboard focus. Click an option to
apply it, or use the arrow keys and Enter. Esc or either Close control collapses
the choices. The footer shows these controls while a chooser is active. Value
selection keeps the data on screen; inspectors and the More action list can
still open a separate view.

Changing range reads only the workspace timeline and comparison summaries.
Classic Overview's harness breakdown, scrub composition, and previous-period
totals load when that view is opened. An already selected range is a no-op;
returning to a cached range applies without a background load. Uncached ranges
use a short cancellable delay so rapid choices do not aggregate every intermediate
window. `loading` in the header means database reads, not collection; narrow
headers shorten it to `read`.

| Key | Action |
|---|---|
| `o` | Change Group |
| `s` | Change Sort |
| `c` | Change Chart |
| `d` | Open full Details |
| `i` | Inspect Cost and pricing coverage |
| `u` | Open Suggestions |
| `a` | Open More actions |
| `Enter` | Drill into the selected row |
| `Esc` | Return to the parent scope or close the current overlay |
| `↑` / `↓` | Move the row selection or scroll the active detail |
| `←` / `→` | Back / open the selected row |
| `F6` | Use Today |
| `F7` | Use 7 days |
| `F8` | Use 30 days |
| `F9` | Use All history |
| `1` to `5` | Open Overview, By Tool, By Model, Sessions, or Activity |

Every visible action has mouse parity. Tabs, ranges, group and sort controls,
chart controls, metric summaries, rows, details, suggestions, and More use the
same state transitions whether activated by keyboard or mouse. Mouse wheel
input scrolls the active list or detail where scrolling is available.

Repeated actions have separate hit regions in the toolbar, selected pane,
suggestion card, and footer. A click at one location cannot replace another
location's target. If an SSH client does not translate touch into terminal mouse
events, use the same actions through the keys above. Text selection and copying
depend on the terminal client's native selection mode or modifier; aiusage does
not provide a clipboard command or claim that touch selection works in every
client.

List search narrows the visible comparison rows without changing the usage
scope or headline totals. Model and project rows open sessions; harness and
provider rows open models. Back restores the prior scope, group, sort, search,
selected identity, and list position. A refresh that removes the selected row
chooses a remaining row and clears the explicit selection marker. Range changes
retain scope and reject results from an older pending read.

## Reading the data

Summary values and table rows come from production usage queries. Runtime code
does not substitute demo fixtures or sample usage when a query is empty.

Token counts use compact K/M/B/T labels, including combined cache counts in the
comparison table. Inspectors retain the detailed accounting. Metric names and
values use bold text and component colors. Secondary notes use quieter italic
text. Terminal fonts have a common cell size, so there is no separate heading
font size or numeric font weight. Italic appearance depends on the terminal font;
labels and selection markers remain meaningful without it or without color.

CPU, memory, and disk show current utilization as horizontal meters. Filled
cells use green below 85%, amber from 85%, and red from 95%. Empty track and
boundaries stay faint. Percentages and capacity readings remain visible at
narrow widths. Samples arrive from the existing background monitor every two
seconds, independently of the selected AI usage range.

Cost keeps its source and coverage visible. Reported events and events priced
from the local rate table remain distinct. Any event without a price is counted
as an unpriced event, and a partial amount remains a lower bound. An empty range,
an unavailable price, and a known measured zero are separate states.

Model-level code changes are unavailable because the current evidence cannot
attribute them to a model without guessing. When a source reports code changes
for a session, Session Details shows the exact recorded session-lifetime
snapshot. That number is not limited by the selected date range, is not a
repository diff, and must not be presented as model-attributed output.

Suggestions are deterministic local rules over bounded aggregate evidence.
They work without an AI endpoint and do not send prompts, source code, or
transcripts over the network.

Missing price coverage has first priority, followed by cache write/read evidence.
For example, 3 unpriced events out of 10 produce an Inspect Cost suggestion with
that denominator and the known amount. Cache writes with few reads produce an
Inspect Cache suggestion to examine the recorded counters. They do not establish
a cache hit rate or savings: reads can fall outside the period, and upstream
sources can omit writes. High cost or concentration alone produces no suggestion.
Empty usage also produces none. Stale evidence stays labeled and asks the user to
refresh before drawing conclusions. Sources without the relevant counters get
no inferred recommendation.

Suggestions presents selectable entries using the existing menu controls. Enter
or a click opens that entry's evidence and named Cost or Cache inspector, using
its captured values, scope, period, and stale status. Back returns to the same
suggestion row; another Back returns to the workspace. These transitions do not
query the database or change the selected model.

## Accounting and code-change limits

All count denominators describe usage events, not requests. Cache means recorded
read plus write tokens; aggregate data cannot recover an omitted cache subtype.
The stored total remains authoritative when token components overlap. A zero
component in a nonempty range is a recorded zero counter, with an explicit warning
that an omitted upstream field can also normalize to zero.

Cost inspectors show exact micro-dollar precision. A fully reported or computed
zero is distinct from unknown cost. Mixed pricing shows reported, computed, and
unpriced event counts together; partial cost is a lower bound. Timeline buckets,
contributors, and headline values use the same captured scope, period, and local
timezone. Activity-call shares and turn-context attribution remain independent
accounting views; adding them together would count the same usage repeatedly.

Only modern OpenCode SQLite currently supplies recorded code-change snapshots.
Its user-message summary provides additions/deletions and a stable change ID.
The latest snapshot replaces the prior one, so repeated polling does not inflate
counts and later decreases remain visible. Explicit integer zero counts are
known zero; absent, null, or empty summaries are unknown, since they cannot
distinguish an undo from missing reporting. Mixed known/unknown snapshots show
partial coverage.

The normalized code-change record has no model identity or source-proven link to
the assistant usage that caused an edit. A session can contain multiple models.
OpenCode's JSON path and the other harness adapters do not emit code-change
records. The first release therefore keeps model counts unavailable and shows
supported session-lifetime observations only. Repeated edits are not unique
surviving output, and line counts do not measure quality. Constructed fixtures
prove mutable-count handling; the checked-in OpenCode live SQL fixture proves
the surrounding schema but contains no nonzero edit-count observation.

## Responsive acceptance

Terminal rows and columns determine layout. Physical screen inches do not.
The allocator budgets the header, footer, range controls, summary, and machine
readings before adding comparison, detail, and chart panes.

| Frame or mode | Allocation or fallback | Evidence |
|---|---|---|
| 120×40 desktop, 200×60 large | Content-sized comparison, selected detail, side chart and suggestion | Geometry, viewport, and per-location mouse tests |
| 55×52 tall narrow | Single column with comparisons and selected detail | Bounds, selection, and mouse tests |
| 160×12 short wide | Compact stats and machine readings; optional panes fold | Responsive frame tests |
| 42×12 minimum | Compact summary and machine readings; More retains comparison/details | Minimum frame and inline-choice tests |
| Below 42×12 | Bounded resize message gives the supported minimum | One-cell-under tests |
| Native light/dark and `NO_COLOR` | Native background, labels, borders, and non-color selection markers | Native-theme and monochrome tests |
| Reduced motion | `AIUSAGE_REDUCED_MOTION=1` or `NO_COLOR=1` makes the heartbeat static; current readings still update | Freshness tests |
| Physical Termius/touch and native text selection | Client-dependent input; keyboard fallback remains available | Manual acceptance; PTY does not prove it |

The production migration retains all five destinations and their classic
interactions. The new workspace owns navigation and presentation; the existing
store owns aggregate queries, the independent daemon owns collection, and the
background machine monitor owns current CPU/memory/disk samples. The provider
predicate is the required backend addition. No new schema or remote analyzer is
needed for this workspace. Classic Overview is the retained navigation fallback;
it does not reverse schema changes from earlier releases.

## Implementation handoff

The workspace uses the existing terminal stack:

- Bubbles provides the table, viewport, help, text input, spinner, and progress widgets.
- Bubble Tea owns the update loop and terminal events.
- Lip Gloss handles bounded layout and styling.
- ntcharts renders plots.
- Bubblezone maps mouse hit regions to the final rendered cells.
- Custom application glue connects query state, scope, selection, responsive
  geometry, accounting labels, and the library widgets.

There is no custom replacement for those libraries and no AI service in the
current workspace path.

The workspace clips and pads already rendered panels with Charm's ANSI helpers
instead of repeatedly wrapping them. Tables receive their final styles before
height calculation. A single retained chart body avoids rebuilding the same
plot on row navigation and live machine ticks. It is replaced when the applied
dataset, chart metric, geometry, or palette changes. `BenchmarkWorkspaceRender`
exercises 120 comparison rows and a priced 24-hour timeline while moving the
selection at 120×40 and 200×60. It measures rendering, not database loading or
SSH network latency.

An optional remote analyzer is only a proposal. It is not implemented. If it is
added later, non-secret endpoint and model settings may belong in aiusage
configuration, while the API key must come from an environment variable and
must never be stored in that configuration. Local rules and the Usage workspace
must continue to work when the endpoint is absent or unreachable.

GitHub issue [#100](https://github.com/RandomCodeSpace/aiusage/issues/100) is the
canonical UX decision map;
[#119](https://github.com/RandomCodeSpace/aiusage/issues/119) tracks the production
implementation, local installation, and validation. The production implementation
has passed the TUI/view package tests, focused provider-filter and read-only
launch tests, and workspace race checks. PTY checks used a disposable
SQLite ledger and covered desktop rendering, narrow resizing, metric inspection,
scrolling, and model-to-session navigation. They do not substitute for a physical
Termius check. Timing probes against an existing ledger are recorded on the
issue; uncached large ranges still require database aggregation.

On 2026-09-13, the actual workspace loader at `394f9ae` was also checked against
independent raw-ledger SQL on a consistent read-only backup of an existing
database. The fixed UTC seven-day window matched every numeric headline field,
all model rows, and the ordered timeline, including pricing provenance. The
temporary helper and private snapshot were removed after the check. This proves
that captured dataset's reconciliation, not every possible source history.
