// Package service installs and supervises aiusage through the platform's
// rootless per-user manager: a systemd user unit on Linux or a LaunchAgent on
// macOS. Both run the same collection command and retain detached collection as
// the caller's compatibility fallback.
//
// It owns detection, unit rendering, install, enable/start, restart, removal
// and status, and nothing else: internal/cmd wires it into the CLI. The
// dependency list is deliberately short because supervision is a fact about
// the machine, not about what aiusage collects or reports.
//
// Three properties matter more than the rest:
//
// Detection is a real probe. The systemctl binary being on PATH proves nothing
// (a container image carries it with no manager to talk to), so availability is
// systemctl plus a successful `systemctl --user show-environment`, which is what
// distinguishes a live user manager from an ssh session that never got one. The
// answer is cached for the process.
//
// Install is create-if-missing. A unit file that already exists is the user's -
// they may have edited the address, the hardening or the arguments - so install
// only ensures it is enabled and running. Rewriting is reserved for an explicit
// Force, and removal refuses any file that does not carry the generated-unit
// stamp: aiusage takes back what aiusage wrote, and nothing else.
//
// Nothing here is ever fatal, and nothing here waits without a bound. Each
// command is killed at Timeout and its pipes are force-closed WaitDelay later,
// so one call cannot outlast the two combined even when the tool leaves a
// grandchild behind holding the output pipe open. That is a per-call
// guarantee only: bounding a whole install - several calls - is the caller's
// job, and internal/cmd does it with one parent deadline. A failure returns an
// error the caller degrades on: the CLI falls back to the detached background
// process it used before this package existed, prints at most one warning line,
// and runs the command the user actually asked for.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultSettle is how long a unit started out of failed state is given before
// its state is read back. A unit that lands in failed state did so by dying a
// few times in a row, and a start from there is accepted by systemd the moment
// the process is forked; whether it lives is only knowable a little later.
// RestartSec on the generated unit is 10s, so a process that died is still
// "activating" when this elapses, and NRestarts catches a shorter one.
const DefaultSettle = 500 * time.Millisecond

// DefaultTimeout bounds each systemctl or loginctl invocation. Supervision runs
// in front of a command the user is waiting on, so a service manager that does
// not answer promptly is treated as absent rather than waited on.
const DefaultTimeout = 5 * time.Second

// waitDelay is how long a killed command's output pipes are given to close
// before they are closed for it.
//
// Killing the process is not enough to unblock reading it. CombinedOutput
// returns when every writer to the pipe is gone, and the direct child is not
// necessarily the last one: systemctl can leave a grandchild holding the
// inherited descriptor, and then the timeout kills the child while the read
// blocks forever. WaitDelay closes the pipes shortly after the context expires,
// which turns the timeout into an actual bound.
const waitDelay = 500 * time.Millisecond

// unitFileMode is the mode of a written unit file. Units are configuration, not
// secrets, and they are read by the user's own service manager.
const unitFileMode = 0o644

// Runner executes one supervision command and returns its combined output.
// It is the injection seam: tests supply a fake so no test can reach a real
// systemd, and the production path uses execRunner.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Manager drives the native per-user manager for this user.
//
// The zero value is the production manager: the runtime platform, real manager
// commands, native configuration directory and default timeout.
type Manager struct {
	// UnitDir overrides where native service files are written. Empty selects
	// DefaultUnitDir on Linux or DefaultLaunchAgentDir on macOS.
	UnitDir string
	// Run overrides how commands are executed. Empty means the real systemctl
	// and loginctl.
	Run Runner
	// Timeout overrides the per-command timeout. Zero means DefaultTimeout.
	Timeout time.Duration
	// Settle overrides how long a unit started from failed state is given to
	// prove it held before the start counts. Zero means DefaultSettle.
	Settle time.Duration
	// GOOS overrides runtime.GOOS. It exists so the Darwin backend can be
	// exercised by focused tests without touching a developer's login services.
	// Production callers leave it empty.
	GOOS string
}

const (
	PersistenceLingerConfirmed  = "linger confirmed"
	PersistenceLingerNotEnabled = "linger not enabled"
	PersistenceLingerUnknown    = "linger unknown"
	PersistenceGUILogin         = "at GUI login"
)

