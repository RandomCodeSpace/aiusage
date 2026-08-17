// Package daemon is how THIS MACHINE supervises a collector: the pidfile and
// its advisory lock, the recorded build identity that keeps CLI and daemon in
// lockstep, the stop-and-wait, and the self-update watch on the executable.
//
// None of that is a library concern (issue #72, decision 3). Package collect
// exports RunOnce and Run and nothing else: a consumer embedding this project
// wants a collection pass, not a pidfile in a directory it did not choose, a
// version stamp in a format only this CLI reads, or a SIGTERM aimed at a pid.
// So the loop that a consumer would want is public and the machinery around it
// is here, one level below cmd, composing collect the way any other consumer
// would.
//
// It deliberately runs its OWN ticker loop rather than calling collect.Run:
// the tick checks whether the executable was replaced BEFORE it spends another
// cycle on superseded code, and returns ErrBinaryReplaced so the caller can exec
// the new build. That is a lifecycle decision taken between ticks, which a plain
// loop has no way to express.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/collect"
)

// Options configures Run.
type Options struct {
	Interval time.Duration  // poll interval; clamped to a sane minimum
	PIDPath  string         // path to write the pidfile; lock is PIDPath+".lock"
	Version  string         // build identity recorded for CLI/daemon version sync
	Logger   *log.Logger    // optional; defaults to log.Default()
	Pricer   collect.Pricer // optional; stamps cost on every new event
	NoRaw    bool           // config privacy.no_raw: store no audit payloads
	// ExecPath is the daemon's own executable, watched for replacement so an
	// upgrade takes effect without waiting for someone to run a command. Empty
	// disables the watch. Resolve it at startup: once the file has been replaced
	// the running process can no longer name it (Linux reports the original path
	// with a " (deleted)" suffix).
	ExecPath string
}

// minInterval guards against a pathological tight loop if a caller passes a
// zero/negative interval. The config layer clamps to [60,1800]s; this is only a
// last-resort floor.
const minInterval = time.Second

// Run runs collection cycles until ctx is cancelled. It enforces a single
// running instance per pidfile using an advisory flock on PIDPath+".lock":
// a second daemon sharing the same pidfile fails fast rather than double-polling.
//
// Lifecycle: acquire lock -> write pid -> run one cycle immediately -> tick every
// Interval. On ctx cancellation the in-flight cycle is allowed to finish, the
// pidfile is removed, and the lock released. Per-cycle stats are logged.
func Run(ctx context.Context, reg *adapter.Registry, st collect.Store, dc adapter.DiscoverConfig, opt Options) error {
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
	writeVersion(opt.PIDPath, opt.Version)
	defer os.Remove(versionPath(opt.PIDPath))

	logger.Printf("aiusage daemon started: pid=%d interval=%s pidfile=%s", os.Getpid(), interval, opt.PIDPath)

	cycleOpts := []collect.Option{collect.WithPricer(opt.Pricer)}
	if opt.NoRaw {
		cycleOpts = append(cycleOpts, collect.WithoutRaw())
	}

	runOne := func() {
		// Cancellation is observed inside RunOnce; an aborted cycle returns the
		// ctx error which we log without treating it as a fatal collection fault.
		stats, err := collect.RunOnce(ctx, reg, st, dc, cycleOpts...)
		if err != nil && ctx.Err() == nil {
			logger.Printf("cycle error: %v", err)
		}
		// A truncated cycle's counts are partial; say so rather than print
		// them in the shape of a completed cycle.
		label := "cycle"
		if stats.Canceled {
			label = "cycle canceled (partial counts)"
		}
		if stats.RollupRebuilt {
			logger.Printf("rebuilt the derived rollup from the ledger")
		}
		logger.Printf("%s: adapters=%d sources=%d seen=%d inserted=%d activity=%d snapshots=%d errors=%d",
			label, stats.Adapters, stats.Sources, stats.EventsSeen, stats.EventsInserted,
			stats.ActivityInserted, stats.Snapshots, len(stats.Errors))
		for _, e := range stats.Errors {
			logger.Printf("  - %s", e)
		}
	}

	// Immediate first cycle, then on the ticker.
	runOne()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// The executable as it was when this daemon started. An upgrade replaces
	// those bytes, and the tick below notices before it spends another cycle
	// running superseded code.
	started, watching := stampExecutable(opt.ExecPath)

	for {
		select {
		case <-ctx.Done():
			logger.Printf("aiusage daemon stopping: %v", ctx.Err())
			return nil
		case <-ticker.C:
			// Checked BEFORE the cycle: the caller execs the new binary, whose
			// own startup cycle then runs immediately, so the upgrade costs no
			// collection. Returning here also runs the deferred lock release and
			// pidfile removal before the exec — briefly leaving no daemon
			// registered, during which a CLI could spawn one. That is a race
			// between two processes for the same flock, which the flock settles:
			// the loser fails fast and exactly one collector survives.
			if watching {
				if now, ok := stampExecutable(opt.ExecPath); ok && !now.same(started) {
					logger.Printf("aiusage daemon: %s was replaced; restarting into the new build", opt.ExecPath)
					return ErrBinaryReplaced
				}
			}
			runOne()
		}
	}
}

// AcquireCollectionLock takes the same advisory lock Run holds, for a
// one-shot cycle (`once`). Without it a one-shot races the daemon: both read
// the same aggregate baseline and insert the same delta under different
// nano-timestamped dedup keys, doubling the count. Contention fails fast with
// an actionable message.
//
// While held, the holder is indistinguishable from a running daemon to
// cmd.ensureDaemon (same flock), so the one-shot stamps its pid and build
// identity the way Run does: with no recorded version a concurrent CLI
// hits the forced-restart path — a stop that stalls up to 3s and a SIGTERM
// aimed at whatever stale pid the pidfile holds. The returned release func
// removes both stamps and must be called when the cycle finishes.
func AcquireCollectionLock(pidPath, version string) (func(), error) {
	lock, err := acquireLock(pidPath)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("the aiusage daemon is already collecting (lock held on %s.lock); skipping this run to avoid double counting", pidPath)
		}
		return nil, err
	}
	_ = writePID(pidPath) // best-effort: the version stamp below averts the restart path
	writeVersion(pidPath, version)
	quiet := log.New(io.Discard, "", 0)
	return func() {
		os.Remove(versionPath(pidPath))
		removePID(pidPath, quiet)
		lock.release(quiet)
	}, nil
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
