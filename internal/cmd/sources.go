package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"aiusage/internal/store"
)

// newSourcesCmd builds the `sources` command: lists the sources discovered by
// each adapter (read-only) alongside the per-tool stats already stored.
func newSourcesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sources",
		Short: "List discovered sources and stored per-tool stats",
		Long: "sources runs each adapter's read-only discovery and prints the located " +
			"files/DBs, then prints the per-tool usage already recorded in the " +
			"database. Nothing is collected or written.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSources(c)
		},
	}
}

func runSources(c *cobra.Command) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := cmdContext(c)
	reg := defaultRegistry()
	dc := discoverConfig(cfg)
	out := c.OutOrStdout()

	fmt.Fprintln(out, "Discovered sources")
	fmt.Fprintln(out, strings.Repeat("-", 18))
	for _, ad := range reg.All() {
		srcs, derr := ad.Discover(ctx, dc)
		if derr != nil {
			fmt.Fprintf(out, "%s (%s): discovery error: %v\n", ad.DisplayName(), ad.ID(), derr)
		}
		if len(srcs) == 0 {
			fmt.Fprintf(out, "%s (%s): no sources found\n", ad.DisplayName(), ad.ID())
			continue
		}
		fmt.Fprintf(out, "%s (%s): %d source(s)\n", ad.DisplayName(), ad.ID(), len(srcs))
		for _, s := range srcs {
			label := s.Label
			if label == "" {
				label = string(s.Class)
			}
			fmt.Fprintf(out, "  [%s] %s — %s\n", s.Class, label, s.Path)
		}
	}

	fmt.Fprintln(out)
	stats, err := st.SourceStats(ctx)
	if err != nil {
		return fmt.Errorf("source stats: %w", err)
	}
	printSourceStats(out, stats)
	return nil
}

// printSourceStats renders the stored per-tool stats as an aligned table.
func printSourceStats(out io.Writer, stats []store.SourceStat) {
	fmt.Fprintln(out, "Stored usage by tool")
	fmt.Fprintln(out, strings.Repeat("-", 20))
	if len(stats) == 0 {
		fmt.Fprintln(out, "(no usage recorded yet)")
		return
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].Total > stats[j].Total })

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tEVENTS\tSESSIONS\tTOTAL\tMODELS\tLAST EVENT")
	for _, s := range stats {
		last := "-"
		if !s.LastEvent.IsZero() {
			last = s.LastEvent.Local().Format("2006-01-02 15:04")
		}
		models := strings.Join(s.Models, ", ")
		if models == "" {
			models = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\n",
			s.Tool, s.Events, s.Sessions, s.Total, models, last)
	}
	tw.Flush()
}
