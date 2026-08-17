package daemon

import (
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/config"
)

func TestDaemonVersionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{PIDPath: filepath.Join(dir, "aiusage.pid")}

	// No file yet → empty (caller treats as mismatch).
	if got := ReadVersion(cfg); got != "" {
		t.Fatalf("ReadVersion with no file = %q, want empty", got)
	}

	writeVersion(cfg.PIDPath, "v1.2.3")
	if got := ReadVersion(cfg); got != "v1.2.3" {
		t.Fatalf("ReadVersion = %q, want v1.2.3", got)
	}

	// The version file sits beside the pidfile.
	if want := filepath.Join(dir, "daemon.version"); versionPath(cfg.PIDPath) != want {
		t.Fatalf("versionPath = %q, want %q", versionPath(cfg.PIDPath), want)
	}

	// Empty id is not written (no misleading empty file overwriting a real one).
	writeVersion(cfg.PIDPath, "")
	if got := ReadVersion(cfg); got != "v1.2.3" {
		t.Fatalf("empty writeVersion clobbered value: got %q", got)
	}
}

// Stop on a config with no running daemon returns promptly (the lock is
// free), exercising the wait-for-lock-free path without spawning a process.
func TestStopNoDaemon(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{PIDPath: filepath.Join(dir, "aiusage.pid")}
	if err := Stop(cfg, 0); err != nil {
		t.Fatalf("Stop with no daemon = %v, want nil", err)
	}
}
