package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/report"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// newTodayCmd builds the `today` command: usage since local midnight, by tool.
func newTodayCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "today",
		Short: "Show today's usage so far, grouped by tool",
		Long: "today summarises usage from local midnight until now, grouped by tool. " +
			"It is shorthand for `summary --since <local-midnight> --by tool`.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			now := timeNow()
			midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			filter := store.Filter{Since: midnight, Until: now, GroupBy: []string{"tool"}}
			return renderSummary(c, filter, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the summary as JSON")
	return cmd
}

// renderSummary is the shared render path for the convenience commands (today,
// last): query the store with filter and emit JSON or an aligned table.
func renderSummary(c *cobra.Command, filter store.Filter, asJSON bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	sum, err := st.Summarize(cmdContext(c), filter)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	if asJSON {
		return report.WriteSummaryJSON(c.OutOrStdout(), sum)
	}
	fmt.Fprintln(c.OutOrStdout(), report.RenderTable(sum, report.Opt{Color: false}))
	return nil
}
