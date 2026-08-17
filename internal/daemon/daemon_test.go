package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/model"
)

// writeFakeBinary lays down a stand-in for the daemon's executable with a
// modification time we control, so a "replacement" is unambiguous rather than
// hostage to filesystem timestamp granularity.
func writeFakeBinary(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// An upgrade landing under a running daemon must not leave the old build
// collecting. Nothing else notices on its own: the CLI's version check only
// fires when someone runs a command, so `go install` followed by walking away
// would keep superseded code polling for as long as the machine stays up.
func TestRunStopsWhenItsBinaryIsReplaced(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "aiusage")
	writeFakeBinary(t, exe, "old build", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opt := Options{
		Interval: time.Millisecond, // clamped to minInterval
		PIDPath:  filepath.Join(dir, "aiusage.pid"),
		Logger:   log.New(discard{}, "", 0),
		ExecPath: exe,
	}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, adapter.NewRegistry(idleAdapter()), newFakeStore(), adapter.DiscoverConfig{}, opt)
	}()

	waitFor(t, 3*time.Second, func() bool { return fileExists(opt.PIDPath) })
	writeFakeBinary(t, exe, "new build, different length", time.Now())

	select {
	case err := <-done:
		if !errors.Is(err, ErrBinaryReplaced) {
			t.Fatalf("Run = %v, want ErrBinaryReplaced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon kept running the superseded build after its executable was replaced")
	}

	// The restart path is only safe if it hands back a daemon-shaped exit: lock
	// released and pidfile removed, so the replacement can take both.
	if fileExists(opt.PIDPath) {
		t.Error("pidfile survived the restart exit; the new build cannot claim it")
	}
}

// An executable that cannot be stat'd is not an upgrade. `go run` deletes its
// temporary binary as soon as the parent exits, and an installer may unlink
// before it renames — restarting into a file that is not there would take the
// collector down over nothing.
func TestRunKeepsRunningWhenItsBinaryDisappears(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "aiusage")
	writeFakeBinary(t, exe, "old build", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	opt := Options{
		Interval: time.Millisecond, // clamped to minInterval
		PIDPath:  filepath.Join(dir, "aiusage.pid"),
		Logger:   log.New(discard{}, "", 0),
		ExecPath: exe,
	}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, adapter.NewRegistry(idleAdapter()), newFakeStore(), adapter.DiscoverConfig{}, opt)
	}()

	waitFor(t, 3*time.Second, func() bool { return fileExists(opt.PIDPath) })
	if err := os.Remove(exe); err != nil {
		t.Fatalf("remove: %v", err)
	}

	select {
	case err := <-done:
		t.Fatalf("daemon exited (%v) because its executable vanished; it should keep collecting", err)
	case <-time.After(2500 * time.Millisecond): // several ticks at the 1s floor
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon returned %v on cancel, want nil", err)
	}
}

func TestStampExecutable(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "aiusage")
	writeFakeBinary(t, file, "bytes", time.Now())
	empty := filepath.Join(dir, "empty")
	writeFakeBinary(t, empty, "", time.Now())

	cases := []struct {
		name string
		path string
		want bool
		why  string
	}{
		{"regular", file, true, "an ordinary executable is what we watch"},
		{"unset", "", false, "no path means the watch is off, not that the file changed"},
		{"missing", filepath.Join(dir, "absent"), false, "a file that is not there is not a replacement"},
		{"directory", dir, false, "a directory is not an executable"},
		{"empty", empty, false, "a zero-length file is a half-finished install, not a build"},
	}
	for _, c := range cases {
		if _, ok := stampExecutable(c.path); ok != c.want {
			t.Errorf("stampExecutable(%s) ok = %v, want %v: %s", c.name, ok, c.want, c.why)
		}
	}

	// Same bytes, same stamp — a stat that merely repeats must never read as a
	// replacement, or the daemon would restart on every tick.
	a, _ := stampExecutable(file)
	b, _ := stampExecutable(file)
	if !a.same(b) {
		t.Error("two stats of an unchanged file disagree; the daemon would restart every tick")
	}

	writeFakeBinary(t, file, "different bytes", time.Now().Add(time.Second))
	c, _ := stampExecutable(file)
	if a.same(c) {
		t.Error("a rewritten executable produced the same stamp; upgrades would go unnoticed")
	}
}

