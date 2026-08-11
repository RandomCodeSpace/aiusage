package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/web"
)

// adapterNotes are operator-facing caveats surfaced by `doctor` for adapters
// whose data depends on external opt-in or is not yet emitted (plan §1).
var adapterNotes = map[string]string{
	model.ToolCopilot: "requires Copilot OpenTelemetry file export " +
		"(COPILOT_OTEL_FILE_EXPORTER_PATH or ~/.copilot/otel/*.jsonl); empty until enabled.",
	model.ToolAgy: "Antigravity emits no token usage until logged in and used; " +
		"adapter is Gemini-shaped and returns empty until data appears.",
}

// absentStatus is the wording for an adapter that is wired in but located
// nothing to read. Reporting a count of zero there reads as "you used nothing";
// the honest statement is that the data source itself does not exist yet. The
// rule is generic: any adapter whose discovery succeeds with no sources gets it.
const absentStatus = "configured, no data source"

// enablementGuides carry the steps that turn a tool's local telemetry on,
// printed only for a tool in the absent state. Keyed by tool id so any adapter
// can supply one; Copilot is the only source that is opt-in today (issue #28).
var enablementGuides = map[string][]string{
	model.ToolCopilot: {
		"Copilot CLI records token usage only through its OpenTelemetry file",
		"exporter, which is off by default. To enable it:",
		"",
		`  mkdir -p "$HOME/.copilot/otel"`,
		"  export COPILOT_OTEL_ENABLED=true",
		"  export COPILOT_OTEL_EXPORTER_TYPE=file",
		`  export COPILOT_OTEL_FILE_EXPORTER_PATH="$HOME/.copilot/otel/copilot-otel-$(date +%Y%m%d-%H%M%S).jsonl"`,
		"",
		"- Put the exports in your shell profile so every session emits.",
		"- Leave content capture (OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT",
		"  / COPILOT_OTEL_CAPTURE_CONTENT) at its false default: token counts",
		"  arrive without it.",
		"- Do NOT set OTEL_EXPORTER_OTLP_ENDPOINT. It auto-enables OpenTelemetry",
		"  but ships to a collector instead of to disk.",
		"- Not retroactive: only sessions run after enabling emit anything.",
	},
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
	fmt.Fprintf(out, "build:    %s\n", buildinfo.Identity())
	fmt.Fprintf(out, "web ui:   %s\n", webUIStatus())
	fmt.Fprintln(out)

	printPermWarnings(out, cfg)

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

// webUIStatus describes this build's web-UI capability for doctor (issue #61).
// It is the same tag that Identity() folds into the build identity, so a user
// comparing `aiusage version` between two installs and a user reading this line
// are being told the same fact.
func webUIStatus() string {
	if web.HasEmbeddedUI() {
		return "embedded (`aiusage serve` available)"
	}
	return "not embedded (`aiusage serve` is unavailable; collection is unaffected)"
}

// printPermWarnings warns when the data dir or the DB is accessible by group or
// other: the raw column holds transcript content, so both must be owner-only
// (daemon start repairs them; a warning here means that repair has not run or
// could not take effect).
func printPermWarnings(out io.Writer, cfg config.Config) {
	checks := []struct {
		label string
		path  string
		want  os.FileMode
	}{
		{"data dir", filepath.Dir(cfg.DBPath), 0o700},
		{"database", cfg.DBPath, 0o600},
	}
	warned := false
	for _, c := range checks {
		fi, err := os.Stat(c.path)
		if err != nil {
			continue
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			fmt.Fprintf(out, "warning: %s %s is accessible by other users (mode %03o, want %03o)\n",
				c.label, c.path, perm, c.want)
			warned = true
		}
	}
	if warned {
		fmt.Fprintln(out)
	}
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
	fmt.Fprintf(out, "schema version: %d (binary: %d)\n", s.SchemaVersion, store.SchemaVersion)
	if !s.EarliestEvent.IsZero() {
		fmt.Fprintf(out, "earliest:       %s\n", s.EarliestEvent.Local().Format("2006-01-02 15:04"))
	}
	if !s.LatestEvent.IsZero() {
		fmt.Fprintf(out, "latest:         %s\n", s.LatestEvent.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintln(out)
}

// printAdapterDiscovery runs each adapter's read-only discovery and prints how
// many sources it located, with notes for opt-in/empty adapters. An adapter that
// discovers nothing reports the absent state rather than a zero count, and gets
// its enablement checklist when one exists.
func printAdapterDiscovery(c *cobra.Command, cfg config.Config) {
	out := c.OutOrStdout()
	ctx := cmdContext(c)
	dc := discoverConfig(cfg)

	fmt.Fprintln(out, "Adapter discovery")
	fmt.Fprintln(out, strings.Repeat("-", 17))
	for _, ad := range defaultRegistry().All() {
		srcs, err := ad.Discover(ctx, dc)
		absent := err == nil && len(srcs) == 0
		status := fmt.Sprintf("%d source(s)", len(srcs))
		switch {
		case err != nil:
			status = fmt.Sprintf("%d source(s), error: %v", len(srcs), err)
		case absent:
			status = absentStatus
		}
		fmt.Fprintf(out, "%-12s %s\n", ad.ID(), status)
		if note, ok := adapterNotes[ad.ID()]; ok {
			fmt.Fprintf(out, "             note: %s\n", note)
		}
		if absent {
			printEnablementGuide(out, ad.ID())
		}
	}
}

// printEnablementGuide prints a tool's opt-in checklist, indented under its
// discovery line. Tools without a guide print nothing. Lines are written
// verbatim (never as a format string) so shell snippets keep their % verbs.
func printEnablementGuide(out io.Writer, tool string) {
	lines, ok := enablementGuides[tool]
	if !ok {
		return
	}
	fmt.Fprintln(out)
	for _, ln := range lines {
		if ln == "" {
			fmt.Fprintln(out)
			continue
		}
		fmt.Fprintln(out, "    "+ln)
	}
	fmt.Fprintln(out)
}