// UnitStatus is one unit's supervision state, as doctor reports it.
type UnitStatus struct {
	// Name is the unit file name.
	Name string
	// Installed reports that the unit file exists in the unit directory. It is
	// a fact about the filesystem, so it is always an answer.
	Installed bool
	// Enabled reports that it starts with the user session.
	Enabled bool
	// Active reports that it is running now.
	Active bool
	// Loaded distinguishes a registered launchd job from a plist that merely
	// exists. It is unused by systemd, whose active/enabled states already name
	// the manager facts users need.
	Loaded bool
	// Persistence states the platform boundary: one of the linger states above
	// on Linux, or at-GUI-login for a macOS LaunchAgent.
	Persistence string
	// StateKnown reports that Enabled and Active are answers rather than the
	// zero value. The service manager is asked under a deadline, and a caller
	// that printed "inactive, not enabled" about a unit it never got a word
	// about would be inventing a diagnosis. It is meaningless when the unit is
	// not installed: nothing is asked about a unit with no file, and its
	// absence is the whole answer.
	StateKnown bool
}

// Kind names the native per-user supervisor selected for this process.
func (m *Manager) Kind() string {
	if m.goos() == "darwin" {
		return "launchd"
	}
	return "systemd"
}

// Result is the account of one operation: plain lines to print, whether
// collection ended up supervised (which is what tells the CLI it can skip the
// detached spawn), whether anything changed, and how many unit files a removal
// refused to touch.
type Result struct {
	Lines []string

	// Changed reports that this operation altered the machine: a unit file
	// written or rewritten, an enable, a start, a restart, an enable rolled
	// back, linger turned on. A run that found everything already in place
	// leaves it false, which is what lets the automatic install report a first
	// install and stay silent for every invocation after it.
	//
	// Lines that explain something which did NOT happen - a linger refusal, a
	// unit that could not be written - deliberately do not set it. They repeat
	// identically on every invocation until a human intervenes, so they ride
	// along with the install that first produced them and are silent
	// afterwards, which is the whole difference between a report and a nag.
	Changed bool

	// Refused counts unit files a removal left in place because they carry no
	// generated-unit stamp. It is not an error - nothing failed - but it is not
	// success either, and a caller that exited zero over it would tell a script
	// the directory is clean when two files are still in it.
	Refused int

	Collecting bool
}

// addf records a line about something that was already as it should be. Use
// change for anything that altered the machine or refused to.
func (r *Result) addf(format string, a ...any) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, a...))
}

// change records a line about something this operation did, or deliberately did
// not do, and marks the result as worth reporting unasked.
func (r *Result) change(format string, a ...any) {
	r.addf(format, a...)
	r.Changed = true
}

// unitPlan is one unit to install.
type unitPlan struct {
	name string
	// rewritten records that Force replaced an existing file. It is the only
	// case where a unit that is already running has to be restarted: the point
	// of rewriting an ExecStart is that the new one runs.
	rewritten bool
}

// detection caches the availability probe for the process. Only the production
// path is cached: an injected runner is a test, and a test may legitimately
// answer differently on the next call.
var (
	detectOnce sync.Once
	detected   bool
)

// Available reports whether this process can supervise units through a systemd
// user manager.
//
// The probe is deliberately two-part. The systemctl binary alone proves
// nothing - it ships in images with no service manager behind it, and an ssh
// session may land without a user manager at all - so availability is settled
// by asking the manager a harmless question and seeing whether it answers.
// Testing XDG_RUNTIME_DIR instead would get the ssh case wrong in both
// directions.
func (m *Manager) Available(ctx context.Context) bool {
	if m.Run == nil {
		detectOnce.Do(func() { detected = m.detect(ctx) })
		return detected
	}
	return m.detect(ctx)
}

func (m *Manager) detect(ctx context.Context) bool {
	if m.goos() == "darwin" {
		return m.detectLaunchd(ctx)
	}
	if m.goos() != "linux" {
		return false
	}
	// LookPath consults the real PATH, which an injected runner has replaced;
	// with one in play the runner is the whole world and gets the only say.
	if m.Run == nil {
		if _, err := exec.LookPath("systemctl"); err != nil {
			return false
		}
	}
	_, err := m.systemctl(ctx, "show-environment")
	return err == nil
}

