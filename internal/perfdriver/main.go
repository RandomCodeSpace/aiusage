// Command perfdriver builds and measures deterministic production fixtures.
// It is a CI tool, not part of the aiusage release binary.
package main

import (
	"encoding/json"
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
		return errors.New("usage: perfdriver <generate-long-ledger|generate-source-farm|source-farm-discovery|source-farm-catchup|source-farm-warm|source-farm-unchanged|source-farm-contract|query-suite|observe> [flags]")
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
	case "generate-source-farm":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := fs.String("root", "", "source-farm directory to create")
		fixtures := fs.String("fixtures", "", "repository root holding canonical adapter fixtures")
		manifest := fs.String("manifest", "", "fixture manifest output path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return generateSourceFarm(*root, *fixtures, *manifest, sourceFarmSources, sourceFarmRecords)
	case "source-farm-discovery":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := fs.String("root", "", "generated source-farm directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var summary sourceFarmSummary
		if err := withNeutralSourceFarmEnvironment(func() error {
			var err error
			summary, err = sourceFarmDiscoverySummary(*root)
			return err
		}); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(summary)
	case "source-farm-catchup":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := fs.String("root", "", "generated source-farm directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var summary sourceFarmSummary
		if err := withNeutralSourceFarmEnvironment(func() error {
			var err error
			summary, err = runSourceFarmCatchup(*root, "")
			return err
		}); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(summary)
	case "source-farm-warm":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := fs.String("root", "", "generated source-farm directory")
		db := fs.String("db", "", "persistent source-farm ledger to create")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var summary sourceFarmSummary
		if err := withNeutralSourceFarmEnvironment(func() error {
			var err error
			summary, err = warmSourceFarm(*root, *db)
			return err
		}); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(summary)
	case "source-farm-unchanged":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := fs.String("root", "", "generated source-farm directory")
		db := fs.String("db", "", "pre-warmed source-farm ledger")
		cycles := fs.Int("cycles", 1, "unchanged collection cycles")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var summary sourceFarmSummary
		if err := withNeutralSourceFarmEnvironment(func() error {
			var err error
			summary, err = runSourceFarmUnchanged(*root, *db, *cycles)
			return err
		}); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(summary)
	case "source-farm-contract":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := fs.String("root", "", "generated source-farm directory")
		out := fs.String("out", "", "contract report output path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withNeutralSourceFarmEnvironment(func() error {
			return runSourceFarmContract(*root, *out, sourceFarmSources, sourceFarmRecords, sourceFarmUnchangedCycles)
		})
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
		root := fs.String("root", "", "source-farm root for source-farm commands")
		name := fs.String("name", "", "metric name")
		command := fs.String("command", "", "version, summary-all, summary-breakdown, summary-provider, export-json, export-csv, export-json-raw, or export-csv-raw")
		purpose := fs.String("purpose", "timed", "measurement purpose: timed, absolute, or correctness")
		samples := fs.Int("samples", 1, "process samples")
		warmups := fs.Int("warmups", 0, "untimed process warmups")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return observeProcesses(*binary, *db, *root, *name, *command, *purpose, *samples, *warmups, os.Stdout)
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
