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
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/adapter/agy"
	"github.com/RandomCodeSpace/aiusage/adapter/claudecode"
	"github.com/RandomCodeSpace/aiusage/adapter/clinecli"
	"github.com/RandomCodeSpace/aiusage/adapter/codex"
	"github.com/RandomCodeSpace/aiusage/adapter/copilot"
	"github.com/RandomCodeSpace/aiusage/adapter/crush"
	"github.com/RandomCodeSpace/aiusage/adapter/dsh"
	"github.com/RandomCodeSpace/aiusage/adapter/goose"
	"github.com/RandomCodeSpace/aiusage/adapter/hermes"
	"github.com/RandomCodeSpace/aiusage/adapter/kimicode"
	"github.com/RandomCodeSpace/aiusage/adapter/opencode"
	"github.com/RandomCodeSpace/aiusage/adapter/pi"
	"github.com/RandomCodeSpace/aiusage/adapter/qwencode"
	"github.com/RandomCodeSpace/aiusage/adapter/reasonix"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/tui"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
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
//
// setup is skipped for the plain reason that it is the command that does the
// installing.
var daemonSkip = map[string]bool{
	"run":        true,
	"once":       true,
	"setup":      true,
	"doctor":     true,
	"completion": true,
	"help":       true,
	"version":    true,
}

