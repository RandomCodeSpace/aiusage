package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
	"github.com/RandomCodeSpace/aiusage/internal/service"
)

// TestMain pins the supervision seam for the whole package before a single test
// runs. The machine running these tests has a real systemd user manager on it,
// with the developer's own aiusage units inside: a test that reached it would
// write unit files into a live configuration and start processes against the
// real ledger. The default here answers every command with a refusal, which is
// exactly the machine-without-systemd case, and any test that wants supervision
// installs its own fake with stubSupervisor.
func TestMain(m *testing.M) {
	collectorProcess = func(context.Context, int) (string, bool) { return "", false }
	newSupervisor = func() *service.Manager {
		return &service.Manager{
			UnitDir: filepath.Join(os.TempDir(), "aiusage-tests-never-written"),
			Run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("no systemd in tests")
			},
		}
	}
	os.Exit(m.Run())
}

// fakeUnits is a scriptable stand-in for systemctl and loginctl, tracking the
// enabled/active state the Manager queries.
type fakeUnits struct {
	pid     int
	calls   []string
	enabled map[string]bool
	active  map[string]bool
	// failed holds units is-active answers "failed" for; failedAt is the
	// microseconds-since-boot systemd reports for entering that state.
	failed   map[string]bool
	failedAt int64
	fail     map[string]error
	// delay makes every call take this long, or until the context expires,
	// whichever comes first: a service manager that answers, just slowly.
	delay time.Duration
}

// fakeLaunchd is the command-boundary stand-in for a macOS GUI launch domain.
// It proves cmd selects and describes launchd without touching the host running
// the test.
type fakeLaunchd struct {
	calls     []string
	available bool
	loaded    bool
	running   bool
	fail      map[string]error
}

func newFakeLaunchd() *fakeLaunchd {
	return &fakeLaunchd{available: true, fail: map[string]error{}}
}

func (f *fakeLaunchd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	verb := name
	if name == "launchctl" && len(args) > 0 {
		verb = args[0]
	}
	if err := f.fail[verb]; err != nil {
		return []byte(verb + " refused"), err
	}
	if name == "plutil" {
		return []byte("OK"), nil
	}
	if name != "launchctl" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	switch args[0] {
	case "print":
		if !strings.Contains(args[1], service.CollectLabel) {
			if f.available {
				return []byte("domain = gui"), nil
			}
			return []byte("Could not find domain"), errors.New("exit status 1")
		}
		if !f.loaded {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
		state := "waiting"
		if f.running {
			state = "running"
		}
		return []byte("state = " + state), nil
	case "print-disabled":
		return []byte(`"` + service.CollectLabel + `" => false`), nil
	case "enable":
		return nil, nil
	case "bootstrap", "kickstart":
		f.loaded = true
		f.running = true
		return nil, nil
	case "bootout":
		f.loaded = false
		f.running = false
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected launchctl verb %s", args[0])
	}
}

func newFakeUnits() *fakeUnits {
	return &fakeUnits{enabled: map[string]bool{}, active: map[string]bool{}, failed: map[string]bool{}, fail: map[string]error{}}
}

func (f *fakeUnits) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))

	if f.delay > 0 {
		t := time.NewTimer(f.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	var words []string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			words = append(words, a)
		}
	}
	verb, unit := "", ""
	if len(words) > 0 {
		verb = words[0]
	}
	if len(words) > 1 {
		unit = words[1]
	}
	if err := f.fail[verb]; err != nil {
		return []byte(verb + " refused"), err
	}

	switch verb {
	case "show":
		// Key=Value lines in REVERSE of the requested order (systemd answers
		// in its own order, not the caller's); bare values under --value.
		var props []string
		bare := false
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--property="); ok {
				props = strings.Split(v, ",")
			}
			if a == "--value" {
				bare = true
			}
		}
		var b strings.Builder
		for i := len(props) - 1; i >= 0; i-- {
			p := props[i]
			if !bare {
				b.WriteString(p + "=")
			}
			switch p {
			case "MainPID":
				b.WriteString(strconv.Itoa(f.pid))
			case "ActiveState":
				switch {
				case f.active[unit]:
					b.WriteString("active")
				case f.failed[unit]:
					b.WriteString("failed")
				default:
					b.WriteString("inactive")
				}
			case "InactiveEnterTimestampMonotonic":
				b.WriteString(strconv.FormatInt(f.failedAt, 10))
			case "NRestarts":
				b.WriteString("0")
			}
			b.WriteString("\n")
		}
		return []byte(b.String()), nil
	case "show-environment", "daemon-reload", "enable-linger":
		return nil, nil
	case "show-user":
		return []byte("Linger=yes\n"), nil
	case "is-enabled":
		if f.enabled[unit] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errors.New("exit status 1")
	case "is-active":
		if f.active[unit] {
			return []byte("active\n"), nil
		}
		if f.failed[unit] {
			return []byte("failed\n"), errors.New("exit status 3")
		}
		return []byte("inactive\n"), errors.New("exit status 1")
	case "reset-failed":
		delete(f.failed, unit)
		return nil, nil
	case "enable":
		f.enabled[unit] = true
		return nil, nil
	case "disable":
		delete(f.enabled, unit)
		return nil, nil
	case "start", "restart":
		f.active[unit] = true
		delete(f.failed, unit)
		return nil, nil
	case "stop":
		delete(f.active, unit)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %q", verb)
}

func (f *fakeUnits) ran(want string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// stubSupervisor installs a fake service manager writing into a temp unit
// directory, for the test's duration. The machine running these tests has a
// real systemd user manager with the developer's own aiusage unit in it, and
// nothing here may reach it.
func stubSupervisor(t *testing.T) (*fakeUnits, string) {
	t.Helper()
	f := newFakeUnits()
	dir := filepath.Join(t.TempDir(), "systemd", "user")
	prev := newSupervisor
	newSupervisor = func() *service.Manager {
		return &service.Manager{UnitDir: dir, Run: f.run, Settle: time.Millisecond}
	}
	t.Cleanup(func() { newSupervisor = prev })
	return f, dir
}

func stubLaunchdSupervisor(t *testing.T) (*fakeLaunchd, string) {
	t.Helper()
	f := newFakeLaunchd()
	dir := filepath.Join(t.TempDir(), "Library", "LaunchAgents")
	prev := newSupervisor
	newSupervisor = func() *service.Manager {
		return &service.Manager{GOOS: "darwin", UnitDir: dir, Run: f.run, Settle: time.Millisecond}
	}
	t.Cleanup(func() { newSupervisor = prev })
	return f, dir
}

// setFlags pins the package-level persistent flags for the test's duration.
func setFlags(t *testing.T, f globalFlags) {
	t.Helper()
	prev := flags
	flags = f
	t.Cleanup(func() { flags = prev })
}

// setBudget shrinks the supervision phase budget for the test's duration, so a
// test about the bound does not have to wait the production one out.
func setBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := supervisionBudget
	supervisionBudget = d
	t.Cleanup(func() { supervisionBudget = prev })
}

