package cmd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
	"github.com/RandomCodeSpace/aiusage/internal/service"
	"github.com/RandomCodeSpace/aiusage/store"
	"github.com/spf13/cobra"
)

func wayfinderConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	for _, name := range discoveryEnv() {
		t.Setenv(name, "")
	}
	for _, name := range config.PathEnvNames() {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", home)
	setFlags(t, globalFlags{home: home, db: filepath.Join(home, "scratch.db"), config: filepath.Join(home, "config.json")})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func stubCollectorEvidence(t *testing.T, database string, verified bool) {
	t.Helper()
	old := collectorProcess
	collectorProcess = func(context.Context, int) (string, bool) { return database, verified }
	t.Cleanup(func() { collectorProcess = old })
}

func TestWayfinderCompletionCreatesNoState(t *testing.T) {
	for _, args := range [][]string{{"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"}, {"__complete", ""}, {"__completeNoDesc", ""}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cfg := wayfinderConfig(t)
			f, _ := stubSupervisor(t)
			calls, restore := stubSpawn(t)
			defer restore()
			out, err := runCmd(t, append([]string{"--db", cfg.DBPath, "--home", cfg.Home, "--config", flags.config}, args...)...)
			if err != nil || out == "" {
				t.Fatalf("completion = %v, %q", err, out)
			}
			if *calls != 0 || len(f.calls) != 0 {
				t.Fatalf("completion lifecycle: spawns=%d manager=%v", *calls, f.calls)
			}
			entries, err := os.ReadDir(cfg.Home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("completion created %v", entries)
			}
		})
	}
}

