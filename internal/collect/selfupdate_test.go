package collect

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
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

// idleAdapter emits nothing: these tests are about the daemon's lifecycle, not
// about what it collects.
func idleAdapter() *fakeAdapter {
	return &fakeAdapter{
		id: model.ToolCodex, class: model.EventLevel,
		emit: func(int) adapter.Observation { return adapter.Observation{} },
	}
}

// An upgrade landing under a running daemon must not leave the old build
// collecting. Nothing else notices on its own: the CLI's version check only
// fires when someone runs a command, so `go install` followed by walking away
// would keep superseded code polling for as long as the machine stays up.
func TestRunDaemonStopsWhenItsBinaryIsReplaced(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "aiusage")
	writeFakeBinary(t, exe, "old build", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opt := DaemonOptions{
		Interval: time.Millisecond, // clamped to minInterval
		PIDPath:  filepath.Join(dir, "aiusage.pid"),
		Logger:   log.New(discard{}, "", 0),
		ExecPath: exe,
	}
	done := make(chan error, 1)
	go func() {
		done <- RunDaemon(ctx, adapter.NewRegistry(idleAdapter()), newFakeStore(), adapter.DiscoverConfig{}, opt)
	}()

	waitFor(t, 3*time.Second, func() bool { return fileExists(opt.PIDPath) })
	writeFakeBinary(t, exe, "new build, different length", time.Now())

	select {
	case err := <-done:
		if !errors.Is(err, ErrBinaryReplaced) {
			t.Fatalf("RunDaemon = %v, want ErrBinaryReplaced", err)
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
func TestRunDaemonKeepsRunningWhenItsBinaryDisappears(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "aiusage")
	writeFakeBinary(t, exe, "old build", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	opt := DaemonOptions{
		Interval: time.Millisecond, // clamped to minInterval
		PIDPath:  filepath.Join(dir, "aiusage.pid"),
		Logger:   log.New(discard{}, "", 0),
		ExecPath: exe,
	}
	done := make(chan error, 1)
	go func() {
		done <- RunDaemon(ctx, adapter.NewRegistry(idleAdapter()), newFakeStore(), adapter.DiscoverConfig{}, opt)
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