// setSetupBudget shrinks the explicit setup command's deadline for the test's
// duration, so a test about the bound does not wait out the production one.
func setSetupBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := setupBudget
	setupBudget = d
	t.Cleanup(func() { setupBudget = prev })
}

// clearPathEnv neutralises every environment variable that would suppress the
// automatic install, so a test that expects one is testing its own scenario and
// not the shell it happens to run in.
//
// Emptying is not neutral for all of them. HOME is an override when it names
// anything other than the account's own home directory, and the empty string is
// something else - so a helper that only emptied the list would suppress the
// install in every test that called it, which is precisely the blind spot the
// production rule was added for. The account's home goes back in, and the
// result is then checked against config itself: a variable this helper fails to
// neutralise fails every test that relies on it, loudly and at the source.
func clearPathEnv(t *testing.T) {
	t.Helper()
	for _, name := range config.PathEnvNames() {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", accountHome(t))
	for _, name := range discoveryEnv() {
		t.Setenv(name, "")
	}
	if env := config.PathEnvOverrides(); len(env) != 0 {
		t.Fatalf("clearPathEnv left %v overriding a path; teach it about them", env)
	}
	if env := discoveryEnvOverrides(); len(env) != 0 {
		t.Fatalf("clearPathEnv left %v overriding adapter discovery; teach it about them", env)
	}
}

// accountHome returns the home directory this account owns, which is what HOME
// has to hold for config not to call it an override. It comes from the user
// database precisely because the environment cannot move it.
func accountHome(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("no account home to compare HOME against: %v", err)
	}
	return u.HomeDir
}

// sandboxConfig returns a Config whose every path is inside the test.
func sandboxConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{
		DBPath:  filepath.Join(dir, "data", "usage.db"),
		PIDPath: filepath.Join(dir, "state", "aiusage.pid"),
		LogPath: filepath.Join(dir, "state", "aiusage.log"),
	}
}

func unitExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// TestAutoInstallRefusesPathOverrides is the one-off trap. `aiusage --db
// /tmp/scratch.db summary` must never leave behind a unit that collects into a
// scratch file forever: a path override describes one command, not the machine.
// --interval is not a path, is clamped to a sane range, and changes nothing
// about which data is read or written, so it does not suppress anything.
func TestAutoInstallRefusesPathOverrides(t *testing.T) {
	tests := []struct {
		name string
		f    globalFlags
		want bool
	}{
		{name: "no overrides", f: globalFlags{}, want: true},
		{name: "interval only", f: globalFlags{interval: 900}, want: true},
		{name: "db override", f: globalFlags{db: "/tmp/scratch.db"}},
		{name: "config override", f: globalFlags{config: "/tmp/other.json"}},
		{name: "home override", f: globalFlags{home: "/tmp/sandbox"}},
		{name: "db and interval", f: globalFlags{db: "/tmp/scratch.db", interval: 900}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearPathEnv(t)
			if got := autoInstall(tc.f); got != tc.want {
				t.Fatalf("autoInstall(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// TestAutoInstallRefusesEveryPathEnvOverride is the same trap arriving through
// the environment, where it is worse.
//
// AIUSAGE_DB=/tmp/scratch.db aiusage today used to write a unit whose
// ReadWritePaths named the scratch directories while its ExecStart carried no
// flags at all - so the supervised daemon resolved the DEFAULT database inside
// a sandbox that did not allow writing to it. A systemd unit does not inherit
// the shell's environment, which is exactly why the CLI and the unit it writes
// cannot agree here.
//
// The table is generated from config.PathEnvNames, so a new variable in
// internal/config fails this test until it is accounted for.
func TestAutoInstallRefusesEveryPathEnvOverride(t *testing.T) {
	names := config.PathEnvNames()
	if len(names) == 0 {
		t.Fatal("config reports no path environment variables at all")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			clearPathEnv(t)
			t.Setenv(name, t.TempDir())
			if autoInstall(globalFlags{}) {
				t.Fatalf("%s moved a path and the automatic install went ahead anyway", name)
			}
		})
	}

	// The cadence is not a path: it is clamped to a sane range and changes
	// nothing about which data is read or written.
	t.Run("AIUSAGE_INTERVAL", func(t *testing.T) {
		clearPathEnv(t)
		t.Setenv("AIUSAGE_INTERVAL", "900")
		if !autoInstall(globalFlags{}) {
			t.Fatal("a collection cadence suppressed the install")
		}
	})

	// A relative XDG base directory is ignored by the XDG spec and by
	// internal/config, so it is not an override and must not suppress anything.
	t.Run("relative XDG_DATA_HOME", func(t *testing.T) {
		clearPathEnv(t)
		t.Setenv("XDG_DATA_HOME", "relative/share")
		if !autoInstall(globalFlags{}) {
			t.Fatal("a relative XDG base directory suppressed the install")
		}
	})

	// HOME at the account's own home directory is the normal state of every
	// shell on the machine. Treating THAT as an override would mean the
	// automatic install never ran anywhere, which is the opposite failure.
	t.Run("HOME at the account home", func(t *testing.T) {
		clearPathEnv(t)
		t.Setenv("HOME", accountHome(t))
		if !autoInstall(globalFlags{}) {
			t.Fatal("an unmoved HOME suppressed the install")
		}
	})
}

// TestAutoInstallRefusesADisplacedHome is the same trap again, and the sharpest
// version of it, so it gets its own test rather than only a row in the table.
//
// HOME is what os.UserHomeDir resolves, so moving it moves the database, the
// state directory, the config file AND the unit directory at once. Measured
// before this rule existed: `HOME=<sandbox> aiusage today` wrote
// <sandbox>/.config/systemd/user/aiusage-collect.service with ReadWritePaths
// naming the sandbox and an ExecStart carrying no flags at all. Worse than the
// other variables, because the unit NAMES are fixed constants: the manager was
// asked about aiusage-collect.service, answered for the REAL one running on the
// machine, reported it already running, and the invocation ended up supervised
// by a unit it had not written, with nothing collecting into the sandbox.
func TestAutoInstallRefusesADisplacedHome(t *testing.T) {
	clearPathEnv(t)
	if !autoInstall(globalFlags{}) {
		t.Fatal("the install was already suppressed before HOME moved")
	}
	t.Setenv("HOME", t.TempDir())
	if autoInstall(globalFlags{}) {
		t.Fatal("HOME pointed somewhere other than the account's home and the automatic install went ahead")
	}
}

// TestAutoInstallRefusesEveryDiscoveryEnvOverride is the trap arriving from the
// other side: these variables move WHAT the adapters read rather than where
// aiusage writes.
//
// Measured before this rule existed: `CLAUDE_CONFIG_DIR=<tmp> aiusage today`
// installed a unit whose supervised daemon reads the DEFAULT Claude directory,
// while the equivalent --home flag correctly suppressed the install. A unit
// inherits none of this, and unlike --db there is not even a flag to forward.
//
// The table is generated from discoveryEnv, so a variable added there is
// covered without touching this test.
func TestAutoInstallRefusesEveryDiscoveryEnvOverride(t *testing.T) {
	names := discoveryEnv()
	if len(names) == 0 {
		t.Fatal("no adapter discovery variables at all")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			clearPathEnv(t)
			t.Setenv(name, t.TempDir())
			if autoInstall(globalFlags{}) {
				t.Fatalf("%s moved an adapter's discovery root and the automatic install went ahead anyway", name)
			}
		})

		t.Run(name+" blank", func(t *testing.T) {
			// Adapters trim before testing the value, so whitespace moves
			// nothing and must not suppress an install over nothing.
			clearPathEnv(t)
			t.Setenv(name, "   ")
			if !autoInstall(globalFlags{}) {
				t.Fatalf("a blank %s suppressed the install", name)
			}
		})
	}
}

// envLookup is one os.Getenv call found in the adapter sources.
type envLookup struct {
	name string
	pos  string
}

// TestDiscoveryEnvCoversEveryAdapterVariable reads the adapter sources and
// fails when one of them consults an environment variable discoveryEnv has not
// been taught about.
//
// Generating the suppression table from a hand-written list only proves the
// list is honoured, not that it is complete, and the failure this guards
// against is precisely an adapter added later with a discovery variable nobody
// remembered to register: the automatic install would then bake a unit that
// collects from somewhere the CLI is not looking. Reading the source is the
// only check that fails on the day the variable appears.
func TestDiscoveryEnvCoversEveryAdapterVariable(t *testing.T) {
	known := make(map[string]bool, len(discoveryEnv()))
	for _, name := range discoveryEnv() {
		known[name] = true
	}
	uses := adapterEnvLookups(t)
	if len(uses) == 0 {
		t.Fatal("found no environment lookups in the adapter sources at all; the scan is broken, not the adapters")
	}
	for _, u := range uses {
		if !known[u.name] {
			t.Errorf("%s reads %s, which discoveryEnv() does not name: an automatic install under it "+
				"would bake a unit whose daemon reads the default location instead", u.pos, u.name)
		}
	}
}

// TestAdapterCIIsolatesEveryDiscoveryVariable keeps the dedicated adapter job
// from silently inheriting a runner or developer installation. The source scan
// above owns the complete variable list; this check makes the workflow consume
// that same contract even though GitHub Actions cannot call Go code for env
// declarations.
func TestAdapterCIIsolatesEveryDiscoveryVariable(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	const sentinel = ": /nonexistent/aiusage-adapter-contract"
	for _, name := range discoveryEnv() {
		if !strings.Contains(string(body), "\n      "+name+sentinel+"\n") {
			t.Errorf("adapter CI does not neutralize %s with the impossible sentinel", name)
		}
	}
}

// adapterEnvLookups parses every non-test source file under adapter and
// returns each environment variable it reads. Constants are resolved within
// their own package, which is how the adapters spell these (a bare literal and
// a package constant are both used today).
func adapterEnvLookups(t *testing.T) []envLookup {
	t.Helper()
	const root = "../../adapter"

	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		dir := filepath.Dir(path)
		byDir[dir] = append(byDir[dir], f)
		return nil
	})
	if err != nil {
		t.Fatalf("read the adapter sources: %v", err)
	}

	var out []envLookup
	for _, files := range byDir {
		consts := stringConsts(files)
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isEnvLookup(call.Fun) || len(call.Args) != 1 {
					return true
				}
				pos := fset.Position(call.Pos()).String()
				name, ok := constString(call.Args[0], consts)
				if !ok {
					t.Errorf("%s: cannot tell which environment variable this reads; "+
						"name it with a package constant so the discovery list can be checked against it", pos)
					return true
				}
				out = append(out, envLookup{name: name, pos: pos})
				return true
			})
		}
	}
	return out
}

// stringConsts collects the string constants declared in one package.
func stringConsts(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if v, ok := stringLit(vs.Values[i]); ok {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// isEnvLookup reports whether an expression is os.Getenv or os.LookupEnv.
func isEnvLookup(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv")
}

// constString resolves an argument to the string it holds, from a literal or a
// constant of the same package.
func constString(arg ast.Expr, consts map[string]string) (string, bool) {
	if v, ok := stringLit(arg); ok {
		return v, true
	}
	if id, ok := arg.(*ast.Ident); ok {
		v, ok := consts[id.Name]
		return v, ok
	}
	return "", false
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// TestEnsureDaemonSkipsSupervisionOnEnvOverride is the behaviour behind that
// rule: the unit directory stays empty and the service manager is never spoken
// to, exactly as with --db. Both lists are covered, because both end in a unit
// that disagrees with the CLI that wrote it - one about where data is kept, the
// other about where it is read from.
func TestEnsureDaemonSkipsSupervisionOnEnvOverride(t *testing.T) {
	for _, name := range append(config.PathEnvNames(), discoveryEnv()...) {
		t.Run(name, func(t *testing.T) {
			f, dir := stubSupervisor(t)
			setFlags(t, globalFlags{})
			clearPathEnv(t)
			t.Setenv(name, t.TempDir())
			calls, restore := stubSpawn(t)
			defer restore()

			if err := ensureDaemon(t.Context(), sandboxConfig(t), io.Discard); err != nil {
				t.Fatalf("ensureDaemon: %v", err)
			}
			if *calls != 1 {
				t.Errorf("expected the detached spawn to run once, got %d", *calls)
			}
			if len(f.calls) != 0 {
				t.Errorf("a %s invocation talked to the service manager: %v", name, f.calls)
			}
			if unitExists(dir, service.CollectUnit) {
				t.Errorf("a %s invocation baked a unit into %s", name, dir)
			}
		})
	}
}

// TestSupervisionCannotOutlastItsBudget: the per-command timeout bounds one
// systemctl, and an install is a dozen of them. A manager that answers
// everything slowly would otherwise add its own latency twelve times over to a
// command the user typed to see a number, so the whole phase shares one
// deadline and abandons supervision when it passes.
func TestSupervisionCannotOutlastItsBudget(t *testing.T) {
	tests := []struct {
		name  string
		delay time.Duration
	}{
		// Slower than the whole budget: supervision is abandoned at the first
		// call, before it has learnt anything about the machine.
		{name: "first call outlasts the budget", delay: 400 * time.Millisecond},
		// Fast enough to get some way in, not fast enough to finish: the phase
		// is abandoned mid-sequence.
		{name: "every call eats part of the budget", delay: 60 * time.Millisecond},
	}
	const budget = 200 * time.Millisecond

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := stubSupervisor(t)
			f.delay = tc.delay
			setBudget(t, budget)
			setFlags(t, globalFlags{})
			clearPathEnv(t)
			calls, restore := stubSpawn(t)
			defer restore()

			var warn strings.Builder
			start := time.Now()
			if err := ensureDaemon(t.Context(), sandboxConfig(t), &warn); err != nil {
				t.Fatalf("ensureDaemon: %v", err)
			}
			elapsed := time.Since(start)

			if limit := budget + time.Second; elapsed > limit {
				t.Errorf("the supervision phase took %v against a %v budget, want under %v",
					elapsed, budget, limit)
			}
			if *calls != 1 {
				t.Errorf("supervision was abandoned without falling back: spawns=%d, calls=%v", *calls, f.calls)
			}
			if n := strings.Count(strings.TrimSpace(warn.String()), "\n"); n > 0 {
				t.Errorf("abandoning printed %d extra lines:\n%s", n, warn.String())
			}
		})
	}
}

