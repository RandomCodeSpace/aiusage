package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// clearXDG isolates a test from the host's XDG/AIUSAGE environment so path
// resolution is deterministic.
func clearXDG(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME",
		"AIUSAGE_DB", "AIUSAGE_INTERVAL", "AIUSAGE_HOME",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestDefaultPaths(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := Default()

	if got, want := cfg.IntervalSeconds, defaultInterval; got != want {
		t.Errorf("IntervalSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.Home, home; got != want {
		t.Errorf("Home = %q, want %q", got, want)
	}
	if got, want := cfg.DBPath, filepath.Join(home, ".local", "share", "aiusage", "usage.db"); got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
	if got, want := cfg.PIDPath, filepath.Join(home, ".local", "state", "aiusage", "aiusage.pid"); got != want {
		t.Errorf("PIDPath = %q, want %q", got, want)
	}
	if got, want := cfg.LogPath, filepath.Join(home, ".local", "state", "aiusage", "aiusage.log"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if cfg.SourceRoots == nil {
		t.Error("SourceRoots is nil, want non-nil empty map")
	}
}

func TestDefaultRespectsXDG(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_STATE_HOME", stateDir)

	cfg := Default()

	if got, want := cfg.DBPath, filepath.Join(dataDir, "aiusage", "usage.db"); got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
	if got, want := cfg.PIDPath, filepath.Join(stateDir, "aiusage", "aiusage.pid"); got != want {
		t.Errorf("PIDPath = %q, want %q", got, want)
	}
	if got, want := cfg.LogPath, filepath.Join(stateDir, "aiusage", "aiusage.log"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}

func TestDefaultIgnoresRelativeXDG(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "relative/path") // must be ignored per XDG spec

	cfg := Default()

	if got, want := cfg.DBPath, filepath.Join(home, ".local", "share", "aiusage", "usage.db"); got != want {
		t.Errorf("DBPath = %q, want %q (relative XDG should be ignored)", got, want)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(missing) error = %v, want nil", err)
	}

	want := Default()
	want.IntervalSeconds = clampInterval(want.IntervalSeconds)
	if cfg.DBPath != want.DBPath {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, want.DBPath)
	}
	if cfg.IntervalSeconds != defaultInterval {
		t.Errorf("IntervalSeconds = %d, want %d", cfg.IntervalSeconds, defaultInterval)
	}
}

func TestLoadEmptyPathReturnsDefaults(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error = %v, want nil", err)
	}
	if cfg.IntervalSeconds != defaultInterval {
		t.Errorf("IntervalSeconds = %d, want %d", cfg.IntervalSeconds, defaultInterval)
	}
}

func TestLoadMergesFileOverDefaults(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	contents := map[string]any{
		"db_path":          "/custom/usage.db",
		"interval_seconds": 600,
		"source_roots":     map[string]string{"claude-code": "/data/claude"},
	}
	writeJSON(t, path, contents)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}

	if got, want := cfg.DBPath, "/custom/usage.db"; got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
	if got, want := cfg.IntervalSeconds, 600; got != want {
		t.Errorf("IntervalSeconds = %d, want %d", got, want)
	}
	if got, want := cfg.SourceRoots["claude-code"], "/data/claude"; got != want {
		t.Errorf("SourceRoots[claude-code] = %q, want %q", got, want)
	}
	// Fields absent from the file keep their default values.
	wantPID := filepath.Join(home, ".local", "state", "aiusage", "aiusage.pid")
	if cfg.PIDPath != wantPID {
		t.Errorf("PIDPath = %q, want default %q", cfg.PIDPath, wantPID)
	}
}

func TestLoadEnvOverridesFileAndDefaults(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeJSON(t, path, map[string]any{
		"db_path":          "/from/file.db",
		"interval_seconds": 600,
	})

	envHome := t.TempDir()
	t.Setenv("AIUSAGE_DB", "/from/env.db")
	t.Setenv("AIUSAGE_HOME", envHome)
	t.Setenv("AIUSAGE_INTERVAL", "900")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}

	if got, want := cfg.DBPath, "/from/env.db"; got != want {
		t.Errorf("DBPath = %q, want %q (env should win)", got, want)
	}
	if got, want := cfg.Home, envHome; got != want {
		t.Errorf("Home = %q, want %q (env should win)", got, want)
	}
	if got, want := cfg.IntervalSeconds, 900; got != want {
		t.Errorf("IntervalSeconds = %d, want %d (env should win)", got, want)
	}
}

func TestLoadClampsInterval(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"below min", 5, minInterval},
		{"at min", minInterval, minInterval},
		{"in range", 300, 300},
		{"at max", maxInterval, maxInterval},
		{"above max", 100000, maxInterval},
		{"zero resets to default then clamps", 0, defaultInterval},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			writeJSON(t, path, map[string]any{"interval_seconds": tc.in})

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load error = %v", err)
			}
			if cfg.IntervalSeconds != tc.want {
				t.Errorf("IntervalSeconds = %d, want %d", cfg.IntervalSeconds, tc.want)
			}
		})
	}
}

func TestLoadClampsIntervalFromEnv(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIUSAGE_INTERVAL", "10") // below min

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.IntervalSeconds != minInterval {
		t.Errorf("IntervalSeconds = %d, want %d", cfg.IntervalSeconds, minInterval)
	}
}

func TestLoadIgnoresMalformedEnvInterval(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIUSAGE_INTERVAL", "not-a-number")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.IntervalSeconds != defaultInterval {
		t.Errorf("IntervalSeconds = %d, want default %d", cfg.IntervalSeconds, defaultInterval)
	}
}

func TestLoadMalformedFileErrors(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load(malformed) error = nil, want non-nil")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
