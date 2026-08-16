package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errExitOne stands in for the exit status systemctl uses to say no. Every
// negative answer from is-enabled and is-active arrives that way, so a fake
// that returned nil there would test a service manager nobody has.
var errExitOne = errors.New("exit status 1")

// fakeSystemd is a scriptable stand-in for systemctl and loginctl. Nothing in
// this package's tests may reach a real service manager: the machine running
// them has one, with the user's own units in it.
type fakeSystemd struct {
	calls   []string
	enabled map[string]bool
	// runtime holds the units enabled with --runtime, which is-enabled answers
	// for with "enabled-runtime": enabled until the next reboot and no longer.
	runtime map[string]bool
	active  map[string]bool
	linger  bool
	// fail maps a verb (start, enable, show-environment, enable-linger, ...) to
	// the error it answers with. A key of "verb unit" refuses that verb for one
	// unit only, which is how a single stubborn unit is told apart from a
	// service manager that refuses everything.
	fail map[string]error
}

func newFake() *fakeSystemd {
	return &fakeSystemd{
		enabled: map[string]bool{},
		runtime: map[string]bool{},
		active:  map[string]bool{},
		fail:    map[string]error{},
	}
}

// run implements Runner.
func (f *fakeSystemd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))

	verb, unit := verbUnit(args)
	if err := f.refusal(verb, unit); err != nil {
		return []byte(verb + " refused\nsecond line nobody should print"), err
	}

	if name == "loginctl" {
		switch verb {
		case "enable-linger":
			f.linger = true
			return nil, nil
		case "show-user":
			if f.linger {
				return []byte("Linger=yes\n"), nil
			}
			return []byte("Linger=no\n"), nil
		}
		return nil, fmt.Errorf("unexpected loginctl verb %q", verb)
	}

	switch verb {
	case "show-environment", "daemon-reload":
		return nil, nil
	case "is-enabled":
		if f.runtime[unit] {
			return []byte("enabled-runtime\n"), nil
		}
		if f.enabled[unit] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errExitOne
	case "is-active":
		if f.active[unit] {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), errExitOne
	case "enable":
		f.enabled[unit] = true
		delete(f.runtime, unit)
		return nil, nil
	case "disable":
		delete(f.enabled, unit)
		delete(f.runtime, unit)
		return nil, nil
	case "start", "restart":
		f.active[unit] = true
		return nil, nil
	case "stop":
		delete(f.active, unit)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected systemctl verb %q", verb)
}

// refusal returns the scripted error for one call: the unit-specific key first,
// then the verb-wide one.
func (f *fakeSystemd) refusal(verb, unit string) error {
	if err := f.fail[verb+" "+unit]; err != nil {
		return err
	}
	return f.fail[verb]
}

// verbUnit picks the verb and its unit argument out of a command line, skipping
// the global options the Manager always passes.
func verbUnit(args []string) (string, string) {
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			continue
		}
		rest = append(rest, a)
	}
	switch len(rest) {
	case 0:
		return "", ""
	case 1:
		return rest[0], ""
	}
	return rest[0], rest[1]
}

func (f *fakeSystemd) ran(want string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// testManager returns a Manager wired to a fake service manager and a unit
// directory under the test's temp dir. Nothing here may reach the real
// systemctl: the machine running these tests has the developer's own units in
// its user manager.
func testManager(t *testing.T) (*Manager, *fakeSystemd) {
	t.Helper()
	f := newFake()
	return &Manager{
		UnitDir: filepath.Join(t.TempDir(), "systemd", "user"),
		Run:     f.run,
	}, f
}

// testOptions returns an install plan whose paths are all inside the test.
func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Exec:     filepath.Join(dir, "bin", "aiusage"),
		DataDir:  filepath.Join(dir, "data"),
		StateDir: filepath.Join(dir, "state"),
	}
}

