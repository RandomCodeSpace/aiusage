# Local TUI readiness

Verified on Linux/amd64 on 2026-09-19 before committing, against changes based
on `f99dbef3cc2781c80e5bd38d7f5c0457afa2ad59`.

## Result

Overview exposes range, group, sort, chart metric, and row filtering before any
interaction. Mouse clicks apply choices directly. The `o`, `s`, and `c` keys
focus the same visible choices; arrows move focus and Enter applies it.
Brackets identify applied state independently of color.

Comparison rows receive space before machine readings. Small terminals keep
the controls and expose a comparison action when a useful table cannot fit.
Sorting preserves the selected record. Filtering changes comparison rows,
while totals and the chart retain the full scope. Unknown and partially priced
costs remain explicit.

The default workspace renders each visible table once. Live range queries use
complete rollup buckets plus an indexed partial ending bucket, retaining exact
time bounds, session counts, pricing provenance, and stale-rollup fallback.
Unaligned starting bounds retain the authoritative ledger path.

Starting collection with a custom database path preserves the parent directory's
permissions. The default application directory and database files still receive
the existing private permissions.

## Preview and references

- [120 by 40 terminal](tui-preview.png)
- [80 by 24 terminal](tui-preview-compact.png)
- [Design rules and tokens](../DESIGN.md)

The previews are captures of the built application running in tmux with 1,008
synthetic usage events. Machine readings come from the verification host.
Terminal text and ANSI styles were rasterized with pyte and Pillow; the image
font belongs to the capture environment, not the application.

The design review inspected Dribbble's
[Compliance Controls Listing](https://dribbble.com/shots/25180909-Compliance-Controls-Listing-Table-with-Filters)
and [Orders and invoices dashboard](https://dribbble.com/shots/17988646-Orders-and-invoices-dashboard-Untitled-UI).
The relevant ideas were persistent control rows, clear active states, and quiet
table alignment. These are visual references, not usability studies.

## Verification

- Go 1.26.5: both `internal/tui` and `internal/tui/views` test suites passed.
  After the final table rendering optimization, all `TestWorkspace*` tests
  passed again.
- Go 1.26.5 with `TZ=Asia/Kolkata`: exact-ending, rollup, summary-routing,
  and summary query-count tests passed in `store`.
- Go 1.26.5: focused permission repair, custom-parent preservation, and
  collection ownership tests passed in `internal/cmd`.
- Scoped `go vet ./store ./internal/cmd`, workflow `actionlint`, shell syntax,
  and whitespace checks passed.
- Version-guard boundary checks accept 1.25.13 and 1.26.5, and reject versions
  below the minimum, above the ceiling, and prerelease/development toolchains.
- Real terminal checks passed for direct mouse sorting, grouping, range and
  chart selection; filter editing, applying and clearing with stable totals;
  keyboard sorting; drill-down/back; details; and clean quit.
- Captures at 42 by 12, 42 by 24, 80 by 24, 120 by 40, and 200 by 60 received an
  independent visual review. The verdict was ship. Post-optimization captures
  confirmed correct selected-row styling after sorting.
- The final pure-Go binary passed version, help, and synthetic JSON-summary
  smoke checks. Its build metadata reports Go 1.26.5 and `CGO_ENABLED=0`.

## Performance evidence

Final Go 1.26.5 `BenchmarkWorkspaceRender`, with a priced 24-bucket chart,
120 comparison rows, and moving selection. These are local microbenchmarks,
not latency percentiles or a throughput guarantee.

| Terminal | Time per frame | Allocations per frame |
|---|---:|---:|
| 120 by 40 | 6.56 ms | 12,722 |
| 200 by 60 | 13.97 ms | 25,357 |

Same-machine query comparison on Go 1.26.2, using a deterministic 100,000-event
ledger. Both paths apply the same exact time boundary.

| Query | Before | After |
|---|---:|---:|
| 30-day daily summary | 211.9 ms | 3.72 ms |
| 30-day model summary | 152.0 ms | 2.95 ms |
| 30-day session summary | 191.6 ms | 3.77 ms |
| Model summary with 43 ending-bucket events | 150.2–150.8 ms | 2.95–3.34 ms |

## Toolchain and remaining limits

`.go-version` pins CI and release to 1.25.13. `GOTOOLCHAIN=local` prevents
automatic upgrades, and the release hook checks the 1.25.13 minimum and
independent 1.26.5 maximum before building. Dependencies are unchanged.

The first pushed revision, `2696504`, passed hosted build/tests, race detection,
staticcheck, compatibility, performance, SonarCloud, and CodeQL checks. Its
vulnerability scan failed on six reachable Go 1.26.5 standard-library issues.
Go 1.25.13 includes the relevant security backports and remains below the hard
ceiling. A local `govulncheck ./...` using Go 1.25.13 reported no vulnerabilities
on 2026-09-20. The build pin was corrected to that version; the earlier local
measurements above still describe the Go versions used at the time.
The TUI, store, and command package tests also passed on Go 1.25.13 with CGO
disabled. Version-guard checks still accept 1.26.5 and reject 1.26.6 and newer.

[CI evidence for the first pushed revision](https://github.com/RandomCodeSpace/aiusage/actions/runs/35491226828).

Native macOS execution and live-provider behavior were not verified in this pass.
Visual captures used a dark terminal; light-terminal and alternate-font
appearance were not visually verified.

The installed GoReleaser is 2.16.0, while this repository pins 2.18.0. Its
configuration check fails on the existing `release.preflight` field in both
the baseline and changed configuration. Hosted CI validated the configuration
with GoReleaser 2.18.0. No release was attempted.

The local review binary is `/tmp/aiusage-visual-check/aiusage`, SHA-256
`0a2a3cf03e50445674966a75c314db1f3b1e5faec94a51346e697ad57d88915f`.
The installed utility, running collector, user database, and pre-existing
untracked documents were left intact.
