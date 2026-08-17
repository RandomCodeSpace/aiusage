// Command collect-once runs exactly one collection pass with every adapter this
// project ships and prints what it wrote. It is the daemon minus the ticker:
// adapter/all.Default() supplies the registry, store.Open creates the database
// at the current schema version, and collect.RunCycle discovers each source,
// reads it read-only and appends what it found. The database is a throwaway
// under os.MkdirTemp, which keeps the pass off a real ledger and also means the
// aggregate adapters diff against baselines this database owns - so nothing is
// shared with a daemon collecting into the user's own file, and no collection
// lock is needed. Events land UNPRICED: a cost is stamped at ingest only when
// the pass is given collect.WithPricer, and building a pricing.Engine here would
// reach for a rate card an example has no business fetching.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/adapter/all"
	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "collect-once:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "aiusage-collect-once-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "aiusage.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Home is where every adapter starts looking; Overrides pins one tool's root
	// somewhere else. Discovery walks the filesystem, so a pass over a large home
	// is not instant - Ctrl-C is honoured below.
	dc := adapter.DiscoverConfig{Home: home}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("collecting into %s\n\n", dbPath)
	stats, err := collect.RunCycle(ctx, all.Default(), st, dc)

	// The stats are printed BEFORE the error is examined, and the cancelled flag
	// is printed with them: a cancelled pass returns counts that cover only the
	// work done so far, and a truncated cycle reported as a complete one is the
	// one way these numbers lie.
	printStats(stats)
	if err != nil {
		return err
	}

	return printTotals(ctx, st)
}

func printStats(s collect.CycleStats) {
	if s.Canceled {
		fmt.Println("PASS TRUNCATED (interrupted) - every count below is partial")
	}
	if s.RollupRebuilt {
		fmt.Println("rebuilt the derived rollup from the ledger")
	}
	fmt.Printf("adapters=%d sources=%d failed=%d\n", s.Adapters, s.Sources, s.SourcesFailed)
	// Three ledgers, three pairs of counters, never added together: usage is
	// tokens, activity is tool/skill/hook invocations, and a turn context is one
	// row per (turn, dimension). A pass that appended no events can still have
	// appended thousands of the other two.
	fmt.Printf("events   seen=%d inserted=%d\n", s.EventsSeen, s.EventsInserted)
	fmt.Printf("activity seen=%d inserted=%d\n", s.ActivitySeen, s.ActivityInserted)
	fmt.Printf("contexts seen=%d inserted=%d\n", s.TurnContextsSeen, s.TurnContextsInserted)
	fmt.Printf("snapshots=%d errors=%d\n", s.Snapshots, len(s.Errors))
	for _, e := range s.Errors {
		fmt.Printf("  - %s\n", e)
	}
}

// printTotals reads back what the pass appended. Per-source errors are
// non-fatal by design, so a cycle that reports some of them still has data worth
// showing.
func printTotals(ctx context.Context, st store.Store) error {
	sum, err := st.Summarize(ctx, store.Filter{GroupBy: []string{"tool"}})
	if err != nil {
		return err
	}
	fmt.Printf("\n%-14s %10s %14s\n", "TOOL", "EVENTS", "TOKENS")
	for _, b := range sum.Buckets {
		fmt.Printf("%-14s %10d %14d\n", b.Keys["tool"], b.Events, b.Total)
	}
	return nil
}
