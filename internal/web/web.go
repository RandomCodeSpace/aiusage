// Package web serves the local dashboard: a small read-only JSON API over the
// stored ledger, a live channel that says when a collection cycle landed, and
// (under the webui build tag) the embedded single-page UI.
//
// It is a PEER of internal/report and internal/tui, not a layer above them: it
// imports internal/store and internal/model and nothing else of this project.
// Anything it needs from a higher layer - the daemon's status, machine resource
// gauges, the build identity - arrives through Options as a value or a function,
// because those are facts the composition root knows and a serving package must
// not go looking for.
//
// Three properties hold for every response, and the tests exist to keep them:
//
//   - THE PROCESS CANNOT WRITE. It is handed a store.OpenReadOnly handle and
//     never takes the collection lock. A serving surface that could append to
//     an append-only ledger would be the only way to corrupt it.
//   - RAW NEVER LEAVES. No endpoint projects usage_events.raw under any
//     parameter: the wire types have no field for it and no handler passes
//     store.WithRaw. Rows appended before the usage-object allow-list landed
//     still hold whole transcript lines, and this surface is unauthenticated.
//   - AGGREGATION HAPPENS IN SQL. /api/summary and /api/facets answer from the
//     derived rollup when it can answer exactly AND covers the ledger, and from
//     the ledger when it cannot; the answer says which. The wire carries
//     buckets; a 300k-row ledger never crosses it. /api/events is the one
//     row-level endpoint and it is capped.
//
// One property holds for every REQUEST: it must be addressed to a Host this
// server answers to (host.go), or it is refused with 421 before a handler sees
// it. Loopback binding alone does not keep a stranger's page out of an
// unauthenticated API - DNS rebinding makes that page same-origin.
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// ContractVersion is the version of the JSON wire shapes in wire.go. The page
// captures it at boot and reloads itself the moment a later /api/meta disagrees,
// so a long-lived tab never decodes a newer wire with older code. Bump it when a
// shape changes in a way that existing UI code cannot read.
const ContractVersion = 1

// DefaultAddr is the loopback address `aiusage serve` binds by default. Loopback
// is not a default to be casually overridden: the API is unauthenticated and the
// ledger describes everything the user has done with their agent CLIs.
const DefaultAddr = "127.0.0.1:37800"

// EventsPageLimit is the HARD server-side cap on rows returned by /api/events.
// A request for more is clamped to it; a range holding more is reported as
// truncated with the true count, never silently sliced.
const EventsPageLimit = 1000

// defaultPollInterval is how often the live watcher stats the database. The
// collection interval is 60s at its shortest, so this is two orders of magnitude
// finer than the thing it watches for and still costs two stat calls a second.
const defaultPollInterval = time.Second

// Reader is the slice of the store this package needs: reads only. It is
// declared here, at the consumer, so a test can drive the handlers with a
// hand-built double and so the write half of store.Store is not even in scope.
type Reader interface {
	Summarize(ctx context.Context, f store.Filter) (*store.Summary, error)
	SummarizeRollup(ctx context.Context, f store.Filter) (*store.RollupSummary, error)
	// RollupStale reports whether the derived rollup covers the ledger. This
	// process cannot repair it, which is exactly why it has to ask: a rollup
	// the v4 migration created empty answers every question with zeros, and
	// only a collection pass fills it.
	RollupStale(ctx context.Context) (bool, error)
	ListEvents(ctx context.Context, f store.Filter, opts ...store.ListOption) ([]model.UsageEvent, error)
	Stats(ctx context.Context) (store.DBStats, error)
	IngestWatermark(ctx context.Context) (time.Time, error)
}

// DaemonInfo is what the serving process is told about the collection daemon.
// The daemon is a separate process and this one is read-only, so every field is
// an observation the composition root made, not something web can look up.
type DaemonInfo struct {
	Running  bool
	PID      int
	Uptime   time.Duration
	Interval time.Duration
}

// Resources are the machine gauges shown in the status bar, each a 0..1
// fraction. Supplied by the composition root (internal/sysmon lives above this
// package's import line).
type Resources struct {
	CPU    float64
	Memory float64
	Disk   float64
}

