package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/collect"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/service"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
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
	fmt.Fprintln(out)

	printSupervision(c, cfg)

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

// printSupervision reports how the collector is being kept alive, which is the
// question behind every complaint that numbers stopped updating.
//
// There are three honest answers and doctor gives whichever holds: systemd user
// units (named, with each one's enabled and running state), an unsupervised
// background process (collecting now, gone at the next reboot), or nothing at
// all. It never installs anything - doctor is in daemonSkip precisely so a
// diagnostic has no side effects.
//
// The whole block shares one deadline, for the reason ensureDaemon has one. It
// is several calls into the service manager - availability, then enabled and
// active per unit - and a manager answering each of them slowly (four seconds
// is a loaded machine, not a broken one) turns a diagnostic into the longest
// wait in the CLI. doctor is
// reached BECAUSE supervision is suspect, so the one thing it may not do is
// hang on it: when the budget expires the block says what it managed to
// establish and moves on, and a unit nobody got an answer about is reported as
// unknown rather than dressed up as inactive.
func printSupervision(c *cobra.Command, cfg config.Config) {
	out := c.OutOrStdout()
	fmt.Fprintln(out, "Supervision")
	fmt.Fprintln(out, strings.Repeat("-", 11))

	ctx, cancel := supervisionContext(cmdContext(c))
	defer cancel()

	m := newSupervisor()
	if m.Available(ctx) {
		units := m.Status(ctx)
		installed := false
		for _, u := range units {
			if u.Installed {
				installed = true
			}
		}
		if installed {
			fmt.Fprintln(out, "systemd user units:")
			for _, u := range units {
				fmt.Fprintf(out, "  %-24s %s\n", u.Name, unitState(u))
			}
			if ctx.Err() != nil {
				fmt.Fprintf(out, "  (the service manager stopped answering within %s; states above may be incomplete)\n",
					supervisionBudget)
			}
			fmt.Fprintln(out)
			return
		}
	}

	// A deadline that expired before the manager answered leaves "no units" as
	// an assumption rather than a finding, so it is not stated. The collection
	// lock is a local file and answers instantly, so whatever it says is still
	// worth printing.
	if ctx.Err() != nil {
		fmt.Fprintf(out, "unknown: the service manager did not answer within %s\n", supervisionBudget)
		if running, pid := collect.DaemonStatus(cfg); running {
			fmt.Fprintf(out, "a collector is running right now regardless (pid %d)\n", pid)
		}
		fmt.Fprintln(out)
		return
	}

	if running, pid := collect.DaemonStatus(cfg); running {
		fmt.Fprintf(out, "unsupervised background process (pid %d); `aiusage setup` installs systemd user units\n\n", pid)
		return
	}
	fmt.Fprintln(out, "none: no collector is running")
	fmt.Fprintln(out)
}

// unitState renders one unit's state for the supervision block.
func unitState(u service.UnitStatus) string {
	switch {
	case !u.Installed:
		return "not installed"
	case !u.StateKnown:
		return "installed, state unknown (no answer from the service manager)"
	}
	state := "inactive"
	if u.Active {
		state = "active"
	}
	if u.Enabled {
		return state + ", enabled"
	}
	return state + ", not enabled"
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
		// The enablement checklist answers "how do I get TOKENS", so it is owed
		// to an adapter that found sources none of which carry any — copilot
		// discovers a session-state source per session for skills and hooks
		// while its usage comes only from the opt-in OTEL export.
		noUsage := err == nil && adapter.CountUsageSources(srcs) == 0
		status := fmt.Sprintf("%d source(s)", len(srcs))
		switch {
		case err != nil:
			status = fmt.Sprintf("%d source(s), error: %v", len(srcs), err)
		case absent:
			status = absentStatus
		case noUsage:
			status = fmt.Sprintf("%d source(s), none carrying usage", len(srcs))
		}
		fmt.Fprintf(out, "%-12s %s\n", ad.ID(), status)
		if note, ok := adapterNotes[ad.ID()]; ok {
			fmt.Fprintf(out, "             note: %s\n", note)
		}
		if noUsage {
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
