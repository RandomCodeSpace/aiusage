package cmd

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/web"
)

// newServeCmd builds the `serve` command: the local web dashboard.
//
// It is read-only by construction: the store handle is the same read-only one
// the TUI opens, and the collection lock is never taken, so it can sit beside
// the daemon for as long as a browser tab is open. It is NOT in daemonSkip: a
// live dashboard with nothing collecting behind it is a picture, so the usual
// pre-run starts (or installs) the collector the way `aiusage` itself does.
func newServeCmd() *cobra.Command {
	var addr string
	var allowedHosts []string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the local web dashboard",
		Long: "serve exposes the stored usage as a small read-only JSON API, a live " +
			"event stream that fires when the collector lands new rows, and the " +
			"dashboard page that renders both. It binds loopback by default: the API " +
			"is unauthenticated and the ledger describes everything you have done " +
			"with your agent CLIs.\n\n" +
			"Requests must be addressed to a Host this server answers to (localhost, " +
			"127.0.0.1 or ::1 unless --allowed-hosts says otherwise) and anything " +
			"else is refused with 421. That check, not the loopback bind, keeps a " +
			"stranger's page out: a site can point a name it owns at 127.0.0.1 and " +
			"read this API as same-origin, but it cannot choose the Host header the " +
			"browser sends. Behind a reverse proxy the public name has to be listed.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			reader, err := openTUIStore(cfg)
			if err != nil {
				return err
			}
			defer reader.Close()

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("serve: listen %s: %w", addr, err)
			}
			ctx, stop := signal.NotifyContext(cmdContext(c), os.Interrupt, syscall.SIGTERM)
			defer stop()

			srv := web.New(reader, web.Options{
				AllowedHosts: allowedHosts,
				Capabilities: toolCapabilities(),
			})
			fmt.Fprintf(c.ErrOrStderr(), "aiusage: dashboard at http://%s (read-only; Ctrl-C to stop)\n", ln.Addr())
			return srv.Run(ctx, ln)
		},
	}
	c.Flags().StringVar(&addr, "addr", web.DefaultAddr, "address to listen on")
	c.Flags().StringSliceVar(&allowedHosts, "allowed-hosts", nil,
		"additional Host names this server answers to (loopback names are always accepted)")
	return c
}