// TestRenderedUnitCarriesTheHardening pins the directives that make a generated
// unit equivalent to the hand-written one it replaces. A sandbox that quietly
// stopped being applied would never show up in behaviour: the daemon would keep
// collecting, with more of the filesystem writable than it needs.
func TestRenderedUnitCarriesTheHardening(t *testing.T) {
	o := testOptions(t)
	body := renderCollect(o)

	want := []string{
		// The stamp is what --remove checks before deleting a file, so a unit
		// rendered without one would be a unit aiusage could not take back.
		unitStamp,
		"Documentation=" + docURL,
		"Type=simple",
		"WorkingDirectory=%h",
		"Restart=always",
		"NoNewPrivileges=yes",
		"PrivateTmp=yes",
		"ProtectSystem=strict",
		"ProtectKernelTunables=yes",
		"ProtectControlGroups=yes",
		"RestrictSUIDSGID=yes",
		"StartLimitIntervalSec=300",
		"StartLimitBurst=5",
		"[Install]",
		"WantedBy=default.target",
		"Description=aiusage collection daemon",
		"After=network.target",
		"ExecStart=" + o.Exec + " run",
		"RestartSec=10",
		"ReadWritePaths=" + o.DataDir + " " + o.StateDir,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("unit is missing %q:\n%s", w, body)
		}
	}
	// aiusage binds no port: the unit has no address to name, and nothing here
	// may grow a subcommand flag by accident.
	for _, d := range []string{"--addr", "--no-daemon", "--allowed-hosts", "RestrictAddressFamilies"} {
		if strings.Contains(body, d) {
			t.Errorf("unit unexpectedly contains %q:\n%s", d, body)
		}
	}
}

// TestRenderedUnitForwardsGlobalFlags: an install made with overrides has to
// pass them on, or the supervised daemon collects into the default database
// while the CLI that installed it reports on another one.
func TestRenderedUnitForwardsGlobalFlags(t *testing.T) {
	o := testOptions(t)
	o.Args = []string{"--db", "/srv/usage.db", "--interval", "300"}

	collect := renderCollect(o)
	if want := "ExecStart=" + o.Exec + " run --db /srv/usage.db --interval 300\n"; !strings.Contains(collect, want) {
		t.Errorf("collect unit ExecStart missing %q:\n%s", want, collect)
	}
}

// TestQuoteProtectsUnitValues covers the two ways a path can mean something
// different to systemd than it did to the user: whitespace splits a value into
// two arguments, and a percent sign is a specifier it expands.
func TestQuoteProtectsUnitValues(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/home/dev/.local/bin/aiusage", "/home/dev/.local/bin/aiusage"},
		{"/home/My User/bin/aiusage", `"/home/My User/bin/aiusage"`},
		{"/tmp/100%/usage.db", "/tmp/100%%/usage.db"},
		{`/tmp/a"b`, `"/tmp/a\"b"`},
		{`/tmp/a\b`, `"/tmp/a\\b"`},
	}
	for _, tc := range tests {
		if got := quote(tc.in); got != tc.want {
			t.Errorf("quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInstallWritesEnablesAndStarts is the first-run path: nothing exists, so
// the unit is written, the manager is reloaded, and the unit is enabled
// (survives a reboot) and started (collects now).
func TestInstallWritesEnablesAndStarts(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)

	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Collecting {
		t.Fatalf("Install did not report collection supervised: %v", res.Lines)
	}

	assertUnitFile(t, m, CollectUnit, true)

	for _, want := range []string{"daemon-reload", "enable " + CollectUnit, "start " + CollectUnit} {
		if !f.ran(want) {
			t.Errorf("Install never ran %q; calls: %v", want, f.calls)
		}
	}
	// Every command is non-interactive: a password prompt in front of a report
	// command hangs a terminal the user thought was printing numbers.
	for _, c := range f.calls {
		if !strings.Contains(c, "--no-ask-password") {
			t.Errorf("interactive command %q", c)
		}
	}
}

// TestInstallKeepsAnExistingUnitFile is the idempotence rule: a unit that is
// already there belongs to whoever edited it last. Install may correct its
// enabled and running state, never its contents.
func TestInstallKeepsAnExistingUnitFile(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)
	const edited = "[Service]\nExecStart=/somewhere/else/aiusage run\n"
	seedUnit(t, m, CollectUnit, edited)

	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := readUnit(t, m, CollectUnit); got != edited {
		t.Fatalf("Install rewrote an existing unit:\n%s", got)
	}
	if !res.Collecting || !f.ran("start "+CollectUnit) {
		t.Errorf("Install left an existing unit unstarted: %v", res.Lines)
	}
}

// TestInstallOnlyActsOnWhatIsMissing: a unit already enabled and running is
// left completely alone, so a repeated install is not a repeated restart.
func TestInstallOnlyActsOnWhatIsMissing(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)

	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	f.calls = nil

	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !res.Collecting {
		t.Fatalf("second Install lost supervision: %v", res.Lines)
	}
	for _, unwanted := range []string{"daemon-reload", "enable ", "start ", "restart "} {
		if f.ran(unwanted) {
			t.Errorf("second Install ran %q on an already-installed machine; calls: %v", unwanted, f.calls)
		}
	}
}

