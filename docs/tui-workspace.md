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
