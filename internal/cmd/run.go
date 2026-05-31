package cmd

import (
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"aiusage/internal/buildinfo"
	"aiusage/internal/collect"
	"aiusage/internal/config"
)

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
