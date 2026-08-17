// Command report-totals prints per-tool token totals for the last seven days
// from an existing aiusage ledger. It is the smallest useful read-only
// consumer: store.OpenReadOnly hands back a *store.Reader - a mode=ro
// connection that creates no schema, runs no migration and touches no file mode
// - and Summarize answers the whole question in one call, one bucket per tool.
//
// The last thing it does is the point of the example. A *store.Reader has NO
// WRITE METHOD, so `st.InsertEvents(...)` here is not a call that gets refused
// at runtime - it is a program that does not build. "This process cannot write
// the ledger" is a property of the type the consumer was handed, not a promise
// checked somewhere below it.
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

	return writesAreAbsent(st)
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

// ledgerWriter is a narrow interface declared the way a consumer of this module
// is meant to declare one: at the point of use, naming only the method this
// file cares about. Package store exports no fat interface to implement, so
// this is also how a consumer fakes the store in its own tests.
//
// It exists here to make the absence VISIBLE. The obvious demonstration -
// calling InsertEvents on the read handle and printing the refusal - cannot be
// written any more, because the compiler rejects it before the program runs.
// Asking whether the handle satisfies the writer at all is the closest a
// running program can get to showing you the same fact.
type ledgerWriter interface {
	InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error)
}

// writesAreAbsent reports that the read handle carries no write method.
func writesAreAbsent(st *store.Reader) error {
	if _, ok := any(st).(ledgerWriter); ok {
		return errors.New("the read handle satisfies ledgerWriter; a serving process must not be able to append")
	}
	fmt.Printf("\nthe read handle has no InsertEvents: a write through it does not compile\n")
	return nil
}
