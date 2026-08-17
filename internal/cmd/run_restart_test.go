package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
)

// stubDaemon replaces the collection loop and the exec syscall for the duration
// of a test, returning a pointer to the options the loop was called with and to
// the path the restart handed to exec.
func stubDaemon(t *testing.T, result error) (*daemon.Options, *string) {
	t.Helper()
	origRun, origExec := runDaemon, execSelf
	t.Cleanup(func() { runDaemon, execSelf = origRun, origExec })

	var opts daemon.Options
	var execed string
	runDaemon = func(_ context.Context, _ *adapter.Registry, _ collect.Store, _ adapter.DiscoverConfig, o daemon.Options) error {
		opts = o
		return result
	}
	execSelf = func(path string) error {
		execed = path
		return nil
	}
	return &opts, &execed
}

// The daemon reports an upgrade by returning ErrBinaryReplaced; `run` must
// answer it by exec'ing the new build in place. Anything less leaves the user
// having to notice the update and restart the collector by hand, which is the
// whole problem.
func TestRunExecsIntoTheReplacementBinary(t *testing.T) {
	isolateState(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	opts, execed := stubDaemon(t, daemon.ErrBinaryReplaced)

	out, err := runCmd(t, "--db", db, "--config", offlineConfig(t), "run")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if *execed == "" {
		t.Fatal("the daemon reported a replaced binary and nothing exec'd; the old build would have simply exited")
	}
	if *execed != opts.ExecPath {
		t.Errorf("exec'd %q, want the watched executable %q", *execed, opts.ExecPath)
	}
}

// The watch is only armed if the daemon knows which file to watch, and the path
// must be resolved before the upgrade lands: once the bytes are replaced the
// running process can no longer name its own executable.
func TestRunWatchesItsOwnExecutable(t *testing.T) {
	isolateState(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	opts, _ := stubDaemon(t, nil)

	if out, err := runCmd(t, "--db", db, "--config", offlineConfig(t), "run"); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if opts.ExecPath == "" {
		t.Error("the daemon was started with no executable to watch; upgrades would go unnoticed")
	}
}

// Every other failure is still a failure: only the upgrade signal may be
// swallowed and turned into an exec.
func TestRunReturnsOrdinaryDaemonErrors(t *testing.T) {
	isolateState(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	want := errors.New("lock held by another daemon")
	_, execed := stubDaemon(t, want)

	_, err := runCmd(t, "--db", db, "--config", offlineConfig(t), "run")
	if !errors.Is(err, want) {
		t.Fatalf("run = %v, want the daemon's own error %v", err, want)
	}
	if *execed != "" {
		t.Errorf("restarted into %q on an ordinary error", *execed)
	}
}
