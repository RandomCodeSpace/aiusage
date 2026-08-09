package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/collect"
	"github.com/RandomCodeSpace/aiusage/internal/config"
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
	if err := ensureDaemon(cfg, io.Discard); err != nil {
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
	if err := ensureDaemon(cfg, io.Discard); err != nil {
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
	if err := ensureDaemon(cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon (1): %v", err)
	}
	if err := ensureDaemon(cfg, io.Discard); err != nil {
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
	if err := ensureDaemon(cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon (1): %v", err)
	}
	// Simulate the spawned daemon taking the lock + recording its version.
	release := holdLock(t, pidPath)
	defer release()
	stampCurrentVersion(t, cfg) // same build → no restart
	// Second call: lock held + version matches -> no spawn.
	if err := ensureDaemon(cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon (2): %v", err)
	}
	if *calls != 1 {
		t.Fatalf("expected exactly 1 spawn (singleton), got %d", *calls)
	}
}

// TestEnsureDaemonDuringOnceCycle covers the once-holds-the-lock direction:
// a concurrent data-facing command's ensureDaemon sees the collection lock
// held and must treat the identity-stamped one-shot as a same-build daemon —
// no stop (a 3s stall aimed at a pid that is not a daemon), no spawn, no
// notice.
func TestEnsureDaemonDuringOnceCycle(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "aiusage.pid")
	release, err := collect.AcquireCollectionLock(pidPath, buildinfo.Identity())
	if err != nil {
		t.Fatalf("acquire collection lock: %v", err)
	}
	defer release()

	calls, restore := stubSpawn(t)
	defer restore()
	stopped := 0
	prevStop := stopDaemon
	stopDaemon = func(config.Config, int) error { stopped++; return nil }
	defer func() { stopDaemon = prevStop }()

	var warn bytes.Buffer
	if err := ensureDaemon(config.Config{PIDPath: pidPath}, &warn); err != nil {
		t.Fatalf("ensureDaemon during once cycle: %v", err)
	}
	if stopped != 0 || *calls != 0 || warn.Len() != 0 {
		t.Fatalf("ensureDaemon during once cycle: stops=%d spawns=%d warn=%q, want 0/0/empty",
			stopped, *calls, warn.String())
	}
}

// TestRestartOnMismatchDecision is the identity-mismatch decision table:
// release↔release mismatches restart, anything involving a dev identity does
// not (a `go run` temp binary is a new identity every invocation — restarting
// would flap the daemon), and an unrecorded version restarts once.
func TestRestartOnMismatchDecision(t *testing.T) {
	tests := []struct {
		name           string
		recorded, self string
		want           bool
	}{
		{"release vs release", "v1.0.0", "v1.1.0", true},
		{"unrecorded vs release", "", "v1.1.0", true},
		{"unrecorded vs dev stamp", "", "dev-100-200", true},
		{"dev stamp vs release", "dev-100-200", "v1.1.0", false},
		{"release vs dev stamp", "v1.0.0", "dev-100-200", false},
		{"dev stamp vs dev stamp", "dev-100-200", "dev-300-400", false},
		{"bare dev vs release", "dev", "v1.1.0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartOnMismatch(tc.recorded, tc.self); got != tc.want {
				t.Fatalf("restartOnMismatch(%q, %q) = %v, want %v", tc.recorded, tc.self, got, tc.want)
			}
		})
	}
}

// setVersion pins buildinfo.Version for the test's duration so
// buildinfo.Identity() returns a chosen release identity.
func setVersion(t *testing.T, v string) {
	t.Helper()
	prev := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = prev })
}

// TestEnsureDaemonIdentityMismatch drives ensureDaemon through the mismatch
// policy against a live (lock-held) fake daemon: release mismatches stop and
// respawn; dev-stamp mismatches leave the daemon alone and only write a notice.
func TestEnsureDaemonIdentityMismatch(t *testing.T) {
	tests := []struct {
		name      string
		recorded  string // "" = no daemon.version file (pre-stamping daemon)
		version   string // pinned buildinfo.Version; "dev" = executable stamp
		wantStop  int
		wantSpawn int
		wantWarn  bool
	}{
		{name: "release upgrade restarts", recorded: "v1.0.0", version: "v1.1.0", wantStop: 1, wantSpawn: 1},
		{name: "unrecorded version restarts once", recorded: "", version: "v1.1.0", wantStop: 1, wantSpawn: 1},
		{name: "dev daemon vs release CLI only warns", recorded: "dev-100-200", version: "v1.1.0", wantWarn: true},
		{name: "release daemon vs dev CLI only warns", recorded: "v1.0.0", version: "dev", wantWarn: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setVersion(t, tc.version)
			if tc.version == "dev" && !isDevIdentity(buildinfo.Identity()) {
				// Identity() may pick up a module version in some build modes;
				// the dev-CLI case is only meaningful with a dev identity.
				t.Skipf("test binary identity %q is not a dev stamp", buildinfo.Identity())
			}

			dir := t.TempDir()
			pidPath := seedLock(t, dir)
			release := holdLock(t, pidPath)
			defer release()

			cfg := config.Config{PIDPath: pidPath}
			if tc.recorded != "" {
				collect.WriteDaemonVersion(cfg, tc.recorded)
			}

			calls, restore := stubSpawn(t)
			defer restore()

			// Stub stopDaemon so we don't block on the test-held flock.
			stopped := 0
			prevStop := stopDaemon
			stopDaemon = func(config.Config, int) error { stopped++; return nil }
			defer func() { stopDaemon = prevStop }()

			var warn bytes.Buffer
			if err := ensureDaemon(cfg, &warn); err != nil {
				t.Fatalf("ensureDaemon: %v", err)
			}
			if stopped != tc.wantStop {
				t.Errorf("stopDaemon calls = %d, want %d", stopped, tc.wantStop)
			}
			if *calls != tc.wantSpawn {
				t.Errorf("spawn calls = %d, want %d", *calls, tc.wantSpawn)
			}
			if gotWarn := strings.Contains(warn.String(), "not auto-restarted"); gotWarn != tc.wantWarn {
				t.Errorf("warn output = %q, wantWarn %v", warn.String(), tc.wantWarn)
			}
		})
	}
}