// Install makes the collection unit exist, be enabled and be running, and is
// safe to repeat: an existing unit file is left exactly as it is unless o.Force
// says otherwise, and an already enabled or already running unit is not touched.
func (m *Manager) Install(ctx context.Context, o Options) (Result, error) {
	if m.goos() == "darwin" {
		return m.installLaunchd(ctx, o)
	}
	var r Result
	if o.Exec == "" {
		return r, errors.New("install: the aiusage binary has no resolvable path")
	}

	if strings.ContainsFunc(o.Exec, func(r rune) bool { return r < 32 || r == 127 }) {
		return r, errors.New("install: systemd cannot represent a control character in the executable path")
	}
	for _, value := range append([]string{o.Exec, o.DataDir, o.StateDir}, o.Args...) {
		if strings.ContainsRune(value, 0) {
			return r, errors.New("install: unit value contains an unrepresentable NUL byte")
		}
	}
	dir := m.unitDir()
	dirExisted := fileExists(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, fmt.Errorf("create unit directory %s: %w", dir, err)
	}
	r.addf("unit directory: %s", dir)

	u := unitPlan{name: CollectUnit}
	path := filepath.Join(dir, u.name)
	existed := fileExists(path)
	wasEnabled, enabledKnown := m.enabledState(ctx, u.name)
	wasActive, activeKnown := m.activeState(ctx, u.name)
	if !enabledKnown || !activeKnown {
		if !dirExisted {
			_ = os.Remove(dir)
		}
		if ctx.Err() != nil {
			return r, fmt.Errorf("inspect %s before installation: supervision deadline: %w", u.name, ctx.Err())
		}
		return r, fmt.Errorf("inspect %s before installation: service state is unknown", u.name)
	}
	var original []byte
	originalMode := os.FileMode(unitFileMode)
	if existed && o.Force {
		var err error
		original, err = os.ReadFile(path)
		if err != nil {
			return r, fmt.Errorf("read %s before replacement: %w", path, err)
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			originalMode = fi.Mode().Perm()
		}
	}
	wrote, rewritten, err := m.writeUnit(dir, u.name, renderCollect(o), o.Force, &r)
	if err != nil {
		if !dirExisted {
			_ = os.Remove(dir)
		}
		return r, err
	}
	u.rewritten = rewritten
	if wrote {
		if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
			cause := fmt.Errorf("systemctl --user daemon-reload: %w", err)
			return r, m.withSystemdRollback(ctx, cause, path, wrote, existed, original, originalMode,
				wasEnabled, wasActive, dir, dirExisted)
		}
	}

	if err := m.activate(ctx, u, &r); err != nil {
		return r, m.withSystemdRollback(ctx, err, path, wrote, existed, original, originalMode,
			wasEnabled, wasActive, dir, dirExisted)
	}
	r.Collecting = true

	m.enableLinger(ctx, &r)
	return r, nil
}

// Restart replaces the code the running unit executes, which is how a build
// mismatch is resolved once supervision is in play.
//
// Only an ACTIVE unit is restarted. An inactive one is either something the user
// stopped on purpose or a unit file sitting beside a daemon that was started
// some other way, and starting it would put a second collector against a lock
// exactly one process may hold. Collecting reports whether the collection unit
// was the thing restarted, so a caller that got false knows supervision did not
// handle the mismatch and it still owns the problem.
func (m *Manager) Restart(ctx context.Context) (Result, error) {
	if m.goos() == "darwin" {
		return m.restartLaunchd(ctx)
	}
	var r Result
	if !fileExists(filepath.Join(m.unitDir(), CollectUnit)) || !m.isActive(ctx, CollectUnit) {
		return r, nil
	}
	if _, err := m.systemctl(ctx, "restart", CollectUnit); err != nil {
		return r, fmt.Errorf("restart %s: %w", CollectUnit, err)
	}
	r.change("restarted %s", CollectUnit)
	r.Collecting = true
	return r, nil
}

// StopCollection stops an active generated collection unit without disabling
// it. The returned bool records whether this call stopped a running unit so a
// database maintenance command can restore exactly the prior lifecycle state.
func (m *Manager) StopCollection(ctx context.Context) (bool, error) {
	if m.goos() == "darwin" {
		return m.stopLaunchdCollection(ctx)
	}
	if !fileExists(filepath.Join(m.unitDir(), CollectUnit)) {
		return false, nil
	}
	out, stateErr := m.systemctl(ctx, "is-active", CollectUnit)
	if stateErr != nil {
		for _, inactive := range []string{"inactive", "failed", "deactivating"} {
			if hasStateLine(out, inactive) {
				return false, nil
			}
		}
		return false, fmt.Errorf("read %s active state: %w", CollectUnit, stateErr)
	}
	if !hasStateLine(out, "active") {
		return false, fmt.Errorf("read %s active state: unrecognized response %q", CollectUnit, strings.TrimSpace(out))
	}
	if _, err := m.systemctl(ctx, "stop", CollectUnit); err != nil {
		return false, fmt.Errorf("stop %s: %w", CollectUnit, err)
	}
	return true, nil
}

