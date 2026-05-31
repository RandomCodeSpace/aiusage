package collect

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"aiusage/internal/config"
)

// holdLock takes the same exclusive non-blocking flock the daemon takes on
// pidPath+".lock" and returns a release func. It mirrors acquireLock without
// the daemon's logging/pidfile side effects so the test can simulate a running
// daemon hermetically.
func holdLock(t *testing.T, pidPath string) func() {
	t.Helper()
	lockPath := pidPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock %s: %v", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("acquire lock %s: %v", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func TestDaemonStatus(t *testing.T) {
	tests := []struct {
		name        string
		writePID    bool
		holdLock    bool
		wantRunning bool
		wantPID     int
	}{
		{name: "no lock file at all", wantRunning: false, wantPID: 0},
		{name: "lock file exists but free", writePID: true, wantRunning: false, wantPID: 0},
		{name: "lock held and pid readable", writePID: true, holdLock: true, wantRunning: true, wantPID: 4242},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidPath := filepath.Join(dir, "aiusage.pid")
			cfg := config.Config{PIDPath: pidPath}

			if tc.writePID {
				// A free lock file exists from a prior daemon; create it so the
				// probe can open it (no O_CREATE in DaemonStatus).
				if err := os.WriteFile(pidPath+".lock", nil, 0o644); err != nil {
					t.Fatalf("seed lock file: %v", err)
				}
				if err := os.WriteFile(pidPath, []byte("4242\n"), 0o644); err != nil {
					t.Fatalf("seed pidfile: %v", err)
				}
			}

			var release func()
			if tc.holdLock {
				release = holdLock(t, pidPath)
				defer release()
			}

			running, pid := DaemonStatus(cfg)
			if running != tc.wantRunning {
				t.Errorf("running=%v, want %v", running, tc.wantRunning)
			}
			if pid != tc.wantPID {
				t.Errorf("pid=%d, want %d", pid, tc.wantPID)
			}
		})
	}
}

// TestDaemonStatusReleasesProbeLock confirms DaemonStatus does not leave the
// lock held after reporting not-running: a subsequent caller must still be able
// to take it (otherwise the probe itself would block a real daemon).
func TestDaemonStatusReleasesProbeLock(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "aiusage.pid")
	if err := os.WriteFile(pidPath+".lock", nil, 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}
	cfg := config.Config{PIDPath: pidPath}

	if running, _ := DaemonStatus(cfg); running {
		t.Fatalf("expected not-running on a free lock")
	}
	// Probe released its lock, so we can take it now.
	release := holdLock(t, pidPath)
	release()
}
