package tui

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/store"
)

type workspaceLoadCapture struct {
	fakeData
	mu      sync.Mutex
	filters []store.Filter
}

func (s *workspaceLoadCapture) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	s.mu.Lock()
	s.filters = append(s.filters, f)
	s.mu.Unlock()
	return s.fakeData.Summarize(ctx, f)
}

func TestWorkspaceLoadCapturesCompactQueryIdentity(t *testing.T) {
	src := &workspaceLoadCapture{}
	fixed := time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	t.Cleanup(m.zoneMgr.Close)
	m.data.now = func() time.Time { return fixed }
	m.crumbs = []Crumb{{Dim: "model", Value: "captured"}}
	m.workspace.group = "session"

	cmd := m.startLoad()
	// A returned tea.Cmd may run after later UI state changes. Its scope and
	// grouping must be the dispatch snapshot, including independent slice data.
	m.crumbs[0].Value = "mutated"
	m.workspace.group = "provider"
	m.rng = Range30d

	msg := cmd().(dataLoadedMsg)
	if msg.err != nil || !msg.now.Equal(fixed) || msg.gen != m.loadGen {
		t.Fatalf("workspace flight result: err=%v now=%v gen=%d", msg.err, msg.now, msg.gen)
	}
	src.mu.Lock()
	filters := append([]store.Filter(nil), src.filters...)
	src.mu.Unlock()
	if len(filters) != 2 {
		t.Fatalf("workspace flight summaries = %d, want timeline + comparison", len(filters))
	}
	wantGroups := [][]string{{"day"}, {"session", "tool", "project"}}
	for i, f := range filters {
		if !reflect.DeepEqual(f.GroupBy, wantGroups[i]) || !reflect.DeepEqual(f.Models, []string{"captured"}) {
			t.Fatalf("summary %d did not retain dispatched group/scope: group=%v models=%v", i, f.GroupBy, f.Models)
		}
		wantSince := time.Date(2026, 9, 7, 0, 0, 0, 0, time.Local)
		if !f.Since.Equal(wantSince) || !f.Until.Equal(fixed) {
			t.Fatalf("summary %d window = [%v,%v), want [%v,%v)", i, f.Since, f.Until, wantSince, fixed)
		}
	}
}
