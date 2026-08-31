// Command perfdriver builds and measures deterministic production fixtures.
// It is a CI tool, not part of the aiusage release binary.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: perfdriver <generate-long-ledger|query-suite|observe> [flags]")
	}
	switch args[0] {
	case "generate-long-ledger":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		db := fs.String("db", "", "database path to create")
		manifest := fs.String("manifest", "", "fixture manifest output path")
		usage := fs.Int("usage", longLedgerUsageEvents, "usage event count")
		activity := fs.Int("activity", longLedgerActivityRows, "activity row count")
		contexts := fs.Int("contexts", longLedgerTurnContexts, "turn-context row count")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return generateLongLedger(*db, *manifest, *usage, *activity, *contexts)
	case "query-suite":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		db := fs.String("db", "", "schema-compatible database path")
		matrix := fs.String("matrix", "full", "query matrix: full or timed")
		samples := fs.Int("samples", 1, "measured samples per query")
		warmups := fs.Int("warmups", 0, "untimed warmups per query")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return runQuerySuite(*db, *matrix, *samples, *warmups, os.Stdout)
	case "observe":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		binary := fs.String("binary", "", "executable to run")
		db := fs.String("db", "", "database path for data commands")
		name := fs.String("name", "", "metric name")
		command := fs.String("command", "", "version, summary-all, summary-breakdown, summary-provider, export-json, export-csv, export-json-raw, or export-csv-raw")
		purpose := fs.String("purpose", "timed", "measurement purpose: timed, absolute, or correctness")
		samples := fs.Int("samples", 1, "process samples")
		warmups := fs.Int("warmups", 0, "untimed process warmups")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return observeProcesses(*binary, *db, *name, *command, *purpose, *samples, *warmups, os.Stdout)
	case "fixture-size":
		// Used by shell arithmetic without duplicating the canonical counts.
		if len(args) != 2 {
			return errors.New("usage: perfdriver fixture-size <usage|activity|contexts>")
		}
		var n int
		switch args[1] {
		case "usage":
			n = longLedgerUsageEvents
		case "activity":
			n = longLedgerActivityRows
		case "contexts":
			n = longLedgerTurnContexts
		default:
			return fmt.Errorf("unknown fixture size %q", args[1])
		}
		_, err := fmt.Fprintln(os.Stdout, strconv.Itoa(n))
		return err
	default:
		return fmt.Errorf("unknown perfdriver command %q", args[0])
	}
}