// StartCollection starts an installed collection unit that maintenance stopped.
// It does not enable or install anything.
func (m *Manager) StartCollection(ctx context.Context) error {
	if m.goos() == "darwin" {
		return m.startLaunchdCollection(ctx)
	}
	if !fileExists(filepath.Join(m.unitDir(), CollectUnit)) {
		return fmt.Errorf("start %s: unit is not installed", CollectUnit)
	}
	reset, err := m.resetFailed(ctx, CollectUnit, nil)
	if err != nil {
		return err
	}
	if _, err := m.systemctl(ctx, "start", CollectUnit); err != nil {
		return fmt.Errorf("start %s: %w", CollectUnit, err)
	}
	if reset {
		return m.confirmHeld(ctx, CollectUnit)
	}
	return nil
}

// Remove stops, disables and deletes the unit. It is the honest counterpart of
// an install that happens by itself: whatever aiusage wrote, aiusage can take
// back.
//
// What aiusage did not write, it refuses to take. Install is create-if-missing
// exactly because a unit file may be the user's own - written by hand before
// this feature existed, or edited since - and deleting one of those would be a
// removal of something this package never installed. The generated-unit stamp
// is the evidence, an unstamped file is named in the result along with the flag
// that overrides the refusal, and force is that flag.
//
// Linger is left alone. It is a property of the user, not of this unit, and
// other services may be relying on it.
func (m *Manager) Remove(ctx context.Context, force bool) (Result, error) {
	if m.goos() == "darwin" {
		return m.removeLaunchd(ctx, force)
	}
	var r Result
	name := CollectUnit
	path := filepath.Join(m.unitDir(), name)
	if !fileExists(path) {
		r.addf("%s is not installed", name)
		return r, nil
	}
	if !force && !hasStamp(path) {
		r.addf("refusing to remove %s: it carries no %q line, so aiusage did not write it; pass --force to delete it anyway",
			path, unitStamp)
		r.Refused++
		return r, nil
	}
	active, activeKnown := m.activeState(ctx, name)
	enabled, enabledKnown := m.enabledState(ctx, name)
	if !activeKnown || !enabledKnown {
		return r, fmt.Errorf("inspect %s before removal: service state is unknown", name)
	}
	if active {
		if _, err := m.systemctl(ctx, "stop", name); err != nil {
			return r, fmt.Errorf("stop %s before removal: %w", name, err)
		} else {
			r.change("stopped %s", name)
		}
	}
	if enabled {
		if _, err := m.systemctl(ctx, "disable", name); err != nil {
			return r, fmt.Errorf("disable %s before removal: %w", name, err)
		} else {
			r.change("disabled %s", name)
		}
	}
	if err := os.Remove(path); err != nil {
		return r, fmt.Errorf("remove %s: %w", path, err)
	}
	r.change("removed %s", path)
	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return r, fmt.Errorf("removed %s, but could not reload the service manager: %w", path, err)
	}
	return r, nil
}

// Status reports the unit, installed or not. A unit with no file is reported as
// absent without asking systemd about it: the file is what this package
// installed, and its absence is the answer.
//
// Every state query obeys the caller's deadline, and a unit whose queries did
// not complete inside it comes back with StateKnown false rather than with the
// zero value dressed up as an answer. The deadline is why: a diagnostic asks
// enabled and active of a manager that may be answering slowly, and the caller
// bounds the lot. What it must not do in exchange is print "inactive, not
// enabled" about a unit nobody managed to ask.
//
// It returns a slice because supervision is a set of units, whatever this
// version happens to install; a caller renders whatever it is handed.
func (m *Manager) Status(ctx context.Context) []UnitStatus {
	if m.goos() == "darwin" {
		return m.statusLaunchd(ctx)
	}
	st := UnitStatus{Name: CollectUnit, Installed: fileExists(filepath.Join(m.unitDir(), CollectUnit))}
	if st.Installed && ctx.Err() == nil {
		var enabledKnown, activeKnown bool
		st.Enabled, enabledKnown = m.enabledState(ctx, CollectUnit)
		st.Active, activeKnown = m.activeState(ctx, CollectUnit)
		st.StateKnown = enabledKnown && activeKnown
		st.Persistence = m.lingerState(ctx)
	}
	return []UnitStatus{st}
}

