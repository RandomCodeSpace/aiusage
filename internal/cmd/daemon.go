package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"aiusage/internal/buildinfo"
	"aiusage/internal/collect"
	"aiusage/internal/config"
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

var spawnDaemon = func(cfg config.Config) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	c := exec.Command(self, "run")
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

	logf, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
// CLI that manages it. A daemon with no recorded version (older build) counts as
// a mismatch and is restarted once.
func ensureDaemon(cfg config.Config) error {
	running, pid := collect.DaemonStatus(cfg)
	if running {
		if collect.ReadDaemonVersion(cfg) == buildinfo.Identity() {
			return nil
		}
		if err := stopDaemon(cfg, pid); err != nil {
			return fmt.Errorf("restart daemon after upgrade: %w", err)
		}
	}
	return spawnDaemon(cfg)
}
