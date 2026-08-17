// Command report-totals prints per-tool token totals for the last seven days
// from an existing aiusage ledger. It is the smallest useful read-only
// consumer: store.OpenReadOnly gives it the handle a serving process gets - a
// mode=ro connection that creates no schema, runs no migration and touches no
// file mode - and store.Summarize answers the whole question in one call, one
// bucket per tool. The last thing it does is the point of the example: an
// InsertEvents on that handle is refused by the store in its own name, so "this
// process cannot write the ledger" holds by construction rather than by the
// append-only triggers being the last line of defence.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// windowDays is the reported window. It is a choice, not a limit: Filter's
// Since/Until are plain times and a zero bound is open at that end.
const windowDays = 7

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "report-totals:", err)
		os.Exit(1)
	}
}

func run() error {
	// The database path is an argument because resolving the default one is the
	// CLI's job: it lives in internal/config, which is not importable from
	// outside this module. A consumer names the file (or reads its own config).
	if len(os.Args) != 2 {
		return errors.New("usage: report-totals <path to aiusage.db>")
	}

	// OpenReadOnly refuses a database whose schema version differs from this
	// binary's in EITHER direction, since migrating would be a write and serving
	// a schema it does not understand is worse than refusing to start.
	st, err := store.OpenReadOnly(os.Args[1])
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	sum, err := st.Summarize(ctx, store.Filter{
		Since:   time.Now().UTC().AddDate(0, 0, -windowDays),
		GroupBy: []string{"tool"},
	})
	if err != nil {
		return err
	}

	fmt.Printf("per-tool totals, last %d days\n\n", windowDays)
	fmt.Printf("%-14s %10s %14s %12s\n", "TOOL", "EVENTS", "TOKENS", "COST")
	for _, b := range sum.Buckets {
		fmt.Printf("%-14s %10d %14d %12s\n", b.Keys["tool"], b.Events, b.Total, cost(b))
	}
	fmt.Printf("%-14s %10d %14d %12s\n", "TOTAL",
		sum.Totals.Events, sum.Totals.Total, cost(sum.Totals))

	return refuseWrite(ctx, st)
}

// cost renders a bucket's stamped cost with the two marks every aiusage surface
// uses. Known is false while EVERY row in the bucket is unpriced: a missing
// price is not a free request, so it renders as "-" and never as $0.00.
// Approximate is set as soon as ONE row carries no stamped cost, because the sum
// is then short of that row and is a floor rather than the bill.
//
// Folding those rows in at today's rates is what store.UnpricedGroups exists
// for, but the arithmetic that does it (report.ResolveCosts) is in an internal
// package and cannot be reached from out here, so this example reports the floor
// and marks it.
func cost(b store.Bucket) string {
	known := b.Events > b.UnpricedEvents
	return model.FormatCost(b.CostMicroUSD, b.UnpricedEvents > 0, known)
}

// refuseWrite proves the handle is read-only by using it wrongly on purpose.
//
// Two details make the proof mean something. The batch is NON-EMPTY, because
// InsertEvents returns (0, nil) for an empty slice before it ever consults the
// guard - a caller demonstrating the refusal with a zero-length batch
// demonstrates nothing. And the event is well-formed (non-empty dedup key,
// non-negative counts), so the only thing that can reject it is the read-only
// guard rather than a CHECK violation that would look the same from here.
func refuseWrite(ctx context.Context, st store.Store) error {
	now := time.Now().UTC()
	n, err := st.InsertEvents(ctx, []model.UsageEvent{{
		Tool:         "example",
		Model:        "example-model",
		DedupKey:     "example|read-only-demo",
		EventTime:    now,
		ObservedTime: now,
		Kind:         model.KindUsage,
	}})
	if err == nil {
		return fmt.Errorf("the read-only handle accepted %d row(s); it must not", n)
	}
	fmt.Printf("\nwrite refused by the read-only handle: %v\n", err)
	return nil
}
