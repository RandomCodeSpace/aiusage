package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"aiusage/internal/config"
	"aiusage/internal/model"
	"aiusage/internal/store"
)

// adapterNotes are operator-facing caveats surfaced by `doctor` for adapters
// whose data depends on external opt-in or is not yet emitted (plan §1).
var adapterNotes = map[string]string{
	model.ToolCopilot: "requires Copilot OpenTelemetry file export " +
		"(COPILOT_OTEL_FILE_EXPORTER_PATH or ~/.copilot/otel/*.jsonl); empty until enabled.",
	model.ToolAgy: "Antigravity emits no token usage until logged in and used; " +
		"adapter is Gemini-shaped and returns empty until data appears.",
}

// newDoctorCmd builds the `doctor` command: configuration, database and adapter
// discovery diagnostics, plus notes for opt-in/empty adapters.
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose configuration, database and adapter discovery",
		Long: "doctor prints the resolved paths, database statistics, and a read-only " +
			"discovery count for every adapter, including notes for adapters that " +
			"depend on external opt-in (Copilot OTEL) or emit no data yet (agy).",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDoctor(c)
		},
	}
}

func runDoctor(c *cobra.Command) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	out := c.OutOrStdout()

	fmt.Fprintln(out, "Configuration")
	fmt.Fprintln(out, strings.Repeat("-", 13))
	fmt.Fprintf(out, "db:       %s\n", cfg.DBPath)
	fmt.Fprintf(out, "pidfile:  %s\n", cfg.PIDPath)
	fmt.Fprintf(out, "logfile:  %s\n", cfg.LogPath)
	fmt.Fprintf(out, "home:     %s\n", cfg.Home)
	fmt.Fprintf(out, "interval: %ds\n", cfg.IntervalSeconds)
	fmt.Fprintln(out)

	st, err := openStore(cfg)
	if err != nil {
		// The DB may legitimately not exist yet; report and continue with
		// discovery so doctor stays useful before the first cycle.
		fmt.Fprintf(out, "Database: cannot open (%v)\n\n", err)
	} else {
		defer st.Close()
		stats, sErr := st.Stats(cmdContext(c))
		if sErr != nil {
			fmt.Fprintf(out, "Database: stats error: %v\n\n", sErr)
		} else {
			printDBStats(out, stats)
		}
	}

	printAdapterDiscovery(c, cfg)
	return nil
}

// printDBStats renders whole-database diagnostics.
func printDBStats(out io.Writer, s store.DBStats) {
	fmt.Fprintln(out, "Database")
	fmt.Fprintln(out, strings.Repeat("-", 8))
	fmt.Fprintf(out, "path:           %s\n", s.Path)
	fmt.Fprintf(out, "events:         %d\n", s.Events)
	fmt.Fprintf(out, "snapshots:      %d\n", s.Snapshots)
	fmt.Fprintf(out, "distinct tools: %d\n", s.DistinctTools)
	fmt.Fprintf(out, "distinct model: %d\n", s.DistinctModel)
	fmt.Fprintf(out, "size:           %d bytes\n", s.SizeBytes)
	fmt.Fprintf(out, "schema version: %d\n", s.SchemaVersion)
	if !s.EarliestEvent.IsZero() {
		fmt.Fprintf(out, "earliest:       %s\n", s.EarliestEvent.Local().Format("2006-01-02 15:04"))
	}
	if !s.LatestEvent.IsZero() {
		fmt.Fprintf(out, "latest:         %s\n", s.LatestEvent.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintln(out)
}

// printAdapterDiscovery runs each adapter's read-only discovery and prints how
// many sources it located, with notes for opt-in/empty adapters.
func printAdapterDiscovery(c *cobra.Command, cfg config.Config) {
	out := c.OutOrStdout()
	ctx := cmdContext(c)
	dc := discoverConfig(cfg)

	fmt.Fprintln(out, "Adapter discovery")
	fmt.Fprintln(out, strings.Repeat("-", 17))
	for _, ad := range defaultRegistry().All() {
		srcs, err := ad.Discover(ctx, dc)
		status := fmt.Sprintf("%d source(s)", len(srcs))
		if err != nil {
			status = fmt.Sprintf("%d source(s), error: %v", len(srcs), err)
		}
		fmt.Fprintf(out, "%-12s %s\n", ad.ID(), status)
		if note, ok := adapterNotes[ad.ID()]; ok {
			fmt.Fprintf(out, "             note: %s\n", note)
		}
	}
}
