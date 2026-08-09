package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/report"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// exportOpts holds the flags for the export command.
type exportOpts struct {
	since      string
	until      string
	format     string
	out        string
	includeRaw bool
}

// newExportCmd builds the `export` command: writes the raw matching events as
// JSON or CSV to stdout or a file.
func newExportCmd() *cobra.Command {
	var o exportOpts
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export raw usage events as JSON or CSV",
		Long: "export writes the raw usage events between --since and --until in the " +
			"chosen --format (json|csv) to stdout, or to --out when given.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runExport(c, o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.since, "since", "", "lower time bound (RFC3339, YYYY-MM-DD, or a span like 7d)")
	f.StringVar(&o.until, "until", "", "upper time bound (RFC3339, YYYY-MM-DD, or a span like 1h)")
	f.StringVar(&o.format, "format", "json", "output format: json or csv")
	f.StringVar(&o.out, "out", "", "output file path (default: stdout)")
	f.BoolVar(&o.includeRaw, "include-raw", false,
		"include the raw provider payload per event (may contain full transcript content)")
	return cmd
}

func runExport(c *cobra.Command, o exportOpts) error {
	format := strings.ToLower(strings.TrimSpace(o.format))
	if format != "json" && format != "csv" {
		return fmt.Errorf("invalid --format %q: want json or csv", o.format)
	}

	since, err := parseTimeFlag(o.since)
	if err != nil {
		return err
	}
	until, err := parseTimeFlag(o.until)
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

	evs, err := st.ListEvents(cmdContext(c), store.Filter{Since: since, Until: until})
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}

	w, closeFn, err := exportWriter(c, o.out, o.includeRaw)
	if err != nil {
		return err
	}
	defer closeFn()

	switch format {
	case "csv":
		if o.includeRaw {
			return report.WriteEventsCSVWithRaw(w, evs)
		}
		return report.WriteEventsCSV(w, evs)
	default:
		if o.includeRaw {
			return report.WriteEventsJSONWithRaw(w, evs)
		}
		return report.WriteEventsJSON(w, evs)
	}
}

// exportWriter resolves the output target: stdout when out is empty, otherwise
// a created/truncated file. A file holding raw payloads is created 0600: raw
// can carry full transcript content. The returned closeFn closes the file (and
// is a no-op for stdout).
func exportWriter(c *cobra.Command, out string, includeRaw bool) (io.Writer, func(), error) {
	if strings.TrimSpace(out) == "" {
		return c.OutOrStdout(), func() {}, nil
	}
	mode := os.FileMode(0o644)
	if includeRaw {
		mode = 0o600
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return nil, nil, fmt.Errorf("create output file %s: %w", out, err)
	}
	if includeRaw {
		// The create mode above only applies to new files; tighten pre-existing
		// ones too before transcript content is written into them.
		_ = f.Chmod(0o600)
	}
	return f, func() { f.Close() }, nil
}
