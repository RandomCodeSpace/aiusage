package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

type codeChangesData struct {
	fakeData
	codeCalls atomic.Int64
	read      func(context.Context, string, string, string) (store.CodeChangeSummary, error)
}

func (s *codeChangesData) SessionCodeChanges(ctx context.Context, tool, session, project string) (store.CodeChangeSummary, error) {
	s.codeCalls.Add(1)
	return s.read(ctx, tool, session, project)
}

func TestSessionCodeChangesCacheIdentityAndFailure(t *testing.T) {
	failure := errors.New("source unavailable")
	src := &codeChangesData{read: func(_ context.Context, tool, session, project string) (store.CodeChangeSummary, error) {
		if session == "failed" {
			return store.CodeChangeSummary{}, failure
		}
		return store.CodeChangeSummary{KnownChanges: 1, LinesAdded: int64(len(tool) + len(session) + len(project))}, nil
	}}
	d := NewData(src)
	for _, key := range [][3]string{{"codex", "s1", "p1"}, {"claude-code", "s1", "p1"}, {"codex", "s2", "p1"}, {"codex", "s1", "p2"}, {"codex", "failed", "p1"}} {
		if _, _, ok := d.SessionCodeChangesCached(key[0], key[1], key[2]); ok {
			t.Fatal("cold identity was cached")
		}
		want, err := d.SessionCodeChanges(context.Background(), key[0], key[1], key[2])
		got, cachedErr, ok := d.SessionCodeChangesCached(key[0], key[1], key[2])
		if !ok || got != want || cachedErr != err {
			t.Fatalf("cached=%+v,%v,%v want=%+v,%v", got, cachedErr, ok, want, err)
		}
		if _, repeatErr := d.SessionCodeChanges(context.Background(), key[0], key[1], key[2]); repeatErr != err {
			t.Fatal(repeatErr)
		}
	}
	if got := src.codeCalls.Load(); got != 5 {
		t.Fatalf("queries=%d, want one per identity", got)
	}
	d.Invalidate()
	if _, _, ok := d.SessionCodeChangesCached("codex", "s1", "p1"); ok {
		t.Fatal("invalidation retained counts")
	}
	if _, _, ok := d.SessionCodeChangesCached("codex", "failed", "p1"); ok {
		t.Fatal("invalidation retained failure")
	}
	if _, err := d.SessionCodeChanges(context.Background(), "codex", "failed", "p1"); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if src.codeCalls.Load() != 6 {
		t.Fatal("refresh did not retry failure")
	}
	unsupported := NewData(&fakeData{})
	if got, err, ok := unsupported.SessionCodeChangesCached("codex", "s", "p"); !ok || err != nil || got != (store.CodeChangeSummary{}) {
		t.Fatalf("unsupported=%+v,%v,%v", got, err, ok)
	}
}

func TestSessionCodeChangesLateFlightCannotRepopulate(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalidate", true: "cancel"}[cancel], func(t *testing.T) {
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			src := &codeChangesData{read: func(context.Context, string, string, string) (store.CodeChangeSummary, error) {
				close(started)
				<-release
				return store.CodeChangeSummary{KnownChanges: 1, LinesAdded: 9}, nil
			}}
			d := NewData(src)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go func() { defer close(done); _, _ = d.SessionCodeChanges(ctx, "codex", "s", "p") }()
			<-started
			if cancel {
				stop()
			} else {
				d.Invalidate()
			}
			close(release)
			<-done
			if _, _, ok := d.SessionCodeChangesCached("codex", "s", "p"); ok {
				t.Fatal("abandoned flight populated current cache")
			}
		})
	}
}

func drillToSessions(t *testing.T, m Model) Model {
	t.Helper()
	m = step(t, m, keyMsg("4"))
	for i := 0; i < 3; i++ {
		m = step(t, m, keyMsg("enter"))
	}
	if m.browse.Dim() != "session" {
		t.Fatal("did not reach session selection")
	}
	return m
}

