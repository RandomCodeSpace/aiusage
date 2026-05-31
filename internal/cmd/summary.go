package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"aiusage/internal/report"
	"aiusage/internal/store"
)

// summaryOpts holds the flags for the summary command.
type summaryOpts struct {
	since     string
	until     string
	by        string
	breakdown bool
	json      bool
	csv       bool
}

// newSummaryCmd builds the `summary` command: grouped usage over a time range.
func newSummaryCmd() *cobra.Command {
	var o summaryOpts
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Summarise stored usage over a time range",
		Long: "summary aggregates stored usage between --since and --until, grouped by " +
			"the --by dimensions (comma-separated: day,tool,model,project,session,...). " +
			"Renders an aligned table by default, or --json for the summary object. " +
			"(--csv exports the matching raw events.)",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSummary(c, o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.since, "since", "", "lower time bound (RFC3339, YYYY-MM-DD, or a span like 7d)")
	f.StringVar(&o.until, "until", "", "upper time bound (RFC3339, YYYY-MM-DD, or a span like 1h)")
	f.StringVar(&o.by, "by", "", "comma-separated grouping dimensions (e.g. day,tool,model)")
	f.BoolVar(&o.breakdown, "breakdown", false, "render the component token breakdown")
	f.BoolVar(&o.json, "json", false, "emit the summary as JSON")
	f.BoolVar(&o.csv, "csv", false, "emit the matching raw events as CSV")
	return cmd
}

// runSummary resolves the filter, queries the store, and renders the result in
// the requested format.
func runSummary(c *cobra.Command, o summaryOpts) error {
	if o.json && o.csv {
		return fmt.Errorf("choose at most one of --json or --csv")
	}

	since, err := parseTimeFlag(o.since)
	if err != nil {
		return err
	}
	until, err := parseTimeFlag(o.until)
	if err != nil {
		return err
	}
	dims, err := parseBy(o.by)
	if err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	filter := store.Filter{Since: since, Until: until, GroupBy: dims}
	ctx := cmdContext(c)

	// --csv reports the underlying raw events (the summary object has no CSV
	// shape); JSON and the table render the grouped summary.
	if o.csv {
		evs, err := st.ListEvents(ctx, filter)
		if err != nil {
			return fmt.Errorf("list events: %w", err)
		}
		return report.WriteEventsCSV(c.OutOrStdout(), evs)
	}

	sum, err := st.Summarize(ctx, filter)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	if o.json {
		return report.WriteSummaryJSON(c.OutOrStdout(), sum)
	}

	out := report.RenderTable(sum, report.Opt{Breakdown: o.breakdown, Color: false})
	fmt.Fprintln(c.OutOrStdout(), out)
	return nil
}
