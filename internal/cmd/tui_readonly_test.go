package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/tui"
	"github.com/RandomCodeSpace/aiusage/store"
)

func seedTUIStore(t *testing.T, path string) {
	t.Helper()
	ledger, err := store.Open(path)
	if err != nil {
		t.Fatalf("seed TUI store: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatalf("close TUI store: %v", err)
	}
}

func TestRootTUILaunchUsesExistingReadOnlyStoreWithoutCollection(t *testing.T) {
	isolateState(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	seedTUIStore(t, db)
	home := t.TempDir()
	cfgPath := offlineConfig(t)

	spawnCalls, restoreSpawn := stubSpawn(t)
	t.Cleanup(restoreSpawn)
	services, _ := stubSupervisor(t)

	prevTTY, prevRun, prevFlags := isTTY, runTUI, flags
	t.Cleanup(func() { isTTY, runTUI, flags = prevTTY, prevRun, prevFlags })
	isTTY = func() bool { return true }
	launched := false
	runTUI = func(source tui.DataSource, _ tui.Options) error {
		if _, ok := source.(*store.Reader); !ok {
			t.Fatalf("TUI data source type = %T, want *store.Reader", source)
		}
		launched = true
		return nil
	}

	if out, err := runCmd(t, "--db", db, "--home", home, "--config", cfgPath); err != nil {
		t.Fatalf("root TUI failed: %v\noutput:\n%s", err, out)
	}
	if !launched {
		t.Fatal("root command did not launch the TUI")
	}
	if *spawnCalls != 0 || len(services.calls) != 0 {
		t.Fatalf("root TUI started collection: spawns=%d service calls=%v", *spawnCalls, services.calls)
	}
}

func TestRootTUIRefusesToInitializeDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, string)
	}{
		{name: "missing"},
		{name: "empty", seed: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("seed empty database: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			db := filepath.Join(t.TempDir(), "usage.db")
			if tc.seed != nil {
				tc.seed(t, db)
			}

			spawnCalls, restoreSpawn := stubSpawn(t)
			t.Cleanup(restoreSpawn)
			services, _ := stubSupervisor(t)

			prevTTY, prevRun, prevFlags := isTTY, runTUI, flags
			t.Cleanup(func() { isTTY, runTUI, flags = prevTTY, prevRun, prevFlags })
			isTTY = func() bool { return true }
			launched := false
			runTUI = func(tui.DataSource, tui.Options) error {
				launched = true
				return nil
			}

			out, err := runCmd(t, "--db", db, "--home", t.TempDir(), "--config", offlineConfig(t))
			if err == nil {
				t.Fatalf("root TUI initialized a %s database", tc.name)
			}
			for _, want := range []string{"read-only", "aiusage once", "aiusage run"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if out != "" {
				t.Fatalf("failed TUI wrote unexpected output: %q", out)
			}
			if launched || *spawnCalls != 0 || len(services.calls) != 0 {
				t.Fatalf("failed TUI had side effects: launched=%v spawns=%d service calls=%v",
					launched, *spawnCalls, services.calls)
			}
			info, statErr := os.Stat(db)
			if tc.seed == nil {
				if !os.IsNotExist(statErr) {
					t.Fatalf("missing database was created: stat=%v", statErr)
				}
			} else if statErr != nil || info.Size() != 0 {
				t.Fatalf("empty database was initialized: info=%v stat=%v", info, statErr)
			}
		})
	}
}
