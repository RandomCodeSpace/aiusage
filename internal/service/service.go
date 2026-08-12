// Package service installs and supervises aiusage as a pair of systemd USER
// units, so that a machine where the user has no root still gets a collector
// that starts on login, restarts after a crash and survives logout.
//
// It owns detection, unit rendering, install, enable/start, restart, removal
// and status, and nothing else: internal/cmd wires it into the CLI. The
// dependency list is deliberately short (standard library plus internal/config
// and internal/buildinfo) because supervision is a fact about the machine, not
// about what aiusage collects or reports.
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
// guarantee only: bounding a whole install - a dozen calls - is the caller's
// job, and internal/cmd does it with one parent deadline. A failure returns an
// error the caller degrades on: the CLI falls back to the detached background
// process it used before this package existed, prints at most one warning line,
// and runs the command the user actually asked for.
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
)

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

// addrProbeTimeout bounds the check for an address already in use. It probes
// loopback in the common case, where a connection either lands immediately or
// is refused immediately.
const addrProbeTimeout = 300 * time.Millisecond

// unitFileMode is the mode of a written unit file. Units are configuration, not
// secrets, and they are read by the user's own service manager.
const unitFileMode = 0o644

// Runner executes one supervision command and returns its combined output.
// It is the injection seam: tests supply a fake so no test can reach a real
// systemd, and the production path uses execRunner.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Manager drives the systemd user manager for this user.
//
// The zero value is the production manager: the real systemctl, the XDG unit
// directory and the default timeout.
type Manager struct {
	// UnitDir overrides where unit files are written. Empty means DefaultUnitDir.
	UnitDir string
	// Run overrides how commands are executed. Empty means the real systemctl
	// and loginctl.
	Run Runner
	// Dial overrides the check for an address already in use. Empty means a real
	// TCP probe. It is the second injection seam, and it exists for the same
	// reason the first one does: a test that dialled the developer's machine
	// would answer differently depending on what that machine happens to be
	// running.
	Dial func(addr string) bool
	// Timeout overrides the per-command timeout. Zero means DefaultTimeout.
	Timeout time.Duration
}

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
	// StateKnown reports that Enabled and Active are answers rather than the
	// zero value. The service manager is asked under a deadline, and a caller
	// that printed "inactive, not enabled" about a unit it never got a word
	// about would be inventing a diagnosis. It is meaningless when the unit is
	// not installed: nothing is asked about a unit with no file, and its
	// absence is the whole answer.
	StateKnown bool
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
	// Lines that explain something which did NOT happen - a dashboard skipped
	// because its port is taken, a linger refusal, a unit that could not be
	// written - deliberately do not set it. They repeat identically on every
	// invocation until a human intervenes, so they ride along with the install
	// that first produced them and are silent afterwards, which is the whole
	// difference between a report and a nag.
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
	body string
	// addr is the TCP address this unit binds, empty for one that binds
	// nothing. A unit that cannot bind is a restart loop, so an address already
	// answering is a reason not to start it.
	addr string
	// rewritten records that Force replaced an existing file. It is the only
	// case where a unit that is already running has to be restarted: the point
	// of rewriting an ExecStart is that the new one runs.
	rewritten bool
}

