package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aiusage/internal/config"
)

// version.go tracks the build identity of the running daemon so the CLI can keep
// the collector in lockstep with itself: when the CLI's identity differs from the
// recorded daemon identity, the CLI stops and respawns the daemon. The identity
// string is opaque to this package (the CLI supplies it via DaemonOptions.Version)
// — collect only persists, reads, and compares it.

// daemonVersionPath is the file recording the running daemon's build identity. It
// sits beside the pidfile in the XDG state dir.
func daemonVersionPath(pidPath string) string {
	return filepath.Join(filepath.Dir(pidPath), "daemon.version")
}

// writeDaemonVersion records the daemon's build identity (best-effort). Empty id
// is skipped so an un-stamped build doesn't leave a misleading empty file.
func writeDaemonVersion(pidPath, id string) {
	if id == "" {
		return
	}
	if dir := filepath.Dir(pidPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	_ = os.WriteFile(daemonVersionPath(pidPath), []byte(id), 0o644)
}

// WriteDaemonVersion records id as the daemon build identity for cfg. Exported so
// callers/tests can stamp the version the way RunDaemon does.
func WriteDaemonVersion(cfg config.Config, id string) { writeDaemonVersion(cfg.PIDPath, id) }

// ReadDaemonVersion returns the recorded daemon build identity for cfg, or "" if
// none was recorded (treated by the caller as a mismatch → restart).
func ReadDaemonVersion(cfg config.Config) string {
	data, err := os.ReadFile(daemonVersionPath(cfg.PIDPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// StopDaemon signals the daemon (pid) to terminate and waits up to ~3s for it to
// release its advisory lock — i.e. for the process to actually exit. Returns nil
// once the lock is free, or an error if the daemon did not stop in time.
func StopDaemon(cfg config.Config, pid int) error {
	if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if running, _ := DaemonStatus(cfg); !running {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) did not exit within timeout", pid)
}