// TestEnsureDaemonInstallsUnits: with no daemon running and a service manager
// answering, collection is handed to systemd and no detached process is spawned.
func TestEnsureDaemonInstallsUnits(t *testing.T) {
	f, dir := stubSupervisor(t)
	setFlags(t, globalFlags{})
	clearPathEnv(t)
	calls, restore := stubSpawn(t)
	defer restore()

	cfg := sandboxConfig(t)
	if err := ensureDaemon(t.Context(), cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if *calls != 0 {
		t.Errorf("supervised install still spawned %d detached daemons", *calls)
	}
	if !unitExists(dir, service.CollectUnit) {
		t.Fatalf("no collection unit written into %s", dir)
	}
	if !f.ran("start " + service.CollectUnit) {
		t.Errorf("collection unit was never started: %v", f.calls)
	}

	// The unit has to name the binary absolutely and carry the data and state
	// directories, or ProtectSystem=strict makes it fail on its first write.
	body, err := os.ReadFile(filepath.Join(dir, service.CollectUnit))
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	for _, want := range []string{filepath.Dir(cfg.DBPath), filepath.Dir(cfg.PIDPath), "ExecStart=/"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("unit missing %q:\n%s", want, body)
		}
	}
}

// TestEnsureDaemonSkipsSupervisionOnOverride: the same run with --db must not
// touch the service manager at all, and must fall back to the detached spawn.
func TestEnsureDaemonSkipsSupervisionOnOverride(t *testing.T) {
	f, dir := stubSupervisor(t)
	cfg := sandboxConfig(t)
	setFlags(t, globalFlags{db: cfg.DBPath})
	calls, restore := stubSpawn(t)
	defer restore()

	if err := ensureDaemon(t.Context(), cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if *calls != 1 {
		t.Errorf("expected the detached spawn to run once, got %d", *calls)
	}
	if len(f.calls) != 0 {
		t.Errorf("a --db invocation talked to the service manager: %v", f.calls)
	}
	if unitExists(dir, service.CollectUnit) {
		t.Errorf("a --db invocation baked a unit into %s", dir)
	}
}

// TestEnsureDaemonDegradesWhenSupervisionFails: every refusal from the service
// manager lands on the behaviour aiusage had before this feature existed, with
// one line of explanation and the user's command still running.
func TestEnsureDaemonDegradesWhenSupervisionFails(t *testing.T) {
	tests := []struct {
		name string
		verb string
	}{
		{name: "no user manager", verb: "show-environment"},
		{name: "enable refused", verb: "enable"},
		{name: "start refused", verb: "start"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := stubSupervisor(t)
			f.fail[tc.verb] = errors.New("exit status 1")
			setFlags(t, globalFlags{})
			clearPathEnv(t)
			calls, restore := stubSpawn(t)
			defer restore()

			var warn strings.Builder
			if err := ensureDaemon(t.Context(), sandboxConfig(t), &warn); err != nil {
				t.Fatalf("ensureDaemon: %v", err)
			}
			if *calls != 1 {
				t.Fatalf("fallback spawn ran %d times, want 1", *calls)
			}
			if n := strings.Count(strings.TrimSpace(warn.String()), "\n"); n > 0 {
				t.Errorf("degrading printed %d extra lines:\n%s", n, warn.String())
			}
		})
	}
}

