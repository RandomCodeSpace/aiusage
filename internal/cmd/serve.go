package cmd

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/collect"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/sysmon"
	"github.com/RandomCodeSpace/aiusage/internal/web"
)

// noEmbeddedUIGuidance is what `serve` prints when the binary carries no web UI
// (issue #61). It lives out here, and not inside the error, because an error
// string is a lowercase fragment with no punctuation and no newlines
// (staticcheck ST1005) - and a five-line explanation is not an error string, it
// is operator guidance that happens to accompany one.
const noEmbeddedUIGuidance = `aiusage serve needs a build with the web UI embedded, and this one has none.

Release binaries from GitHub carry it. To build one yourself:

    cd webui && npm ci && npm run build   # writes internal/web/dist
    cd .. && go build -tags webui ./...

` + "`aiusage doctor`" + ` reports which kind of build this is, and collection is
unaffected either way: the daemon keeps recording usage without the UI.
`

// noEmbeddedUIDaemonNotice is the collector's half of the same fact. The daemon
// does not need the UI and must never stop over it, so this is one line in its
// log rather than the refusal `serve` owes the user.
const noEmbeddedUIDaemonNotice = "notice: this build has no embedded web UI, so `aiusage serve` is unavailable; collection is unaffected"

// errNoEmbeddedUI is the failure `serve` exits 1 with. The guidance above is
// already on stderr by then; this is the exit code, not the explanation.
var errNoEmbeddedUI = errors.New("serve: this build has no embedded web ui")

// newServeCmd builds the `serve` command: the local web dashboard.
//
// It is READ-ONLY by construction, which is the whole reason it can run beside a
// collecting daemon: the store handle comes from store.OpenReadOnly (mode=ro, no
// migration, no chmod), and the collection lock is never taken - a serving
// process that held it would silently stop collection for as long as a browser
// tab was open. It is also in daemonSkip, so starting a dashboard never spawns a
// daemon as a side effect; if none is running, `serve` says so and shows the
// ledger as it stands.
func newServeCmd() *cobra.Command {
	var addr string
	var allowedHosts []string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the local web dashboard",
		Long: "serve exposes the stored usage over a small read-only JSON API and, " +
			"in a build with the UI embedded, the dashboard page itself. It binds " +
			"loopback by default: the API is unauthenticated and the ledger " +
			"describes everything you have done with your agent CLIs.\n\n" +
			"Requests must be addressed to a Host this server answers to - " +
			"localhost, 127.0.0.1 or ::1 unless --allowed-hosts says otherwise - " +
			"and anything else is refused with 421. That check, not the loopback " +
			"bind, is what keeps a stranger's page out: a site can point a name it " +
			"owns at 127.0.0.1 and read this API as same-origin, but it cannot " +
			"choose the Host header the browser sends.\n\n" +
			"Behind a reverse proxy the public name has to be listed, because a " +
			"proxy preserves it: Caddy forwards the Host it was asked for, so a " +
			"deployment at aiusage.example.net runs with " +
			"--allowed-hosts aiusage.example.net.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runServe(c, addr, allowedHosts)
		},
	}
	c.Flags().StringVar(&addr, "addr", web.DefaultAddr,
		"address to bind (loopback by default; anything else publishes the ledger)")
	c.Flags().StringSliceVar(&allowedHosts, "allowed-hosts", nil,
		"extra Host header names to answer to, comma-separated (always: localhost, 127.0.0.1, ::1). "+
			"A reverse proxy preserves the public Host, so a proxied deployment needs its name here, "+
			"e.g. --allowed-hosts aiusage.randomcodespace.dev (dev: aiusage-dev.randomcodespace.dev)")
	return c
}

func runServe(c *cobra.Command, addr string, allowedHosts []string) error {
	errOut := c.ErrOrStderr()
	if !web.HasEmbeddedUI() {
		fmt.Fprint(errOut, noEmbeddedUIGuidance)
		return errNoEmbeddedUI
	}

	// Resolve the default here rather than leaving it to web.New: the warning and
	// the URL printed below have to name the address that will actually be bound.
	if strings.TrimSpace(addr) == "" {
		addr = web.DefaultAddr
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// Read-only, and refused outright on a schema this binary does not know: a
	// server that migrated the database would be a writer, and one that served a
	// schema it half understood would publish wrong numbers confidently.
	st, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open %s read-only: %w", cfg.DBPath, err)
	}
	defer st.Close()

	monitor := sysmon.New(filepath.Dir(cfg.DBPath))
	srv, err := web.New(st, web.Options{
		Addr:          addr,
		DBPath:        cfg.DBPath,
		AllowedHosts:  allowedHosts,
		ServerVersion: buildinfo.Identity(),
		Daemon:        daemonProbe(cfg),
		Resources: func() web.Resources {
			s := monitor.Sample()
			return web.Resources{
				CPU:    gaugeFraction(s.CPU),
				Memory: gaugeFraction(s.Mem),
				Disk:   gaugeFraction(s.Disk),
			}
		},
		Logger: log.New(errOut, "", log.LstdFlags),
	})
	if err != nil {
		return err
	}

	if !isLoopbackAddr(addr) {
		fmt.Fprintf(errOut, "warning: %s is not loopback; the API is unauthenticated and anyone who can reach it can read your whole usage ledger\n", addr)
	}
	if running, _ := collect.DaemonStatus(cfg); !running {
		fmt.Fprintln(errOut, "notice: no collection daemon is running; the dashboard will show the ledger as it stands (start one with `aiusage run`)")
	}
	fmt.Fprintf(c.OutOrStdout(), "aiusage dashboard on http://%s\n", addr)

	ctx, stop := signal.NotifyContext(cmdContext(c), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.ListenAndServe(ctx)
}

// daemonProbe reports the collection daemon's status to the web layer. It is a
// closure because internal/web must not import internal/collect: the daemon is a
// fact the composition root knows, not something a serving package goes looking
// for.
//
// Uptime is measured from the pidfile's modification time, which RunDaemon
// writes once at startup. It is the only start instant a separate, read-only
// process can observe without asking the kernel about someone else's process.
func daemonProbe(cfg config.Config) func() web.DaemonInfo {
	return func() web.DaemonInfo {
		running, pid := collect.DaemonStatus(cfg)
		info := web.DaemonInfo{
			Running:  running,
			PID:      pid,
			Interval: time.Duration(cfg.IntervalSeconds) * time.Second,
		}
		if running {
			if fi, err := os.Stat(cfg.PIDPath); err == nil {
				info.Uptime = time.Since(fi.ModTime())
			}
		}
		return info
	}
}

// gaugeFraction renders one sysmon gauge as the 0..1 fraction the wire carries.
// An unknown reading becomes 0, because the wire has no third state for it: CPU
// is a rate and its first sample is always unknown, so a freshly started server
// reports 0% CPU for one poll rather than inventing a number.
func gaugeFraction(g sysmon.Gauge) float64 {
	if !g.Known {
		return 0
	}
	return g.Frac
}

// isLoopbackAddr reports whether a listen address is confined to this machine.
// An empty or wildcard host is NOT loopback: ":37800" binds every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
