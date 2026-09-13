package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWayfinderStateQueriesAreKnownPerCall(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, active string
		fail, timeout, known  bool
	}{
		{"negative", "disabled", "inactive", true, false, true},
		{"runtime", "enabled-runtime", "active", false, false, true},
		{"activating", "enabled", "activating", false, false, false},
		{"deactivating", "enabled", "deactivating", false, false, false},
		{"reloading", "enabled", "reloading", false, false, false},
		{"maintenance", "enabled", "maintenance", false, false, false},
		{"unknown", "enabled", "unknown", false, false, false},
		{"malformed", "nonsense", "nonsense", false, false, false},
		{"refused", "refused", "refused", true, false, false},
		{"per-call timeout", "disabled", "inactive", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, CollectUnit), []byte(unitStamp), 0600); err != nil {
				t.Fatal(err)
			}
			m := &Manager{GOOS: "linux", UnitDir: dir, Timeout: time.Millisecond, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if tc.timeout {
					<-ctx.Done()
					return []byte(tc.active), ctx.Err()
				}
				out := tc.active
				if strings.Contains(strings.Join(args, " "), "is-enabled") {
					out = tc.enabled
				}
				if tc.fail {
					return []byte(out), errors.New("exit status 1")
				}
				return []byte(out), nil
			}}
			got := m.Status(context.Background())[0]
			if got.StateKnown != tc.known {
				t.Fatalf("status = %+v", got)
			}
		})
	}
}

func TestWayfinderRemovePreservesFileOnOperationalFailure(t *testing.T) {
	for _, verb := range []string{"is-active", "stop", "disable", "daemon-reload"} {
		t.Run(verb, func(t *testing.T) {
			f := newFake()
			f.active[CollectUnit] = true
			f.enabled[CollectUnit] = true
			f.fail[verb] = errors.New("refused")
			dir := t.TempDir()
			path := filepath.Join(dir, CollectUnit)
			if err := os.WriteFile(path, []byte(unitStamp), 0600); err != nil {
				t.Fatal(err)
			}
			m := &Manager{GOOS: "linux", UnitDir: dir, Run: f.run}
			result, err := m.Remove(t.Context(), true)
			if err == nil {
				t.Fatalf("remove unexpectedly succeeded: %+v", result)
			}
			_, statErr := os.Stat(path)
			if verb == "daemon-reload" {
				if !os.IsNotExist(statErr) || !strings.Contains(err.Error(), "removed") || !result.Changed {
					t.Fatalf("partial result = %+v, %v; file=%v", result, err, statErr)
				}
			} else if statErr != nil {
				t.Fatalf("unit removed after %s failed", verb)
			}
			if (verb == "is-active" || verb == "stop") && (!f.active[CollectUnit] || !f.enabled[CollectUnit]) {
				t.Fatal("failed precondition changed lifecycle")
			}
			if verb == "disable" && (f.active[CollectUnit] || !f.enabled[CollectUnit]) {
				t.Fatal("partial stopped/enabled state was lost")
			}
		})
	}
}

func TestWayfinderCollectorPID(t *testing.T) {
	for _, tc := range []struct {
		goos, out string
		pid       int
		known     bool
		fail      bool
	}{
		{"linux", "42", 42, true, false}, {"linux", "0", 0, true, false}, {"linux", "garbage", 0, false, false}, {"linux", "0", 0, false, true},
		{"darwin", "state = running\npid = 42", 42, true, false}, {"darwin", "Could not find service", 0, true, true}, {"darwin", "refused", 0, false, true},
	} {
		t.Run(tc.goos+tc.out, func(t *testing.T) {
			m := &Manager{GOOS: tc.goos, Run: func(context.Context, string, ...string) ([]byte, error) {
				if tc.fail {
					return []byte(tc.out), errors.New("refused")
				}
				return []byte(tc.out), nil
			}}
			pid, known := m.CollectorPID(t.Context())
			if pid != tc.pid || known != tc.known {
				t.Fatalf("pid=%d known=%v", pid, known)
			}
		})
	}
}

func TestWayfinderUnitControlsRemainOneValue(t *testing.T) {
	value := "/tmp/space % quote\" back\\\nEnvironment=INJECTED=yes\r\tend"
	rendered := renderCollect(Options{Exec: value, Args: []string{"--db", value}, DataDir: value, StateDir: value})
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "Environment=") {
			t.Fatal("value injected a directive")
		}
	}
	for _, escaped := range []string{`\x0a`, `\x0d`, `\x09`, `%%`, `\"`, `\\`} {
		if !strings.Contains(rendered, escaped) {
			t.Fatalf("missing %q: %s", escaped, rendered)
		}
	}
	f := newFake()
	dir := filepath.Join(t.TempDir(), "units")
	_, err := (&Manager{GOOS: "linux", UnitDir: dir, Run: f.run}).Install(t.Context(), Options{Exec: "/tmp/aiusage", Args: []string{"bad\x00value"}})
	if err == nil {
		t.Fatal("NUL accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid install wrote unit directory")
	}
}

func TestWayfinderSystemdParserPreservesControlValues(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native systemd parser")
	}
	parser, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze unavailable")
	}
	dir := t.TempDir()
	value := filepath.Join(dir, "space % quote\" back\\\nEnvironment=INJECTED=yes\r\tend")
	manager := &Manager{GOOS: "linux", UnitDir: filepath.Join(dir, "units"), Run: newFake().run}
	if _, err := manager.Install(t.Context(), Options{Exec: value}); err == nil {
		t.Fatal("systemd-incompatible executable path was installed")
	}
	if _, err := os.Stat(manager.UnitDir); !os.IsNotExist(err) {
		t.Fatal("rejected executable wrote a unit")
	}
	unit := filepath.Join(dir, CollectUnit)
	if err := os.WriteFile(unit, []byte(renderCollect(Options{Exec: "/bin/true", Args: []string{"--db", value}, DataDir: value, StateDir: dir})), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// verify parses this temporary unit offline. No service is installed or started.
	output, err := exec.CommandContext(ctx, parser, "verify", "--generators=no", "--man=no", unit).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd parser: %v\n%s", err, output)
	}
}

func TestWayfinderRemoveRefusesTransitionalStates(t *testing.T) {
	for _, state := range []string{"activating", "deactivating", "reloading", "maintenance"} {
		t.Run(state, func(t *testing.T) {
			f := newFake()
			f.active[CollectUnit], f.enabled[CollectUnit] = true, true
			dir := t.TempDir()
			path := filepath.Join(dir, CollectUnit)
			if err := os.WriteFile(path, []byte(unitStamp), 0600); err != nil {
				t.Fatal(err)
			}
			m := &Manager{GOOS: "linux", UnitDir: dir, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				verb, _ := verbUnit(args)
				if verb == "is-active" {
					return []byte(state), nil
				}
				return f.run(ctx, name, args...)
			}}
			result, err := m.Remove(t.Context(), true)
			if err == nil || !strings.Contains(err.Error(), "unknown") || result.Changed {
				t.Fatalf("remove = %+v, %v", result, err)
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != unitStamp {
				t.Fatalf("unit changed: %q, %v", body, err)
			}
			if !f.active[CollectUnit] || !f.enabled[CollectUnit] {
				t.Fatal("transitional state changed lifecycle")
			}
			for _, call := range f.calls {
				for _, mutation := range []string{" stop ", " disable ", " daemon-reload"} {
					if strings.Contains(call, mutation) {
						t.Fatalf("unknown state performed %s", call)
					}
				}
			}
		})
	}
}