// writeUnit creates one unit file. It reports whether it wrote anything, and
// whether what it wrote replaced a file that was already there - the caller
// needs the second answer because a running unit only has to be restarted when
// its file changed under it.
//
// An existing file is otherwise kept: it may carry edits nobody asked us to
// discard.
func (m *Manager) writeUnit(dir, name, body string, force bool, r *Result) (wrote, rewritten bool, err error) {
	path := filepath.Join(dir, name)
	existed := fileExists(path)
	if !force && existed {
		r.addf("%s already present, left as it is", name)
		return false, false, nil
	}
	if err := writeAtomicFile(path, []byte(body), unitFileMode); err != nil {
		return false, false, fmt.Errorf("write %s: %w", path, err)
	}
	if existed {
		r.change("rewrote %s", path)
	} else {
		r.change("wrote %s", path)
	}
	return true, existed, nil
}

func (m *Manager) withSystemdRollback(parent context.Context, cause error, path string, wrote, existed bool,
	original []byte, originalMode os.FileMode, wasEnabled, wasActive bool, dir string, dirExisted bool,
) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), m.timeout())
	defer cancel()
	var failures []string
	if wrote {
		if existed {
			if err := writeAtomicFile(path, original, originalMode); err != nil {
				failures = append(failures, "restore prior unit: "+err.Error())
			}
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, "remove new unit: "+err.Error())
		}
		if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
			failures = append(failures, "reload restored units: "+err.Error())
		}
	}

	activeNow := m.isActive(ctx, CollectUnit)
	switch {
	case wasActive && wrote:
		verb := "restart"
		if !activeNow {
			verb = "start"
		}
		if _, err := m.systemctl(ctx, verb, CollectUnit); err != nil {
			failures = append(failures, "restore running state: "+err.Error())
		}
	case !wasActive && activeNow:
		if _, err := m.systemctl(ctx, "stop", CollectUnit); err != nil {
			failures = append(failures, "restore stopped state: "+err.Error())
		}
	}

	enabledNow := m.isEnabled(ctx, CollectUnit)
	switch {
	case wasEnabled && !enabledNow:
		if _, err := m.systemctl(ctx, "enable", CollectUnit); err != nil {
			failures = append(failures, "restore enabled state: "+err.Error())
		}
	case !wasEnabled && enabledNow:
		if _, err := m.systemctl(ctx, "disable", CollectUnit); err != nil {
			failures = append(failures, "restore disabled state: "+err.Error())
		}
	}
	if !dirExisted {
		_ = os.Remove(dir)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%w; rollback failed: %s", cause, strings.Join(failures, "; "))
	}
	return cause
}

// activate brings one unit to the state Install promises: enabled, so it starts
// with the session, and running, so it collects today. Whichever half is
// already done is left alone, which is what keeps a repeated install from being
// a repeated restart.
//
// Two departures from that, each earned:
//
// A unit whose file Force just rewrote is restarted rather than reported as
// already running. The point of rewriting an ExecStart is that the new one runs;
// saying "already running" about the old one would be true and useless.
//
// An enable this call performed is rolled back when the start then fails.
// Leaving it enabled would start the unit at the next login against the
// collection lock the fallback daemon may by then be holding.
func (m *Manager) activate(ctx context.Context, u unitPlan, r *Result) error {
	active := m.isActive(ctx, u.name)

	enabledHere := false
	if m.isEnabled(ctx, u.name) {
		r.addf("%s already enabled", u.name)
	} else if _, err := m.systemctl(ctx, "enable", u.name); err != nil {
		return fmt.Errorf("enable %s: %w", u.name, err)
	} else {
		r.change("enabled %s", u.name)
		enabledHere = true
	}

	if active {
		if !u.rewritten {
			r.addf("%s already running", u.name)
			return nil
		}
		if _, err := m.systemctl(ctx, "restart", u.name); err != nil {
			return fmt.Errorf("restart %s after rewriting its unit file: %w", u.name, err)
		}
		r.change("restarted %s so the rewritten unit file is what runs", u.name)
		return nil
	}

	reset, err := m.resetFailed(ctx, u.name, r)
	if err != nil {
		return err
	}
	if _, err := m.systemctl(ctx, "start", u.name); err != nil {
		if enabledHere {
			if _, derr := m.systemctl(ctx, "disable", u.name); derr == nil {
				r.change("disabled %s again: this install enabled it and it would not start", u.name)
			}
		}
		return fmt.Errorf("start %s: %w", u.name, err)
	}
	if reset {
		if err := m.confirmHeld(ctx, u.name); err != nil {
			if enabledHere {
				if _, derr := m.systemctl(ctx, "disable", u.name); derr == nil {
					r.change("disabled %s again: this install enabled it and it would not stay up", u.name)
				}
			}
			return err
		}
	}
	r.change("started %s", u.name)
	return nil
}