// addrInUse reports whether something is already accepting TCP connections on
// addr. An address that cannot be dialled is free as far as this is concerned:
// the question is only ever whether starting a unit would collide with a server
// that is already there.
func (m *Manager) addrInUse(addr string) bool {
	if m.Dial != nil {
		return m.Dial(addr)
	}
	c, err := net.DialTimeout("tcp", addr, addrProbeTimeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
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

// Install makes both units exist, be enabled and be running, and is safe to
// repeat: an existing unit file is left exactly as it is unless o.Force says
// otherwise, and an already enabled or already running unit is not touched.
//
// The web unit is installed only in a build that carries the embedded UI. serve
// exits 1 without it, and a unit that exits 1 under Restart=always is a restart
// loop rather than a dashboard.
//
// The returned error is the collection unit's, and only ever the collection
// unit's: a dashboard that will not start, will not bind or will not enable is
// reported in the lines and otherwise ignored, because none of that stops
// anything from being recorded. The collection unit is also installed first, so
// a dashboard that fails cannot even delay it.
func (m *Manager) Install(ctx context.Context, o Options) (Result, error) {
	var r Result
	if o.Exec == "" {
		return r, errors.New("install: the aiusage binary has no resolvable path")
	}

	dir := m.unitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, fmt.Errorf("create unit directory %s: %w", dir, err)
	}
	r.addf("unit directory: %s", dir)

	units := []unitPlan{{name: CollectUnit, body: renderCollect(o)}}
	switch {
	case o.Web && buildinfo.HasWebUI:
		units = append(units, unitPlan{name: WebUnit, body: renderWeb(o), addr: o.WebAddr})
	case o.Web:
		r.addf("skipped %s: this build has no embedded web UI", WebUnit)
	}

	wrote := false
	written := make([]unitPlan, 0, len(units))
	for _, u := range units {
		w, rewritten, err := m.writeUnit(dir, u.name, u.body, o.Force, &r)
		switch {
		case err != nil && u.name == CollectUnit:
			return r, err
		case err != nil:
			// A dashboard that cannot be written is a line, not a failure: the
			// collection unit beside it is what records data.
			r.addf("%s: %v", u.name, err)
			continue
		}
		u.rewritten = rewritten
		wrote = wrote || w
		written = append(written, u)
	}
	units = written
	if wrote {
		if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
			return r, fmt.Errorf("systemctl --user daemon-reload: %w", err)
		}
	}

	for _, u := range units {
		err := m.activate(ctx, u, &r)
		switch {
		case err != nil && u.name == CollectUnit:
			return r, err
		case err != nil:
			r.addf("%s: %v", u.name, err)
		case u.name == CollectUnit:
			r.Collecting = true
		}
	}

	m.enableLinger(ctx, &r)
	return r, nil
}

// Restart replaces the code the running units execute, which is how a build
// mismatch is resolved once supervision is in play.
//
// Only ACTIVE units are restarted. An inactive one is either something the user
// stopped on purpose or a unit file sitting beside a daemon that was started
// some other way, and starting it would put a second collector against a lock
// exactly one process may hold. Collecting reports whether the collection unit
// was the thing restarted, so a caller that got false knows supervision did not
// handle the mismatch and it still owns the problem.
func (m *Manager) Restart(ctx context.Context) (Result, error) {
	var r Result
	dir := m.unitDir()
	for _, name := range []string{CollectUnit, WebUnit} {
		if !fileExists(filepath.Join(dir, name)) || !m.isActive(ctx, name) {
			continue
		}
		if _, err := m.systemctl(ctx, "restart", name); err != nil {
			if name == CollectUnit {
				return r, fmt.Errorf("restart %s: %w", name, err)
			}
			r.addf("could not restart %s: %v", name, err)
			continue
		}
		r.change("restarted %s", name)
		if name == CollectUnit {
			r.Collecting = true
		}
	}
	return r, nil
}

// Remove stops, disables and deletes both units. It is the honest counterpart
// of an install that happens by itself: whatever aiusage wrote, aiusage can
// take back.
//
// What aiusage did not write, it refuses to take. Install is create-if-missing
// exactly because a unit file may be the user's own - written by hand before
// this feature existed, or edited since - and deleting one of those would be a
// removal of something this package never installed. The generated-unit stamp
// is the evidence, an unstamped file is named in the result along with the flag
// that overrides the refusal, and force is that flag.
//
// Linger is left alone. It is a property of the user, not of these units, and
// other services may be relying on it.
func (m *Manager) Remove(ctx context.Context, force bool) (Result, error) {
	var r Result
	dir := m.unitDir()
	removed := false
	for _, name := range []string{CollectUnit, WebUnit} {
		path := filepath.Join(dir, name)
		if !fileExists(path) {
			r.addf("%s is not installed", name)
			continue
		}
		if !force && !hasStamp(path) {
			r.addf("refusing to remove %s: it carries no %q line, so aiusage did not write it; pass --force to delete it anyway",
				path, unitStamp)
			r.Refused++
			continue
		}
		if m.isActive(ctx, name) {
			if _, err := m.systemctl(ctx, "stop", name); err != nil {
				r.addf("could not stop %s: %v", name, err)
			} else {
				r.change("stopped %s", name)
			}
		}
		if m.isEnabled(ctx, name) {
			if _, err := m.systemctl(ctx, "disable", name); err != nil {
				r.addf("could not disable %s: %v", name, err)
			} else {
				r.change("disabled %s", name)
			}
		}
		if err := os.Remove(path); err != nil {
			return r, fmt.Errorf("remove %s: %w", path, err)
		}
		r.change("removed %s", path)
		removed = true
	}
	if removed {
		if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
			r.addf("could not reload the service manager: %v", err)
		}
	}
	return r, nil
}

