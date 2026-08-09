package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/collect"
	"github.com/RandomCodeSpace/aiusage/internal/config"
)

// spawnDaemon launches a detached background collection daemon (`aiusage run`).
// It is a package-level var so tests can stub it (count calls, avoid spawning a
// real long-running process).
//
// The child is fully detached: a new session (Setsid) so it survives the parent
// exiting and is not in the foreground process group; stdin closed; stdout and
// stderr appended to cfg.LogPath. We Start (never Wait): ensureDaemon must
// return immediately and never block the foreground command.
// stopDaemon signals a running daemon to exit and waits for it to release its
// lock. It is a package-level var so tests can stub it (the real StopDaemon
// blocks on the kernel lock, which a flock-based fake holds for the whole test).
var stopDaemon = collect.StopDaemon

// daemonArgs builds the argv for the spawned `self run`.
//
// Every persistent flag that changes what the daemon does must be forwarded:
// dropped, the child resolves the default config, collects into the default DB
// or polls at the default cadence while the CLI reports on the flagged one.
// --no-daemon is the sole deliberate exception — the spawned process *is* the
// daemon, so forwarding the opt-out would contradict it. --interval is passed
// through unclamped; the child re-clamps it in loadConfig exactly as the parent
// did, so both ends land on the same value.
//
// TestDaemonArgsCoversEveryPersistentFlag enumerates globalFlags and fails on
// any field this function has not been taught about.
func daemonArgs(f globalFlags) []string {
	args := []string{"run"}
	if f.db != "" {
		args = append(args, "--db", f.db)
	}
	if f.config != "" {
		args = append(args, "--config", f.config)
	}
	if f.home != "" {
		args = append(args, "--home", f.home)
	}
	if f.interval > 0 {
		args = append(args, "--interval", strconv.Itoa(f.interval))
	}
	return args
}

// maxDaemonLogBytes caps the daemon log: persistent per-source errors repeat
// every cycle and would otherwise grow it without bound.
const maxDaemonLogBytes = 10 << 20

// rotateDaemonLog renames an oversized log to <path>.old before the daemon
// appends to it, replacing any previous rotation. Best-effort: a failed stat
// or rename just leaves the current log in place.
func rotateDaemonLog(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= maxDaemonLogBytes {
		return
	}
	_ = os.Rename(path, path+".old")
}

var spawnDaemon = func(cfg config.Config) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	c := exec.Command(self, daemonArgs(flags)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdin = nil

	// On first run the per-user state dir (~/.local/state/aiusage) does not exist
	// yet. The daemon's own acquireLock would create it, but that runs in the
	// child AFTER we open the log here, so create the parent ourselves first.
	if dir := filepath.Dir(cfg.LogPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create daemon log dir %s: %w", dir, err)
		}
	}

	rotateDaemonLog(cfg.LogPath)

	// 0600: cycle logs can echo adapter errors that include source paths.
	logf, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log %s: %w", cfg.LogPath, err)
	}
	// The child inherits the fd; the parent's copy is closed after Start.
	defer logf.Close()
	c.Stdout = logf
	c.Stderr = logf

	if err := c.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Do NOT Wait: the daemon runs independently. Release our handle on the
	// child so its exit is reaped by init once we exit, not left as a zombie.
	if c.Process != nil {
		_ = c.Process.Release()
	}
	return nil
}

// ensureDaemon makes sure a collection daemon is running for cfg, spawning a
// detached one if not.
//
// Singleton + self-heal both reduce to the same flock check (collect.DaemonStatus):
//   - if a daemon is running, the lock is held -> DaemonStatus reports running
//     -> we do nothing (no second daemon);
//   - if no daemon is running (never started, or crashed/killed so the kernel
//     dropped its lock), DaemonStatus reports not-running -> we spawn a fresh
//     one. A crashed daemon's stale pidfile is harmless: the freed lock is what
//     matters, and the new daemon overwrites the pidfile and re-takes the lock.
//
// Catchup is inherent: RunDaemon runs an immediate first RunCycle on startup,
// so a freshly (re)spawned daemon backfills any gap before its first tick.
//
// Version sync: if a daemon is running but was built from a different binary
// than this CLI (detected via buildinfo.Identity vs the recorded daemon.version),
// it is stopped and respawned so the collector always runs the same code as the
// CLI that manages it — except for dev-stamp identities, which only get a notice
// on warn (see restartOnMismatch). A daemon with no recorded version (older
// build) counts as a mismatch and is restarted once.
func ensureDaemon(cfg config.Config, warn io.Writer) error {
	running, pid := collect.DaemonStatus(cfg)
	if !running {
		return spawnDaemon(cfg)
	}
	recorded := collect.ReadDaemonVersion(cfg)
	self := buildinfo.Identity()
	if recorded == self {
		return nil
	}
	if !restartOnMismatch(recorded, self) {
		// The old daemon holds the flock, so a bare `aiusage run` cannot
		// replace it — the kill step is part of the advice. pid 0 means the
		// pidfile was unreadable; `kill 0` signals the caller's own process
		// group, so never advise it.
		if pid > 0 {
			fmt.Fprintf(warn, "notice: daemon build %s differs from CLI build %s; dev builds are not auto-restarted (kill %d, then run `aiusage run` to replace it)\n", recorded, self, pid)
		} else {
			fmt.Fprintf(warn, "notice: daemon build %s differs from CLI build %s; dev builds are not auto-restarted (stop the process holding %s.lock, then run `aiusage run` to replace it)\n", recorded, self, cfg.PIDPath)
		}
		return nil
	}
	if err := stopDaemon(cfg, pid); err != nil {
		if pid > 0 {
			return fmt.Errorf("daemon (pid %d, build %s) still running and collecting with the old build — stop it manually (kill %d): %w", pid, recorded, pid, err)
		}
		return fmt.Errorf("daemon (build %s) still running and collecting with the old build — stop the process holding %s.lock: %w", recorded, cfg.PIDPath, err)
	}
	return spawnDaemon(cfg)
}

// restartOnMismatch decides whether an identity mismatch between the recorded
// daemon build and this CLI warrants an automatic stop+respawn.
//
// Release identities restart: an upgraded install must not leave an old
// collector running. Dev identities ("dev", or the dev-<size>-<mtime>
// executable stamp) never auto-restart: `go run` produces a fresh temp binary
// every time, so acting on those mismatches flaps the daemon on each
// invocation — a synchronous stop of up to 3s inside PersistentPreRunE plus an
// immediate full collection cycle per respawn. A dev CLI meeting a release
// daemon is left alone for the same reason (and a go-run temp executable is a
// poor thing to respawn from: the path vanishes when the parent exits). An
// unrecorded version ("") predates version stamping and restarts once so the
// replacement daemon records one.
func restartOnMismatch(recorded, self string) bool {
	if recorded == "" {
		return true
	}
	return !isDevIdentity(recorded) && !isDevIdentity(self)
}

// isDevIdentity reports whether id is an unstamped build identity: the literal
// "dev" default or the dev-<size>-<mtime> executable fallback stamp.
func isDevIdentity(id string) bool {
	return id == "dev" || strings.HasPrefix(id, "dev-")
}