// enableLinger asks logind to keep this user's units running when no session of
// theirs is open. Without it the collector dies at logout and starts again at
// the next login, which loses nothing from the ledger (the adapters read files
// that are still there) but stops recording in the meantime.
//
// It is best-effort by design. Enabling linger goes through polkit, and on a
// machine where the user has no administrative rights it can simply be refused
// - which is the exact machine this whole feature exists for. A refusal is
// reported and the install continues.
func (m *Manager) enableLinger(ctx context.Context, r *Result) {
	name := currentUser()
	before := m.lingerStateFor(ctx, name)
	if before == PersistenceLingerConfirmed {
		r.addf("persistence: %s (starts at boot and survives logout)", PersistenceLingerConfirmed)
		return
	}
	if _, err := m.loginctl(ctx, "enable-linger"); err != nil {
		state := before
		if state != PersistenceLingerNotEnabled {
			state = PersistenceLingerUnknown
		}
		r.addf("persistence: %s (%v); the service may stop after the last logout and starts again at login", state, err)
		if name != "" {
			r.addf("an administrator can fix that with: loginctl enable-linger %s", name)
		}
		return
	}
	r.change("persistence: %s (starts at boot and survives logout)", PersistenceLingerConfirmed)
}

func (m *Manager) lingerState(ctx context.Context) string {
	return m.lingerStateFor(ctx, currentUser())
}

// lingerStateFor reports the three factual states doctor and setup distinguish.
// Asking first keeps a repeated install from putting a polkit prompt in front
// of a user who settled this the first time.
func (m *Manager) lingerStateFor(ctx context.Context, name string) string {
	if name == "" {
		return PersistenceLingerUnknown
	}
	out, err := m.loginctl(ctx, "show-user", name, "--property=Linger")
	if err != nil {
		return PersistenceLingerUnknown
	}
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "=yes"):
		return PersistenceLingerConfirmed
	case strings.Contains(lower, "=no"):
		return PersistenceLingerNotEnabled
	default:
		return PersistenceLingerUnknown
	}
}

// isEnabled reports whether the unit PERSISTENTLY starts with the user session.
// systemctl exits non-zero for every negative answer (disabled, not-found), so
// the exit status settles most of it and the state word settles the rest.
//
// The state word has to match exactly. A unit enabled with --runtime answers
// "enabled-runtime", which is enabled until the next reboot and no longer:
// treating it as enabled would leave the machine with a supervision that quietly
// stops existing, so it is not enabled here and install gives it a persistent
// enable.
func (m *Manager) isEnabled(ctx context.Context, name string) bool {
	out, err := m.systemctl(ctx, "is-enabled", name)
	return err == nil && hasStateLine(out, "enabled")
}

// isActive reports whether the unit is running now. Exact for the same reason
// isEnabled is: "activating" and "active" differ by the part that matters.
func (m *Manager) isActive(ctx context.Context, name string) bool {
	out, err := m.systemctl(ctx, "is-active", name)
	return err == nil && hasStateLine(out, "active")
}

func (m *Manager) enabledState(ctx context.Context, name string) (bool, bool) {
	return m.queryState(ctx, "is-enabled", name, "enabled", []string{"disabled", "enabled-runtime", "static", "indirect", "masked", "masked-runtime", "linked", "linked-runtime", "alias", "generated", "transient", "not-found"})
}

func (m *Manager) activeState(ctx context.Context, name string) (bool, bool) {
	// Transitional states do not prove that the collector has stopped. Treat
	// them as unknown so removal cannot skip stop and unlink a live unit.
	return m.queryState(ctx, "is-active", name, "active", []string{"inactive", "failed"})
}