// TestDaemonArgsForwardsFlags: the spawned `self run` must carry the parent's
// --db/--config/--home so the daemon collects into the same DB the CLI reads.
func TestDaemonArgsForwardsFlags(t *testing.T) {
	tests := []struct {
		name string
		f    globalFlags
		want []string
	}{
		{name: "no flags", f: globalFlags{}, want: []string{"run"}},
		{name: "db only", f: globalFlags{db: "/tmp/x.db"}, want: []string{"run", "--db", "/tmp/x.db"}},
		{
			name: "interval only",
			f:    globalFlags{interval: 900},
			want: []string{"run", "--interval", "900"},
		},
		{
			name: "all forwarded",
			f:    globalFlags{db: "/tmp/x.db", config: "/tmp/c.json", home: "/tmp/h", interval: 900},
			want: []string{"run", "--db", "/tmp/x.db", "--config", "/tmp/c.json", "--home", "/tmp/h", "--interval", "900"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonArgs(tc.f); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("daemonArgs(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// TestDaemonArgsAbsolutizesConfigPath covers the working-directory split: the
// CLI resolves a relative --config against the shell the user is standing in,
// while the daemon it spawns keeps whatever CWD it inherited. Forwarded
// verbatim the two processes read DIFFERENT files - and since config.Load
// anchors the relative paths written inside a config file to that file's own
// directory, they would then disagree about the database as well. The failure
// is silent, because a missing config file is not an error: the daemon would
// quietly collect into the default DB while the CLI reports on the configured
// one. So the forwarded value must be absolute.
func TestDaemonArgsAbsolutizesConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Read the CWD back rather than trusting dir: Getwd reports the resolved
	// path, and filepath.Abs builds on exactly that.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	got := daemonArgs(globalFlags{config: filepath.Join("cfg", "aiusage.json")})
	idx := indexOf(got, "--config")
	if idx < 0 || idx+1 >= len(got) {
		t.Fatalf("daemonArgs = %v, want a forwarded --config with its value", got)
	}
	if value := got[idx+1]; !filepath.IsAbs(value) {
		t.Fatalf("--config forwarded as %q: a relative path resolves against the daemon's CWD, not the CLI's", value)
	} else if want := filepath.Join(wd, "cfg", "aiusage.json"); value != want {
		t.Errorf("--config = %q, want %q", value, want)
	}

	// An already-absolute path is the common case and must pass through
	// unchanged, so the daemon still receives the exact file the user named.
	abs := filepath.Join(dir, "explicit.json")
	got = daemonArgs(globalFlags{config: abs})
	idx = indexOf(got, "--config")
	if idx < 0 || idx+1 >= len(got) {
		t.Fatalf("daemonArgs = %v, want a forwarded --config with its value", got)
	}
	if value := got[idx+1]; value != abs {
		t.Errorf("--config = %q, want the untouched %q", value, abs)
	}
}

// flagRole classifies one globalFlags field for the spawned-daemon argv.
type flagRole struct {
	flag      string // persistent flag bound to the field
	sample    string // value to parse; "" means a boolean flag, passed bare
	forwarded bool   // must appear in the spawned daemon's argv
	why       string // why it is NOT forwarded (required when forwarded is false)
}

// daemonFlagRoles is the classification of every globalFlags field. It is
// keyed by struct field name so TestDaemonArgsCoversEveryPersistentFlag can
// enumerate the struct and fail on anything unclassified: the bug this guards
// (#30) is a new global flag landing while daemonArgs is never updated, and a
// hardcoded list of flag names would go stale in exactly the same way.
var daemonFlagRoles = map[string]flagRole{
	"db":       {flag: "db", sample: "/tmp/spawn.db", forwarded: true},
	"config":   {flag: "config", sample: "/tmp/spawn.json", forwarded: true},
	"home":     {flag: "home", sample: "/tmp/spawn-home", forwarded: true},
	"interval": {flag: "interval", sample: "900", forwarded: true},
	"noDaemon": {flag: "no-daemon", why: "the spawned process is the daemon; forwarding the opt-out would contradict it"},
}

// TestDaemonArgsCoversEveryPersistentFlag walks globalFlags field by field and
// asserts each one is classified, is really bound to a root persistent flag,
// and — when it changes daemon behaviour — reaches the spawned daemon's argv
// with its value. Flags are set by parsing a real command line, so the test
// exercises the same path a user's invocation takes.
func TestDaemonArgsCoversEveryPersistentFlag(t *testing.T) {
	// daemonArgs reads the package-level flags var; restore it for later tests.
	prev := flags
	t.Cleanup(func() { flags = prev })

	root := newRootCmd()

	var argv []string
	for field, role := range daemonFlagRoles {
		if role.flag == "" {
			t.Fatalf("globalFlags.%s is classified with no flag name", field)
		}
		if root.PersistentFlags().Lookup(role.flag) == nil {
			t.Fatalf("globalFlags.%s claims flag --%s, which the root command does not define", field, role.flag)
		}
		if !role.forwarded && role.why == "" {
			t.Fatalf("globalFlags.%s is not forwarded but carries no reason", field)
		}
		if role.sample == "" {
			argv = append(argv, "--"+role.flag)
			continue
		}
		argv = append(argv, "--"+role.flag, role.sample)
	}

	typ := reflect.TypeOf(globalFlags{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := daemonFlagRoles[name]; !ok {
			t.Errorf("globalFlags.%s is unclassified: decide whether the spawned daemon needs it, "+
				"then update daemonArgs and daemonFlagRoles", name)
		}
	}

	if err := root.PersistentFlags().Parse(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}

	got := daemonArgs(flags)
	if len(got) == 0 || got[0] != "run" {
		t.Fatalf("daemonArgs argv = %v, want it to start with \"run\"", got)
	}
	for field, role := range daemonFlagRoles {
		if !role.forwarded {
			if idx := indexOf(got, "--"+role.flag); idx >= 0 {
				t.Errorf("--%s was forwarded to the daemon but must not be (%s): %v", role.flag, role.why, got)
			}
			continue
		}
		idx := indexOf(got, "--"+role.flag)
		if idx < 0 {
			t.Errorf("globalFlags.%s (--%s) never reaches the spawned daemon: %v", field, role.flag, got)
			continue
		}
		if idx+1 >= len(got) || got[idx+1] != role.sample {
			t.Errorf("--%s forwarded without its value %q: %v", role.flag, role.sample, got)
		}
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
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

// TestRepairPrivatePermsTightensExistingInstall lays down an install the way
// pre-#25 releases did (dir 0755, DB/WAL/SHM/log 0644) and asserts the
// daemon-start repair makes everything owner-only.
func TestRepairPrivatePermsTightensExistingInstall(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil { // explicit: MkdirAll is umask-subject
		t.Fatalf("chmod dir: %v", err)
	}

	db := filepath.Join(dataDir, "usage.db")
	logPath := filepath.Join(t.TempDir(), "aiusage.log")
	files := []string{db, db + "-wal", db + "-shm", logPath}
	for _, p := range files {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatalf("chmod %s: %v", p, err)
		}
	}

	repairPrivatePerms(config.Config{DBPath: db, LogPath: logPath})

	fi, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("data dir mode = %03o, want 700", perm)
	}
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %03o, want 600", p, perm)
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

// TestRotateDaemonLog: an oversized log is renamed to .old before the daemon
// appends (so it cannot grow unbounded); a small or missing log is left alone.
func TestRotateDaemonLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aiusage.log")

	// Missing file: no-op.
	rotateDaemonLog(path)
	if _, err := os.Stat(path + ".old"); err == nil {
		t.Fatalf("rotation invented a .old for a missing log")
	}

	// Small log stays in place.
	if err := os.WriteFile(path, []byte("cycle ok\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	rotateDaemonLog(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("small log was rotated away: %v", err)
	}
	if _, err := os.Stat(path + ".old"); err == nil {
		t.Fatalf("small log should not rotate")
	}

	// Oversized log rotates to .old (sparse truncate keeps the test fast).
	if err := os.Truncate(path, maxDaemonLogBytes+1); err != nil {
		t.Fatalf("grow log: %v", err)
	}
	rotateDaemonLog(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized log still present (err=%v)", err)
	}
	fi, err := os.Stat(path + ".old")
	if err != nil {
		t.Fatalf("rotated log missing: %v", err)
	}
	if fi.Size() != maxDaemonLogBytes+1 {
		t.Fatalf("rotated size = %d, want %d", fi.Size(), maxDaemonLogBytes+1)
	}

	// A second rotation replaces the previous .old.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate log: %v", err)
	}
	if err := os.Truncate(path, maxDaemonLogBytes+2); err != nil {
		t.Fatalf("regrow log: %v", err)
	}
	rotateDaemonLog(path)
	fi, err = os.Stat(path + ".old")
	if err != nil {
		t.Fatalf("second rotation: %v", err)
	}
	if fi.Size() != maxDaemonLogBytes+2 {
		t.Fatalf("second rotation did not replace .old (size=%d)", fi.Size())
	}
}
