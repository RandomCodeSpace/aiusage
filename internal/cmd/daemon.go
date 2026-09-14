package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
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
// lock. It is a package-level var so tests can stub it (the real daemon.Stop
// blocks on the kernel lock, which a flock-based fake holds for the whole test).
var stopDaemon = daemon.Stop

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
// --config is absolutized first. A relative one means "relative to the shell
// the user is standing in", and the child does not inherit that: it keeps the
// CWD it was spawned with. Forwarded verbatim it would resolve to a different
// file - and config.Load anchors every relative path IN that file to the
// directory holding it, so the two processes would disagree about the database
// as well. Worse, mergeFile treats a missing config as "use the defaults", so
// the miss is silent: the daemon would collect into the default DB while the
// CLI reports on the configured one, with no error at either end.
//
// TestDaemonArgsCoversEveryPersistentFlag enumerates globalFlags and fails on
// any field this function has not been taught about.
func daemonArgs(f globalFlags) []string {
	return append([]string{"run"}, globalArgs(f)...)
}

// globalArgs renders the persistent flags that must follow aiusage into another
// process: the spawned daemon above, and the systemd units internal/service
// writes. Both need the same set for the same reason - a child that resolves
// its own defaults while the CLI reports on an override is a silent
// disagreement about which database is the real one.
func globalArgs(f globalFlags) []string {
	var args []string
	if f.db != "" {
		args = append(args, "--db", absPath(f.db))
	}
	if f.config != "" {
		args = append(args, "--config", absPath(f.config))
	}
	if f.home != "" {
		args = append(args, "--home", absPath(f.home))
	}
	if f.interval > 0 {
		args = append(args, "--interval", strconv.Itoa(f.interval))
	}
	return args
}

