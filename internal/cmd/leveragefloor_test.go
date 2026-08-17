package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/tui"
	"github.com/RandomCodeSpace/aiusage/store"
)

// The hero's leverage floor is configurable (issue #39), which is only true if
// the value survives the whole path: config file -> Load -> tui.Options. cmd is
// the composition root, and the Options value never leaves the RunE closure, so
// the runTUI seam is the only place the wiring is observable. A default that
// forgot to read the config, or a field wired to the wrong key, fails here.
func TestRootHandsConfiguredLeverageFloorToTheTUI(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath,
		[]byte(`{"pricing":{"refresh":false},"tui":{"leverage_input_floor":750000}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	db := filepath.Join(t.TempDir(), "usage.db")
	home := t.TempDir()

	prevTTY, prevRun, prevFlags := isTTY, runTUI, flags
	t.Cleanup(func() { isTTY, runTUI, flags = prevTTY, prevRun, prevFlags })

	var got tui.Options
	var launched bool
	isTTY = func() bool { return true }
	runTUI = func(_ store.Store, opt tui.Options) error {
		got, launched = opt, true
		return nil
	}

	if out, err := runCmd(t, "--db", db, "--home", home, "--config", cfgPath, "--no-daemon"); err != nil {
		t.Fatalf("root command failed: %v\noutput:\n%s", err, out)
	}
	if !launched {
		t.Fatal("the root command did not launch the TUI; nothing was wired")
	}
	if got.LeverageFloor != 750_000 {
		t.Errorf("tui.Options.LeverageFloor = %d, want 750000 from the config file", got.LeverageFloor)
	}

	// Absent from the config, the option stays at the sentinel the view reads as
	// "derive from the bucket span" — never a floor of zero tokens.
	launched = false
	if out, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t), "--no-daemon"); err != nil {
		t.Fatalf("root command failed: %v\noutput:\n%s", err, out)
	}
	if !launched {
		t.Fatal("the second run did not launch the TUI")
	}
	if got.LeverageFloor != 0 {
		t.Errorf("unconfigured tui.Options.LeverageFloor = %d, want 0", got.LeverageFloor)
	}
}

// A negative floor is not a smaller floor; Load normalizes it to unset so the
// consumer's span-derived default runs instead of every bucket clearing a
// negative threshold.
func TestNegativeLeverageFloorNormalizesToUnset(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"tui":{"leverage_input_floor":-1}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	flags = globalFlags{config: cfgPath}
	t.Cleanup(func() { flags = globalFlags{} })

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.TUI.LeverageInputFloor != 0 {
		t.Errorf("negative leverage_input_floor resolved to %d, want 0", cfg.TUI.LeverageInputFloor)
	}
}
