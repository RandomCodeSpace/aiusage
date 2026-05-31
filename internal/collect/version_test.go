package collect

import (
	"path/filepath"
	"testing"

	"aiusage/internal/config"
)

func TestDaemonVersionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{PIDPath: filepath.Join(dir, "aiusage.pid")}

	// No file yet → empty (caller treats as mismatch).
	if got := ReadDaemonVersion(cfg); got != "" {
		t.Fatalf("ReadDaemonVersion with no file = %q, want empty", got)
	}

	writeDaemonVersion(cfg.PIDPath, "v1.2.3")
	if got := ReadDaemonVersion(cfg); got != "v1.2.3" {
		t.Fatalf("ReadDaemonVersion = %q, want v1.2.3", got)
	}

	// The version file sits beside the pidfile.
	if want := filepath.Join(dir, "daemon.version"); daemonVersionPath(cfg.PIDPath) != want {
		t.Fatalf("daemonVersionPath = %q, want %q", daemonVersionPath(cfg.PIDPath), want)
	}

	// Empty id is not written (no misleading empty file overwriting a real one).
	writeDaemonVersion(cfg.PIDPath, "")
	if got := ReadDaemonVersion(cfg); got != "v1.2.3" {
		t.Fatalf("empty writeDaemonVersion clobbered value: got %q", got)
	}
}

// StopDaemon on a config with no running daemon returns promptly (the lock is
// free), exercising the wait-for-lock-free path without spawning a process.
func TestStopDaemonNoDaemon(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{PIDPath: filepath.Join(dir, "aiusage.pid")}
	if err := StopDaemon(cfg, 0); err != nil {
		t.Fatalf("StopDaemon with no daemon = %v, want nil", err)
	}
}
