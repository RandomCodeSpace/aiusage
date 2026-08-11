package web

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestWatcherAnnouncesASettledChange: a write to the database (or its WAL)
// becomes exactly one frame, and only after the mtime has stopped moving.
func TestWatcherAnnouncesASettledChange(t *testing.T) {
	srv, path := newTestServer(t, defaultEvents())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.watch(ctx)

	// The database as it stands is the baseline, not news: nothing may be
	// announced before something changes.
	time.Sleep(50 * time.Millisecond)
	if _, ok := lastFrame(srv); ok {
		t.Fatal("the watcher announced a cycle for a database that never changed")
	}

	at := time.Now().Add(time.Second).Truncate(time.Second)
	touch(t, path+"-wal", at)

	frame := waitForFrame(t, srv)
	if frame.CycleAt != at.Unix() {
		t.Errorf("cycle_at = %d, want the write time %d", frame.CycleAt, at.Unix())
	}
	if want := seedTime.Add(2*time.Hour + time.Minute).Unix(); frame.Watermark != want {
		t.Errorf("watermark = %d, want the newest observed time %d", frame.Watermark, want)
	}
}

// TestWatcherIgnoresAMissingDatabase: no file yet is not an error and not a
// cycle. The first mtime that appears becomes the baseline.
func TestWatcherIgnoresAMissingDatabase(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	srv.opt.DBPath = t.TempDir() + "/not-created-yet.db"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.watch(ctx)

	time.Sleep(50 * time.Millisecond)
	if _, ok := lastFrame(srv); ok {
		t.Error("a missing database produced a cycle frame")
	}
}

// TestHubDropsAStalledClient: the watcher must never block on a socket, so a
// client that stopped reading is dropped instead of queued forever.
func TestHubDropsAStalledClient(t *testing.T) {
	h := newHub()
	c := &wsConn{conn: &fakeConn{}, out: make(chan wsMessage), done: make(chan struct{})}
	if accepted, replayed := h.add(c); !accepted || replayed {
		t.Fatalf("add = (%v,%v), want accepted with nothing to replay", accepted, replayed)
	}
	if h.count() != 1 {
		t.Fatalf("clients = %d, want 1", h.count())
	}

	// No writer is draining c.out and it is unbuffered, so the send cannot be
	// accepted: the client must be dropped rather than the broadcast stalling.
	done := make(chan struct{})
	go func() {
		h.broadcast(liveFrame{Watermark: 1, CycleAt: 2})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a stalled client")
	}
	if h.count() != 0 {
		t.Errorf("clients = %d, want the stalled one dropped", h.count())
	}
}

// TestHubReplaysTheLastFrameToANewClient: a page that connects between cycles
// learns the current position immediately instead of waiting an interval.
func TestHubReplaysTheLastFrameToANewClient(t *testing.T) {
	h := newHub()
	h.broadcast(liveFrame{Watermark: 7, CycleAt: 9})

	c := &wsConn{conn: &fakeConn{}, out: make(chan wsMessage, 1), done: make(chan struct{})}
	accepted, replayed := h.add(c)
	if !accepted || !replayed {
		t.Fatalf("add = (%v,%v), want an accepted client with the last frame replayed", accepted, replayed)
	}
	select {
	case m := <-c.out:
		var f liveFrame
		if err := json.Unmarshal(m.data, &f); err != nil {
			t.Fatalf("decode replayed frame: %v", err)
		}
		if f.Watermark != 7 || f.CycleAt != 9 {
			t.Errorf("replayed %+v, want the last broadcast", f)
		}
	default:
		t.Fatal("nothing was queued for the joining client")
	}
}

// TestHubCloseAllCutsHijackedConnections: http.Server.Shutdown cannot see a
// hijacked connection, so the hub has to.
func TestHubCloseAllCutsHijackedConnections(t *testing.T) {
	h := newHub()
	c := &wsConn{conn: &fakeConn{}, out: make(chan wsMessage, 1), done: make(chan struct{})}
	h.add(c)

	h.closeAll()
	if h.count() != 0 {
		t.Errorf("clients = %d after closeAll, want 0", h.count())
	}
	select {
	case <-c.done:
	default:
		t.Error("the connection was not closed")
	}
	if accepted, _ := h.add(c); accepted {
		t.Error("a closed hub accepted a client")
	}
	if h.count() != 0 {
		t.Error("a closed hub registered a client")
	}
}

// lastFrame reports the hub's last broadcast, if any.
func lastFrame(s *Server) (liveFrame, bool) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.hub.last, s.hub.hasLast
}

// waitForFrame waits for the watcher to announce a cycle.
func waitForFrame(t *testing.T, s *Server) liveFrame {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f, ok := lastFrame(s); ok {
			return f
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no live frame within the deadline")
	return liveFrame{}
}

// touch sets a file's modification time, creating it if needed - the WAL
// sidecar of a checkpointed database may not exist.
func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		f.Close()
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}
