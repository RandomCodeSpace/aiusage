package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"aiusage/internal/collect"
)

// newOnceCmd builds the `once` command: a single collection cycle, then exit.
func newOnceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "Run a single collection cycle and exit",
		Long: "once performs exactly one read-only poll of every discovered source, " +
			"appends new usage to the database, and prints the cycle statistics. " +
			"Useful for cron-style scheduling and for verifying discovery.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			stats, err := collect.RunCycle(cmdContext(c), defaultRegistry(), st, discoverConfig(cfg))
			if err != nil {
				return fmt.Errorf("collection cycle: %w", err)
			}

			printCycleStats(c, stats)
			return nil
		},
	}
}

// printCycleStats writes a human-readable summary of one cycle to stdout.
func printCycleStats(c *cobra.Command, s collect.CycleStats) {
	out := c.OutOrStdout()
	fmt.Fprintf(out, "adapters=%d sources=%d seen=%d inserted=%d snapshots=%d errors=%d\n",
		s.Adapters, s.Sources, s.EventsSeen, s.EventsInserted, s.Snapshots, len(s.Errors))
	for _, e := range s.Errors {
		fmt.Fprintf(out, "  - %s\n", e)
	}
}