func (m *Manager) queryState(ctx context.Context, verb, name, positive string, negatives []string) (bool, bool) {
	out, err := m.systemctl(ctx, verb, name)
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false, false
	}
	if err == nil && hasStateLine(out, positive) {
		return true, true
	}
	for _, state := range negatives {
		if hasStateLine(out, state) {
			return false, true
		}
	}
	return false, false
}

// resetFailed clears systemd's failed state from a unit about to be started,
// and reports whether it did.
//
// A unit that exited non-zero often enough trips its start rate limit and
// parks in "failed", where a plain start is refused with "start request
// repeated too quickly". The collection unit lands there whenever it loses
// the ledger lock to a detached collector a few times in a row, which is a
// fallback this package itself produces. reset-failed is what a start from
// that state needs first; a unit in any other state is not touched. r is
// optional and, when given, records the reset as a change.
//
// The caller owes a confirmHeld after the start that follows. Before the
// reset, a broken unit refused to start and the caller fell back to a
// detached collector that worked; after it, the same unit starts, dies, and
// would be reported as collecting with nothing behind it.
func (m *Manager) resetFailed(ctx context.Context, name string, r *Result) (bool, error) {
	out, err := m.systemctl(ctx, "is-active", name)
	if err == nil || !hasStateLine(out, "failed") {
		return false, nil
	}
	if _, err := m.systemctl(ctx, "reset-failed", name); err != nil {
		return false, fmt.Errorf("reset-failed %s: %w", name, err)
	}
	if r != nil {
		r.change("reset %s from its failed state", name)
	}
	return true, nil
}

// confirmHeld waits Settle and then requires a unit just started out of
// failed state to be active and not yet restarted. Anything else is the unit
// dying again, and is an error so the caller falls back the way it did when
// the rate limit refused the start.
func (m *Manager) confirmHeld(ctx context.Context, name string) error {
	settle := m.Settle
	if settle <= 0 {
		settle = DefaultSettle
	}
	t := time.NewTimer(settle)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return fmt.Errorf("confirm %s after start: %w", name, ctx.Err())
	}
	props, err := m.showProperties(ctx, name, "ActiveState", "NRestarts")
	if err != nil {
		return fmt.Errorf("confirm %s after start: %w", name, err)
	}
	if props["ActiveState"] == "active" && props["NRestarts"] == "0" {
		return nil
	}
	return fmt.Errorf("%s was started from failed state but did not stay up (ActiveState=%s NRestarts=%s)",
		name, props["ActiveState"], props["NRestarts"])
}

// CollectorFailure reports whether the collection unit sits in systemd's
// failed state and, if so, when it got there, as CLOCK_MONOTONIC time since
// boot (systemd's InactiveEnterTimestampMonotonic). known is false when the
// manager gave no usable answer, or on macOS, where launchd keeps no such
// state.
func (m *Manager) CollectorFailure(ctx context.Context) (failedAt time.Duration, failed, known bool) {
	if m.goos() == "darwin" {
		return 0, false, false
	}
	props, err := m.showProperties(ctx, CollectUnit, "ActiveState", "InactiveEnterTimestampMonotonic")
	if err != nil {
		return 0, false, false
	}
	if props["ActiveState"] != "failed" {
		return 0, false, true
	}
	micros, err := strconv.ParseInt(props["InactiveEnterTimestampMonotonic"], 10, 64)
	if err != nil {
		return 0, false, false
	}
	return time.Duration(micros) * time.Microsecond, true, true
}

// showProperties reads named unit properties as a map. It deliberately does
// not use --value: systemd prints properties in ITS order, not the order
// asked for (measured: `--property=ActiveState,NRestarts --value` answers
// NRestarts first), so positional parsing reads one property as another.
// Every requested key is present in the result; a missing one is an error.
func (m *Manager) showProperties(ctx context.Context, name string, keys ...string) (map[string]string, error) {
	out, err := m.systemctl(ctx, "show", name, "--property="+strings.Join(keys, ","))
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	props := make(map[string]string, len(keys))
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(ln, "=")
		if ok {
			props[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	for _, k := range keys {
		if _, ok := props[k]; !ok {
			return nil, fmt.Errorf("show %s: no %s in %q", name, k, strings.TrimSpace(out))
		}
	}
	return props, nil
}

// CollectorPID asks the running native manager which process it owns. Zero with
// known=true means it has no active collector; an unavailable answer is unknown.
func (m *Manager) CollectorPID(ctx context.Context) (pid int, known bool) {
	if m.goos() == "darwin" {
		out, err := m.launchctl(ctx, "print", m.launchTarget())
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return 0, false
			}
			for _, absent := range []string{"could not find service", "service could not be found"} {
				if strings.Contains(strings.ToLower(out), absent) {
					return 0, true
				}
			}
			return 0, false
		}
		for _, line := range strings.Split(out, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
				pid, err := strconv.Atoi(value)
				return pid, err == nil && pid > 0
			}
		}
		return 0, hasStateLine(out, "state = waiting") || hasStateLine(out, "state = not running")
	}
	out, err := m.systemctl(ctx, "show", CollectUnit, "--property=MainPID", "--value")
	if err != nil || ctx.Err() != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(out))
	return pid, err == nil && pid >= 0
}