func TestWayfinderExplicitPathsStayAbsoluteInChild(t *testing.T) {
	cfg := wayfinderConfig(t)
	t.Chdir(cfg.Home)
	f := globalFlags{home: "./sandbox space %", db: "ledger %.db", config: "config.json"}
	resolved, err := resolveGlobalPaths(f)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := loadConfigFlags(resolved)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := supervisionOptions(parent, globalArgs(resolved))
	if err != nil {
		t.Fatal(err)
	}
	if opts.DataDir != cfg.Home || opts.StateDir != filepath.Dir(parent.PIDPath) {
		t.Fatalf("wrong writable directories: %+v", opts)
	}
	t.Chdir(t.TempDir())
	child, err := loadConfigFlags(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if child.DBPath != parent.DBPath || child.Home != parent.Home || child.PIDPath != parent.PIDPath {
		t.Fatalf("child=%+v parent=%+v", child, parent)
	}
}

func TestWayfinderFreshInitializationPrecedesSpawn(t *testing.T) {
	cfg := wayfinderConfig(t)
	previous := spawnDaemon
	t.Cleanup(func() { spawnDaemon = previous })
	spawnDaemon = func(got config.Config) error {
		result, err := store.Verify(t.Context(), got.DBPath)
		if err != nil || result.State != store.VerificationOK {
			t.Fatalf("spawn saw unfinished store: %+v %v", result, err)
		}
		return nil
	}
	if _, err := runCmd(t, "--home", cfg.Home, "--db", cfg.DBPath, "--config", flags.config, "today"); err != nil {
		t.Fatal(err)
	}
}

func TestWayfinderConcurrentFreshOpenWaitsForLock(t *testing.T) {
	cfg := wayfinderConfig(t)
	release, err := daemon.AcquireCollectionLock(cfg.PIDPath, buildinfo.Identity())
	if err != nil {
		t.Fatal(err)
	}
	// A second caller observes the empty file while the first owns initialization.
	if err := os.WriteFile(cfg.DBPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		st, err := openStore(cfg)
		if err == nil {
			err = st.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("open escaped initialization lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	st, err := openStoreLocked(cfg)
	if err != nil {
		release()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result, err := store.Verify(t.Context(), cfg.DBPath)
	if err != nil || result.State != store.VerificationOK {
		t.Fatalf("verify=%+v %v", result, err)
	}
}

func TestWayfinderLegacyLockTransition(t *testing.T) {
	for _, tc := range []struct {
		name              string
		held, known, same bool
		spawns            int
		wantErr           bool
	}{
		{"free", false, true, false, 1, false}, {"same ledger", true, true, true, 0, false}, {"other ledger", true, true, false, 1, false}, {"unknown", true, false, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := wayfinderConfig(t)
			f, _ := stubSupervisor(t)
			_ = f
			database := filepath.Join(cfg.Home, "other.db")
			if tc.same {
				database = cfg.DBPath
			}
			stubCollectorEvidence(t, database, tc.known)
			legacy := cfg
			legacy.PIDPath = cfg.LegacyPIDPath()
			if tc.held {
				release, err := daemon.AcquireCollectionLock(legacy.PIDPath, buildinfo.Identity())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			calls, restore := stubSpawn(t)
			defer restore()
			var warning bytes.Buffer
			err := ensureDaemon(t.Context(), cfg, &warning)
			if (err != nil) != tc.wantErr || *calls != tc.spawns {
				t.Fatalf("err=%v spawns=%d warning=%s", err, *calls, warning.String())
			}
			if tc.held {
				if running, _ := daemon.Status(legacy); !running {
					t.Fatal("legacy collector was changed")
				}
			}
			if tc.same && !strings.Contains(warning.String(), legacy.PIDPath) {
				t.Fatal("legacy state not reported")
			}
			if tc.wantErr && !strings.Contains(err.Error(), "legacy holder") {
				t.Fatal(err)
			}
		})
	}
}

func TestWayfinderCollectorRechecksLegacyBeforeAcquiring(t *testing.T) {
	cfg := wayfinderConfig(t)
	stubSupervisor(t)
	stubCollectorEvidence(t, cfg.DBPath, true)
	if _, same, err := legacyCollector(t.Context(), cfg); err != nil || same {
		t.Fatalf("initial observation %v %v", same, err)
	}
	release, err := daemon.AcquireCollectionLock(cfg.LegacyPIDPath(), buildinfo.Identity())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if acquired, err := acquireCollectorLock(t.Context(), cfg); err == nil {
		acquired()
		t.Fatal("second collector ignored intervening legacy owner")
	}
}

func TestWayfinderNamespacedLockRemainsHeldAfterGuard(t *testing.T) {
	cfg := wayfinderConfig(t)
	release, started, err := acquireCollectorStartupLock(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	legacy := cfg
	legacy.PIDPath = cfg.LegacyPIDPath()
	if running, _ := daemon.Status(legacy); !running {
		t.Fatal("startup did not retain legacy guard")
	}
	started()
	if second, err := daemon.AcquireCollectionLock(cfg.PIDPath, "test"); err == nil {
		second()
		t.Fatal("handoff released namespaced lock")
	}

	if running, _ := daemon.Status(legacy); running {
		t.Fatal("transition guard still owns default collector")
	}
}

func TestWayfinderMaintenanceUsesPIDOwnership(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		nativePID                         int
		queryFails, verified, stopFails   bool
		wantNative, wantDetached, wantErr bool
	}{
		{"native despite db flag", 42, false, false, false, true, false, false},
		{"detached beside unrelated native", 99, false, true, false, false, true, false},
		{"unknown manager", 0, true, true, false, false, false, true},
		{"unverified process", 99, false, false, false, false, false, true},
		{"failed native stop", 42, false, false, true, false, false, true},
		{"detached with inactive native", 0, false, true, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := wayfinderConfig(t)
			cfg.PIDPath = filepath.Join(cfg.Home, "explicit.pid")
			f, dir := stubSupervisor(t)
			f.pid = tc.nativePID
			f.active[service.CollectUnit] = tc.nativePID > 0
			if tc.queryFails {
				f.fail["show"] = errors.New("refused")
			}
			if tc.stopFails {
				f.fail["stop"] = errors.New("refused")
			}
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, service.CollectUnit), []byte("unit"), 0600); err != nil {
				t.Fatal(err)
			}
			release := holdLock(t, cfg.PIDPath)
			defer release()
			if err := os.WriteFile(cfg.PIDPath, []byte("42"), 0600); err != nil {
				t.Fatal(err)
			}
			stubCollectorEvidence(t, cfg.DBPath, tc.verified)
			oldStop := stopDaemon
			direct := 0
			stopDaemon = func(config.Config, int) error { direct++; return nil }
			defer func() { stopDaemon = oldStop }()
			prior, err := pauseCollectionForMaintenance(t.Context(), cfg)
			if (err != nil) != tc.wantErr || prior.nativeStopped != tc.wantNative || prior.detached != tc.wantDetached {
				t.Fatalf("prior=%+v err=%v calls=%v", prior, err, f.calls)
			}
			if direct != boolInt(tc.wantDetached) {
				t.Fatalf("direct stops=%d", direct)
			}
			if tc.wantErr {
				return
			}
			spawns, restore := stubSpawn(t)
			defer restore()
			if err := resumeCollectionAfterMaintenance(t.Context(), cfg, prior, io.Discard); err != nil {
				t.Fatal(err)
			}
			if *spawns != boolInt(tc.wantDetached) {
				t.Fatalf("resume spawned %d", *spawns)
			}
			if f.ran("enable "+service.CollectUnit) || f.ran("daemon-reload") {
				t.Fatal("resume installed/enabled a unit")
			}
			if tc.nativePID == 99 && !f.active[service.CollectUnit] {
				t.Fatal("unrelated native unit stopped")
			}
		})
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

type failingExportFile struct {
	bytes.Buffer
	chmodErr, writeErr, closeErr error
	truncated, closed            bool
}

func (f *failingExportFile) Chmod(os.FileMode) error { return f.chmodErr }
func (f *failingExportFile) Truncate(int64) error    { f.truncated = true; return nil }
func (f *failingExportFile) Close() error            { f.closed = true; return f.closeErr }
func (f *failingExportFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.Buffer.Write(p)
}

func TestWayfinderRawExportPermissionFailureDoesNotTruncate(t *testing.T) {
	sentinel := errors.New("chmod refused")
	file := &failingExportFile{chmodErr: sentinel}
	previous := openExportFile
	t.Cleanup(func() { openExportFile = previous })
	openExportFile = func(_ string, flags int, _ os.FileMode) (exportFile, error) {
		if flags&os.O_TRUNC != 0 {
			t.Fatal("raw file opened with truncation")
		}
		return file, nil
	}
	_, _, err := exportWriter(&cobra.Command{}, "target", true)
	if !errors.Is(err, sentinel) || !file.closed || file.truncated || file.Len() != 0 {
		t.Fatalf("err=%v file=%+v", err, file)
	}
}
func TestWayfinderExportCloseErrorsReachCommand(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		t.Run(fmt.Sprint(writeFails), func(t *testing.T) {
			cfg := wayfinderConfig(t)
			closed := errors.New("close failed")
			written := errors.New("write failed")
			file := &failingExportFile{closeErr: closed}
			if writeFails {
				file.writeErr = written
			}
			old := openExportFile
			t.Cleanup(func() { openExportFile = old })
			openExportFile = func(string, int, os.FileMode) (exportFile, error) { return file, nil }
			cmd := &cobra.Command{}
			err := runExport(cmd, exportOpts{format: "json", out: filepath.Join(cfg.Home, "export.json")})
			if !errors.Is(err, closed) || !file.closed || (writeFails && !errors.Is(err, written)) {
				t.Fatalf("error=%v file=%+v", err, file)
			}
		})
	}
}
func TestWayfinderRawExportTightensExistingSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	w, closeFn, err := exportWriter(&cobra.Command{}, link, true)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("permissions not tightened before write")
	}
	if _, err := io.WriteString(w, "new"); err != nil {
		t.Fatal(err)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("target=%q %v", got, err)
	}
}

type discoveryResultAdapter struct {
	failingAdapter
	id      string
	sources []adapter.Source
	err     error
}

func (a discoveryResultAdapter) ID() string          { return a.id }
func (a discoveryResultAdapter) DisplayName() string { return a.id }
func (a discoveryResultAdapter) Discover(context.Context, adapter.DiscoverConfig) ([]adapter.Source, error) {
	return a.sources, a.err
}
func TestWayfinderSourcesKeepsPartialResultsAndFails(t *testing.T) {
	cfg := wayfinderConfig(t)
	sentinel := errors.New("probe failed")
	old := sourcesRegistry
	t.Cleanup(func() { sourcesRegistry = old })
	sourcesRegistry = func() *adapter.Registry {
		return adapter.NewRegistry(
			discoveryResultAdapter{id: "failed", err: sentinel},
			discoveryResultAdapter{id: "partial", err: sentinel, sources: []adapter.Source{{Path: "partial-source"}}},
			discoveryResultAdapter{id: "ok", sources: []adapter.Source{{Path: "good-source"}}},
		)
	}
	out, err := runCmd(t, "--no-daemon", "--home", cfg.Home, "--db", cfg.DBPath, "--config", flags.config, "sources")
	if !errors.Is(err, sentinel) || strings.Contains(out, "no sources found") || !strings.Contains(out, "partial-source") || !strings.Contains(out, "good-source") || !strings.Contains(out, "Stored usage") {
		t.Fatalf("sources err=%v output=%s", err, out)
	}
}

func TestWayfinderDarwinInvocationEvidence(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		db    string
		known bool
	}{
		{[]string{"run", "--db", "/tmp/scratch.db"}, "/tmp/scratch.db", true},
		{[]string{"run", "--db=/tmp/scratch.db"}, "/tmp/scratch.db", true},
		{[]string{"run"}, "", true},
		{[]string{"run", "--db", "/tmp/with spaces.db"}, "/tmp/with spaces.db", true},
		{[]string{"today", "--db", "/tmp/scratch.db"}, "", false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			old := readDarwinProcessArgs
			t.Cleanup(func() { readDarwinProcessArgs = old })
			raw := make([]byte, 4)
			binary.NativeEndian.PutUint32(raw, uint32(len(tc.args)+1))
			raw = append(raw, []byte("/tmp/aiusage\x00\x00/tmp/aiusage\x00"+strings.Join(tc.args, "\x00")+"\x00")...)
			readDarwinProcessArgs = func(int) ([]byte, error) { return raw, nil }
			db, known := darwinCollectorProcess(t.Context(), 42)
			if db != tc.db || known != tc.known {
				t.Fatalf("got %q,%v", db, known)
			}
		})
	}
}

