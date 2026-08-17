package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/tui"
)

// A discovery sweep that never gets to run must report every tool as unknown -
// absent from the map - rather than as zero sources. The TUI turns a zero into
// the sentence "configured, no data source", which would be a lie about the
// machine whenever the sweep was merely cut short (issue #44).
func TestDiscoveredSourcesReportsUnknownWhenCutShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := config.Default()
	got := discoveredSources(ctx, cfg)

	for tool, n := range got {
		t.Errorf("cancelled discovery reported %s = %d sources; want the tool absent (unknown)", tool, n)
	}
}

// The sweep runs before the first frame, so it must be bounded: a hung or
// unreachable source root must not keep the dashboard from opening.
func TestDiscoveredSourcesIsBounded(t *testing.T) {
	if discoveryBudget <= 0 {
		t.Fatalf("discoveryBudget = %v; startup discovery must be bounded", discoveryBudget)
	}

	done := make(chan map[string]int, 1)
	go func() { done <- discoveredSources(context.Background(), config.Default()) }()

	select {
	case <-done:
	case <-time.After(discoveryBudget + 5*time.Second):
		t.Fatal("discoveredSources did not return within its budget; startup would block")
	}
}

// The sweep above is only worth running if its result reaches the dashboard.
// cmd is the composition root - internal/tui must not import adapter -
// so the counts travel exactly one way: the Sources argument of the tui.Options
// the root command hands to Run. Nothing about that argument is load-bearing at
// compile time, so this drives the real command and asserts the VALUES that
// arrive: the two claude-code roots the fixture home lays down, and, key for
// key, the same map a direct sweep over the same resolved config produces. A
// wiring that fed a different config, an empty map or none at all fails here.
func TestRootHandsDiscoveredSourcesToTheTUI(t *testing.T) {
	// The root command resolves the daemon pid/log and the TUI's ui-state.json
	// under the state dir; without this the run reaches into the developer's
	// real XDG_STATE_HOME, as every other command test in this package avoids
	// doing. The StatePath assertion below keeps the isolation honest.
	stateDir := isolateState(t)
	home := t.TempDir()
	// claude-code accepts both <home>/.config/claude and <home>/.claude when
	// each holds a projects/ dir, so this fixture home has exactly two of its
	// sources - a count neither zero nor one, which a nil or empty map cannot
	// coincidentally match.
	for _, root := range []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".config", "claude", "projects"),
	} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", root, err)
		}
	}

	// Ambient env that would hand an adapter a source outside the sandbox.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COPILOT_OTEL_FILE_EXPORTER_PATH", "")
	t.Setenv("OPENCODE_DATA_DIR", "")

	db := filepath.Join(t.TempDir(), "usage.db")
	cfgPath := offlineConfig(t)

	prevTTY, prevRun, prevFlags := isTTY, runTUI, flags
	t.Cleanup(func() { isTTY, runTUI, flags = prevTTY, prevRun, prevFlags })

	var got tui.Options
	var launched bool
	isTTY = func() bool { return true }
	runTUI = func(_ tui.DataSource, opt tui.Options) error {
		got, launched = opt, true
		return nil
	}

	// --no-daemon keeps the run hermetic; the root command with no subcommand is
	// the TUI launcher.
	if out, err := runCmd(t, "--db", db, "--home", home, "--config", cfgPath, "--no-daemon"); err != nil {
		t.Fatalf("root command failed: %v\noutput:\n%s", err, out)
	}
	if !launched {
		t.Fatal("the root command did not launch the TUI; nothing was wired")
	}

	// Everything the command resolves must stay inside the sandbox, the state
	// dir included: a run that persisted UI state to the host would also be
	// reading a host daemon's pid file.
	if !strings.HasPrefix(got.StatePath, stateDir) {
		t.Errorf("tui.Options.StatePath = %q, want it under the isolated state dir %q", got.StatePath, stateDir)
	}

	if n, ok := got.Sources["claude-code"]; !ok || n != 2 {
		t.Errorf("tui.Options.Sources[claude-code] = %d (present=%v), want 2 - the count the fixture home discovers",
			n, ok)
	}

	// The same sweep, run directly over the config the flags resolve to.
	flags = globalFlags{db: db, home: home, config: cfgPath}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := discoveredSources(context.Background(), cfg)
	if len(want) == 0 {
		t.Fatal("the direct sweep discovered nothing; the fixture cannot pin the wiring")
	}
	if len(got.Sources) != len(want) {
		t.Errorf("tui.Options.Sources has %d tools, want %d: got %v, want %v",
			len(got.Sources), len(want), got.Sources, want)
	}
	for tool, n := range want {
		if g, ok := got.Sources[tool]; !ok || g != n {
			t.Errorf("tui.Options.Sources[%s] = %d (present=%v), want %d", tool, g, ok, n)
		}
	}
}
