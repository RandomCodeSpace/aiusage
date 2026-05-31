package collect

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// DaemonOptions configures RunDaemon.
type DaemonOptions struct {
	Interval time.Duration // poll interval; clamped to a sane minimum
	PIDPath  string        // path to write the pidfile; lock is PIDPath+".lock"
	Version  string        // build identity recorded for CLI/daemon version sync
	Logger   *log.Logger   // optional; defaults to log.Default()
}

// minInterval guards against a pathological tight loop if a caller passes a
// zero/negative interval. The config layer clamps to [60,1800]s; this is only a
// last-resort floor.
const minInterval = time.Second

// RunDaemon runs collection cycles until ctx is cancelled. It enforces a single
// running instance per pidfile using an advisory flock on PIDPath+".lock":
// a second daemon sharing the same pidfile fails fast rather than double-polling.
//
// Lifecycle: acquire lock -> write pid -> run one cycle immediately -> tick every
// Interval. On ctx cancellation the in-flight cycle is allowed to finish, the
// pidfile is removed, and the lock released. Per-cycle stats are logged.
func RunDaemon(ctx context.Context, reg *adapter.Registry, st store.Store, dc adapter.DiscoverConfig, opt DaemonOptions) error {
	logger := opt.Logger
	if logger == nil {
		logger = log.Default()
	}
	interval := opt.Interval
	if interval < minInterval {
		interval = minInterval
	}

	lock, err := acquireLock(opt.PIDPath)
	if err != nil {
		return err
	}
	defer lock.release(logger)

	if err := writePID(opt.PIDPath); err != nil {
		return fmt.Errorf("write pidfile %s: %w", opt.PIDPath, err)
	}
	defer removePID(opt.PIDPath, logger)

	// Record this daemon's build identity so a CLI of a different build detects
	// the mismatch and restarts us (keeping CLI + daemon in lockstep).
	writeDaemonVersion(opt.PIDPath, opt.Version)
	defer os.Remove(daemonVersionPath(opt.PIDPath))

	logger.Printf("aiusage daemon started: pid=%d interval=%s pidfile=%s", os.Getpid(), interval, opt.PIDPath)

	runOne := func() {
		// Cancellation is observed inside RunCycle; an aborted cycle returns the
		// ctx error which we log without treating it as a fatal collection fault.
		stats, err := RunCycle(ctx, reg, st, dc)
		if err != nil && ctx.Err() == nil {
			logger.Printf("cycle error: %v", err)
		}
		logger.Printf("cycle: adapters=%d sources=%d seen=%d inserted=%d snapshots=%d errors=%d",
			stats.Adapters, stats.Sources, stats.EventsSeen, stats.EventsInserted, stats.Snapshots, len(stats.Errors))
		for _, e := range stats.Errors {
			logger.Printf("  - %s", e)
		}
	}

	// Immediate first cycle, then on the ticker.
	runOne()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Printf("aiusage daemon stopping: %v", ctx.Err())
			return nil
		case <-ticker.C:
			runOne()
		}
	}
}

// fileLock holds an advisory (flock) lock on an open lock file.
type fileLock struct {
	f    *os.File
	path string
}

// acquireLock opens (creating if needed) PIDPath+".lock" and takes a
// non-blocking exclusive advisory lock. A second instance gets EWOULDBLOCK and
// a clear error. The lock is held for the daemon's lifetime and released on
// process exit (kernel drops it automatically even on crash).
func acquireLock(pidPath string) (*fileLock, error) {
	lockPath := pidPath + ".lock"
	if dir := filepath.Dir(lockPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create pidfile dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another aiusage daemon is already running (lock held on %s): %w", lockPath, err)
	}
	return &fileLock{f: f, path: lockPath}, nil
}

func (l *fileLock) release(logger *log.Logger) {
	if l == nil || l.f == nil {
		return
	}
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		logger.Printf("warning: failed to release lock %s: %v", l.path, err)
	}
	if err := l.f.Close(); err != nil {
		logger.Printf("warning: failed to close lock file %s: %v", l.path, err)
	}
}

func writePID(pidPath string) error {
	if dir := filepath.Dir(pidPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	pid := strconv.Itoa(os.Getpid())
	return os.WriteFile(pidPath, []byte(pid+"\n"), 0o644)
}

func removePID(pidPath string, logger *log.Logger) {
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		logger.Printf("warning: failed to remove pidfile %s: %v", pidPath, err)
	}
}