// Status reports both units, installed or not, in a fixed order so a diagnostic
// reads the same on every machine. A unit with no file is reported as absent
// without asking systemd about it: the file is what this package installed, and
// its absence is the answer.
//
// Every state query obeys the caller's deadline, and a unit whose queries did
// not complete inside it comes back with StateKnown false rather than with the
// zero value dressed up as an answer. The deadline is why: a diagnostic asks
// about two units, that is four calls into a manager that may be answering
// slowly, and the caller bounds the lot. What it must not do in exchange is
// print "inactive, not enabled" about a unit nobody managed to ask.
func (m *Manager) Status(ctx context.Context) []UnitStatus {
	dir := m.unitDir()
	out := make([]UnitStatus, 0, 2)
	for _, name := range []string{CollectUnit, WebUnit} {
		st := UnitStatus{Name: name, Installed: fileExists(filepath.Join(dir, name))}
		if st.Installed && ctx.Err() == nil {
			st.Enabled = m.isEnabled(ctx, name)
			st.Active = m.isActive(ctx, name)
			st.StateKnown = ctx.Err() == nil
		}
		out = append(out, st)
	}
	return out
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
	if err := os.WriteFile(path, []byte(body), unitFileMode); err != nil {
		return false, false, fmt.Errorf("write %s: %w", path, err)
	}
	if existed {
		r.change("rewrote %s", path)
	} else {
		r.change("wrote %s", path)
	}
	return true, existed, nil
}

// activate brings one unit to the state Install promises: enabled, so it starts
// with the session, and running, so it collects today. Whichever half is
// already done is left alone, which is what keeps a repeated install from being
// a repeated restart.
//
// Three departures from that, each earned:
//
// A unit whose address is already answering is installed and then left entirely
// alone - not started, and not even enabled. Something else holds that port,
// most likely a dashboard the user started by hand, and a unit that cannot bind
// under Restart=always is a restart loop until StartLimitBurst trips. Enabling
// it would only move the same fight to the next login.
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
	if !active && u.addr != "" && m.addrInUse(u.addr) {
		r.addf("%s installed but not started: %s is already in use; free that address, then run: systemctl --user enable --now %s",
			u.name, u.addr, u.name)
		return nil
	}

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

	if _, err := m.systemctl(ctx, "start", u.name); err != nil {
		if enabledHere {
			if _, derr := m.systemctl(ctx, "disable", u.name); derr == nil {
				r.change("disabled %s again: this install enabled it and it would not start", u.name)
			}
		}
		return fmt.Errorf("start %s: %w", u.name, err)
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
	if name != "" && m.lingerEnabled(ctx, name) {
		r.addf("linger already enabled for %s", name)
		return
	}
	if _, err := m.loginctl(ctx, "enable-linger"); err != nil {
		r.addf("could not enable linger (%v); the units stop when your last session ends", err)
		if name != "" {
			r.addf("an administrator can fix that with: loginctl enable-linger %s", name)
		}
		return
	}
	r.change("enabled linger: the units keep running after you log out")
}

// lingerEnabled reports whether logind already keeps this user's units alive.
// Asking first keeps a repeated install from putting a polkit prompt in front
// of a user who settled this the first time.
func (m *Manager) lingerEnabled(ctx context.Context, name string) bool {
	out, err := m.loginctl(ctx, "show-user", name, "--property=Linger")
	return err == nil && strings.Contains(strings.ToLower(out), "=yes")
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