// TestRunLogsCanceledCycle: the daemon's cycle line must say the counts
// are partial when the cycle was cut short. The context is already cancelled,
// so the immediate first cycle truncates at the first adapter and Run
// returns without waiting on the ticker.
func TestRunLogsCanceledCycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ad := idleAdapter()

	var buf bytes.Buffer
	opt := Options{
		Interval: time.Hour,
		PIDPath:  filepath.Join(t.TempDir(), "aiusage.pid"),
		Logger:   log.New(&buf, "", 0),
	}
	if err := Run(ctx, adapter.NewRegistry(ad), newFakeStore(), adapter.DiscoverConfig{}, opt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var cycleLine string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "adapters=") {
			cycleLine = line
			break
		}
	}
	if cycleLine == "" {
		t.Fatalf("daemon logged no cycle line:\n%s", buf.String())
	}
	if !strings.Contains(cycleLine, "canceled") {
		t.Fatalf("truncated cycle logged as a normal one: %q", cycleLine)
	}
}

// ---------------------------------------------------------------------------
// daemon: single-instance lock + immediate first cycle + graceful stop.
// ---------------------------------------------------------------------------

func TestRunSingleInstanceAndImmediateCycle(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "aiusage.pid")

	ev := model.UsageEvent{
		Tool: model.ToolCodex, EventTime: refDay.Add(6 * time.Hour),
		TotalTokens: 42, DedupKey: "codex|daemon-1", Kind: model.KindUsage,
	}
	ad := &fakeAdapter{
		id:   model.ToolCodex,
		emit: func() adapter.Observation { return adapter.Observation{Events: []model.UsageEvent{ev}} },
	}
	reg := adapter.NewRegistry(ad)
	st := newFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	opt := Options{
		Interval: time.Hour, // long, so only the immediate cycle runs
		PIDPath:  pidPath,
		Logger:   log.New(discard{}, "", 0),
	}

	done := make(chan error, 1)
	go func() { done <- Run(ctx, reg, st, adapter.DiscoverConfig{}, opt) }()

	// Wait for the immediate first cycle to materialise the event and the
	// pidfile + lock to exist.
	waitFor(t, time.Second, func() bool {
		return st.stored() == 1 && fileExists(pidPath) && fileExists(pidPath+".lock")
	})

	// A second daemon on the same pidfile must fail fast on the lock.
	err2 := Run(context.Background(), reg, st, adapter.DiscoverConfig{}, opt)
	if err2 == nil {
		t.Fatalf("second daemon should have failed to acquire lock")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("daemon did not stop after cancel")
	}

	// Pidfile removed on clean shutdown.
	if fileExists(pidPath) {
		t.Fatalf("pidfile %s should be removed on shutdown", pidPath)
	}
}

// TestAcquireCollectionLockContention proves a one-shot cycle cannot interleave
// with a lock-holding daemon (the cross-process aggregate double count), gets an
// actionable error, and that the lock is usable again after release — in both
// directions.
func TestAcquireCollectionLockContention(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "aiusage.pid")

	daemonLock, err := acquireLock(pidPath)
	if err != nil {
		t.Fatalf("daemon lock: %v", err)
	}

	if _, err := AcquireCollectionLock(pidPath, "v-test"); err == nil {
		t.Fatalf("one-shot acquired the lock while the daemon holds it")
	} else if !strings.Contains(err.Error(), "already collecting") {
		t.Fatalf("contention error not actionable: %v", err)
	}

	daemonLock.release(log.New(discard{}, "", 0))

	release, err := AcquireCollectionLock(pidPath, "v-test")
	if err != nil {
		t.Fatalf("lock after daemon release: %v", err)
	}
	// While `once` holds the lock, a starting daemon must fail fast too.
	if _, err := acquireLock(pidPath); err == nil {
		t.Fatalf("daemon acquired the lock while a one-shot holds it")
	}
	release()

	lock, err := acquireLock(pidPath)
	if err != nil {
		t.Fatalf("daemon lock after one-shot release: %v", err)
	}
	lock.release(log.New(discard{}, "", 0))
}

// TestAcquireCollectionLockStampsIdentity: while a one-shot holds the
// collection lock it is indistinguishable from a running daemon (same flock),
// so it must stamp its own pid + build identity — otherwise a concurrent
// ensureDaemon reads an unrecorded version and force-restarts against a stale
// pid. Both stamps must be gone after release.
func TestAcquireCollectionLockStampsIdentity(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "aiusage.pid")

	release, err := AcquireCollectionLock(pidPath, "v-test")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := readPID(pidPath); got != os.Getpid() {
		t.Errorf("pidfile pid = %d, want own pid %d", got, os.Getpid())
	}
	if data, err := os.ReadFile(versionPath(pidPath)); err != nil || string(data) != "v-test" {
		t.Errorf("recorded version = %q (err=%v), want v-test", data, err)
	}

	release()
	if fileExists(pidPath) {
		t.Errorf("pidfile %s not removed on release", pidPath)
	}
	if fileExists(versionPath(pidPath)) {
		t.Errorf("version stamp %s not removed on release", versionPath(pidPath))
	}
}