func TestWayfinderLegacyVersionRestartPreservesMode(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			cfg := wayfinderConfig(t)
			setVersion(t, "v9.9.9")
			f, dir := stubSupervisor(t)
			stubCollectorEvidence(t, cfg.DBPath, true)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, service.CollectUnit), []byte("unit"), 0600); err != nil {
				t.Fatal(err)
			}
			legacy := cfg
			legacy.PIDPath = cfg.LegacyPIDPath()
			release, err := daemon.AcquireCollectionLock(legacy.PIDPath, "v1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			released := false
			releaseOnce := func() {
				if !released {
					release()
					released = true
				}
			}
			defer releaseOnce()
			f.active[service.CollectUnit] = native
			if native {
				f.pid = os.Getpid()
			}
			oldSupervisor := newSupervisor
			t.Cleanup(func() { newSupervisor = oldSupervisor })
			newSupervisor = func() *service.Manager {
				m := oldSupervisor()
				run := m.Run
				m.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
					out, err := run(ctx, name, args...)
					if err == nil && strings.Contains(strings.Join(args, " "), "stop "+service.CollectUnit) {
						releaseOnce()
					}
					return out, err
				}
				return m
			}
			oldStop := stopDaemon
			stops := 0
			stopDaemon = func(got config.Config, _ int) error {
				stops++
				if got.PIDPath != legacy.PIDPath {
					t.Fatal("stopped wrong identity")
				}
				releaseOnce()
				return nil
			}
			defer func() { stopDaemon = oldStop }()
			oldSpawn := spawnDaemon
			spawns := 0
			spawnDaemon = func(got config.Config) error {
				spawns++
				if got.PIDPath != cfg.PIDPath {
					t.Fatal("restart did not use new identity")
				}
				return nil
			}
			defer func() { spawnDaemon = oldSpawn }()
			if err := ensureDaemon(t.Context(), cfg, io.Discard); err != nil {
				t.Fatal(err)
			}
			if native {
				if stops != 0 || spawns != 0 || !f.ran("stop "+service.CollectUnit) || !f.ran("start "+service.CollectUnit) {
					t.Fatalf("native restart direct=%d spawn=%d calls=%v", stops, spawns, f.calls)
				}
			} else if stops != 1 || spawns != 1 {
				t.Fatalf("detached stop=%d spawn=%d", stops, spawns)
			}
			if f.ran("enable "+service.CollectUnit) || f.ran("daemon-reload") {
				t.Fatal("version restart enabled or installed a unit")
			}
		})
	}
}

