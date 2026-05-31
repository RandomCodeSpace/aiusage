package collect

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/RandomCodeSpace/aiusage/internal/config"
)

// DaemonStatus reports whether a collection daemon is currently running for the
// given configuration, using the SAME advisory lock the daemon takes in
// RunDaemon: a non-blocking exclusive flock on cfg.PIDPath+".lock".
//
// The lock is the single source of truth. A live daemon holds LOCK_EX for its
// whole lifetime and the kernel drops it automatically on exit (clean or crash),
// so:
//   - if we acquire the lock, no daemon is running — we release it immediately
//     and report (false, 0);
//   - if we fail to acquire it (EWOULDBLOCK), a daemon holds it — we read the
//     pid from cfg.PIDPath and report (true, pid). A pid of 0 means the pidfile
//     was unreadable/empty but the lock is held, so running is still true.
//
// DaemonStatus never blocks and never spawns. It is safe to call from the TUI
// header, doctor, and ensureDaemon.
func DaemonStatus(cfg config.Config) (running bool, pid int) {
	lockPath := cfg.PIDPath + ".lock"

	// O_RDWR (no O_CREATE): we are only probing. If the lock file does not exist
	// yet, the daemon has never run, so it is not running.
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		return false, 0
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Lock is held by a running daemon. Report its pid if readable.
		return true, readPID(cfg.PIDPath)
	}

	// We took the lock, so nobody else holds it: not running. Release at once so
	// we do not block a daemon that starts a moment later.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, 0
}

// readPID parses the integer pid written by writePID. It returns 0 on any
// read/parse failure; callers treat 0 as "unknown pid".
func readPID(pidPath string) int {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}