// TestInstallForceRewrites: --force is the one way a unit file is replaced, and
// it reloads the manager so the replacement is what runs.
func TestInstallForceRewrites(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)
	seedUnit(t, m, CollectUnit, "[Service]\nExecStart=/somewhere/else/aiusage run\n")

	o.Force = true
	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatalf("Install --force: %v", err)
	}
	body := readUnit(t, m, CollectUnit)
	if !strings.Contains(body, "ExecStart="+o.Exec+" run") {
		t.Fatalf("--force did not rewrite the unit:\n%s", body)
	}
	if !f.ran("daemon-reload") {
		t.Errorf("--force rewrote a unit without reloading the manager; calls: %v", f.calls)
	}
}

// TestInstallEnablesAPresentButStoppedUnit covers the repair case: the file is
// there, but nothing starts it. Install fixes exactly that half.
func TestInstallEnablesAPresentButStoppedUnit(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)
	seedUnit(t, m, CollectUnit, renderCollect(o))
	f.enabled[CollectUnit] = true // enabled already; just not running

	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if f.ran("enable " + CollectUnit) {
		t.Errorf("Install re-enabled an already enabled unit; calls: %v", f.calls)
	}
	if !f.ran("start "+CollectUnit) || !res.Collecting {
		t.Errorf("Install did not start a stopped unit: %v", res.Lines)
	}
}

// TestInstallInstallsExactlyOneUnit: aiusage supervises the collector and
// nothing else. A second unit file appearing in the directory would be a
// process this package never accounted for.
func TestInstallInstallsExactlyOneUnit(t *testing.T) {
	m, _ := testManager(t)

	if _, err := m.Install(t.Context(), testOptions(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	entries, err := os.ReadDir(m.unitDir())
	if err != nil {
		t.Fatalf("read unit dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != CollectUnit {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unit directory holds %v, want only %s", names, CollectUnit)
	}
}

// TestInstallFailuresDegrade is the whole safety contract in one table: every
// way the service manager can refuse has to come back as an error the CLI can
// fall back from, never as a panic and never as a false claim that collection
// is supervised.
func TestInstallFailuresDegrade(t *testing.T) {
	tests := []struct {
		name           string
		verb           string
		wantErr        bool
		wantCollecting bool
	}{
		{name: "daemon-reload refused", verb: "daemon-reload", wantErr: true},
		{name: "enable refused", verb: "enable", wantErr: true},
		{name: "start refused", verb: "start", wantErr: true},
		// Linger is the no-root case this feature exists for: polkit refuses,
		// and the install carries on with the units it did manage to start.
		{name: "linger refused", verb: "enable-linger", wantCollecting: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, f := testManager(t)
			f.fail[tc.verb] = errExitOne

			res, err := m.Install(t.Context(), testOptions(t))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Install error = %v, wantErr %v", err, tc.wantErr)
			}
			if res.Collecting != tc.wantCollecting {
				t.Errorf("Collecting = %v, want %v (lines: %v)", res.Collecting, tc.wantCollecting, res.Lines)
			}
			if err != nil && strings.Contains(err.Error(), "nobody should print") {
				t.Errorf("error carries more than one line of tool output: %v", err)
			}
		})
	}
}

// TestInstallRefusesWithoutAnExecutablePath: a unit with no binary in its
// ExecStart is a unit that fails every start for the life of the machine.
func TestInstallRefusesWithoutAnExecutablePath(t *testing.T) {
	m, _ := testManager(t)
	o := testOptions(t)
	o.Exec = ""
	if _, err := m.Install(t.Context(), o); err == nil {
		t.Fatal("Install accepted an empty executable path")
	}
}

