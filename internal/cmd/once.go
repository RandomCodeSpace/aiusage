package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
)

// onceRegistry is a seam so tests can drive `once` with failing adapters.
var onceRegistry = defaultRegistry

// newOnceCmd builds the `once` command: a single collection cycle, then exit.
func newOnceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "Run a single collection cycle and exit",
		Long: "once performs exactly one read-only poll of every discovered source, " +
			"appends new usage to the database, and prints the cycle statistics. " +
			"Useful for cron-style scheduling and for verifying discovery. " +
			"Exits nonzero when the daemon already holds the collection lock or " +
			"when every source fails; partial failures keep exit 0.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// The daemon and `once` share one collection lock: concurrent
			// cycles diff aggregate sources against the same stored baseline
			// and both insert the delta, double counting it. The identity is
			// stamped so a concurrent ensureDaemon treats this cycle as a
			// same-build daemon instead of force-restarting it.
			release, err := daemon.AcquireCollectionLock(cfg.PIDPath, buildinfo.Identity())
			if err != nil {
				return err
			}
			defer release()

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			stats, err := collect.RunOnce(cmdContext(c), onceRegistry(), st, discoverConfig(cfg),
				cycleOptions(cfg)...)
			if err != nil {
				return fmt.Errorf("collection cycle: %w", err)
			}

			printCycleStats(c, stats)
			if stats.AllFailed() {
				return fmt.Errorf("collection failed: every source failed (%d errors above)", len(stats.Errors))
			}
			return nil
		},
	}
}

// printCycleStats writes a human-readable summary of one cycle to stdout.
//
// The rebuild line is printed only when a rebuild happened, and it happens
// rarely: after the v4 migration, or when the derived rollup was found to
// disagree with the ledger. The daemon logs the same fact, and `once` was the
// one path where a several-hundred-millisecond pause had no explanation.
func printCycleStats(c *cobra.Command, s collect.CycleStats) {
	out := c.OutOrStdout()
	if s.RollupRebuilt {
		fmt.Fprintln(out, "rebuilt the derived rollup from the ledger")
	}
	// activity is reported separately from inserted: they count different
	// ledgers, and a pass that appended tens of thousands of tool calls while
	// reporting only its event count would be describing half the work it did.
	fmt.Fprintf(out, "adapters=%d sources=%d seen=%d inserted=%d activity=%d snapshots=%d errors=%d\n",
		s.Adapters, s.Sources, s.EventsSeen, s.EventsInserted, s.ActivityInserted, s.Snapshots, len(s.Errors))
	for _, e := range s.Errors {
		fmt.Fprintf(out, "  - %s\n", e)
	}
}
