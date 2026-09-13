package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/report"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

const exportPageSize = 4096

type eventPageWriter interface {
	WritePage([]model.UsageEvent) error
	Close() error
}

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

func runExport(c *cobra.Command, o exportOpts) (resultErr error) {
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

	w, closeFn, err := exportWriter(c, o.out, o.includeRaw)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeFn()) }()
	return streamEventExport(cmdContext(c), st.Reader,
		store.Filter{Since: since, Until: until}, format, o.includeRaw, w)
}

func streamEventExport(ctx context.Context, reader *store.Reader, filter store.Filter, format string, includeRaw bool, out io.Writer) error {
	var stream eventPageWriter
	var err error
	switch format {
	case "csv":
		stream, err = report.NewEventsCSVWriter(out, includeRaw)
	default:
		stream, err = report.NewEventsJSONWriter(out, includeRaw)
	}
	if err != nil {
		return err
	}

	var afterTime time.Time
	var afterID int64
	for {
		listOpts := []store.ListOption{store.WithEventTimeKeyset(afterTime, afterID, exportPageSize)}
		if includeRaw {
			listOpts = append(listOpts, store.WithRaw())
		}
		page, err := reader.ListEvents(ctx, filter, listOpts...)
		if err != nil {
			return fmt.Errorf("list events: %w", err)
		}
		if len(page) == 0 {
			break
		}
		if err := stream.WritePage(page); err != nil {
			return err
		}
		last := page[len(page)-1]
		if last.EventTime.Before(afterTime) ||
			(last.EventTime.Equal(afterTime) && last.ID <= afterID) {
			return fmt.Errorf("list events: event-time cursor did not advance past row %d", afterID)
		}
		afterTime, afterID = last.EventTime, last.ID
		if len(page) < exportPageSize {
			break
		}
	}
	return stream.Close()
}

// exportWriter resolves the output target: stdout when out is empty, otherwise
// a created/truncated file. A file holding raw payloads is created 0600: raw
// can carry full transcript content. The returned closeFn closes the file (and
// is a no-op for stdout).
type exportFile interface {
	io.Writer
	Chmod(os.FileMode) error
	Truncate(int64) error
	Close() error
}

var openExportFile = func(path string, flag int, mode os.FileMode) (exportFile, error) {
	return os.OpenFile(path, flag, mode)
}

func exportWriter(c *cobra.Command, out string, includeRaw bool) (io.Writer, func() error, error) {
	if strings.TrimSpace(out) == "" {
		return c.OutOrStdout(), func() error { return nil }, nil
	}
	mode := os.FileMode(0o644)
	if includeRaw {
		mode = 0o600
	}
	openFlags := os.O_CREATE | os.O_WRONLY
	if !includeRaw {
		openFlags |= os.O_TRUNC
	}
	f, err := openExportFile(out, openFlags, mode)
	if err != nil {
		return nil, nil, fmt.Errorf("create output file %s: %w", out, err)
	}
	if includeRaw {
		// The create mode above only applies to new files; tighten pre-existing
		// ones too before transcript content is written into them.
		if err := f.Chmod(0o600); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("secure raw output %s: %w", out, err), f.Close())
		}
		if err := f.Truncate(0); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("truncate raw output %s: %w", out, err), f.Close())
		}
	}
	return f, func() error {
		if err := f.Close(); err != nil {
			return fmt.Errorf("close output file %s: %w", out, err)
		}
		return nil
	}, nil
}
