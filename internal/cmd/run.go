package cmd

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/collect"
	"github.com/RandomCodeSpace/aiusage/internal/config"
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
func daemonOptions(cfg config.Config) collect.DaemonOptions {
	return collect.DaemonOptions{
		Interval: time.Duration(cfg.IntervalSeconds) * time.Second,
		PIDPath:  cfg.PIDPath,
		Version:  buildinfo.Identity(),
		Pricer:   newPricer(cfg),
	}
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
			return collect.RunDaemon(ctx, defaultRegistry(), st, discoverConfig(cfg), opt)
		},
	}
}
