package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"aiusage/internal/buildinfo"
	"aiusage/internal/collect"
	"aiusage/internal/config"
)

// stampCurrentVersion records the running build's identity as the daemon version
// so ensureDaemon's version-match check passes (treats the held-lock daemon as
// the same build → no restart).
func stampCurrentVersion(t *testing.T, cfg config.Config) {
	t.Helper()
	collect.WriteDaemonVersion(cfg, buildinfo.Identity())
}

// stubSpawn replaces the package-level spawnDaemon with a counter for the test's
// duration and returns the counter pointer plus a restore func. No real process
// is ever started.
func stubSpawn(t *testing.T) (*int, func()) {
	t.Helper()
	var calls int
	prev := spawnDaemon
	spawnDaemon = func(config.Config) error {
		calls++
		return nil
	}
	return &calls, func() { spawnDaemon = prev }
}

// seedLock creates pidPath+".lock" (so DaemonStatus can open it) and returns the
// path. It does NOT hold the lock — that simulates "lock file exists, daemon not
// running" (self-heal / first-run-after-crash).
func seedLock(t *testing.T, dir string) string {
	t.Helper()
	pidPath := filepath.Join(dir, "aiusage.pid")
	if err := os.WriteFile(pidPath+".lock", nil, 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}
	return pidPath
}

// holdLock takes the daemon's exclusive non-blocking flock and returns a release
// func, simulating a live daemon for the duration of the test.
func holdLock(t *testing.T, pidPath string) func() {
	t.Helper()
	f, err := os.OpenFile(pidPath+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("acquire lock: %v", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

// TestEnsureDaemonNoSpawnWhenRunning: a held lock means a daemon is up, so
// ensureDaemon must not spawn a second one.
func TestEnsureDaemonNoSpawnWhenRunning(t *testing.T) {
	dir := t.TempDir()
	pidPath := seedLock(t, dir)
	release := holdLock(t, pidPath)
	defer release()

	calls, restore := stubSpawn(t)
	defer restore()

	cfg := config.Config{PIDPath: pidPath}
	stampCurrentVersion(t, cfg) // same build → no restart
	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("expected 0 spawns when daemon running, got %d", *calls)
	}
}

// TestEnsureDaemonSpawnsWhenNotRunning: a free (or absent) lock means no daemon,
// so ensureDaemon spawns exactly one.
func TestEnsureDaemonSpawnsWhenNotRunning(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "aiusage.pid")

	calls, restore := stubSpawn(t)
	defer restore()

	cfg := config.Config{PIDPath: pidPath}
	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("expected 1 spawn when daemon not running, got %d", *calls)
	}
}

// TestEnsureDaemonSelfHeal: with a stale lock file present but the lock free
// (daemon crashed), ensureDaemon respawns. Two consecutive calls while the lock
// stays free both spawn — there is no live daemon to suppress them.
func TestEnsureDaemonSelfHeal(t *testing.T) {
	dir := t.TempDir()
	pidPath := seedLock(t, dir)
	// A stale pidfile from the crashed daemon must not fool ensureDaemon.
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("seed stale pidfile: %v", err)
	}

	calls, restore := stubSpawn(t)
	defer restore()

	cfg := config.Config{PIDPath: pidPath}
	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon (1): %v", err)
	}
	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon (2): %v", err)
	}
	if *calls != 2 {
		t.Fatalf("expected 2 spawns across a freed lock (self-heal), got %d", *calls)
	}
}

// TestEnsureDaemonSingletonAfterSpawn: once a daemon holds the lock (simulating
// the first spawn having taken it), a subsequent ensureDaemon does not spawn —
// the flock is the hard singleton guarantee.
func TestEnsureDaemonSingletonAfterSpawn(t *testing.T) {
	dir := t.TempDir()
	pidPath := seedLock(t, dir)

	calls, restore := stubSpawn(t)
	defer restore()

	cfg := config.Config{PIDPath: pidPath}

	// First call: no daemon yet -> spawn.
	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon (1): %v", err)
	}
	// Simulate the spawned daemon taking the lock + recording its version.
	release := holdLock(t, pidPath)
	defer release()
	stampCurrentVersion(t, cfg) // same build → no restart
	// Second call: lock held + version matches -> no spawn.
	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon (2): %v", err)
	}
	if *calls != 1 {
		t.Fatalf("expected exactly 1 spawn (singleton), got %d", *calls)
	}
}

// TestEnsureDaemonRestartsOnVersionMismatch: a running daemon built from a
// different version is stopped and respawned, keeping CLI + daemon in lockstep.
func TestEnsureDaemonRestartsOnVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	pidPath := seedLock(t, dir)
	release := holdLock(t, pidPath)
	defer release()

	cfg := config.Config{PIDPath: pidPath}
	collect.WriteDaemonVersion(cfg, "some-old-build") // != current identity

	calls, restore := stubSpawn(t)
	defer restore()

	// Stub stopDaemon so we don't block on the test-held flock; record the call.
	stopped := 0
	prevStop := stopDaemon
	stopDaemon = func(config.Config, int) error { stopped++; return nil }
	defer func() { stopDaemon = prevStop }()

	if err := ensureDaemon(cfg); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if stopped != 1 {
		t.Fatalf("stopDaemon calls = %d, want 1", stopped)
	}
	if *calls != 1 {
		t.Fatalf("spawn calls = %d, want 1 (respawn after stop)", *calls)
	}
}