// TestEnsureDaemonRestartsTheUnitOnBuildMismatch is version sync under
// supervision: the collector is replaced by restarting its unit, not by killing
// a process systemd would only start again.
func TestEnsureDaemonRestartsTheUnitOnBuildMismatch(t *testing.T) {
	setVersion(t, "v9.9.9")
	f, dir := stubSupervisor(t)
	setFlags(t, globalFlags{})
	clearPathEnv(t)

	dirState := t.TempDir()
	pidPath := seedLock(t, dirState)
	if err := os.WriteFile(pidPath, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	release := holdLock(t, pidPath)
	defer release()

	cfg := config.Config{PIDPath: pidPath, DBPath: filepath.Join(t.TempDir(), "usage.db")}
	daemon.WriteVersion(cfg, "v1.0.0")

	// Installed and running, which is what makes this a restart.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir units: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, service.CollectUnit), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", service.CollectUnit, err)
	}
	f.active[service.CollectUnit] = true
	f.enabled[service.CollectUnit] = true
	f.pid = 12345

	calls, restore := stubSpawn(t)
	defer restore()
	stopped := 0
	prevStop := stopDaemon
	stopDaemon = func(config.Config, int) error { stopped++; return nil }
	defer func() { stopDaemon = prevStop }()

	if err := ensureDaemon(t.Context(), cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if !f.ran("restart " + service.CollectUnit) {
		t.Errorf("the collection unit was not restarted: %v", f.calls)
	}
	if stopped != 0 || *calls != 0 {
		t.Errorf("supervised restart also killed and respawned: stops=%d spawns=%d", stopped, *calls)
	}
}

// TestNoDaemonSkipsSupervisionEntirely: --no-daemon means no spawn, no install,
// nothing. It is the escape hatch, and an escape hatch that wrote unit files
// would not be one.
func TestNoDaemonSkipsSupervisionEntirely(t *testing.T) {
	f, dir := stubSupervisor(t)
	calls, restore := stubSpawn(t)
	defer restore()

	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// newRootCmd binds --no-daemon, which resets flags.noDaemon to false, so the
	// flag value is pinned after the command is built - and restored after the
	// test, like every other flag override in this package.
	root := newRootCmd()
	setFlags(t, globalFlags{noDaemon: true})
	target, _, err := root.Find([]string{"today"})
	if err != nil {
		t.Fatalf("find today: %v", err)
	}
	if err := root.PersistentPreRunE(target, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	if *calls != 0 || len(f.calls) != 0 || unitExists(dir, service.CollectUnit) {
		t.Fatalf("--no-daemon did something: spawns=%d service calls=%v", *calls, f.calls)
	}
}

// TestDoctorReportsSupervision covers the three honest answers to "what is
// keeping the collector alive": systemd units, an unsupervised background
// process, or nothing.
func TestDoctorReportsSupervision(t *testing.T) {
	tests := []struct {
		name      string
		units     bool
		active    bool
		daemon    bool
		available bool
		want      []string
	}{
		{
			name: "systemd units", units: true, active: true, available: true,
			want: []string{"systemd user service:", service.CollectUnit, "active, enabled", "linger confirmed", "starts at boot and survives logout"},
		},
		{
			name: "installed but stopped", units: true, available: true,
			want: []string{"systemd user service:", "inactive, not enabled", "linger confirmed"},
		},
		{
			name: "unsupervised daemon", daemon: true,
			want: []string{"detached background process", "not persistent across logout or reboot"},
		},
		{name: "nothing", want: []string{"none: no collector is running"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := stubSupervisor(t)
			if !tc.available {
				f.fail["show-environment"] = errors.New("exit status 1")
			}
			if tc.units {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir units: %v", err)
				}
				name := service.CollectUnit
				if err := os.WriteFile(filepath.Join(dir, name), []byte("[Service]\n"), 0o644); err != nil {
					t.Fatalf("seed %s: %v", name, err)
				}
				f.active[name] = tc.active
				f.enabled[name] = tc.active
			}

			home := t.TempDir()
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			configPath := offlineConfig(t)
			dbPath := filepath.Join(t.TempDir(), "usage.db")
			args := []string{"--home", home, "--config", configPath,
				"--db", dbPath, "--no-daemon", "doctor"}

			if tc.daemon {
				// The lock must use the same per-database path as doctor.
				cfg, err := loadConfigFlags(globalFlags{home: home, config: configPath, db: dbPath})
				if err != nil {
					t.Fatalf("load config: %v", err)
				}
				rel, err := daemon.AcquireCollectionLock(cfg.PIDPath, buildinfo.Identity())
				if err != nil {
					t.Fatalf("acquire lock: %v", err)
				}
				defer rel()
			}

			out, err := runCmd(t, args...)
			if err != nil {
				t.Fatalf("doctor: %v\n%s", err, out)
			}
			if !strings.Contains(out, "Supervision") {
				t.Fatalf("doctor printed no supervision block:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("supervision block missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestSetupInstallsAndRemoves drives the explicit command end to end: install,
// repeat (idempotent), then remove.
func TestSetupInstallsAndRemoves(t *testing.T) {
	f, dir := stubSupervisor(t)
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)

	out, err := runCmd(t, "--config", offlineConfig(t), "setup")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !unitExists(dir, service.CollectUnit) {
		t.Fatalf("setup wrote no collection unit:\n%s", out)
	}
	for _, want := range []string{"unit directory: " + dir, "wrote ", "enabled ", "started "} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output missing %q:\n%s", want, out)
		}
	}

	out, err = runCmd(t, "--config", offlineConfig(t), "setup")
	if err != nil {
		t.Fatalf("second setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already present, left as it is") {
		t.Errorf("a repeated setup did not report the units as already installed:\n%s", out)
	}
	if strings.Contains(out, "wrote ") {
		t.Errorf("a repeated setup rewrote a unit file:\n%s", out)
	}

	out, err = runCmd(t, "--config", offlineConfig(t), "setup", "--remove")
	if err != nil {
		t.Fatalf("setup --remove: %v\n%s", err, out)
	}
	if unitExists(dir, service.CollectUnit) {
		t.Fatalf("--remove left the unit behind:\n%s", out)
	}
	if !strings.Contains(out, "removed ") || !f.ran("disable "+service.CollectUnit) {
		t.Errorf("--remove did not stop and disable the units:\n%s\ncalls: %v", out, f.calls)
	}
}

// TestSetupBakesTheFlagsItWasGiven is the deliberate asymmetry with the
// automatic install: being asked is consent, so an explicit setup writes the
// overrides it was handed into the unit.
func TestSetupBakesTheFlagsItWasGiven(t *testing.T) {
	isolateState(t)
	_, dir := stubSupervisor(t)
	db := filepath.Join(t.TempDir(), "elsewhere.db")

	out, err := runCmd(t, "--db", db, "--config", offlineConfig(t), "setup")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, service.CollectUnit))
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(body), "--db "+db) {
		t.Errorf("explicit setup dropped --db:\n%s", body)
	}
}

// TestSetupInstallsOnlyTheCollector: aiusage supervises one process. A second
// unit file in the directory would be one this CLI never accounted for.
func TestSetupInstallsOnlyTheCollector(t *testing.T) {
	isolateState(t)
	_, dir := stubSupervisor(t)

	out, err := runCmd(t, "--config", offlineConfig(t), "setup")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read unit dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != service.CollectUnit {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("setup wrote %v, want only %s:\n%s", names, service.CollectUnit, out)
	}
}

// TestSetupForceRewrites: the only path that replaces a unit file the user may
// have edited.
func TestSetupForceRewrites(t *testing.T) {
	isolateState(t)
	_, dir := stubSupervisor(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir units: %v", err)
	}
	path := filepath.Join(dir, service.CollectUnit)
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/old/aiusage run\n"), 0o644); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	out, err := runCmd(t, "--config", offlineConfig(t), "setup", "--force")
	if err != nil {
		t.Fatalf("setup --force: %v\n%s", err, out)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if strings.Contains(string(body), "/old/aiusage") {
		t.Fatalf("--force did not rewrite the unit:\n%s", body)
	}
	if !strings.Contains(out, "rewrote ") {
		t.Errorf("--force did not say it rewrote anything:\n%s", out)
	}
}

// TestSetupWithoutSystemdSaysSo: on a machine with no user manager the command
// is not an error - it reports the fallback and changes nothing.
func TestSetupWithoutSystemdSaysSo(t *testing.T) {
	f, dir := stubSupervisor(t)
	f.fail["show-environment"] = errors.New("exit status 1")

	out, err := runCmd(t, "--config", offlineConfig(t), "setup")
	if err != nil {
		t.Fatalf("setup on a machine without systemd returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no systemd user session") {
		t.Errorf("setup did not explain the fallback:\n%s", out)
	}
	if unitExists(dir, service.CollectUnit) {
		t.Error("setup wrote a unit with no service manager to run it")
	}
}

func TestSetupAndDoctorUseLaunchdOnDarwin(t *testing.T) {
	f, dir := stubLaunchdSupervisor(t)
	home := t.TempDir()
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	args := []string{"--home", home, "--db", db, "--config", cfg}

	out, err := runCmd(t, append(args, "setup")...)
	if err != nil {
		t.Fatalf("Darwin setup: %v\n%s", err, out)
	}
	path := filepath.Join(dir, service.CollectPlist)
	if !unitExists(dir, service.CollectPlist) || !f.loaded || !f.running {
		t.Fatalf("Darwin setup left plist=%t loaded=%t running=%t\n%s", unitExists(dir, service.CollectPlist), f.loaded, f.running, out)
	}
	for _, want := range []string{"supervised by launchd", "GUI login", service.CollectLabel} {
		if !strings.Contains(out, want) {
			t.Errorf("Darwin setup output missing %q:\n%s", want, out)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<string>--home</string>") || !strings.Contains(string(body), "<string>"+home+"</string>") {
		t.Fatalf("LaunchAgent did not preserve explicit setup flags:\n%s", body)
	}

	out, err = runCmd(t, append(args, "--no-daemon", "doctor")...)
	if err != nil {
		t.Fatalf("Darwin doctor: %v\n%s", err, out)
	}
	for _, want := range []string{"launchd LaunchAgent:", "installed, loaded, running", "at GUI login", "stops at logout"} {
		if !strings.Contains(out, want) {
			t.Errorf("Darwin doctor output missing %q:\n%s", want, out)
		}
	}

	out, err = runCmd(t, append(args, "setup", "--remove")...)
	if err != nil {
		t.Fatalf("Darwin setup --remove: %v\n%s", err, out)
	}
	if unitExists(dir, service.CollectPlist) || f.loaded {
		t.Fatal("Darwin removal left the plist or loaded job")
	}
}

func TestSetupWithoutDarwinGUIChangesNothing(t *testing.T) {
	f, dir := stubLaunchdSupervisor(t)
	f.available = false
	out, err := runCmd(t, "--home", t.TempDir(), "--config", offlineConfig(t), "setup")
	if err != nil {
		t.Fatalf("setup without GUI domain: %v\n%s", err, out)
	}
	for _, want := range []string{"no macOS GUI launch domain", "GUI login session", "not persistent across logout or reboot"} {
		if !strings.Contains(out, want) {
			t.Errorf("unavailable launchd notice missing %q:\n%s", want, out)
		}
	}
	if unitExists(dir, service.CollectPlist) || unitExists(filepath.Dir(dir), filepath.Base(dir)) {
		t.Fatal("unavailable GUI domain created a persistent LaunchAgent path")
	}
}

func TestSetupRestoresDetachedCollectorAfterLaunchdActivationFailure(t *testing.T) {
	stubCollectorEvidence(t, "", true)
	f, dir := stubLaunchdSupervisor(t)
	f.fail["bootstrap"] = errors.New("bootstrap refused")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	pidPath := filepath.Join(home, ".local", "state", "aiusage", "aiusage.pid")
	release, err := daemon.AcquireCollectionLock(pidPath, "detached-test")
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})

	prevStop := stopDaemon
	stopped := 0
	stopDaemon = func(config.Config, int) error {
		stopped++
		if !released {
			release()
			released = true
		}
		return nil
	}
	t.Cleanup(func() { stopDaemon = prevStop })
	spawns, restoreSpawn := stubSpawn(t)
	defer restoreSpawn()

	out, err := runCmd(t, "--home", home, "--config", offlineConfig(t), "setup")
	if err == nil {
		t.Fatalf("failed launchd activation reported success:\n%s", out)
	}
	if stopped != 1 || *spawns != 1 {
		t.Fatalf("promotion rollback stopped=%d spawned=%d, want one each\n%s", stopped, *spawns, out)
	}
	if !strings.Contains(out, "restored the detached collector") {
		t.Fatalf("promotion rollback was not reported:\n%s", out)
	}
	if unitExists(dir, service.CollectPlist) || f.loaded {
		t.Fatal("failed launchd promotion left a native job behind")
	}
}

// TestAutomaticInstallReportsItselfExactlyOnce is the account the automatic
// path owes the user.
//
// Measured before it gave one: an ordinary `aiusage today` installed a systemd
// user unit, enabled it and STARTED a long-lived service - and wrote ZERO bytes
// to stderr. Installing a background process is not a side effect to perform
// without a word.
//
// The second half of the rule matters as much: the steady state is a dozen
// report commands a day finding everything already in place, and a notice on
// each of them is a notice nobody reads.
func TestAutomaticInstallReportsItselfExactlyOnce(t *testing.T) {
	f, dir := stubSupervisor(t)
	setFlags(t, globalFlags{})
	clearPathEnv(t)
	calls, restore := stubSpawn(t)
	defer restore()

	cfg := sandboxConfig(t)
	var warn strings.Builder
	if err := ensureDaemon(t.Context(), cfg, &warn); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	first := warn.String()
	t.Logf("stderr of the installing run:\n%s", first)

	if !unitExists(dir, service.CollectUnit) {
		t.Fatalf("no unit was installed, so there is nothing to report: %v", f.calls)
	}
	if first == "" {
		t.Fatal("the automatic install wrote unit files, enabled and started a service, and said nothing")
	}
	for _, want := range []string{"wrote ", "enabled " + service.CollectUnit, "started " + service.CollectUnit} {
		if !strings.Contains(first, want) {
			t.Errorf("the install notice never mentions %q:\n%s", want, first)
		}
	}
	// Second run: everything is already in place, so nothing may be printed.
	warn.Reset()
	if err := ensureDaemon(t.Context(), cfg, &warn); err != nil {
		t.Fatalf("second ensureDaemon: %v", err)
	}
	if warn.String() != "" {
		t.Errorf("a run that changed nothing still printed:\n%s", warn.String())
	}
	if *calls != 0 {
		t.Errorf("the supervised runs also spawned %d detached daemons", *calls)
	}
}

// TestAutomaticInstallNeverBakesAnInterval: --interval is exempt from the
// override trap (it is clamped, and it names no path), which is not the same as
// being fit to make permanent. `aiusage --interval 61 today` used to write
// `run --interval 61` into the unit, leaving a supervised daemon polling at 61
// seconds forever with nothing on the machine to explain why.
//
// The assertion is the strong one: the automatic install may bake NO flags at
// all, so a flag added to globalArgs later cannot leak in through a path nobody
// re-read. `aiusage setup --interval 61` is still the way to ask for it.
func TestAutomaticInstallNeverBakesAnInterval(t *testing.T) {
	_, dir := stubSupervisor(t)
	setFlags(t, globalFlags{interval: 61})
	clearPathEnv(t)
	calls, restore := stubSpawn(t)
	defer restore()

	if err := ensureDaemon(t.Context(), sandboxConfig(t), io.Discard); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("--interval suppressed the install; it is exempt from the trap: spawns=%d", *calls)
	}
	line := execStartLine(t, filepath.Join(dir, service.CollectUnit))
	if strings.Contains(line, "--interval") {
		t.Errorf("a one-off --interval was baked into the unit: %q", line)
	}
	if !strings.HasSuffix(line, " run") {
		t.Errorf("the automatic install baked flags into the unit: %q", line)
	}
}

// execStartLine returns the unit's ExecStart value.
func execStartLine(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	for _, ln := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(ln, "ExecStart=") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "ExecStart="))
		}
	}
	t.Fatalf("no ExecStart in %s:\n%s", path, body)
	return ""
}