// absPath resolves p against the current working directory. A path that cannot
// be resolved (Getwd failed) is returned unchanged: forwarding the user's own
// spelling is no worse than what we had, and refusing to spawn a daemon over it
// would be.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func resolveGlobalPaths(f globalFlags) (globalFlags, error) {
	for name, p := range map[string]*string{"db": &f.db, "home": &f.home, "config": &f.config} {
		if *p == "" {
			continue
		}
		absolute, err := filepath.Abs(*p)
		if err != nil {
			return f, fmt.Errorf("resolve --%s: %w", name, err)
		}
		*p = absolute
	}
	return f, nil
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

	daemon.RotateLog(cfg.LogPath)

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
// Singleton + self-heal both reduce to the same flock check (daemon.Status):
//   - if a daemon is running, the lock is held -> daemon.Status reports running
//     -> we do nothing (no second daemon);
//   - if no daemon is running (never started, or crashed/killed so the kernel
//     dropped its lock), daemon.Status reports not-running -> we spawn a fresh
//     one. A crashed daemon's stale pidfile is harmless: the freed lock is what
//     matters, and the new daemon overwrites the pidfile and re-takes the lock.
//
// Catchup is inherent: daemon.Run runs an immediate first RunOnce on startup,
// so a freshly (re)spawned daemon backfills any gap before its first tick.
//
// Version sync: if a daemon is running but was built from a different binary
// than this CLI (detected via buildinfo.Identity vs the recorded daemon.version),
// it is stopped and respawned so the collector always runs the same code as the
// CLI that manages it — except for dev-stamp identities, which only get a notice
// on warn (see restartOnMismatch). A daemon with no recorded version (older
// build) counts as a mismatch and is restarted once.
//
// Supervision (internal/service) is tried first at both points where this
// function would otherwise start a process: systemd owns a daemon far better
// than a detached spawn does, since it restarts one that dies and starts one at
// login. Every supervision step is allowed to decline - no user manager, a path
// override that must not be baked into a unit, a manager that refuses - and
// each declining path lands back on exactly the spawn this function performed
// before the feature existed.
//
// All of that happens in front of a command the user is waiting on, so the
// whole of it shares one deadline (supervisionBudget), derived here and not per
// attempt: the mismatch path talks to the service manager twice, and two
// budgets would be two waits.
func ensureDaemon(ctx context.Context, cfg config.Config, warn io.Writer) error {
	ctx, cancel := supervisionContext(ctx)
	defer cancel()

	requested := cfg
	previous, legacy, err := legacyCollector(ctx, cfg)
	if err != nil {
		return err
	}
	if legacy {
		cfg = previous
		fmt.Fprintf(warn, "notice: reusing legacy collector state at %s for %s\n", cfg.PIDPath, cfg.DBPath)
	}
	running, pid := daemon.Status(cfg)
	if !running {
		if superviseStart(ctx, cfg, flags, warn) {
			return nil
		}
		return spawnDaemon(cfg)
	}
	recorded := daemon.ReadVersion(cfg)
	self := buildinfo.Identity()
	// Normalised, not verbatim: the same release installed two ways spells its
	// version differently (GoReleaser strips the leading v, the module version
	// keeps it), and comparing the spellings would restart the daemon on every
	// single invocation. A capability suffix an older binary stamped is NOT
	// normalised away - that daemon is a different build.
	if buildinfo.SameIdentity(recorded, self) {
		if legacy {
			return nil
		}
		return adoptCollector(ctx, cfg, pid, warn)
	}
	if !restartOnMismatch(recorded, self, collectorExecutable(pid) == selfPath()) {
		// The old daemon holds the flock, so a bare `aiusage run` cannot
		// replace it — the kill step is part of the advice. pid 0 means the
		// pidfile was unreadable; `kill 0` signals the caller's own process
		// group, so never advise it.
		if pid > 0 {
			fmt.Fprintf(warn, "notice: daemon build %s differs from CLI build %s; a dev build is not auto-restarted unless the collector runs this same executable (kill %d, then run `aiusage run` to replace it)\n", recorded, self, pid)
		} else {
			fmt.Fprintf(warn, "notice: daemon build %s differs from CLI build %s; a dev build is not auto-restarted unless the collector runs this same executable (stop the process holding %s.lock, then run `aiusage run` to replace it)\n", recorded, self, cfg.PIDPath)
		}
		return nil
	}
	m := newSupervisor()
	native, err := collectorOwner(ctx, m, pid)
	if err != nil {
		return err
	}
	if native && !legacy {
		res, err := m.Restart(ctx)
		if err != nil {
			return err
		}
		if !res.Collecting {
			return fmt.Errorf("native collector pid %d was not restarted", pid)
		}
		reportSupervision(warn, res)
		return nil
	}
	if native {
		stopped, err := m.StopCollection(ctx)
		if err != nil {
			return err
		}
		if !stopped {
			return fmt.Errorf("native legacy collector pid %d was not stopped", pid)
		}
		// Restart the recorded native mode. The new binary derives its PID path.
		if err := waitCollectorRelease(ctx, cfg); err != nil {
			return err
		}
		return m.StartCollection(ctx)
	}
	if err := stopDaemon(cfg, pid); err != nil {
		return fmt.Errorf("stop detached collector pid %d: %w", pid, err)
	}
	// The replacement goes to the supervisor first, exactly as a cold start
	// does: a detached collector is the fallback, not the steady state, and a
	// restart is the moment to stop being in it.
	if superviseStart(ctx, requested, flags, warn) {
		return nil
	}
	return spawnDaemon(requested)
}