func TestSessionCodeChangesDetailStaysLocal(t *testing.T) {
	src := &codeChangesData{read: func(_ context.Context, tool, session, project string) (store.CodeChangeSummary, error) {
		if tool != "claude-code" || project != "/work/a" {
			t.Fatalf("wrong session identity: %q/%q/%q", tool, session, project)
		}
		if session == "sess-1" {
			return store.CodeChangeSummary{KnownChanges: 1, UnknownChanges: 1, LinesAdded: 12, LinesRemoved: 3}, nil
		}
		return store.CodeChangeSummary{KnownChanges: 1}, nil
	}}
	m := newPinnedModel(t, src, time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC))
	if src.codeCalls.Load() != 0 {
		t.Fatal("non-session view queried code changes")
	}
	m = drillToSessions(t, m)
	if src.codeCalls.Load() != 1 || !strings.Contains(plainFrame(m), "+12/-3 partial") {
		t.Fatalf("initial detail: queries=%d\n%s", src.codeCalls.Load(), plainFrame(m))
	}
	var cmd tea.Cmd
	if n := queriesDuring(&src.fakeData, func() { tm, c := m.Update(keyMsg("down")); m, cmd = tm.(Model), c }); n != 0 || src.codeCalls.Load() != 1 {
		t.Fatal("cursor move queried source")
	}
	if cmd == nil || !strings.Contains(plainFrame(m), "loading") || strings.Contains(plainFrame(m), "+12/-3") {
		t.Fatal("selection leaked prior session counts")
	}
	tm, cmd := m.Update(detailDebounceMsg{seq: m.detailSeq})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("detail did not dispatch")
	}
	msg := cmd()
	if src.codeCalls.Load() != 2 {
		t.Fatal("detail flight did not query selected session")
	}
	if n := queriesDuring(&src.fakeData, func() { m = send(m, msg) }); n != 0 || src.codeCalls.Load() != 2 {
		t.Fatal("detail apply queried source")
	}
	if !strings.Contains(plainFrame(m), "+0/-0") || strings.Contains(plainFrame(m), "not reported") {
		t.Fatal("reported zero was treated as unknown")
	}
	m = step(t, m, keyMsg("up"))
	if src.codeCalls.Load() != 2 || !strings.Contains(plainFrame(m), "+12/-3 partial") {
		t.Fatal("warm session detail did not reuse cache")
	}
	// Model/time changes must not change this lifetime identity.
	m.crumbs[1].Value = "another-model"
	m.loadBrowseCodeChanges(true)
	if src.codeCalls.Load() != 2 {
		t.Fatal("model changed lifetime cache identity")
	}
	if n := queriesDuring(&src.fakeData, func() {
		tm, c := m.Update(keyMsg("r"))
		m, cmd = tm.(Model), c
	}); n != 0 || src.codeCalls.Load() != 2 {
		t.Fatal("refresh dispatch queried source")
	}
	if strings.Contains(plainFrame(m), "+12/-3") || !strings.Contains(plainFrame(m), "loading") {
		t.Fatal("refresh retained stale session counts")
	}
	if cmd == nil {
		t.Fatal("refresh did not dispatch")
	}
	m = send(m, cmd())
	if src.codeCalls.Load() != 3 || !strings.Contains(plainFrame(m), "+12/-3 partial") {
		t.Fatal("refresh did not reload session counts")
	}
}

func TestSessionCodeChangesUnavailableAndUnsupported(t *testing.T) {
	src := &codeChangesData{read: func(context.Context, string, string, string) (store.CodeChangeSummary, error) {
		return store.CodeChangeSummary{}, errors.New("unavailable")
	}}
	m := drillToSessions(t, newPinnedModel(t, src, time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)))
	if !strings.Contains(plainFrame(m), "Lifetime unavailable") || m.browse.PreviewErr() || m.detailWanted {
		t.Fatalf("failure state leaked to trend or rearmed: %s", plainFrame(m))
	}
	m.syncBrowsePreview()
	if src.codeCalls.Load() != 1 || m.detailWanted {
		t.Fatal("failed detail retried on UI thread")
	}
	m = drillToSessions(t, newPinnedModel(t, &fakeData{}, time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)))
	if !strings.Contains(plainFrame(m), "Lifetime not reported") {
		t.Fatalf("unsupported source must stay unknown: %s", plainFrame(m))
	}
	// A model-first drill has no tool identity and may combine sessions from
	// different harnesses. Do not turn that ambiguity into a failed SQL query.
	m.crumbs = m.crumbs[1:]
	m.data = NewData(src)
	before := src.codeCalls.Load()
	m.loadBrowseCodeChanges(false)
	m.loadBrowseCodeChanges(true)
	if src.codeCalls.Load() != before || !strings.Contains(plainFrame(m), "Lifetime not reported") {
		t.Fatal("missing tool identity queried or invented session counts")
	}
}

