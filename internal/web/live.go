package web

import (
	"context"
	"os"
	"sync"
	"time"
)

// The live channel.
//
// This process is READ-ONLY and the collector is a different process, so there
// is no callback to hook and no notification to subscribe to. What there is, is
// a file: SQLite in WAL mode lands a commit in <db>-wal and leaves the main file
// untouched until a checkpoint, which can be many minutes later or never while
// the daemon holds the database open. So the write time is the NEWEST of the two
// files, and a cycle that changed something is a change in that time. (The TUI
// reaches the same conclusion in internal/tui/live.go; the code is deliberately
// duplicated rather than shared, because internal/web must not import the TUI.)
//
// A cycle that inserted nothing leaves no trace at all. That is not a gap in the
// detection - it is the honest position: nothing changed, so there is nothing to
// tell a client that already has the previous answer.

// hub fans one live frame out to every connected client.
type hub struct {
	mu      sync.Mutex
	clients map[*wsConn]struct{}
	last    liveFrame
	hasLast bool
	closed  bool
}

func newHub() *hub {
	return &hub{clients: make(map[*wsConn]struct{})}
}

// add registers a client and queues the last broadcast frame if there is one, so
// a page that connects between cycles learns the current position immediately
// instead of showing nothing until the next one.
//
// It reports whether the client was ACCEPTED - a hub that has been closed
// accepts none, and the caller must drop the connection rather than leave it
// running against a hub that will never speak to it again - and whether a frame
// was replayed, so the caller can state the position itself when the hub has
// nothing to replay yet. Both happen under the one lock: a broadcast landing
// between a registration and a separate replay would be sent twice.
func (h *hub) add(c *wsConn) (accepted, replayed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false, false
	}
	h.clients[c] = struct{}{}
	if !h.hasLast {
		return true, false
	}
	payload, err := encodeFrame(h.last)
	if err != nil {
		return true, false
	}
	return true, c.send(payload)
}

func (h *hub) remove(c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

// broadcast sends one frame to every client. A client whose queue is full is
// dropped: the frame is a notification, and a page too wedged to read one
// notification will not read the next either. Its socket closes and the browser
// reconnects, which is the recovery path the client already implements.
func (h *hub) broadcast(f liveFrame) {
	payload, err := encodeFrame(f)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.last, h.hasLast = f, true
	stalled := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		if !c.send(payload) {
			stalled = append(stalled, c)
		}
	}
	for _, c := range stalled {
		delete(h.clients, c)
	}
	h.mu.Unlock()

	for _, c := range stalled {
		c.close()
	}
}

// closeAll cuts every live connection. http.Server.Shutdown cannot: a hijacked
// connection is no longer its business, so without this a cancelled serve waits
// on sockets nobody owns.
func (h *hub) closeAll() {
	h.mu.Lock()
	h.closed = true
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.clients = make(map[*wsConn]struct{})
	h.mu.Unlock()

	for _, c := range clients {
		c.close()
	}
}

// count reports the number of connected clients. Test seam.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// watch polls the database for collection cycles and broadcasts one frame per
// settled change, until ctx is cancelled.
//
// Settled is the important word. A cycle commits several batches, each moving
// the WAL mtime; broadcasting on every observed change would fan out a burst of
// frames and make every client refetch several times for one cycle. So a change
// is announced only once the mtime has stopped moving for a poll - which costs
// one interval of latency and collapses the burst into a single frame.
//
// The mtime observed at startup is the baseline, never a frame: an existing
// database is not news.
func (s *Server) watch(ctx context.Context) {
	if s.opt.DBPath == "" {
		return
	}
	ticker := time.NewTicker(s.opt.PollInterval)
	defer ticker.Stop()

	var previous, announced time.Time
	started := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mt := newestMTime(s.opt.DBPath)
			if mt.IsZero() {
				// No database yet. When one appears its first observed mtime
				// becomes the baseline, exactly as at startup.
				continue
			}
			if !started {
				previous, announced, started = mt, mt, true
				continue
			}
			if mt.Equal(previous) && !mt.Equal(announced) {
				announced = mt
				s.announce(ctx, mt)
			}
			previous = mt
		}
	}
}

// announce reads the new watermark and broadcasts it with the write time that
// revealed it. A watermark that cannot be read is logged and skipped: the next
// cycle will announce again, and a frame with an invented watermark would make
// the page believe it had refetched something.
func (s *Server) announce(ctx context.Context, at time.Time) {
	watermark, err := s.reader.IngestWatermark(ctx)
	if err != nil {
		s.log.Printf("web: read ingest watermark: %v", err)
		return
	}
	s.hub.broadcast(liveFrame{Watermark: unixOrZero(watermark), CycleAt: at.Unix()})
}

// newestMTime returns the newest modification time of the database file and its
// write-ahead log, or the zero time when neither can be stat'd. See the package
// comment above for why the WAL is not optional here.
func newestMTime(path string) time.Time {
	if path == "" {
		return time.Time{}
	}
	newest := time.Time{}
	for _, p := range []string{path, path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if mt := fi.ModTime(); mt.After(newest) {
			newest = mt
		}
	}
	return newest
}
