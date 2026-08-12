package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/service"
	"github.com/RandomCodeSpace/aiusage/internal/web"
)

// newSupervisor returns the systemd user-service manager this process drives.
// It is a package-level var so tests can inject a fake command runner and a
// temporary unit directory: a test suite that reached the developer's real
// service manager would install units on the machine running it.
var newSupervisor = func() *service.Manager { return &service.Manager{} }

// supervisionBudget bounds the WHOLE supervision phase of one invocation, not
// one call inside it.
//
// internal/service bounds a single systemctl; an install is a dozen of them, so
// a service manager that answers everything just slowly enough - four seconds
// each, which is a machine under load rather than a broken one - would add most
// of a minute to a command the user typed to see a number. One parent deadline
// caps the lot: when it expires, whatever supervision had reached is abandoned
// mid-sequence, the detached spawn takes over, and the user gets at most one
// line about it.
//
// It is a var only so tests can shrink it; nothing else assigns to it.
var supervisionBudget = 5 * time.Second

// supervisionContext derives the deadline that bounds every supervision call
// one invocation makes. Each attempt derives it again from the same parent, and
// context.WithTimeout keeps whichever deadline is earlier, so nesting cannot
// extend the phase.
func supervisionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, supervisionBudget)
}

// supervisionOptions builds the install plan for cfg, with args as the global
// flags to bake into both units.
//
// The caller supplies args rather than a globalFlags, because the two install
// paths disagree about them on purpose: `aiusage setup` bakes what it was given
// (see runSetup), and the automatic path bakes nothing at all (see autoArgs).
//
// The web unit is asked for unconditionally; service.Install is what refuses it
// in a build with no embedded UI, so the capability rule lives in one place.
// The address is passed explicitly rather than left to serve's default, because
// a unit file that names the port is a unit file an operator can read - and
// because service.Install checks that address before starting the unit, which
// it cannot do for a port it was never told about.
func supervisionOptions(cfg config.Config, args []string) (service.Options, error) {
	exe, err := service.SelfExec()
	if err != nil {
		return service.Options{}, fmt.Errorf("resolve executable: %w", err)
	}
	return service.Options{
		Exec:     exe,
		Args:     args,
		DataDir:  filepath.Dir(cfg.DBPath),
		StateDir: filepath.Dir(cfg.PIDPath),
		Web:      true,
		WebAddr:  web.DefaultAddr,
	}, nil
}

// autoArgs is everything the AUTOMATIC install may write into a unit file:
// nothing.
//
// The flags that would otherwise land there are already gone by the time this
// is reached - autoInstall refuses the invocation outright when --db, --config
// or --home is set - so the only one that could survive is --interval, which is
// exempt from that refusal because it names no path. Exempt from the refusal is
// not the same as fit to be permanent: `aiusage --interval 61 today` is a
// sentence about one command, and baking it would leave a supervised daemon
// polling at 61 seconds forever with nothing on the machine to explain why.
// `aiusage setup --interval 61` is the way to ask for that, because being asked
// is the difference.
//
// Returning nil rather than a filtered globalArgs is the point: a flag added to
// globalArgs later cannot leak into a unit through a path nobody re-read.
func autoArgs() []string { return nil }

// autoInstall reports whether THIS invocation may write units by itself.
//
// The trap it exists for: `aiusage --db /tmp/scratch.db report` would otherwise
// bake /tmp/scratch.db into a unit that then collects there forever, long after
// the scratch file is gone and with no hint of where the numbers went. A path
// override is a statement about one command, not about the machine, so any of
// them suppresses the automatic install entirely and the invocation falls back
// to the detached spawn, exactly as before this feature.
//
// The environment counts as an override too, and for a sharper reason than the
// flags do. config.PathEnvOverrides names the variables that moved a path
// (AIUSAGE_DB, AIUSAGE_HOME, the three XDG base directories, and HOME, which
// moves all of them at once); a systemd unit does not inherit the shell's
// environment, so an install made under one of them writes a unit whose
// ReadWritePaths name the overridden directories while its ExecStart carries no
// flags at all - a supervised daemon sandboxed for one database and collecting
// into another. Suppressing is the only answer that cannot be wrong.
//
// discoveryEnvOverrides is the same trap arriving from the other side. Those
// variables (CLAUDE_CONFIG_DIR, CODEX_HOME and the rest) move what the adapters
// READ rather than where aiusage writes, they have no flag to forward either,
// and a unit installed under one of them collects from the default locations
// while the CLI that installed it reports on the overridden ones.
//
// Neither --interval nor AIUSAGE_INTERVAL is in either set: both are clamped to
// a sane range and change nothing about which data is read or written. They are
// still not written into a unit; see autoArgs.
//
// The explicit `aiusage setup` command has the opposite rule - it bakes
// whatever it was given, because being asked is the difference.
func autoInstall(f globalFlags) bool {
	if f.db != "" || f.config != "" || f.home != "" {
		return false
	}
	return len(config.PathEnvOverrides()) == 0 && len(discoveryEnvOverrides()) == 0
}