// TestRestartOnlyTouchesRunningUnits: restart is version sync, and an inactive
// unit is either one the user stopped or one sitting beside a daemon that was
// started another way. Starting it would put a second collector against a lock
// exactly one process may hold.
func TestRestartOnlyTouchesRunningUnits(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)
	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatalf("Install: %v", err)
	}
	f.calls = nil

	res, err := m.Restart(t.Context())
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !res.Collecting || !f.ran("restart "+CollectUnit) {
		t.Errorf("Restart did not replace the running collector: %v", res.Lines)
	}

	// Stopped, so there is nothing to replace and nothing may be started.
	delete(f.active, CollectUnit)
	f.calls = nil
	res, err = m.Restart(t.Context())
	if err != nil {
		t.Fatalf("Restart of a stopped unit: %v", err)
	}
	if res.Collecting || f.ran("restart ") || f.ran("start ") {
		t.Errorf("Restart started a stopped unit; calls: %v", f.calls)
	}
}

// TestRestartWithoutUnitsDeclines: with nothing installed there is nothing to
// restart, and the caller has to be told so it can handle the mismatch itself.
func TestRestartWithoutUnitsDeclines(t *testing.T) {
	m, f := testManager(t)
	res, err := m.Restart(t.Context())
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if res.Collecting {
		t.Fatal("Restart claimed to have restarted a unit that does not exist")
	}
	if len(f.calls) != 0 {
		t.Errorf("Restart talked to the service manager about absent units: %v", f.calls)
	}
}

// TestRemoveStopsDisablesAndDeletes: the honest counterpart of an install that
// happens by itself.
func TestRemoveStopsDisablesAndDeletes(t *testing.T) {
	m, f := testManager(t)
	if _, err := m.Install(t.Context(), testOptions(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	f.calls = nil

	res, err := m.Remove(t.Context(), false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertUnitFile(t, m, CollectUnit, false)
	for _, want := range []string{"stop " + CollectUnit, "disable " + CollectUnit, "daemon-reload"} {
		if !f.ran(want) {
			t.Errorf("Remove never ran %q; calls: %v", want, f.calls)
		}
	}
	if len(res.Lines) == 0 {
		t.Error("Remove reported nothing it did")
	}

	// Idempotent: a second removal is a no-op that says so.
	res, err = m.Remove(t.Context(), false)
	if err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	for _, ln := range res.Lines {
		if !strings.Contains(ln, "not installed") {
			t.Errorf("second Remove did something: %q", ln)
		}
	}
}

// TestRemoveCountsWhatItRefused: removal refuses a unit file that carries no
// generated-unit stamp, which is right, and the caller has to be able to tell
// that from a removal that emptied the directory. Nothing failed, so it is not
// an error; nothing was removed either, so it is not success.
func TestRemoveCountsWhatItRefused(t *testing.T) {
	m, _ := testManager(t)
	dir := m.unitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir units: %v", err)
	}
	path := filepath.Join(dir, CollectUnit)
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/usr/bin/aiusage run\n"), 0o644); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	res, err := m.Remove(t.Context(), false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Refused != 1 {
		t.Errorf("Refused = %d, want 1 (the hand-written unit still in %s)", res.Refused, dir)
	}
	if !fileExists(path) {
		t.Error("Remove refused the file and deleted it anyway")
	}
	if res.Changed {
		t.Error("a removal that removed nothing reported a change")
	}

	// With force it goes, and there is nothing left to refuse.
	res, err = m.Remove(t.Context(), true)
	if err != nil {
		t.Fatalf("Remove --force: %v", err)
	}
	if res.Refused != 0 || fileExists(path) {
		t.Errorf("force did not remove the file: refused=%d exists=%v", res.Refused, fileExists(path))
	}
	if !res.Changed {
		t.Error("a removal that deleted a unit file reported no change")
	}
}

// TestInstallMarksOnlyRunsThatChangedSomething is what lets the automatic
// install speak once and then keep quiet: the first install altered the
// machine, and every repeat after it found the same machine already in the
// state it wanted.
func TestInstallMarksOnlyRunsThatChangedSomething(t *testing.T) {
	m, _ := testManager(t)
	o := testOptions(t)

	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Fatalf("the install that wrote, enabled and started the units reported no change:\n%v", res.Lines)
	}

	res, err = m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if res.Changed {
		t.Errorf("a repeat install that found everything in place reported a change:\n%v", res.Lines)
	}
	if !res.Collecting {
		t.Error("a repeat install stopped reporting collection as supervised")
	}
}

