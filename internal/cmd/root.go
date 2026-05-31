// Package cmd implements the aiusage command-line interface using cobra.
//
// The root command owns the persistent flags (--db, --config, --interval,
// --home) and a small set of helpers that every subcommand reuses: resolving
// the effective Config (config.Load over the --config path, then applying the
// flag overrides), opening the append-only store read/write for collection or
// read-only for reporting, and wiring the default adapter registry plus the
// DiscoverConfig built from the resolved config.
//
// All reporting/browse subcommands are strictly read-only over already-stored
// data. Collection (run/once) is the only writer and only ever appends.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/agy"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/claudecode"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/codex"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/copilot"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/gemini"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/hermes"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/opencode"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/tui"
)

// globalFlags holds the values bound to the root command's persistent flags.
type globalFlags struct {
	db       string
	config   string
	interval int
	home     string
	noDaemon bool
}

var flags globalFlags

// daemonSkip lists the subcommands whose PersistentPreRunE must NOT auto-start
// the daemon: run *becomes* the daemon, once is an explicit single cycle, and
// doctor/completion/help/version are diagnostics that should never have a side
// effect. Everything else (the root TUI default plus today/last/summary/
// sources/export) is data-facing and triggers ensureDaemon.
var daemonSkip = map[string]bool{
	"run":        true,
	"once":       true,
	"doctor":     true,
	"completion": true,
	"help":       true,
	"version":    true,
}

// isTTY reports whether stdout is an interactive terminal. It is a seam so the
// non-TTY (help-instead-of-TUI) path is testable without a real PTY.
var isTTY = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// newRootCmd builds the cobra root command with its persistent flags and the
// full subcommand tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "aiusage",
		Short: "Local, read-only AI-agent token-usage daemon and TUI",
		Long: "aiusage polls AI-agent CLI files read-only and stores observed token " +
			"usage in append-only SQLite, then reports and browses it. " +
			"Later agent cleanup can never reduce a past interval's reported total.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE runs before every command's RunE. It auto-starts the
		// per-user daemon for data-facing actions (skipping run/once/doctor/etc.)
		// unless --no-daemon is set. A spawn failure here is non-fatal: report it
		// and continue so a reporting/TUI command still works without collection.
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			if flags.noDaemon || daemonSkip[c.Name()] {
				return nil
			}
			// Bare `aiusage` only launches the TUI (the data-facing action) when
			// stdout is a terminal; otherwise RunE prints help. A piped/redirected
			// invocation is a help/diagnostic action, so it must NOT spawn a
			// background daemon. Subcommands (today/summary/...) are explicitly
			// data-facing and spawn regardless of TTY.
			if !c.HasParent() && !isTTY() {
				return nil
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := ensureDaemon(cfg); err != nil {
				fmt.Fprintf(c.ErrOrStderr(), "warning: could not start daemon: %v\n", err)
			}
			return nil
		},
		// RunE on the root is the TUI launcher: bare `aiusage` opens the
		// dashboard when stdout is a terminal, and prints help otherwise (so a
		// piped/redirected invocation never hangs headless).
		RunE: func(c *cobra.Command, _ []string) error {
			if !isTTY() {
				return c.Help()
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
			return tui.Run(st, tui.Options{DBPath: cfg.DBPath, StatePath: uiStatePath(cfg)})
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flags.db, "db", "", "path to the usage database (overrides config)")
	pf.StringVar(&flags.config, "config", "", "path to the JSON config file (default: XDG config path)")
	pf.IntVar(&flags.interval, "interval", 0, "collection interval in seconds (overrides config; clamped [60,1800])")
	pf.StringVar(&flags.home, "home", "", "discovery home directory (overrides config; for testing/sandboxing)")
	pf.BoolVar(&flags.noDaemon, "no-daemon", false, "do not auto-start the background collection daemon")

	root.AddCommand(
		newRunCmd(),
		newOnceCmd(),
		newSummaryCmd(),
		newTodayCmd(),
		newLastCmd(),
		newSourcesCmd(),
		newDoctorCmd(),
		newExportCmd(),
		newVersionCmd(),
	)
	return root
}

// Execute builds and runs the root command. main.go calls this and reports any
// error to stderr.
func Execute() error {
	return newRootCmd().Execute()
}

// loadConfig resolves the effective configuration: config.Load over the
// --config path (an empty path means "use the default config location, which
// may legitimately not exist"), then applies the --db/--interval/--home
// overrides. The interval is re-clamped after a flag override so the documented
// [60,1800] bound always holds.
func loadConfig() (config.Config, error) {
	path := flags.config
	if path == "" {
		path = config.DefaultConfigPath()
	}

	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	if flags.db != "" {
		cfg.DBPath = flags.db
	}
	if flags.home != "" {
		cfg.Home = flags.home
	}
	if flags.interval > 0 {
		cfg.IntervalSeconds = clampInterval(flags.interval)
	}
	if cfg.SourceRoots == nil {
		cfg.SourceRoots = map[string]string{}
	}
	return cfg, nil
}

// clampInterval bounds an explicit --interval flag to the documented range so
// a flag override obeys the same limits as the config layer.
func clampInterval(n int) int {
	const (
		minInterval = 60
		maxInterval = 1800
	)
	if n < minInterval {
		return minInterval
	}
	if n > maxInterval {
		return maxInterval
	}
	return n
}

// uiStatePath returns the path to the TUI's persisted ui-state.json, which lives
// in the XDG state dir alongside the daemon pid/log (derived from PIDPath's
// directory) — incidental UI state, never mixed into config.json.
func uiStatePath(cfg config.Config) string {
	if cfg.PIDPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.PIDPath), "ui-state.json")
}

// openStore opens the configured database. The collector (run/once) needs a
// writable handle; everything else is read-only over the data but the store's
// Open is the single entry point either way.
func openStore(cfg config.Config) (store.Store, error) {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", cfg.DBPath, err)
	}
	return st, nil
}

// discoverConfig builds the adapter DiscoverConfig from the resolved config.
func discoverConfig(cfg config.Config) adapter.DiscoverConfig {
	return adapter.DiscoverConfig{Home: cfg.Home, Overrides: cfg.SourceRoots}
}

// defaultRegistry returns the registry wired with every built-in adapter.
//
// The wiring lives here (in cmd) rather than in package adapter because each
// sub-adapter package imports aiusage/internal/adapter for the Adapter contract
// types; having package adapter import them back would create an import cycle.
// cmd is the natural composition root, so it owns the concrete wiring.
func defaultRegistry() *adapter.Registry {
	return adapter.NewRegistry(
		claudecode.New(),
		codex.New(),
		copilot.New(),
		opencode.New(),
		hermes.New(),
		gemini.New(),
		agy.New(),
	)
}

// cmdContext returns the context for a command invocation. cobra wires
// signal-aware contexts when set up via ExecuteContext; here we fall back to the
// command's context so subcommands stay cancellation-aware.
func cmdContext(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// timeNow is a seam so command timestamp logic (today/last) is deterministic in
// tests. It returns local time because day/range buckets are clock-relative.
var timeNow = func() time.Time { return time.Now() }
