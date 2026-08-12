package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestLoadRejectsUnknownConfigKeys: a typoed key must fail loudly (naming the
// key), not silently leave the default in place.
func TestLoadRejectsUnknownConfigKeys(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, path, map[string]any{"db_pth": "/typo/usage.db"})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(unknown key) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "db_pth") {
		t.Fatalf("error does not name the unknown key: %v", err)
	}
}

// TestHomeOverrideDerivation is the derivation table: an overridden home (file
// or env) moves DBPath/PIDPath/LogPath with it, unless a path was explicitly
// overridden (file/env pin it) or an absolute XDG_*_HOME env dir claims it.
func TestHomeOverrideDerivation(t *testing.T) {
	realHome := t.TempDir()
	override := t.TempDir()
	stateDir := t.TempDir()

	derivedDB := filepath.Join(override, ".local", "share", "aiusage", "usage.db")
	derivedPID := filepath.Join(override, ".local", "state", "aiusage", "aiusage.pid")
	derivedLog := filepath.Join(override, ".local", "state", "aiusage", "aiusage.log")

	tests := []struct {
		name                     string
		file                     map[string]any
		env                      map[string]string
		wantDB, wantPID, wantLog string
	}{
		{
			name:    "no override keeps real-home defaults",
			wantDB:  filepath.Join(realHome, ".local", "share", "aiusage", "usage.db"),
			wantPID: filepath.Join(realHome, ".local", "state", "aiusage", "aiusage.pid"),
			wantLog: filepath.Join(realHome, ".local", "state", "aiusage", "aiusage.log"),
		},
		{
			name:    "env home moves all derived paths",
			env:     map[string]string{"AIUSAGE_HOME": override},
			wantDB:  derivedDB,
			wantPID: derivedPID,
			wantLog: derivedLog,
		},
		{
			name:    "file home moves all derived paths",
			file:    map[string]any{"home": override},
			wantDB:  derivedDB,
			wantPID: derivedPID,
			wantLog: derivedLog,
		},
		{
			name:    "explicit env DB stays pinned",
			env:     map[string]string{"AIUSAGE_HOME": override, "AIUSAGE_DB": "/pinned/usage.db"},
			wantDB:  "/pinned/usage.db",
			wantPID: derivedPID,
			wantLog: derivedLog,
		},
		{
			name:    "explicit file pid_path stays pinned",
			file:    map[string]any{"home": override, "pid_path": "/pinned/aiusage.pid"},
			wantDB:  derivedDB,
			wantPID: "/pinned/aiusage.pid",
			wantLog: derivedLog,
		},
		{
			name:    "absolute XDG_STATE_HOME wins over home derivation",
			env:     map[string]string{"AIUSAGE_HOME": override, "XDG_STATE_HOME": stateDir},
			wantDB:  derivedDB,
			wantPID: filepath.Join(stateDir, "aiusage", "aiusage.pid"),
			wantLog: filepath.Join(stateDir, "aiusage", "aiusage.log"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearXDG(t)
			t.Setenv("HOME", realHome)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			path := ""
			if tc.file != nil {
				path = filepath.Join(t.TempDir(), "config.json")
				writeJSON(t, path, tc.file)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load error = %v", err)
			}
			if cfg.DBPath != tc.wantDB {
				t.Errorf("DBPath = %q, want %q", cfg.DBPath, tc.wantDB)
			}
			if cfg.PIDPath != tc.wantPID {
				t.Errorf("PIDPath = %q, want %q", cfg.PIDPath, tc.wantPID)
			}
			if cfg.LogPath != tc.wantLog {
				t.Errorf("LogPath = %q, want %q", cfg.LogPath, tc.wantLog)
			}
		})
	}
}

// TestSetHomeRespectsExplicitOverrides: SetHome (the --home flag path) moves
// only still-derived paths.
func TestSetHomeRespectsExplicitOverrides(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIUSAGE_DB", "/pinned/usage.db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}

	home := t.TempDir()
	cfg.SetHome(home)

	if cfg.Home != home {
		t.Errorf("Home = %q, want %q", cfg.Home, home)
	}
	if cfg.DBPath != "/pinned/usage.db" {
		t.Errorf("DBPath = %q, want pinned /pinned/usage.db", cfg.DBPath)
	}
	if want := filepath.Join(home, ".local", "state", "aiusage", "aiusage.pid"); cfg.PIDPath != want {
		t.Errorf("PIDPath = %q, want %q", cfg.PIDPath, want)
	}
	if want := filepath.Join(home, ".local", "state", "aiusage", "aiusage.log"); cfg.LogPath != want {
		t.Errorf("LogPath = %q, want %q", cfg.LogPath, want)
	}
}

// TestLoadResolvesRelativeEnvDB: a relative AIUSAGE_DB resolves against the
// config's home (env home when set, real home otherwise), never the process
// cwd — the CLI and the daemon it spawns must agree on the target.
func TestLoadResolvesRelativeEnvDB(t *testing.T) {
	t.Run("against env home", func(t *testing.T) {
		clearXDG(t)
		t.Setenv("HOME", t.TempDir())
		override := t.TempDir()
		t.Setenv("AIUSAGE_HOME", override)
		t.Setenv("AIUSAGE_DB", "sub/usage.db")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load error = %v", err)
		}
		if want := filepath.Join(override, "sub", "usage.db"); cfg.DBPath != want {
			t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
		}
	})

	t.Run("against real home", func(t *testing.T) {
		clearXDG(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("AIUSAGE_DB", "sub/usage.db")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load error = %v", err)
		}
		if want := filepath.Join(home, "sub", "usage.db"); cfg.DBPath != want {
			t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
		}
	})

	t.Run("absolute stays verbatim", func(t *testing.T) {
		clearXDG(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("AIUSAGE_DB", "/abs/usage.db")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load error = %v", err)
		}
		if cfg.DBPath != "/abs/usage.db" {
			t.Errorf("DBPath = %q, want /abs/usage.db", cfg.DBPath)
		}
	})
}

