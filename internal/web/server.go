// Package web serves the embedded dashboard: a read-only JSON API over the
// ledger, a Server-Sent Events stream that fires when the collector lands new
// rows, and the static page that renders both. It reads through the same
// read-only handle the TUI uses and never writes, so it can run beside a
// collecting daemon indefinitely.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

//go:embed static
var static embed.FS

// DefaultAddr is where `aiusage serve` listens unless told otherwise. Loopback
// only: the API is unauthenticated and the ledger describes everything the
// user has done with their agent CLIs.
const DefaultAddr = "127.0.0.1:8930"

// DefaultPoll is how often the live stream re-reads the ledger watermark. One
// indexed row read; cheap enough to run well under the collector's cadence so
// a push follows a pass by at most this much.
const DefaultPoll = 5 * time.Second

// Source is the slice of the store the dashboard reads. It is an interface so
// tests can hand the server a fake, and it is narrow on purpose: nothing here
// can write.
type Source interface {
	Summarize(ctx context.Context, f store.Filter) (*store.Summary, error)
	UnpricedGroups(ctx context.Context, f store.Filter) ([]store.UnpricedGroup, error)
	SummarizeRollup(ctx context.Context, f store.Filter) (*store.RollupSummary, error)
	SummarizeTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter) (*store.TurnContextSummary, error)
	LastEventTimes(ctx context.Context) (map[string]time.Time, error)
	IngestWatermark(ctx context.Context) (time.Time, error)
	RollupStale(ctx context.Context) (bool, error)
}

var _ Source = (*store.Reader)(nil)

// Options configures a Server. Every field has a usable zero value.
type Options struct {
	// AllowedHosts extends the Host allow-list beyond the loopback names.
	AllowedHosts []string
	// Poll overrides DefaultPoll.
	Poll time.Duration
	// Now is the clock; tests pin it.
	Now func() time.Time
	// Capabilities describes each tool's cost provenance and verification
	// tier, keyed by tool id, the way the TUI receives them.
	Capabilities map[string]model.ToolCapability
}

// Server is the dashboard: handlers plus the live watermark poller.
type Server struct {
	src   Source
	hosts hostSet
	now   func() time.Time
	poll  time.Duration
	caps  map[string]model.ToolCapability
	live  *broadcaster
	mux   http.Handler
}

// New builds a Server over src.
func New(src Source, o Options) *Server {
	s := &Server{
		src:   src,
		hosts: newHostSet(o.AllowedHosts),
		now:   o.Now,
		poll:  o.Poll,
		caps:  o.Capabilities,
		live:  newBroadcaster(),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.poll <= 0 {
		s.poll = DefaultPoll
	}
	s.mux = s.routes()
	return s
}

// Handler is the full HTTP surface, host guard included.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() http.Handler {
	pages, err := fs.Sub(static, "static")
	if err != nil {
		panic("web: embedded static tree is missing: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, pages, "index.html")
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(pages)))
	mux.HandleFunc("GET /api/now", s.handleNow)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /events", s.handleEvents)
	return s.guardHost(secureHeaders(mux))
}

// secureHeaders is the page's content policy. The page loads its chart library
// and its typeface from two named CDNs and nothing else; ledger strings (project
// paths, model ids, MCP tool names) are rendered as text nodes, and the policy
// is the second line against one of them ever being interpreted as markup.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; "+
				"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; "+
				"connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// Run serves on ln until ctx is cancelled, then shuts down. Request contexts
// derive from ctx, which is what lets an open event stream end: net/http's
// Shutdown waits for idle connections, and a stream is never idle.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	if wm, err := s.src.IngestWatermark(ctx); err == nil {
		s.live.set(wm)
	}
	go s.live.poll(ctx, s.src, s.poll)

	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err := srv.Serve(ln)
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	snap, err := s.buildNow(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, snap)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, err := s.buildHistory(r.Context(), q.Get("dim"), q.Get("range"))
	if err != nil {
		var bad *badRequest
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusBadRequest)
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, h)
}

// handleEvents is the live stream: one `tick` event carrying the ledger
// watermark on connect, one more each time the watermark moves, and a comment
// line every 25 seconds so an idle proxy does not close the socket. The client
// owns the reaction (a refetch); the stream carries no data of its own.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusNotImplemented)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, unsubscribe := s.live.subscribe()
	defer unsubscribe()

	send := func(wm time.Time) bool {
		payload, _ := json.Marshal(struct {
			Watermark time.Time `json:"watermark,omitzero"`
		}{wm})
		if _, err := w.Write([]byte("event: tick\ndata: " + string(payload) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send(s.live.current()) {
		return
	}
	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case wm := <-ch:
			if !send(wm) {
				return
			}
		case <-keep.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

type badRequest struct{ msg string }

func (b *badRequest) Error() string { return b.msg }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		// Headers are gone; nothing useful left to say to the client.
		return
	}
}

func fail(w http.ResponseWriter, err error) {
	http.Error(w, "aiusage: "+err.Error(), http.StatusInternalServerError)
}