// isTTY reports whether stdout is an interactive terminal. It is a seam so the
// non-TTY (help-instead-of-TUI) path is testable without a real PTY.
var isTTY = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// runTUI launches the dashboard. It is a seam for the same reason isTTY is: the
// composition root's job is the Options it hands over - the db path, the state
// path, the collect interval and the discovery counts (issue #44) - and a value
// that never leaves this function cannot be asserted on without a real
// terminal. Every wiring value here is otherwise unobservable from a test.
var runTUI = tui.Run

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
			if err := ensureDaemon(cmdContext(c), cfg, c.ErrOrStderr()); err != nil {
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
			return runTUI(st, tui.Options{
				DBPath:          cfg.DBPath,
				StatePath:       uiStatePath(cfg),
				CollectInterval: time.Duration(cfg.IntervalSeconds) * time.Second,
				Sources:         discoveredSources(cmdContext(c), cfg),
				Capabilities:    toolCapabilities(),
				LeverageFloor:   cfg.TUI.LeverageInputFloor,
			})
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flags.db, "db", "", "path to the usage database (overrides config)")
	pf.StringVar(&flags.config, "config", "", "path to the JSON config file (default: XDG config path)")
	pf.IntVar(&flags.interval, "interval", 0, "collection interval in seconds (overrides config; clamped [60,1800])")
	pf.StringVar(&flags.home, "home", "", "discovery home directory (overrides config; for testing/sandboxing)")
	pf.BoolVar(&flags.noDaemon, "no-daemon", false,
		"do not auto-start the collection daemon or install its systemd user units "+
			"(the explicit setup command still installs them)")

	root.AddCommand(
		newRunCmd(),
		newOnceCmd(),
		newSummaryCmd(),
		newTodayCmd(),
		newLastCmd(),
		newSourcesCmd(),
		newDoctorCmd(),
		newExportCmd(),
		newSetupCmd(),
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
// may legitimately not exist"), then applies the --home/--db/--interval
// overrides (--home re-derives the DB/PID/log paths it moves; see
// config.SetHome). The interval is re-clamped after a flag override so the
// documented [60,1800] bound always holds.
func loadConfig() (config.Config, error) {
	path := flags.config
	if path == "" {
		path = config.DefaultConfigPath()
	}

	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	// Home first: SetHome moves the still-derived DB/PID/log paths with it, and
	// the explicit --db below must override the home-derived DB path, not the
	// other way around.
	if flags.home != "" {
		cfg.SetHome(flags.home)
	}
	if flags.db != "" {
		cfg.DBPath = flags.db
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
// discoveryBudget caps the startup discovery sweep. It is generous enough for a
// cold cache on a large home directory and short enough that a hung or
// unreachable source root cannot keep the dashboard from opening.
const discoveryBudget = 2 * time.Second

func discoverConfig(cfg config.Config) adapter.DiscoverConfig {
	return adapter.DiscoverConfig{Home: cfg.Home, Overrides: cfg.SourceRoots}
}

// discoveredSources runs every adapter's read-only discovery once and reports
// how many sources each located, keyed by tool id — the same signal `doctor`
// prints. The TUI takes it at startup so a statement like "no data source" is a
// fact about the machine instead of an inference from an empty range
// (issue #44); it is resolved here because cmd is the composition root and
// internal/tui must not import adapter.
//
// An adapter whose discovery ERRORS is left out of the map entirely: unknown is
// not zero, and a failed glob must not be reported as an absent source. A
// discovery cut short by the deadline below is an error for this purpose, so a
// slow home directory yields "unknown" rather than a false "no data source".
//
// Discovery walks the filesystem, so it is bounded: the dashboard's contract is
// that nothing blocks the first frame, and this runs before it. Whatever has
// not answered by then is simply unknown, which every caller already handles.
func discoveredSources(ctx context.Context, cfg config.Config) map[string]int {
	ctx, cancel := context.WithTimeout(ctx, discoveryBudget)
	defer cancel()

	dc := discoverConfig(cfg)
	out := make(map[string]int)
	for _, ad := range defaultRegistry().All() {
		srcs, err := ad.Discover(ctx, dc)
		if err != nil || ctx.Err() != nil {
			continue
		}
		// USAGE sources only. The question this answers is whether a tool has a
		// token source, and copilot discovers session-state sources that carry
		// skills and hooks and never a token (adapter.MetaNoUsage). For every
		// other adapter this is len(srcs).
		out[ad.ID()] = adapter.CountUsageSources(srcs)
	}
	return out
}

// toolCapabilities collects every adapter's own capability declaration, keyed by
// tool id, for the TUI to render in its By-Tool detail card. It is resolved here
// for the same reason discoveredSources is: cmd is the composition root, and
// internal/tui must not import adapter.
//
// The registry is asked, not a table: each adapter states where its cost figures
// come from, whether a tool call can be joined to the turn that paid for it and
// how well it is verified, so the statement cannot drift from the code it
// describes (issue #72, decision 1).
//
// Retired tools go in FIRST and are therefore overwritten by any adapter that
// claims the same id. usage_events is append-only, so a ledger still holds rows
// for tools nothing collects any more and the dashboard still has to describe
// them; a tool that comes back to life is described by its adapter, and the list
// of the retired stays a claim about what is NOT registered rather than a second
// opinion about what is (TestRetiredToolsHaveNoAdapter).
//
// Keyed by ad.ID() rather than by the declaration's own Tool field: the registry
// is the authority on which tool an adapter is, and the two agreeing is a
// property worth testing rather than assuming (TestCapabilityDeclarationsNameTheirOwnTool).
func toolCapabilities() map[string]model.ToolCapability {
	out := make(map[string]model.ToolCapability)
	for _, c := range model.RetiredCapabilities() {
		out[c.Tool] = c
	}
	for _, ad := range defaultRegistry().All() {
		out[ad.ID()] = ad.Capabilities()
	}
	return out
}

// defaultRegistry returns the registry wired with every built-in adapter.
//
// The wiring lives here (in cmd) rather than in package adapter because each
// sub-adapter package imports aiusage/adapter for the Adapter contract
// types; having package adapter import them back would create an import cycle.
// cmd is the natural composition root, so it owns the concrete wiring.
func defaultRegistry() *adapter.Registry {
	return adapter.NewRegistry(
		claudecode.New(),
		codex.New(),
		copilot.New(),
		opencode.New(),
		hermes.New(),
		agy.New(),
		clinecli.New(),
		crush.New(),
		dsh.New(),
		goose.New(),
		kimicode.New(),
		// Pi and OpenClaw are two harnesses over one session format, so one
		// package serves both. They are separate registry entries because they
		// are separate tools: their rows must never be summed into one.
		pi.NewPi(),
		pi.NewOpenClaw(),
		qwencode.New(),
		reasonix.New(),
	)
}

// discoveryEnv names every environment variable that moves WHAT the adapters
// read, as opposed to where aiusage keeps its own files (config.PathEnvNames
// covers those). It is assembled here because cmd is the composition root: each
// adapter owns its variable, and package adapter cannot collect them without
// importing the packages that import it.
//
// The list matters for supervision. A systemd unit does not inherit the
// installing shell's environment, so a unit written under CLAUDE_CONFIG_DIR
// supervises a daemon that reads the DEFAULT Claude directory while the CLI
// that installed it reads the overridden one - the same disagreement a path
// override causes, arriving through what is collected rather than where it is
// stored. TestDiscoveryEnvCoversEveryAdapterVariable reads the adapter sources
// and fails when one consults a variable this list has not been taught about.
func discoveryEnv() []string {
	return []string{
		claudecode.ConfigDirEnv,
		// Cline resolves four nested rungs independently: CLINE_DIR moves the
		// root, and each of the other three moves one directory below it
		// whether or not the root moved. Any one of them alone puts the
		// adapter's sessions tree or its discovery index somewhere a unit
		// written under it would not look.
		clinecli.DirEnv,
		clinecli.DataDirEnv,
		clinecli.SessionDataDirEnv,
		clinecli.DBDataDirEnv,
		codex.HomeEnv,
		copilot.ExporterEnv,
		crush.GlobalDataEnv,
		// XDG_DATA_HOME is also one of config's own path variables, and it
		// belongs here as well: it moves where CRUSH keeps projects.json and
		// where GOOSE keeps sessions.db (goose.DataHomeEnv is the same name,
		// listed once), which is a different consequence from where aiusage
		// keeps its database.
		crush.XDGDataHomeEnv,
		dsh.HomeEnv,
		// GOOSE_PATH_ROOT relocates every goose directory at once and outranks
		// the XDG root above it.
		goose.PathRootEnv,
		hermes.HomeEnv,
		kimicode.HomeEnv,
		kimicode.DataDirEnv,
		opencode.DataDirEnv,
		// Pi and OpenClaw share one package and one session format, and
		// PI_CODING_AGENT_DIR moves BOTH surfaces: OpenClaw resolves its agent
		// dir as OPENCLAW_AGENT_DIR || PI_CODING_AGENT_DIR.
		//
		// Only the variables the package actually READS are listed. The two it
		// merely declares are not: OPENCLAW_PROFILE relocates the state root to
		// ~/.openclaw-<name>, which discovery finds by globbing the sibling
		// roots whether the variable is exported or not, and
		// OPENCLAW_CONFIG_PATH names a config this adapter does not parse, so a
		// unit and the shell that installed it read exactly the same tree under
		// both. Listing them would cost supervision for no disagreement.
		pi.AgentDirEnv,
		pi.SessionDirEnv,
		pi.OpenClawStateDirEnv,
		pi.OpenClawHomeEnv,
		pi.OpenClawAgentDirEnv,
		// Qwen Code's own precedence: QWEN_RUNTIME_DIR wins, then QWEN_HOME,
		// then ~/.qwen. The rung between them, settings' advanced.runtimeOutputDir,
		// is not an environment variable and the adapter deliberately does not
		// read it, so there is nothing to name here.
		qwencode.RuntimeDirEnv,
		qwencode.HomeEnv,
		// Reasonix resolves its state root from the first of these two that is
		// set, so either one moves every stats file the adapter reads.
		reasonix.StateHomeEnv,
		reasonix.HomeEnv,
	}
}

// discoveryEnvOverrides returns the discoveryEnv variables currently set to a
// value an adapter would act on, in the same fixed order. Adapters trim before
// testing, so a blank value moves nothing and is not reported.
//
// A name internal/config already owns is left to config's own rule rather than
// answered twice with a weaker test. XDG_DATA_HOME is both: it moves where
// aiusage keeps its database AND where Crush keeps projects.json, and the spec
// says a relative value is ignored — which config implements and the adapters
// follow. Reporting it here on presence alone would call a relative value an
// override that nothing acts on, and suppress an install over nothing.
func discoveryEnvOverrides() []string {
	owned := make(map[string]bool)
	for _, name := range config.PathEnvNames() {
		owned[name] = true
	}
	var out []string
	for _, name := range discoveryEnv() {
		if owned[name] {
			continue
		}
		if strings.TrimSpace(os.Getenv(name)) != "" {
			out = append(out, name)
		}
	}
	return out
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
