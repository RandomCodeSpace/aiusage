# aiusage

A local, read-only daemon and terminal dashboard that records what your AI agent
CLIs spend. It reads the transcripts, session databases and telemetry exports
those tools already write, appends the usage it finds to an immutable SQLite
ledger, and reports on it. Pure Go, no network required to collect, nothing
written back to any harness.

## Install

```sh
go install github.com/RandomCodeSpace/aiusage@latest
```

## Examples

Five packages at the module root are public API - `model`, `adapter`, `store`,
`pricing` and `collect` - and `examples/` shows the three things a consumer
usually wants from them:

- `examples/report-totals` - open a ledger with the read-only handle and print
  per-tool totals for the last seven days.
- `examples/collect-once` - run one collection pass with every built-in adapter
  into a throwaway database.
- `examples/custom-adapter` - implement `adapter.Adapter` out of tree and run it
  in the same registry as the built-ins.

## Stability

While the module is at v0.x, breaking changes land only at MINOR version bumps
and are named in that release's notes; a patch release never breaks a consumer.
Inside a minor line the two surfaces an out-of-tree consumer builds against grow
rather than change shape: `adapter.Adapter` and the `store.Store` handle gain
methods, and the methods already there keep their signatures and their meaning.
Growth of the Adapter interface itself is still a break - `Capabilities` was one,
since every out-of-tree implementation stops compiling until it declares itself -
so it arrives at a minor bump like any other, while behaviour that is genuinely
optional goes in a side interface the way `adapter.Incremental` does and needs no
bump at all. v1.0 lands when the fixture-tier wave settles: adapters whose
formats are verified against constructed fixtures rather than against sessions
run on a real install are the part of this surface still moving, and promoting
them to live tier is what finishes its shape.