// TestDoctorSupervisionCannotOutlastItsBudget: doctor asks the service manager
// several questions (availability, then enabled and active per unit), and a
// manager answering each of them in four seconds - a loaded machine, not a
// broken one - made the supervision block take a measured 20 seconds with
// nothing printed until it finished. doctor is reached BECAUSE supervision is
// suspect, so it may not hang on it: the block reports what it established and
// says the rest is unknown.
func TestDoctorSupervisionCannotOutlastItsBudget(t *testing.T) {
	const budget = 200 * time.Millisecond
	f, dir := stubSupervisor(t)
	f.delay = 150 * time.Millisecond
	setBudget(t, budget)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir units: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, service.CollectUnit), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", service.CollectUnit, err)
	}

	home := t.TempDir()
	isolateState(t)
	args := []string{"--home", home, "--config", offlineConfig(t),
		"--db", filepath.Join(t.TempDir(), "usage.db"), "--no-daemon", "doctor"}

	start := time.Now()
	out, err := runCmd(t, args...)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	// The budget bounds the supervision block; the rest of doctor (discovery,
	// database stats) is not supervision and is not being measured here.
	if limit := budget + 2*time.Second; elapsed > limit {
		t.Errorf("doctor took %v against a %v supervision budget, want under %v", elapsed, budget, limit)
	}
	if !strings.Contains(out, "state unknown") && !strings.Contains(out, "did not answer") {
		t.Errorf("doctor reported unit states it never got an answer about:\n%s", out)
	}
}