func TestWayfinderLinuxInvocationNeedsOpenLedgerProof(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux proc format")
	}
	cfg := wayfinderConfig(t)
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	pidRoot := filepath.Join(root, "42")
	for _, dir := range []string{"fd", "fdinfo"} {
		if err := os.MkdirAll(filepath.Join(pidRoot, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/tmp/aiusage", filepath.Join(pidRoot, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidRoot, "cmdline"), []byte("aiusage\x00run\x00--config\x00changed-config.json\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfg.DBPath, filepath.Join(pidRoot, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	old := collectorProcRoot
	collectorProcRoot = root
	defer func() { collectorProcRoot = old }()
	for _, tc := range []struct{ flags, want string }{{"0100000", ""}, {"0100002", cfg.DBPath}} {
		if err := os.WriteFile(filepath.Join(pidRoot, "fdinfo", "3"), []byte("flags:\t"+tc.flags+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		db, collector := inspectCollectorProcess(t.Context(), 42)
		if db != tc.want || !collector {
			t.Fatalf("flags=%s got %q,%v", tc.flags, db, collector)
		}
	}
}

func TestWayfinderUnknownLegacyStillReportsStoredData(t *testing.T) {
	cfg := wayfinderConfig(t)
	stubSupervisor(t)
	stubCollectorEvidence(t, "", false)
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	release, err := daemon.AcquireCollectionLock(cfg.LegacyPIDPath(), buildinfo.Identity())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	calls, restore := stubSpawn(t)
	defer restore()
	out, err := runCmd(t, "--db", cfg.DBPath, "--home", cfg.Home, "--config", flags.config, "today")
	if err != nil || *calls != 0 || !strings.Contains(out, "legacy holder") || !strings.Contains(out, "warning: could not start daemon") {
		t.Fatalf("stored report err=%v spawns=%d output=%s", err, *calls, out)
	}
}
