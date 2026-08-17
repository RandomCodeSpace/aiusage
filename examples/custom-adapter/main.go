// Command custom-adapter implements adapter.Adapter from outside the project
// and runs it in one registry beside every built-in. The interface is the whole
// third-party contract - five methods, one of them the Capabilities declaration
// a sixteenth adapter cannot compile without - and adapter.NewRegistry takes any
// mixture of implementations, so an out-of-tree harness rides the same collector,
// the same append-only ledger and the same dedup rules the shipped ones do. The
// adapter here reports one hard-coded usage event instead of parsing a surface,
// and the pass is run TWICE against the same database to show what that buys:
// the second cycle inserts nothing, because idempotence is a property of the
// dedup key and not of the collector remembering anything.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/adapter/all"
	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// exampleTool is the tool id every row this adapter produces carries. The ledger
// stores it as free-form text, so a third-party id needs no change anywhere in
// aiusage - but it must be unique in practice: usage_events is append-only, and
// two harnesses that shared an id could never be told apart again.
const exampleTool = "example-harness"

// exampleAdapter is a third-party adapter. A real one discovers files or
// databases under the configured root and parses them; this one reports a
// constant so the plumbing is visible without a harness to read. What a real one
// must keep is the rule that has no exceptions: adapters are STRICTLY
// OBSERVATIONAL. They read what the agent CLI already wrote - read-only opens,
// no writes, no write locks, no rotation - and they tolerate a missing, partial
// or corrupt source by returning best-effort results with a non-fatal error
// rather than failing the whole cycle.
type exampleAdapter struct{}

// The interface is the contract, so the assertion belongs at the top of the file
// where a signature drift shows up as a compile error rather than as a registry
// that silently declines the value.
var _ adapter.Adapter = exampleAdapter{}

// ID is the stable tool identifier, and DisplayName is what a surface prints.
func (exampleAdapter) ID() string { return exampleTool }

// DisplayName is the human-friendly name of the harness.
func (exampleAdapter) DisplayName() string { return "Example Harness" }

// Capabilities is the declaration the interface requires, and it is a statement
// about THIS code kept beside it, which is why it is a method rather than a row
// in a table somewhere else. Cost is CostComputed because nothing here stamps a
// price, so every row it produces is valued from the public rate card or left
// unpriced; Activity is ActivityNone because it emits no activity rows at all;
// Tier is TierFixture because nothing here was verified against a real install.
//
// Reasoning is READ from model.ReasoningReportFor rather than restated, so this
// declaration and the pricing engine can never disagree about what the source
// reports. An id that table does not know reports no reasoning count at all,
// which is exactly what "not reported" means - and different from claiming the
// count exists and sits inside output.
func (exampleAdapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      exampleTool,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(exampleTool),
		Tier:      model.TierFixture,
	}
}

// Discover names what this adapter would read. cfg.Root resolves the tool's
// explicit override when one is configured and falls back to the user's home
// directory, so an adapter never reads the environment for a root itself. There
// is no file behind this source, but Path is still what usage_events.source_path
// records and what a checkpoint would be keyed by, so it has to be stable across
// passes rather than freshly invented on each one.
func (exampleAdapter) Discover(_ context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	return []adapter.Source{{
		Tool:  exampleTool,
		Class: model.EventLevel,
		Path:  filepath.Join(cfg.Root(exampleTool, ""), ".example-harness", "usage.jsonl"),
		Label: "example harness (hard-coded event)",
	}}, nil
}

// Collect reads one source. The DedupKey carries the entire idempotence
// contract: inserts conflict-skip on it, so a key derived from the RECORD's own
// identity makes a re-read a no-op, while a key derived from a read position - a
// byte offset, a line number - recounts the same usage every time the file is
// walked from the top. This one is a constant, which is why the second cycle in
// main inserts nothing.
//
// Provider is left empty on purpose. It means "unknown", and surfaces render it
// as such; naming a billing identity the source never stated would be a guess
// stored in an append-only table. Raw is the audit payload and is built from an
// ALLOW-LIST of usage and model fields - never by stripping content out of a
// whole record - so it cannot carry a prompt or a tool's input. The collector
// drops it entirely when the pass runs under collect.WithoutRaw, which is why an
// adapter never has to think about that switch.
func (exampleAdapter) Collect(_ context.Context, src adapter.Source) (adapter.Observation, error) {
	now := time.Now().UTC()
	ev := model.UsageEvent{
		Tool:         exampleTool,
		Model:        "example-model-v1",
		SessionID:    "example-session-1",
		Project:      "/example/project",
		EventTime:    now,
		ObservedTime: now,

		InputTokens:  1200,
		OutputTokens: 340,

		MessageID:  "msg_example_1",
		SourcePath: src.Path,
		DedupKey:   exampleTool + "|msg_example_1",
		Kind:       model.KindUsage,
		Raw:        `{"model":"example-model-v1","input_tokens":1200,"output_tokens":340}`,
	}
	// TotalTokens is provider-authoritative wherever the source states one.
	// ComputedTotal is the Anthropic-style sum for a source that states none; a
	// provider that counts cache tokens inside input must not use it.
	ev.TotalTokens = ev.ComputedTotal()

	return adapter.Observation{Events: []model.UsageEvent{ev}}, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "custom-adapter:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "aiusage-custom-adapter-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(filepath.Join(dir, "aiusage.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	// One registry, built-ins plus the newcomer. Default() returns a fresh
	// registry holding no state, so appending to its slice is safe.
	reg := adapter.NewRegistry(append(all.Default().All(), exampleAdapter{})...)

	// The adapter declares, the registry aggregates: Capabilities() returns
	// every registered declaration keyed by tool id, so a consumer describing
	// the tools it can report on never keeps a table of its own.
	if c, ok := reg.Capabilities()[exampleTool]; ok {
		fmt.Printf("registered %s: cost=%s activity=%s reasoning=%s tier=%s\n\n",
			c.Tool, c.Cost, c.Activity, c.Reasoning, c.Tier)
	}

	// Home points at an EMPTY directory so the built-in adapters discover
	// nothing and the output below is this adapter's alone. It is not a sandbox:
	// several built-ins take their root from the harness's own environment
	// variable (CLAUDE_CONFIG_DIR, CODEX_HOME, ...), which this does not clear,
	// so a machine with one of those set may collect real usage into the
	// throwaway database too.
	emptyHome := filepath.Join(dir, "home")
	if err := os.Mkdir(emptyHome, 0o700); err != nil {
		return err
	}
	dc := adapter.DiscoverConfig{Home: emptyHome}

	ctx := context.Background()
	for pass := 1; pass <= 2; pass++ {
		stats, err := collect.RunOnce(ctx, reg, st, dc)
		if err != nil {
			return err
		}
		fmt.Printf("pass %d: seen=%d inserted=%d errors=%d\n",
			pass, stats.EventsSeen, stats.EventsInserted, len(stats.Errors))
	}

	sum, err := st.Summarize(ctx, store.Filter{
		Tools:   []string{exampleTool},
		GroupBy: []string{"tool", "model"},
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%-16s %-18s %8s %10s\n", "TOOL", "MODEL", "EVENTS", "TOKENS")
	for _, b := range sum.Buckets {
		fmt.Printf("%-16s %-18s %8d %10d\n", b.Keys["tool"], b.Keys["model"], b.Events, b.Total)
	}
	return nil
}
