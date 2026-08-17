package cmd

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
)

// repairPrivatePerms tightens permissions left behind by older releases, which
// created the data dir 0755 and the DB/log 0644. store.Open only fixes files it
// touches and never re-modes an existing dir, so existing installs are repaired
// here on daemon start. Best-effort: a failure must not stop collection, and
// doctor surfaces perms that stay loose.
func repairPrivatePerms(cfg config.Config) {
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		_ = os.Chmod(dir, 0o700)
	}
	for _, p := range []string{cfg.DBPath, cfg.DBPath + "-wal", cfg.DBPath + "-shm", cfg.LogPath} {
		if p != "" {
			_ = os.Chmod(p, 0o600)
		}
	}
}

// daemonOptions builds the DaemonOptions for cfg (Logger left for the caller to
// set). It always stamps Version with buildinfo.Identity() so ensureDaemon
// restarts the daemon only when the binary actually changes; leaving it empty
// would make the recorded version never match the CLI's identity and restart the
// daemon on every CLI invocation.
func daemonOptions(cfg config.Config) daemon.Options {
	return daemon.Options{
		Interval: time.Duration(cfg.IntervalSeconds) * time.Second,
		PIDPath:  cfg.PIDPath,
		Version:  buildinfo.Identity(),
		Pricer:   newPricer(cfg),
		NoRaw:    cfg.Privacy.NoRaw,
		ExecPath: selfPath(),
	}
}

// selfPath resolves this process's own executable, or "" if it cannot be named
// — which disables the upgrade watch rather than guessing at a path to exec.
func selfPath() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	return self
}

// execSelf replaces this process with a fresh run of path, keeping the pid, the
// environment and the daemon's own arguments. A package-level var so the
// restart path is testable: the real syscall.Exec never returns.
var execSelf = func(path string) error {
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
}

// runDaemon is the collection loop, as a package-level var so the restart path
// can be driven in a test without replacing the test binary on disk.
var runDaemon = daemon.Run

// cycleOptions builds the RunOnce options for cfg, so a one-shot cycle honours
// exactly the same pricing and privacy settings the daemon does.
func cycleOptions(cfg config.Config) []collect.Option {
	opts := []collect.Option{collect.WithPricer(newPricer(cfg))}
	if cfg.Privacy.NoRaw {
		opts = append(opts, collect.WithoutRaw())
	}
	return opts
}

// newRunCmd builds the `run` command: the foreground collection daemon.
func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the collection daemon in the foreground",
		Long: "run polls every discovered AI-agent source on the configured interval " +
			"and appends observed usage to the database. It enforces a single " +
			"instance via a pidfile lock and stops gracefully on SIGINT/SIGTERM.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			repairPrivatePerms(cfg)

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			ctx, stop := signal.NotifyContext(cmdContext(c), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			opt := daemonOptions(cfg)
			opt.Logger = log.New(c.ErrOrStderr(), "", log.LstdFlags)
			err = runDaemon(ctx, defaultRegistry(), st, discoverConfig(cfg), opt)
			if !errors.Is(err, daemon.ErrBinaryReplaced) {
				return err
			}
			// An upgrade landed under us. Close the database explicitly: exec
			// replaces the process image, so the deferred close above would
			// never run and the new image would inherit a WAL nobody finished
			// with. (If the exec fails we return instead, and that deferred
			// close lands on an already-closed store, which is harmless.)
			_ = st.Close()
			if err := execSelf(opt.ExecPath); err != nil {
				return fmt.Errorf("restart into the upgraded binary %s: %w", opt.ExecPath, err)
			}
			return nil
		},
	}
}