// superviseStart hands the collection daemon to systemd when this machine and
// this invocation both allow it, and reports whether it succeeded. A false
// return means the caller owns the problem and should spawn the detached
// background process it always did.
//
// Nothing here is fatal, and nothing here is unbounded. A machine with no user
// manager returns false silently; a manager that refuses, or one that is still
// answering when the phase budget runs out, gets one warning line and the same
// fallback.
func superviseStart(ctx context.Context, cfg config.Config, f globalFlags, warn io.Writer) bool {
	m := newSupervisor()
	if !autoInstall(f) {
		return false
	}
	ctx, cancel := supervisionContext(ctx)
	defer cancel()

	if !m.Available(ctx) {
		return false
	}
	o, err := supervisionOptions(cfg, autoArgs())
	if err != nil {
		fmt.Fprintf(warn, "notice: could not install the aiusage services: %v\n", err)
		return false
	}
	res, err := m.Install(ctx, o)
	if err != nil {
		// A failure degrades to ONE line, the same one it always did. The
		// account of a half-finished install would be several, in front of a
		// command that is about to fall back and work anyway.
		fmt.Fprintf(warn, "notice: could not install the aiusage services: %v\n", err)
		return false
	}
	reportSupervision(warn, res)
	return res.Collecting
}

// reportSupervision writes an automatic operation's account, and only when that
// operation changed the machine.
//
// The automatic install runs behind a command the user typed to see a number,
// which pulls in two directions. Silence is wrong when something happened:
// installing, enabling and STARTING two long-lived services - one of them a
// network listener - is not a side effect to perform without a word, and the
// case that made this necessary is a dashboard unit written but deliberately
// not started because its port was taken, where the explanation was assembled
// and then dropped on the floor. Noise is wrong the rest of the time: the
// steady state is a dozen invocations a day finding everything already in
// place, and a notice on each of them is a notice nobody reads.
//
// service.Result.Changed draws that line, so what prints here is exactly the
// account of a run that did something: the first install, a rewrite, an enable
// or a start that had to happen. See the Result documentation for which lines
// set it. Failures are not routed here at all - they keep the single warning
// line the caller already prints before falling back.
func reportSupervision(warn io.Writer, res service.Result) {
	if !res.Changed {
		return
	}
	fmt.Fprintln(warn, "notice: aiusage changed its own systemd user services (`aiusage setup --remove` removes them):")
	// Written verbatim rather than as format strings: the lines carry paths,
	// and a path with a percent sign in it is not a format verb.
	for _, ln := range res.Lines {
		fmt.Fprintln(warn, "  "+ln)
	}
}

// superviseRestart resolves a build mismatch through the service manager, which
// is the supervised equivalent of stopping a daemon and spawning a new one.
//
// It restarts only what is already running, so it answers false whenever the
// running collector is not the unit - a detached daemon from before the units
// existed, say - leaving the stop-and-respawn path to the caller. The dashboard
// is restarted alongside, since it does not re-exec itself when the binary
// under it is replaced the way the collector does.
func superviseRestart(ctx context.Context, f globalFlags, warn io.Writer) bool {
	m := newSupervisor()
	if !autoInstall(f) {
		return false
	}
	ctx, cancel := supervisionContext(ctx)
	defer cancel()

	if !m.Available(ctx) {
		return false
	}
	res, err := m.Restart(ctx)
	if err != nil {
		fmt.Fprintf(warn, "notice: could not restart the aiusage services: %v\n", err)
		return false
	}
	reportSupervision(warn, res)
	return res.Collecting
}