func TestSessionCodeChangesSQLiteLifetimeAndRefresh(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	ledger, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -14)
	changes := []model.CodeChange{
		{Tool: "opencode", ChangeID: "old-turn", SessionID: "s", Project: "/work/a", Known: true, LinesAdded: 20, LinesRemoved: 4, UpdatedAt: old, ObservedTime: old},
		{Tool: "opencode", ChangeID: "current-turn", SessionID: "s", Project: "/work/a", Known: true, LinesAdded: 3, LinesRemoved: 1, UpdatedAt: now, ObservedTime: now},
		{Tool: "opencode", ChangeID: "unknown-turn", SessionID: "s", Project: "/work/a", UpdatedAt: now, ObservedTime: now},
		{Tool: "opencode", ChangeID: "other-session", SessionID: "other", Project: "/work/a", Known: true, LinesAdded: 999, UpdatedAt: now, ObservedTime: now},
	}
	_, err = ledger.ApplyBatch(ctx, store.ObservationBatch{CodeChanges: changes, Events: []model.UsageEvent{
		{DedupKey: "old-usage", Tool: model.ToolOpenCode, SessionID: "s", Project: "/work/a", Model: "old-model", EventTime: old, Kind: model.KindUsage, InputTokens: 10, TotalTokens: 10},
		{DedupKey: "current-usage", Tool: model.ToolOpenCode, SessionID: "s", Project: "/work/a", Model: "current-model", EventTime: now.Add(-time.Hour), Kind: model.KindUsage, InputTokens: 10, TotalTokens: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	m := drillToSessions(t, newPinnedModel(t, reader, now))
	if m.crumbs[1].Value != "current-model" {
		t.Fatalf("expected current-window model, got %v", m.crumbs)
	}
	if !strings.Contains(plainFrame(m), "+23/-5 partial") {
		t.Fatalf("lifetime rows outside selected model/window omitted:\n%s", plainFrame(m))
	}
	changes[0].LinesAdded, changes[0].LinesRemoved = 0, 0
	changes[0].UpdatedAt = now.Add(time.Minute)
	if _, err := ledger.ApplyBatch(ctx, store.ObservationBatch{CodeChanges: changes[:1]}); err != nil {
		t.Fatal(err)
	}
	m.data.Invalidate()
	m.syncBrowsePreview()
	if strings.Contains(plainFrame(m), "+23/-5") {
		t.Fatal("refresh retained old source snapshot")
	}
	m = send(m, m.detailLoadCmd()())
	if !strings.Contains(plainFrame(m), "+3/-1 partial") {
		t.Fatalf("updated snapshot not loaded:\n%s", plainFrame(m))
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	m.data.Invalidate()
	m = send(m, m.detailLoadCmd()())
	if !strings.Contains(plainFrame(m), "Lifetime unavailable") {
		t.Fatalf("SQLite failure displayed as unknown or zero:\n%s", plainFrame(m))
	}
}

func TestSessionCodeChangesFrameFits(t *testing.T) {
	for _, width := range []int{42, 60, 80, 120} {
		for _, height := range []int{24, 40} {
			t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) {
				src := &codeChangesData{read: func(context.Context, string, string, string) (store.CodeChangeSummary, error) {
					return store.CodeChangeSummary{KnownChanges: 1, UnknownChanges: 1, LinesAdded: 12, LinesRemoved: 3}, nil
				}}
				m := drillToSessions(t, newTestModelWH(t, src, width, height))
				out := plainFrame(m)
				for _, label := range []string{"events", "total", "Recorded session lines", "Lifetime", "+12/-3", "partial"} {
					if !strings.Contains(out, label) {
						t.Fatalf("missing %q:\n%s", label, out)
					}
				}
				if lipgloss.Width(out) > width || lipgloss.Height(out) > height {
					t.Fatalf("frame %dx%d exceeds %dx%d", lipgloss.Width(out), lipgloss.Height(out), width, height)
				}
			})
		}
	}
}