// TestPersistentPreRunSkipsDaemon verifies the root's PersistentPreRunE makes
// the right spawn decision per target command: it never spawns for skip-listed
// commands (run/once/doctor/completion/help/version) nor when --no-daemon is
// set, and DOES spawn for data-facing commands (root default, today, summary).
//
// It invokes PersistentPreRunE directly for the resolved target command rather
// than running the command body, so commands like `run` (which would block in
// the foreground daemon loop) never execute — the spawn decision is all we test.
func TestPersistentPreRunSkipsDaemon(t *testing.T) {
	tests := []struct {
		name      string
		target    string // command to resolve; "" = root default (TUI)
		noDaemon  bool
		tty       bool
		wantSpawn bool
	}{
		// Bare `aiusage` only spawns when interactive (RunE launches the TUI). A
		// non-TTY bare invocation prints help instead, so it must not spawn.
		{name: "root default (TTY) spawns", target: "", tty: true, wantSpawn: true},
		{name: "root default (non-TTY) skips", target: "", tty: false, wantSpawn: false},
		// Explicit data-facing subcommands spawn regardless of TTY.
		{name: "today spawns", target: "today", wantSpawn: true},
		{name: "summary spawns", target: "summary", wantSpawn: true},
		{name: "last spawns", target: "last", wantSpawn: true},
		{name: "sources spawns", target: "sources", wantSpawn: true},
		{name: "export spawns", target: "export", wantSpawn: true},
		{name: "run skips", target: "run", wantSpawn: false},
		{name: "once skips", target: "once", wantSpawn: false},
		{name: "doctor skips", target: "doctor", wantSpawn: false},
		{name: "help skips", target: "help", wantSpawn: false},
		{name: "no-daemon flag skips", target: "today", noDaemon: true, wantSpawn: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prevTTY := isTTY
			isTTY = func() bool { return tc.tty }
			defer func() { isTTY = prevTTY }()

			dir := t.TempDir()
			// Redirect XDG paths so loadConfig (called inside PersistentPreRunE)
			// resolves PID/log paths into the temp dir, never ~/.local.
			t.Setenv("XDG_STATE_HOME", dir)
			t.Setenv("XDG_DATA_HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", dir)
			t.Setenv("AIUSAGE_DB", "")
			t.Setenv("AIUSAGE_HOME", "")
			t.Setenv("AIUSAGE_INTERVAL", "")
			t.Setenv("CLAUDE_CONFIG_DIR", "")

			calls, restore := stubSpawn(t)
			defer restore()

			root := newRootCmd()
			// newRootCmd binds --no-daemon (resetting flags.noDaemon to false), so
			// set the flag value AFTER building the command to mimic a parsed flag.
			flags.noDaemon = tc.noDaemon

			// Resolve the command cobra would dispatch PersistentPreRunE against.
			// For built-ins not registered as findable subcommands (help), build a
			// stand-in with the same Name() so daemonSkip[c.Name()] is exercised.
			target := resolveTarget(t, root, tc.target)

			if err := root.PersistentPreRunE(target, nil); err != nil {
				t.Fatalf("PersistentPreRunE(%q): %v", tc.target, err)
			}

			got := *calls > 0
			if got != tc.wantSpawn {
				t.Fatalf("target=%q noDaemon=%v: spawn=%v, want %v (calls=%d)",
					tc.target, tc.noDaemon, got, tc.wantSpawn, *calls)
			}
		})
	}
}

// resolveTarget returns the *cobra.Command that PersistentPreRunE should run
// against for a given target name. "" means the root default (TUI). Registered
// subcommands are resolved via Find; built-ins not exposed as findable commands
// (help) get a stand-in carrying the same Name() so the daemonSkip lookup —
// which keys off c.Name() at runtime — is faithfully exercised.
func resolveTarget(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	if name == "" {
		return root
	}
	if c, _, err := root.Find([]string{name}); err == nil && c.Name() == name {
		return c
	}
	return &cobra.Command{Use: name}
}

// TestNonTTYRootPrintsHelp: when stdout is not a terminal, bare `aiusage` prints
// help instead of launching the TUI (so it never hangs headless).
func TestNonTTYRootPrintsHelp(t *testing.T) {
	prev := isTTY
	isTTY = func() bool { return false }
	defer func() { isTTY = prev }()

	// Stub the spawn so the root's PersistentPreRunE does not start a process.
	_, restore := stubSpawn(t)
	defer restore()

	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	out, err := runCmd(t, "--db", filepath.Join(dir, "usage.db"),
		"--config", filepath.Join(dir, "absent.json"), "--no-daemon")
	if err != nil {
		t.Fatalf("bare aiusage (non-TTY) errored: %v\n%s", err, out)
	}
	if !contains(out, "Usage:") || !contains(out, "aiusage") {
		t.Fatalf("expected help output on non-TTY root, got:\n%s", out)
	}
}

// TestDaemonOptionsStampsVersion guards the version-sync wiring: the daemon's
// options must carry buildinfo.Identity() as Version. A regression here (Version
// left empty) makes RunDaemon skip writing daemon.version, so ReadDaemonVersion
// always returns "" != the CLI identity and ensureDaemon needlessly restarts the
// daemon on every CLI invocation.
func TestDaemonOptionsStampsVersion(t *testing.T) {
	opt := daemonOptions(config.Config{PIDPath: filepath.Join(t.TempDir(), "aiusage.pid")})
	if opt.Version == "" {
		t.Fatal("daemonOptions left Version empty — version-sync will restart the daemon every call")
	}
	if opt.Version != buildinfo.Identity() {
		t.Fatalf("daemonOptions Version = %q, want buildinfo.Identity() %q", opt.Version, buildinfo.Identity())
	}
}

// TestTUISubcommandRemoved: the standalone `tui` subcommand no longer exists.
func TestTUISubcommandRemoved(t *testing.T) {
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "tui" {
			t.Fatalf("`tui` subcommand should have been removed")
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