// TestTimeoutSaysADeadlineExpired: a command killed for running out of time
// reported the signal that killed it, so the caller's whole account of the
// failure was "systemctl --user daemon-reload: signal: killed" - the mechanism,
// with the cause left out. Both deadlines in play are named, because telling
// them apart is the operator's next question: this call was slow, or the phase
// around it had already given up.
//
// It runs the real runner against sleep, which is the only way to produce the
// signal wording that made this necessary.
func TestTimeoutSaysADeadlineExpired(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("no sleep binary to stall with: %v", err)
	}

	t.Run("the call itself", func(t *testing.T) {
		m := &Manager{Timeout: 100 * time.Millisecond}
		_, err := m.command(t.Context(), "sleep", "30")
		if err == nil {
			t.Fatal("a command killed by its timeout reported success")
		}
		if !strings.Contains(err.Error(), "timed out after 100ms") {
			t.Errorf("the error does not name the timeout: %v", err)
		}
		if strings.Contains(err.Error(), "signal:") {
			t.Errorf("the error still reports the signal instead of the reason: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the error does not carry the deadline it came from: %v", err)
		}
	})

	t.Run("the phase around it", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		m := &Manager{Timeout: 30 * time.Second}
		_, err := m.command(ctx, "sleep", "30")
		if err == nil {
			t.Fatal("a command killed by the phase deadline reported success")
		}
		if !strings.Contains(err.Error(), "supervision deadline expired") {
			t.Errorf("the error does not name the phase deadline: %v", err)
		}
		if strings.Contains(err.Error(), "signal:") {
			t.Errorf("the error still reports the signal instead of the reason: %v", err)
		}
	})
}

// TestStatusReportsUnknownRatherThanInactive: a diagnostic asks its questions
// under one deadline, and a manager that stops answering must not be reported
// as a manager that said "inactive, not enabled". The unit file is on disk
// either way, so Installed stays an answer.
func TestStatusReportsUnknownRatherThanInactive(t *testing.T) {
	m, _ := testManager(t)
	if _, err := m.Install(t.Context(), testOptions(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, st := range m.Status(ctx) {
		if !st.Installed {
			continue
		}
		if st.StateKnown {
			t.Errorf("%s reported a state (%+v) nobody was able to ask about", st.Name, st)
		}
	}
}

// TestStatusReportsTheUnit feeds the doctor line. A unit with no file is
// reported absent without asking the service manager: the file is what this
// package installs, and its absence is the answer.
func TestStatusReportsTheUnit(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)

	got := m.Status(t.Context())
	if len(got) != 1 {
		t.Fatalf("Status returned %d units, want 1", len(got))
	}
	if got[0] != (UnitStatus{Name: CollectUnit}) {
		t.Errorf("status of an uninstalled unit = %+v, want absent", got[0])
	}
	if len(f.calls) != 0 {
		t.Errorf("Status asked the service manager about an absent unit: %v", f.calls)
	}

	seedUnit(t, m, CollectUnit, renderCollect(o))
	f.enabled[CollectUnit] = true
	f.active[CollectUnit] = true

	got = m.Status(t.Context())
	if len(got) != 1 {
		t.Fatalf("Status returned %d units, want 1", len(got))
	}
	if got[0] != (UnitStatus{Name: CollectUnit, Installed: true, Enabled: true, Active: true, StateKnown: true}) {
		t.Errorf("collect status = %+v", got[0])
	}
}

// TestAvailableProbesTheManager: the systemctl binary proves nothing on its own
// (images carry it with no manager behind it, and an ssh session may land
// without one), so availability is settled by a question the manager has to
// answer.
func TestAvailableProbesTheManager(t *testing.T) {
	tests := []struct {
		name string
		fail bool
		want bool
	}{
		{name: "manager answers", want: true},
		{name: "no user manager", fail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, f := testManager(t)
			if tc.fail {
				f.fail["show-environment"] = errExitOne
			}
			if got := m.Available(t.Context()); got != tc.want {
				t.Fatalf("Available = %v, want %v", got, tc.want)
			}
			if !f.ran("show-environment") {
				t.Errorf("Available never probed the manager: %v", f.calls)
			}
		})
	}
}

// TestDefaultUnitDir pins where units live. XDG_CONFIG_HOME wins when it is
// absolute; a relative value is ignored per the XDG spec, exactly as
// internal/config treats the same variable.
func TestDefaultUnitDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got, want := DefaultUnitDir(), filepath.Join(dir, "systemd", "user"); got != want {
		t.Errorf("DefaultUnitDir = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if got, want := DefaultUnitDir(), filepath.Join(home, ".config", "systemd", "user"); got != want {
		t.Errorf("DefaultUnitDir with a relative XDG_CONFIG_HOME = %q, want %q", got, want)
	}
}