// TestSetupCannotOutlastItsBudget: the explicit command gets a deadline too,
// and a much larger one - the user asked for this and is watching it, and
// abandoning an install between writing a unit and starting it is worse than
// waiting. What it may not do is hang: whatever happened is printed, and the
// error names the deadline rather than the signal a killed process reports.
func TestSetupCannotOutlastItsBudget(t *testing.T) {
	isolateState(t)
	f, dir := stubSupervisor(t)
	f.delay = 150 * time.Millisecond
	setSetupBudget(t, 200*time.Millisecond)

	start := time.Now()
	out, err := runCmd(t, "--config", offlineConfig(t), "setup")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("setup against a manager that stopped answering succeeded:\n%s", out)
	}
	if limit := 5 * time.Second; elapsed > limit {
		t.Errorf("setup took %v with a 200ms budget, want under %v", elapsed, limit)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("the error does not say a deadline expired: %v", err)
	}
	if strings.Contains(err.Error(), "signal:") {
		t.Errorf("the error reports the signal instead of the reason: %v", err)
	}
	// Degrade, not vanish: what did happen before the deadline is on stdout.
	if !strings.Contains(out, "unit directory: "+dir) {
		t.Errorf("setup abandoned the attempt without reporting what it had done:\n%s", out)
	}
}

// TestSetupRemoveRefusalIsNotSuccess: --remove refuses a unit file aiusage did
// not write, which is right, and used to exit 0 while doing it - so
// `aiusage setup --remove && rm -rf ~/.config/systemd/user` carried on as
// though the directory were clean with both files still in it.
func TestSetupRemoveRefusalIsNotSuccess(t *testing.T) {
	_, dir := stubSupervisor(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir units: %v", err)
	}
	path := filepath.Join(dir, service.CollectUnit)
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/usr/bin/aiusage run\n"), 0o644); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	out, err := runCmd(t, "--config", offlineConfig(t), "setup", "--remove")
	if err == nil {
		t.Fatalf("--remove refused to delete a unit file and still exited zero:\n%s", out)
	}
	if !strings.Contains(out, "refusing to remove") {
		t.Errorf("the refusal was not explained:\n%s", out)
	}
	if !unitExists(dir, service.CollectUnit) {
		t.Error("--remove refused the file and deleted it anyway")
	}

	// --force is the answer it points at, and that one succeeds.
	out, err = runCmd(t, "--config", offlineConfig(t), "setup", "--remove", "--force")
	if err != nil {
		t.Fatalf("--remove --force: %v\n%s", err, out)
	}
	if unitExists(dir, service.CollectUnit) {
		t.Errorf("--force left the unit behind:\n%s", out)
	}

	// Nothing installed is not a refusal: there is nothing to refuse.
	if _, err := runCmd(t, "--config", offlineConfig(t), "setup", "--remove"); err != nil {
		t.Errorf("--remove over an empty unit directory failed: %v", err)
	}
}

// TestSetupIsNotADaemonSpawner: setup is the command that does the installing,
// so the automatic install hook must not also fire in front of it.
func TestSetupIsNotADaemonSpawner(t *testing.T) {
	if !daemonSkip["setup"] {
		t.Error(`"setup" is not in daemonSkip`)
	}
}