// hasStateLine reports whether any line of a systemctl answer is exactly want.
// The scan is per line because output is combined: a warning systemctl wrote to
// stderr can arrive ahead of the state word, and the state word is still the
// answer.
func hasStateLine(out, want string) bool {
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == want {
			return true
		}
	}
	return false
}

// systemctl runs one systemctl user command. Every invocation is
// non-interactive: a password prompt in front of a report command would hang a
// terminal the user thought was printing numbers.
func (m *Manager) systemctl(ctx context.Context, args ...string) (string, error) {
	return m.command(ctx, "systemctl", append([]string{"--user", "--no-ask-password"}, args...)...)
}

// loginctl runs one loginctl command, non-interactive for the same reason.
// Enabling linger is a polkit action, and the answer on a machine where the
// user has no rights must be a refusal we can carry on from, not a prompt.
func (m *Manager) loginctl(ctx context.Context, args ...string) (string, error) {
	return m.command(ctx, "loginctl", append([]string{"--no-ask-password"}, args...)...)
}

// command runs one supervision command under the per-command timeout, folding
// the tool's first output line into any error so a caller printing one warning
// line prints a useful one. It returns within the timeout plus waitDelay; see
// execRunner for why the second term is needed to make the first one true.
//
// A command that ran out of time is reported as one, in those words. What
// CommandContext hands back is the signal it used - the caller's whole account
// of the failure ends up being "systemctl --user daemon-reload: signal: killed",
// which describes the mechanism and hides the cause, and the two deadlines in
// play here are exactly what an operator is trying to tell apart: this call
// taking too long, or the phase around it having already given up.
func (m *Manager) command(parent context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, m.timeout())
	defer cancel()

	raw, err := m.runner()(ctx, name, args...)
	out := strings.TrimSpace(string(raw))
	if err == nil {
		return out, nil
	}
	// Deadlines first: a killed command's own error names a signal, and the
	// context is the only thing that knows why the signal was sent.
	switch {
	case parent.Err() != nil:
		return out, fmt.Errorf("the supervision deadline expired: %w", parent.Err())
	case ctx.Err() != nil:
		return out, fmt.Errorf("timed out after %s: %w", m.timeout(), ctx.Err())
	case out != "":
		return out, fmt.Errorf("%w: %s", err, firstLine(out))
	}
	return out, err
}

func (m *Manager) unitDir() string {
	if m.UnitDir != "" {
		return m.UnitDir
	}
	if m.goos() == "darwin" {
		return DefaultLaunchAgentDir()
	}
	return DefaultUnitDir()
}

func (m *Manager) runner() Runner {
	if m.Run != nil {
		return m.Run
	}
	return execRunner
}

func (m *Manager) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return DefaultTimeout
}

func (m *Manager) goos() string {
	if m.GOOS != "" {
		return m.GOOS
	}
	return runtime.GOOS
}

// execRunner is the production Runner.
//
// WaitDelay is what makes the context deadline a real bound. CommandContext
// kills the direct child when the context expires, but CombinedOutput reads
// until every writer to the output pipe is gone, and the child is not
// necessarily the last one holding it: a systemctl that leaves a grandchild
// behind leaves that descriptor open, and the read then blocks with no deadline
// on it at all - a report command that never returns. WaitDelay closes the
// pipes waitDelay after the kill, so this function returns within the caller's
// timeout plus that grace whatever the tool leaves running.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = waitDelay
	return c.CombinedOutput()
}

// currentUser returns the login name, or "" when it cannot be determined. It is
// used for advice text and for the linger query, never for authorisation.
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// firstLine trims a tool's output to its first line. systemctl answers a
// refusal with a paragraph; a CLI that degrades quietly owes the user one line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