// TestSelfExecIsAbsolute: a unit that named a relative path would resolve it
// against whatever working directory the service manager happens to have.
func TestSelfExecIsAbsolute(t *testing.T) {
	got, err := SelfExec()
	if err != nil {
		t.Fatalf("SelfExec: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("SelfExec = %q, want an absolute path", got)
	}
}

// TestExecRunnerReturnsWhenAGrandchildHoldsThePipe is what the per-command
// timeout is worth in practice.
//
// CommandContext kills the direct child when the deadline passes, but
// CombinedOutput reads until every writer to the output pipe is gone. A tool
// that leaves a grandchild behind leaves that descriptor open, so the read
// carried on with no deadline on it at all and a report command simply never
// returned. WaitDelay closes the pipes shortly after the kill; the shell below
// reproduces the shape exactly, with a backgrounded sleep inheriting the pipe
// and outliving the shell that spawned it.
func TestExecRunnerReturnsWhenAGrandchildHoldsThePipe(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell to fork a grandchild from: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := execRunner(ctx, sh, "-c", "sleep 10 & sleep 10"); err == nil {
		t.Fatal("execRunner reported success for a command its context killed")
	}
	// The grandchild sleeps for ten seconds; the bound is the context deadline
	// plus waitDelay, and anything near ten means the pipe was waited on.
	if elapsed, limit := time.Since(start), 3*time.Second; elapsed > limit {
		t.Fatalf("execRunner blocked for %v behind a grandchild, want under %v", elapsed, limit)
	}
}

// TestActivateRollsBackAnEnableThatCouldNotStart: enable and start are two
// calls, and the second one can fail. A unit left enabled but not started comes
// up at the next login against a collection lock the fallback daemon may by
// then be holding, so the enable this call performed is undone.
func TestActivateRollsBackAnEnableThatCouldNotStart(t *testing.T) {
	m, f := testManager(t)
	f.fail["start"] = errExitOne

	if _, err := m.Install(t.Context(), testOptions(t)); err == nil {
		t.Fatal("Install reported success although nothing started")
	}
	if !f.ran("disable " + CollectUnit) {
		t.Errorf("the failed start left the unit enabled; calls: %v", f.calls)
	}
	if f.enabled[CollectUnit] {
		t.Error("the unit is still enabled and will start itself at the next login")
	}
}

// TestActivateKeepsAnEnableItDidNotPerform: the rollback undoes this install's
// own enable, never a state the user set. A unit that was already enabled stays
// enabled however badly the start goes.
func TestActivateKeepsAnEnableItDidNotPerform(t *testing.T) {
	m, f := testManager(t)
	f.fail["start"] = errExitOne
	f.enabled[CollectUnit] = true

	if _, err := m.Install(t.Context(), testOptions(t)); err == nil {
		t.Fatal("Install reported success although nothing started")
	}
	if f.ran("disable " + CollectUnit) {
		t.Errorf("a failed start disabled a unit this install did not enable; calls: %v", f.calls)
	}
}

// TestInstallForceRestartsAUnitItRewrote: --force exists to replace an
// ExecStart, and an ExecStart that is not running is not replaced. Reporting
// the old process as "already running" would be true and useless.
func TestInstallForceRestartsAUnitItRewrote(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)
	seedUnit(t, m, CollectUnit, "[Service]\nExecStart=/somewhere/else/aiusage run\n")
	f.enabled[CollectUnit] = true
	f.active[CollectUnit] = true

	o.Force = true
	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatalf("Install --force: %v", err)
	}
	if !f.ran("restart " + CollectUnit) {
		t.Fatalf("--force rewrote the unit file and left the old ExecStart running; calls: %v", f.calls)
	}
	if !res.Collecting {
		t.Errorf("a restarted collection unit was not reported as supervised: %v", res.Lines)
	}
	for _, ln := range res.Lines {
		if strings.Contains(ln, "already running") {
			t.Errorf("--force claimed the rewritten unit was already running: %q", ln)
		}
	}
}

