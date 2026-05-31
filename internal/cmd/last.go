package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// newLastCmd builds the `last` command: usage over the trailing span DUR, where
// DUR matches ^([0-9]+)(m|h|d)$ (e.g. 30m, 6h, 2d), grouped by tool.
func newLastCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "last <duration>",
		Short: "Show usage over the trailing duration (e.g. 30m, 6h, 2d)",
		Long: "last summarises usage from now minus the given duration until now, " +
			"grouped by tool. The duration must match ^([0-9]+)(m|h|d)$, e.g. 30m, 6h, 2d.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			span, ok := parseSpan(args[0])
			if !ok {
				return fmt.Errorf("invalid duration %q: want a value like 30m, 6h or 2d", args[0])
			}
			now := timeNow()
			filter := store.Filter{Since: now.Add(-span), Until: now, GroupBy: []string{"tool"}}
			return renderSummary(c, filter, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the summary as JSON")
	return cmd
}