// TestEnsureDaemonAdoptsDetachedCollectorIntoFailedUnit: identities match, the
// unit is in failed state, and has been for longer than the backoff. The
// detached holder is stopped, the unit is reset and started, and nothing is
// respawned detached.
func TestEnsureDaemonAdoptsDetachedCollectorIntoFailedUnit(t *testing.T) {
	setVersion(t, "v9.9.9")
	f, dir := stubSupervisor(t)
	setFlags(t, globalFlags{})
	clearPathEnv(t)
	stubCollectorEvidence(t, "", true)

	dirState := t.TempDir()
	pidPath := seedLock(t, dirState)
	if err := os.WriteFile(pidPath, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	release := holdLock(t, pidPath)
	defer release()
	cfg := config.Config{PIDPath: pidPath, DBPath: filepath.Join(t.TempDir(), "usage.db")}
	daemon.WriteVersion(cfg, "v9.9.9")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir units: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, service.CollectUnit), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", service.CollectUnit, err)
	}
	f.enabled[service.CollectUnit] = true
	f.failed[service.CollectUnit] = true
	f.failedAt = int64(90 * time.Second / time.Microsecond)
	f.pid = 0 // a failed unit owns no process, so 12345 is the detached holder
	stubMonotonicNow(t, 90*time.Second+adoptionBackoff+time.Second, true)

	calls, restore := stubSpawn(t)
	defer restore()
	stopped := 0
	prevStop := stopDaemon
	stopDaemon = func(config.Config, int) error { stopped++; return nil }
	defer func() { stopDaemon = prevStop }()

	var warn bytes.Buffer
	if err := ensureDaemon(t.Context(), cfg, &warn); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if stopped != 1 {
		t.Errorf("stopDaemon calls = %d, want 1", stopped)
	}
	if !f.ran("reset-failed "+service.CollectUnit) || !f.ran("start "+service.CollectUnit) || !f.active[service.CollectUnit] {
		t.Errorf("failed unit was not reset and started: %v", f.calls)
	}
	if *calls != 0 {
		t.Errorf("spawned detached %d times after handing the collector to the unit", *calls)
	}
	if !strings.Contains(warn.String(), "reset "+service.CollectUnit) {
		t.Errorf("the reset was not reported: %q", warn.String())
	}
}

// TestEnsureDaemonLeavesARecentlyFailedUnitAlone: same shape, but the unit
// failed inside the backoff window. A unit that cannot run at all fails again
// the moment it is adopted, and acting on a fresh failure would repeat that
// stop-fail-respawn once per command; so a fresh failure waits out the window,
// whichever kind it is. The same holds when the unit is not failed at all,
// when the clock is unknown, and when a path override suppresses the
// automatic install.
func TestEnsureDaemonLeavesARecentlyFailedUnitAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failed  bool
		clockOK bool
		now     time.Duration
		flags   globalFlags
	}{
		{name: "failure inside the window", failed: true, clockOK: true, now: 90*time.Second + adoptionBackoff - time.Second},
		{name: "unit not failed", failed: false, clockOK: true, now: 90*time.Second + adoptionBackoff + time.Hour},
		{name: "clock unknown", failed: true, clockOK: false},
		{name: "path override", failed: true, clockOK: true, now: 90*time.Second + adoptionBackoff + time.Hour, flags: globalFlags{db: "/tmp/x.db"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setVersion(t, "v9.9.9")
			f, dir := stubSupervisor(t)
			setFlags(t, tc.flags)
			clearPathEnv(t)
			stubCollectorEvidence(t, "", true)

			pidPath := seedLock(t, t.TempDir())
			if err := os.WriteFile(pidPath, []byte("12345"), 0600); err != nil {
				t.Fatal(err)
			}
			release := holdLock(t, pidPath)
			defer release()
			cfg := config.Config{PIDPath: pidPath, DBPath: filepath.Join(t.TempDir(), "usage.db")}
			daemon.WriteVersion(cfg, "v9.9.9")

			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, service.CollectUnit), []byte("[Service]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			f.enabled[service.CollectUnit] = true
			f.failed[service.CollectUnit] = tc.failed
			f.failedAt = int64(90 * time.Second / time.Microsecond)
			stubMonotonicNow(t, tc.now, tc.clockOK)

			calls, restore := stubSpawn(t)
			defer restore()
			stopped := 0
			prevStop := stopDaemon
			stopDaemon = func(config.Config, int) error { stopped++; return nil }
			defer func() { stopDaemon = prevStop }()

			var warn bytes.Buffer
			if err := ensureDaemon(t.Context(), cfg, &warn); err != nil {
				t.Fatalf("ensureDaemon: %v", err)
			}
			if stopped != 0 || *calls != 0 || f.ran("start") || f.ran("reset-failed") || warn.Len() != 0 {
				t.Fatalf("holder was touched: stops=%d spawns=%d calls=%v warn=%q", stopped, *calls, f.calls, warn.String())
			}
		})
	}
}

// TestEnsureDaemonSupervisesTheReplacementAfterAMismatch: a release mismatch
// against a DETACHED holder stops it and then hands the replacement to the
// supervisor, the way a cold start does, instead of respawning detached.
func TestEnsureDaemonSupervisesTheReplacementAfterAMismatch(t *testing.T) {
	setVersion(t, "v9.9.9")
	f, dir := stubSupervisor(t)
	setFlags(t, globalFlags{})
	clearPathEnv(t)
	stubCollectorEvidence(t, "", true)

	pidPath := seedLock(t, t.TempDir())
	if err := os.WriteFile(pidPath, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	release := holdLock(t, pidPath)
	defer release()
	cfg := config.Config{PIDPath: pidPath, DBPath: filepath.Join(t.TempDir(), "usage.db")}
	daemon.WriteVersion(cfg, "v1.0.0")

	calls, restore := stubSpawn(t)
	defer restore()
	stopped := 0
	prevStop := stopDaemon
	stopDaemon = func(config.Config, int) error { stopped++; return nil }
	defer func() { stopDaemon = prevStop }()

	if err := ensureDaemon(t.Context(), cfg, io.Discard); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if stopped != 1 || *calls != 0 || !f.active[service.CollectUnit] || !unitExists(dir, service.CollectUnit) {
		t.Fatalf("stops=%d spawns=%d active=%v unit=%v calls=%v",
			stopped, *calls, f.active[service.CollectUnit], unitExists(dir, service.CollectUnit), f.calls)
	}
}