// TestInstallDoesNotRestartAUnitItLeftAlone: the restart is tied to the rewrite,
// not to --force. A unit --force did not touch (because it was absent, or
// because the file it wrote is the first one) is not restarted for nothing.
func TestInstallDoesNotRestartAUnitItLeftAlone(t *testing.T) {
	m, f := testManager(t)
	o := testOptions(t)
	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	f.calls = nil

	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if f.ran("restart ") {
		t.Errorf("a repeated install restarted a unit it left alone; calls: %v", f.calls)
	}
}

// TestInstallReplacesARuntimeEnable: `systemctl --user enable --runtime` answers
// is-enabled with "enabled-runtime", which is enabled until the next reboot and
// no longer. Treating that as enabled would leave the machine with supervision
// that quietly stops existing.
func TestInstallReplacesARuntimeEnable(t *testing.T) {
	m, f := testManager(t)
	f.runtime[CollectUnit] = true

	res, err := m.Install(t.Context(), testOptions(t))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !f.ran("enable " + CollectUnit) {
		t.Fatalf("a runtime-enabled unit never got a persistent enable; calls: %v", f.calls)
	}
	if !f.enabled[CollectUnit] || f.runtime[CollectUnit] {
		t.Errorf("unit still enabled only until reboot: enabled=%v runtime=%v",
			f.enabled[CollectUnit], f.runtime[CollectUnit])
	}
	for _, ln := range res.Lines {
		if strings.Contains(ln, "already enabled") {
			t.Errorf("a runtime enable was reported as enabled: %q", ln)
		}
	}
}

// TestRemoveRefusesAUnitItDidNotWrite is the counterpart of create-if-missing.
// Install refuses to overwrite a unit file the user may have written, so remove
// must refuse to delete one: the body below is the shape of the hand-written
// units this feature replaces, and it carries no stamp.
func TestRemoveRefusesAUnitItDidNotWrite(t *testing.T) {
	const handWritten = "[Unit]\nDescription=aiusage collection daemon\n\n" +
		"[Service]\nExecStart=/home/dev/.local/bin/aiusage run\n\n" +
		"[Install]\nWantedBy=default.target\n"

	m, f := testManager(t)
	seedUnit(t, m, CollectUnit, handWritten)
	f.active[CollectUnit] = true
	f.enabled[CollectUnit] = true

	res, err := m.Remove(t.Context(), false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertUnitFile(t, m, CollectUnit, true)
	if got := readUnit(t, m, CollectUnit); got != handWritten {
		t.Errorf("Remove touched a unit it did not write:\n%s", got)
	}
	if f.ran("stop ") || f.ran("disable ") {
		t.Errorf("Remove stopped or disabled a unit it refused to delete; calls: %v", f.calls)
	}
	refusal := strings.Join(res.Lines, "\n")
	for _, want := range []string{filepath.Join(m.unitDir(), CollectUnit), "--force"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, refusal)
		}
	}

	// --force is the override, and it is the only one.
	if _, err := m.Remove(t.Context(), true); err != nil {
		t.Fatalf("Remove --force: %v", err)
	}
	assertUnitFile(t, m, CollectUnit, false)
	if !f.ran("stop "+CollectUnit) || !f.ran("disable "+CollectUnit) {
		t.Errorf("Remove --force deleted the file without stopping the unit; calls: %v", f.calls)
	}
}

// TestRemoveDeletesWhatInstallWrote: the stamp is on every rendered unit, so an
// ordinary install/remove round trip is unaffected by the check above.
func TestRemoveDeletesWhatInstallWrote(t *testing.T) {
	m, _ := testManager(t)
	if _, err := m.Install(t.Context(), testOptions(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !hasStamp(filepath.Join(m.unitDir(), CollectUnit)) {
		t.Fatal("an installed unit carries no generated-unit stamp")
	}
	if _, err := m.Remove(t.Context(), false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertUnitFile(t, m, CollectUnit, false)
}

func seedUnit(t *testing.T, m *Manager, name, body string) {
	t.Helper()
	if err := os.MkdirAll(m.unitDir(), 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(m.unitDir(), name), []byte(body), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func readUnit(t *testing.T, m *Manager, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.unitDir(), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func assertUnitFile(t *testing.T, m *Manager, name string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(m.unitDir(), name))
	if got := err == nil; got != want {
		t.Errorf("unit %s present = %v, want %v", name, got, want)
	}
}
