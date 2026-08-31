package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/daemon"
)

type fakeLaunchd struct {
	calls           []string
	domainAvailable bool
	loaded          bool
	running         bool
	disabled        bool
	failOnce        map[string]int
}

func newFakeLaunchd() *fakeLaunchd {
	return &fakeLaunchd{domainAvailable: true, failOnce: map[string]int{}}
}

func (f *fakeLaunchd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	verb := name
	if name == "launchctl" && len(args) > 0 {
		verb = args[0]
	}
	if f.failOnce[verb] > 0 {
		f.failOnce[verb]--
		return []byte(verb + " refused"), errors.New("exit status 1")
	}
	if name == "plutil" {
		return []byte(args[len(args)-1] + ": OK"), nil
	}
	if name != "launchctl" || len(args) == 0 {
		return nil, errors.New("unexpected command")
	}

	switch args[0] {
	case "print":
		if len(args) != 2 {
			return nil, errors.New("unexpected launchctl print")
		}
		if !strings.Contains(args[1], CollectLabel) {
			if f.domainAvailable {
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
		return []byte("state = " + state + "\n"), nil
	case "print-disabled":
		value := "false"
		if f.disabled {
			value = "true"
		}
		return []byte(`"` + CollectLabel + `" => ` + value + "\n"), nil
	case "enable":
		f.disabled = false
		return nil, nil
	case "disable":
		f.disabled = true
		return nil, nil
	case "bootstrap":
		f.loaded = true
		f.running = true // RunAtLoad=true.
		return nil, nil
	case "kickstart":
		f.loaded = true
		f.running = true
		return nil, nil
	case "bootout":
		f.loaded = false
		f.running = false
		return nil, nil
	default:
		return nil, errors.New("unexpected launchctl verb " + args[0])
	}
}

func (f *fakeLaunchd) ran(want string) bool {
	for _, call := range f.calls {
		if strings.Contains(call, want) {
			return true
		}
	}
	return false
}

func testLaunchdManager(t *testing.T) (*Manager, *fakeLaunchd) {
	t.Helper()
	f := newFakeLaunchd()
	return &Manager{
		GOOS:    "darwin",
		UnitDir: filepath.Join(t.TempDir(), "Library", "LaunchAgents"),
		Run:     f.run,
	}, f
}

func testLaunchdOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	return Options{
		Exec:       filepath.Join(root, "bin", "aiusage"),
		Args:       []string{"--home", filepath.Join(root, "Agent & Data")},
		DataDir:    filepath.Join(root, "data"),
		StateDir:   filepath.Join(root, "state"),
		WorkingDir: filepath.Join(root, "User Home"),
		LogPath:    filepath.Join(root, "state", "aiusage.log"),
	}
}

func writeTestLaunchAgent(t *testing.T, m *Manager) string {
	t.Helper()
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := renderLaunchAgent(testLaunchdOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.UnitDir, CollectPlist)
	if err := os.WriteFile(path, []byte(body), unitFileMode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenderedLaunchAgentCarriesLifecycleContract(t *testing.T) {
	o := testLaunchdOptions(t)
	body, err := renderLaunchAgent(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		launchdStamp,
		"<string>" + CollectLabel + "</string>",
		"<string>" + o.Exec + "</string>",
		"<string>run</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>ProcessType</key>\n  <string>Background</string>",
		"<key>ThrottleInterval</key>\n  <integer>10</integer>",
		"<string>" + o.WorkingDir + "</string>",
		"<string>" + o.LogPath + "</string>",
		"Agent &amp; Data",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("LaunchAgent missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "sh -c") {
		t.Fatal("LaunchAgent routed arguments through a shell")
	}
}

func TestLaunchdInstallIsValidatedIdempotentAndObservable(t *testing.T) {
	m, f := testLaunchdManager(t)
	o := testLaunchdOptions(t)
	if !m.Available(t.Context()) {
		t.Fatal("available fake GUI domain was rejected")
	}

	res, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.UnitDir, CollectPlist)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Collecting || !res.Changed || !hasStamp(path) || !f.loaded || !f.running {
		t.Fatalf("install = %+v, loaded=%t running=%t", res, f.loaded, f.running)
	}
	for _, want := range []string{"plutil -lint", "launchctl enable", "launchctl bootstrap", "launchctl kickstart -k", "launchctl print"} {
		if !f.ran(want) {
			t.Errorf("install never ran %q: %v", want, f.calls)
		}
	}

	f.calls = nil
	second, err := m.Install(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if second.Changed || !second.Collecting {
		t.Fatalf("idempotent install = %+v", second)
	}
	if string(after) != string(first) {
		t.Fatal("idempotent install rewrote the plist")
	}
	if f.ran("plutil") || f.ran("bootstrap") || f.ran("kickstart") {
		t.Fatalf("idempotent install reactivated a running job: %v", f.calls)
	}

	got := m.Status(t.Context())[0]
	if !got.Installed || !got.Enabled || !got.Loaded || !got.Active || !got.StateKnown || got.Persistence != PersistenceGUILogin {
		t.Fatalf("launchd status = %+v", got)
	}
}

func TestLaunchdValidationFailureLeavesNoPersistentMutation(t *testing.T) {
	m, f := testLaunchdManager(t)
	f.failOnce["plutil"] = 1
	if _, err := m.Install(t.Context(), testLaunchdOptions(t)); err == nil {
		t.Fatal("invalid plist was accepted")
	}
	if fileExists(filepath.Join(m.UnitDir, CollectPlist)) || f.loaded {
		t.Fatalf("failed validation left plist=%t loaded=%t", fileExists(filepath.Join(m.UnitDir, CollectPlist)), f.loaded)
	}
}

func TestLaunchdActivationFailureRestoresForcedReplacement(t *testing.T) {
	m, f := testLaunchdManager(t)
	old := testLaunchdOptions(t)
	old.Exec = "/old/aiusage"
	body, err := renderLaunchAgent(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.UnitDir, CollectPlist)
	if err := os.WriteFile(path, []byte(body), unitFileMode); err != nil {
		t.Fatal(err)
	}
	f.loaded = true
	f.running = true
	f.failOnce["kickstart"] = 1

	next := testLaunchdOptions(t)
	next.Force = true
	if _, err := m.Install(t.Context(), next); err == nil {
		t.Fatal("failed replacement activation reported success")
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != body {
		t.Fatalf("rollback did not restore prior plist:\n%s", restored)
	}
	if !f.loaded || !f.running {
		t.Fatalf("rollback did not restore prior running job: loaded=%t running=%t", f.loaded, f.running)
	}
}

func TestLaunchdFailedFirstActivationRemovesNewJob(t *testing.T) {
	m, f := testLaunchdManager(t)
	f.failOnce["bootstrap"] = 1
	if _, err := m.Install(t.Context(), testLaunchdOptions(t)); err == nil {
		t.Fatal("failed bootstrap reported success")
	}
	if fileExists(filepath.Join(m.UnitDir, CollectPlist)) || f.loaded {
		t.Fatalf("failed activation left plist=%t loaded=%t", fileExists(filepath.Join(m.UnitDir, CollectPlist)), f.loaded)
	}
}

func TestLaunchdPreservesUnstampedPlist(t *testing.T) {
	m, f := testLaunchdManager(t)
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.UnitDir, CollectPlist)
	handWritten := []byte("<plist><dict><!-- user owned --></dict></plist>\n")
	if err := os.WriteFile(path, handWritten, unitFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(t.Context(), testLaunchdOptions(t)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(handWritten) {
		t.Fatal("ordinary install rewrote an unstamped plist")
	}
	res, err := m.Remove(t.Context(), false)
	if err != nil || res.Refused != 1 {
		t.Fatalf("ordinary removal = %+v, %v", res, err)
	}
	if !fileExists(path) || !f.loaded {
		t.Fatal("ordinary removal altered an unstamped plist or its running job")
	}
	if _, err := m.Remove(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if fileExists(path) || f.loaded {
		t.Fatal("forced removal left the plist or loaded job")
	}
}

func TestLaunchdStopStartRestartAndRemovalPreserveData(t *testing.T) {
	m, f := testLaunchdManager(t)
	o := testLaunchdOptions(t)
	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(o.DataDir, "usage.db")
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("ledger"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.running = false
	stopped, err := m.StopCollection(t.Context())
	if err != nil || stopped || !f.loaded {
		t.Fatalf("StopCollection waiting job = %t, %v, loaded=%t", stopped, err, f.loaded)
	}
	f.running = true
	stopped, err = m.StopCollection(t.Context())
	if err != nil || !stopped || f.loaded {
		t.Fatalf("StopCollection = %t, %v, loaded=%t", stopped, err, f.loaded)
	}
	if err := m.StartCollection(t.Context()); err != nil || !f.loaded || !f.running {
		t.Fatalf("StartCollection = %v, loaded=%t running=%t", err, f.loaded, f.running)
	}
	res, err := m.Restart(t.Context())
	if err != nil || !res.Collecting {
		t.Fatalf("Restart = %+v, %v", res, err)
	}
	if _, err := m.Remove(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(m.UnitDir, CollectPlist)) || f.loaded {
		t.Fatal("removal left the LaunchAgent installed or loaded")
	}
	if got, err := os.ReadFile(data); err != nil || string(got) != "ledger" {
		t.Fatalf("removal changed data: %q, %v", got, err)
	}
}

func TestLaunchdRotatesTheConfiguredLogBeforeOpeningAReplacement(t *testing.T) {
	m, _ := testLaunchdManager(t)
	o := testLaunchdOptions(t)
	if err := os.MkdirAll(filepath.Dir(o.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.LogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(o.LogPath, daemon.MaxLogBytes+1); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Install(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.Stat(o.LogPath + ".old")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Size() != daemon.MaxLogBytes+1 {
		t.Fatalf("install rotation size = %d", rotated.Size())
	}
	if got := launchdLogPath(filepath.Join(m.UnitDir, CollectPlist)); got != o.LogPath {
		t.Fatalf("LaunchAgent log path = %q, want %q", got, o.LogPath)
	}

	if err := os.WriteFile(o.LogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(o.LogPath, daemon.MaxLogBytes+2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	rotated, err = os.Stat(o.LogPath + ".old")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Size() != daemon.MaxLogBytes+2 {
		t.Fatalf("restart rotation size = %d", rotated.Size())
	}
}

func TestLaunchdUnavailableGUIHasNoFilesystemSideEffect(t *testing.T) {
	m, f := testLaunchdManager(t)
	f.domainAvailable = false
	if m.Available(t.Context()) {
		t.Fatal("missing GUI domain reported available")
	}
	if fileExists(m.UnitDir) {
		t.Fatal("availability probe created the LaunchAgents directory")
	}
}

func TestLaunchdLifecycleFailuresRemainVisible(t *testing.T) {
	t.Run("restart unknown", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.failOnce["print"] = 1
		if _, err := m.Restart(t.Context()); err == nil {
			t.Fatal("unknown launchd state was reported as a successful restart")
		}
	})

	t.Run("restart refused", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.loaded, f.running = true, true
		f.failOnce["kickstart"] = 1
		if _, err := m.Restart(t.Context()); err == nil {
			t.Fatal("refused launchd restart was reported as successful")
		}
	})

	t.Run("stop unknown", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.failOnce["print"] = 1
		if _, err := m.StopCollection(t.Context()); err == nil {
			t.Fatal("unknown launchd state was reported as a successful stop")
		}
	})

	t.Run("stop refused", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.loaded, f.running = true, true
		f.failOnce["bootout"] = 1
		if _, err := m.StopCollection(t.Context()); err == nil {
			t.Fatal("refused launchd stop was reported as successful")
		}
	})

	t.Run("start missing", func(t *testing.T) {
		m, _ := testLaunchdManager(t)
		if err := m.StartCollection(t.Context()); err == nil {
			t.Fatal("missing LaunchAgent was reported as started")
		}
	})

	t.Run("start unknown", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.failOnce["print"] = 1
		if err := m.StartCollection(t.Context()); err == nil {
			t.Fatal("unknown launchd state was reported as a successful start")
		}
	})

	t.Run("load refused", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.failOnce["bootstrap"] = 1
		if err := m.StartCollection(t.Context()); err == nil {
			t.Fatal("refused launchd load was reported as a successful start")
		}
	})

	t.Run("start refused", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.loaded = true
		f.failOnce["kickstart"] = 1
		if err := m.StartCollection(t.Context()); err == nil {
			t.Fatal("refused launchd start was reported as successful")
		}
	})

	t.Run("remove unknown", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.failOnce["print"] = 1
		if _, err := m.Remove(t.Context(), false); err == nil {
			t.Fatal("unknown launchd state was reported as a successful removal")
		}
	})

	t.Run("remove refused", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		writeTestLaunchAgent(t, m)
		f.loaded = true
		f.failOnce["bootout"] = 1
		if _, err := m.Remove(t.Context(), false); err == nil {
			t.Fatal("refused launchd bootout was reported as a successful removal")
		}
	})
}

func TestLaunchdHelpersReportBoundaries(t *testing.T) {
	t.Run("render requirements", func(t *testing.T) {
		o := testLaunchdOptions(t)
		o.WorkingDir = ""
		if _, err := renderLaunchAgent(o); err == nil {
			t.Fatal("empty working directory was accepted")
		}
		o = testLaunchdOptions(t)
		o.LogPath = ""
		if _, err := renderLaunchAgent(o); err == nil {
			t.Fatal("empty log path was accepted")
		}
	})

	t.Run("boolean false", func(t *testing.T) {
		var b strings.Builder
		plistBool(&b, "Disabled", false)
		if got := b.String(); !strings.Contains(got, "<false/>") {
			t.Fatalf("false plist value = %q", got)
		}
	})

	t.Run("platform identity", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got := DefaultLaunchAgentDir(); got != filepath.Join(home, "Library", "LaunchAgents") {
			t.Fatalf("LaunchAgent directory = %q", got)
		}
		if got := (&Manager{GOOS: "darwin"}).Kind(); got != "launchd" {
			t.Fatalf("Darwin manager = %q", got)
		}
		if got := (&Manager{GOOS: "linux"}).Kind(); got != "systemd" {
			t.Fatalf("Linux manager = %q", got)
		}
		if (&Manager{GOOS: "windows", Run: newFakeLaunchd().run}).Available(t.Context()) {
			t.Fatal("unsupported platform reported a native manager")
		}
	})

	t.Run("log path errors", func(t *testing.T) {
		if got := launchdLogPath(filepath.Join(t.TempDir(), "missing.plist")); got != "" {
			t.Fatalf("missing plist log path = %q", got)
		}
		path := filepath.Join(t.TempDir(), "broken.plist")
		if err := os.WriteFile(path, []byte("<plist>"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := launchdLogPath(path); got != "" {
			t.Fatalf("malformed plist log path = %q", got)
		}
	})

	t.Run("manager answers", func(t *testing.T) {
		m, f := testLaunchdManager(t)
		f.failOnce["print-disabled"] = 1
		if _, known := m.launchdDisabled(t.Context()); known {
			t.Fatal("failed persistence query reported a known answer")
		}

		m.Run = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("unrelated label => true"), nil
		}
		if disabled, known := m.launchdDisabled(t.Context()); disabled || !known {
			t.Fatalf("missing label = disabled %t, known %t", disabled, known)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		m.Run = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("transient failure"), errors.New("exit status 1")
		}
		if loaded, running, known := m.launchdState(ctx); loaded || running || known {
			t.Fatalf("cancelled state = loaded %t, running %t, known %t", loaded, running, known)
		}
	})

	t.Run("atomic restore needs a parent", func(t *testing.T) {
		err := writeAtomicFile(filepath.Join(t.TempDir(), "missing", "agent.plist"), []byte("body"), 0o600)
		if err == nil {
			t.Fatal("atomic restore unexpectedly created its missing parent")
		}
	})
}