// adoptCollector hands a detached collector back to the native supervisor when
// the collection unit is sitting in failed state beside it.
//
// The state this repairs is one the fallback itself produces: the CLI spawned
// a detached collector while the user manager was unreachable (over ssh, say),
// and the unit, started later by a login or a restart, lost the ledger lock to
// it enough times to trip its start rate limit. From then on doctor shows a
// failed unit beside a working detached daemon, and nothing moves — the
// identities match, so version sync never fires, and the lock is held, so the
// cold-start path never runs. Collection continues, unsupervised, until a
// version bump.
//
// The guard is AGE: the failure must be older than adoptionBackoff. A unit can
// also be failed for its own reasons — a stale ReadWritePaths after the
// database moved, a binary that is not where ExecStart says — and adopting
// that one stops a working collector, watches the unit fail again and spawns
// a fresh collector. Without the guard the next command would find the same
// shape and do it all again; with it, the failure that attempt produced is
// fresh, and the shape is left alone until the window has passed. So a broken
// unit costs one bounce and one notice per window, which is also how it gets
// noticed, and a failure that is merely recent waits the same window. The two
// timestamps are the same clock, CLOCK_MONOTONIC: systemd's for when the unit
// entered failed state, and monotonicNow for the present, so a suspend moves
// neither against the other. The order of events was considered and is not
// usable — the holder that CAUSED the failure is routinely gone by the time
// anyone looks, replaced by a later fallback that postdates it.
//
// Every decline is silent and costs one systemctl show. The path-override and
// environment rules that suppress the automatic install suppress this too, for
// the same reason: a unit the CLI must not install is a unit it must not hand
// a collector to.
func adoptCollector(ctx context.Context, cfg config.Config, pid int, warn io.Writer) error {
	if !autoInstall(flags) {
		return nil
	}
	m := newSupervisor()
	failedAt, failed, known := m.CollectorFailure(ctx)
	if !known || !failed {
		return nil
	}
	now, ok := monotonicNow()
	if !ok || now-failedAt < adoptionBackoff {
		return nil
	}
	native, err := collectorOwner(ctx, m, pid)
	if err != nil || native {
		return err
	}
	if err := stopDaemon(cfg, pid); err != nil {
		return fmt.Errorf("stop detached collector pid %d: %w", pid, err)
	}
	if superviseStart(ctx, cfg, flags, warn) {
		return nil
	}
	return spawnDaemon(cfg)
}

// adoptionBackoff is how old a unit's failure must be before adoptCollector
// acts on it, and therefore the most often a unit that cannot run costs a
// working detached collector a restart.
const adoptionBackoff = 10 * time.Minute

// collectorExecutable resolves the executable file a collector process is
// running, for the dev-build half of the restart policy. A replaced binary
// keeps its path with a " (deleted)" suffix, which is stripped: the point is
// whether it is the file this CLI was started from, not whether that file
// still has the same bytes. Empty on any failure and on platforms without
// procfs, which restartOnMismatch reads as "not the same executable".
var collectorExecutable = func(pid int) string {
	if pid <= 0 {
		return ""
	}
	exe, err := os.Readlink(filepath.Join(collectorProcRoot, strconv.Itoa(pid), "exe"))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(exe, " (deleted)")
}

// restartOnMismatch decides whether an identity mismatch between the recorded
// daemon build and this CLI warrants an automatic stop+respawn.
//
// Release identities restart: an upgraded install must not leave an old
// collector running. A mismatch involving a dev identity ("dev", or the
// dev-<size>-<mtime> executable stamp) restarts only when the collector runs
// the SAME executable file this CLI does — a local build copied over the
// installed binary, which is the dev workflow that wants sync. It is refused
// otherwise, because `go run` produces a fresh temp binary every time: acting
// on those mismatches would flap the daemon on each invocation — a synchronous
// stop of up to 3s inside PersistentPreRunE plus an immediate full collection
// cycle per respawn — and a go-run temp executable is a poor thing to respawn
// from besides, since the path vanishes when the parent exits. A temp binary
// is never the collector's file, so the executable test excludes it by
// construction. An unrecorded version ("") predates version stamping and
// restarts once so the replacement daemon records one.
func restartOnMismatch(recorded, self string, sameExecutable bool) bool {
	if recorded == "" {
		return true
	}
	if !isDevIdentity(recorded) && !isDevIdentity(self) {
		return true
	}
	return sameExecutable
}

// isDevIdentity reports whether id is an unstamped build identity: the literal
// "dev" default or the dev-<size>-<mtime> executable fallback stamp. It
// classifies the VERSION part only - a capability suffix an older binary
// stamped does not turn a dev stamp into a release.
func isDevIdentity(id string) bool {
	base := buildinfo.BaseVersion(id)
	return base == "dev" || strings.HasPrefix(base, "dev-")
}