// TestLoadResolvesRelativeFilePaths: a hand-written config that uses relative
// paths must resolve them against the config file's own directory. Used
// verbatim they would resolve against the process working directory, which
// differs between the CLI (wherever the user stands) and the daemon it spawns
// (the CWD of the shell that started it) — two ledgers, no error. The test
// loads from an unrelated working directory to prove the result does not
// depend on it.
func TestLoadResolvesRelativeFilePaths(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "config.json")
	writeJSON(t, path, map[string]any{
		"db_path":      "data/usage.db",
		"pid_path":     "run/aiusage.pid",
		"log_path":     "run/aiusage.log",
		"source_roots": map[string]string{"claude-code": "roots/claude"},
	})

	t.Chdir(t.TempDir())

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}

	checks := []struct {
		field, got, want string
	}{
		{"DBPath", cfg.DBPath, filepath.Join(cfgDir, "data", "usage.db")},
		{"PIDPath", cfg.PIDPath, filepath.Join(cfgDir, "run", "aiusage.pid")},
		{"LogPath", cfg.LogPath, filepath.Join(cfgDir, "run", "aiusage.log")},
		{"SourceRoots[claude-code]", cfg.SourceRoots["claude-code"], filepath.Join(cfgDir, "roots", "claude")},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (anchored at the config file's directory)", c.field, c.got, c.want)
		}
	}
}

// TestLoadResolvesRelativeHomeFromConfigDir: a relative home is anchored the
// same way, and the paths derived from it inherit the anchoring instead of
// silently trailing the process working directory.
func TestLoadResolvesRelativeHomeFromConfigDir(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "config.json")
	writeJSON(t, path, map[string]any{"home": "sandbox"})

	t.Chdir(t.TempDir())

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}

	wantHome := filepath.Join(cfgDir, "sandbox")
	if cfg.Home != wantHome {
		t.Errorf("Home = %q, want %q", cfg.Home, wantHome)
	}
	if want := filepath.Join(wantHome, ".local", "share", "aiusage", "usage.db"); cfg.DBPath != want {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
	}
}

// TestLoadLeavesAbsoluteFilePathsVerbatim: anchoring must not touch a path the
// config file already gave in absolute form.
func TestLoadLeavesAbsoluteFilePathsVerbatim(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, path, map[string]any{
		"db_path":      "/abs/usage.db",
		"source_roots": map[string]string{"codex": "/abs/codex"},
	})

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.DBPath != "/abs/usage.db" {
		t.Errorf("DBPath = %q, want /abs/usage.db", cfg.DBPath)
	}
	if got := cfg.SourceRoots["codex"]; got != "/abs/codex" {
		t.Errorf("SourceRoots[codex] = %q, want /abs/codex", got)
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

// TestPathEnvNamesIncludesHome pins HOME into the list a caller reads before
// copying this process's paths into another one.
//
// It is not one variable among five. os.UserHomeDir resolves it, so it moves
// the database, the state directory, the config file and the systemd unit
// directory together, and unlike AIUSAGE_DB it has no flag that could carry it
// into a unit file instead.
func TestPathEnvNamesIncludesHome(t *testing.T) {
	names := PathEnvNames()
	for _, n := range names {
		if n == "HOME" {
			return
		}
	}
	t.Fatalf("PathEnvNames() = %v, with no HOME in it", names)
}

// TestHomeIsAnOverrideOnlyWhenItMoved: HOME is always set, so being set cannot
// be the test. The account's own home directory - which the user database holds
// and the environment cannot touch - is what it is compared against.
func TestHomeIsAnOverrideOnlyWhenItMoved(t *testing.T) {
	clearXDG(t)
	real := accountHome()
	if real == "" {
		t.Skip("this account has no resolvable home directory to compare against")
	}

	tests := []struct {
		name string
		home string
		want bool
	}{
		{name: "the account's own home", home: real},
		{name: "the same home spelled with a trailing separator", home: real + string(filepath.Separator)},
		{name: "somewhere else entirely", home: t.TempDir(), want: true},
		{name: "unset", home: "", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", tc.home)
			got := false
			for _, n := range PathEnvOverrides() {
				if n == "HOME" {
					got = true
				}
			}
			if got != tc.want {
				t.Errorf("HOME=%q reported as an override = %v, want %v", tc.home, got, tc.want)
			}
		})
	}
}

// TestUnverifiableHomeCountsAsMoved: with no account home to compare against,
// the value cannot be vouched for. Callers ask this question before baking
// these paths into something permanent, so the unverifiable case is treated as
// an override - a refusal costs one fallback, a wrong answer costs a unit
// supervising the wrong directory until somebody notices.
func TestUnverifiableHomeCountsAsMoved(t *testing.T) {
	clearXDG(t)
	prev := accountHome
	accountHome = func() string { return "" }
	t.Cleanup(func() { accountHome = prev })

	t.Setenv("HOME", t.TempDir())
	if !slices.Contains(PathEnvOverrides(), "HOME") {
		t.Fatal("an unverifiable HOME was reported as no override at all")
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