// Options configures a Server. Everything except the Reader is optional: the
// zero value serves correct, honest responses with empty daemon and resource
// readings, which is exactly what a test wants.
type Options struct {
	// Addr is the listen address for ListenAndServe. Empty means DefaultAddr.
	Addr string
	// DBPath is the database file the live watcher stats for collection cycles.
	// Empty disables the watcher; the API still serves.
	DBPath string
	// AllowedHosts extends the Host header allowlist beyond localhost,
	// 127.0.0.1 and ::1. A deployment reached through a reverse proxy needs its
	// public name here: proxies preserve the public Host, and a Host this
	// server does not answer to is refused with 421. Ports are ignored.
	AllowedHosts []string
	// ServerVersion is the build identity reported by /api/meta.
	ServerVersion string
	// Daemon reports the collection daemon's status. Nil means "not known",
	// which serves zeros rather than inventing a running daemon.
	Daemon func() DaemonInfo
	// Resources reports the machine gauges. Nil serves zeros.
	Resources func() Resources
	// Logger receives serve-time diagnostics. Nil discards them.
	Logger *log.Logger
	// PollInterval overrides how often the watcher stats the database.
	PollInterval time.Duration
	// Now is the clock, a seam for tests. Nil means time.Now.
	Now func() time.Time
}

// Server is the HTTP surface. Build it with New, mount it with Handler, or let
// ListenAndServe own the listener and the live watcher.
type Server struct {
	reader Reader
	opt    Options
	now    func() time.Time
	log    *log.Logger
	hub    *hub
	mux    *http.ServeMux
	// gate caches whether the derived rollup covers the ledger, so the routing
	// decision does not cost two aggregate queries per request. See rollup.go.
	gate rollupGate
	// hosts is the Host header allowlist every request is matched against.
	hosts hostSet
	// handler is mux behind the Host guard: what Handler and ServeListener
	// serve, and the only form in which these routes may be reached.
	handler http.Handler
}

// New builds a Server over r. It does not touch the network or the filesystem.
func New(r Reader, o Options) (*Server, error) {
	if r == nil {
		return nil, errors.New("web: nil store reader")
	}
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	logger := o.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	s := &Server{reader: r, opt: o, now: now, log: logger, hub: newHub()}
	s.hosts = newHostSet(o.AllowedHosts)
	s.mux = s.routes()
	s.handler = s.guardHost(s.mux)
	return s, nil
}

// routes wires the mux. Every /api path is claimed here, including the ones that
// do not exist: without the "/api/" catch-all a typo would fall through to the
// SPA and answer a JSON request with an HTML page.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/meta", s.handleMeta)
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/facets", s.handleFacets)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/ws", s.handleWS)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	mux.Handle("/", uiHandler())
	return mux
}

// Handler returns the mounted routes behind the Host allowlist. Exported so a
// test (or an embedding process) can serve them without binding a port - and it
// returns the GUARDED handler, because an embedder that mounted the bare mux
// would silently drop the rebinding defence.
func (s *Server) Handler() http.Handler { return s.handler }

// HasEmbeddedUI reports whether this build carries the single-page UI. It is the
// webui build tag, surfaced as a value so the command layer can refuse to serve
// a dashboard that is not in the binary.
func HasEmbeddedUI() bool { return hasEmbeddedUI }

// stubUIHandler answers page requests in a build with no embedded UI. It states
// the fact rather than 404ing anonymously: the API on this port is working, and
// the difference between "wrong URL" and "this binary has no page" is the whole
// diagnosis. Shared by embed_stub.go and by embed_webui.go's degraded path.
func stubUIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "this build has no embedded web ui; the api under /api is available")
	})
}

// ListenAndServe binds Options.Addr and serves until ctx is cancelled, then
// shuts down gracefully. It also starts the live watcher, which is the only
// background work this package does.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opt.Addr)
	if err != nil {
		return fmt.Errorf("web: listen on %s: %w", s.opt.Addr, err)
	}
	return s.ServeListener(ctx, ln)
}

// ServeListener runs the HTTP server and the live watcher over a listener the
// caller already owns, until ctx is cancelled. It is what ListenAndServe is
// built from, and it is exported so a test can bind port 0 and drive the real
// server rather than a mounted handler.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go s.watch(watchCtx)

	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /api/ws hijacks the connection and holds it for the
		// life of the page. A write deadline would sever the live channel on a
		// timer for no benefit; the read header timeout still bounds a client
		// that connects and says nothing.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("web: serve: %w", err)
	case <-ctx.Done():
	}

	// Cancelled: stop accepting, let in-flight requests finish, then cut the
	// hijacked live connections that Shutdown cannot see.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	s.hub.closeAll()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("web: shutdown: %w", err)
	}
	return nil
}
