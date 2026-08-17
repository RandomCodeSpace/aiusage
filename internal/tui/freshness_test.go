package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/store"
)

// freshness_test.go covers issue #7: the freshness enum (J-cut chip contract),
// the L-cut hold on failed loads, the cold-only error panel, the ingest
// heartbeat + dead-collector banner, and the chip click zone.

// flakySource serves fakeData rows until fail is set, then errors every query.
type flakySource struct {
	fakeData
	fail atomic.Bool
}

func (s *flakySource) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	if s.fail.Load() {
		return nil, errors.New("db locked")
	}
	return s.fakeData.Summarize(ctx, f)
}

// TestFreshnessTransitions locks the enum edges: cold → live on the first
// apply, live → cutIn synchronously on dispatch, cutIn → stale on a failed
// apply, stale → cutIn on redispatch, cutIn → live on a successful apply.
func TestFreshnessTransitions(t *testing.T) {
	src := &flakySource{}
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(Model)
	if m.fresh != FreshCold {
		t.Fatalf("initial freshness = %v, want cold", m.fresh)
	}

	m = loadOnce(m)
	if m.fresh != FreshLive {
		t.Fatalf("after first apply freshness = %v, want live", m.fresh)
	}

	cmd := (&m).startLoad() // dispatch: the chip cuts synchronously
	if m.fresh != FreshCutIn {
		t.Fatalf("after dispatch freshness = %v, want cutIn", m.fresh)
	}

	src.fail.Store(true)
	m.data.Invalidate() // force the flight back to the (now failing) source
	m = send(m, cmd())
	if m.fresh != FreshStale {
		t.Fatalf("after failed apply freshness = %v, want stale", m.fresh)
	}
	if m.err == nil {
		t.Fatal("failed apply did not record the error")
	}

	cmd = (&m).startLoad()
	if m.fresh != FreshCutIn {
		t.Fatalf("stale redispatch freshness = %v, want cutIn", m.fresh)
	}

	src.fail.Store(false)
	m = send(m, cmd())
	if m.fresh != FreshLive {
		t.Fatalf("recovery apply freshness = %v, want live", m.fresh)
	}
	if m.err != nil {
		t.Fatalf("recovery left err = %v", m.err)
	}
}

// TestChipCutsBeforeDataLands is the J-cut contract on the text channel: the
// frame rendered BETWEEN dispatch and dataLoadedMsg carries the "◐ sync" chip
// while the prior body content is still on screen; the apply lands in one
// frame and the chip returns to "● live".
func TestChipCutsBeforeDataLands(t *testing.T) {
	m := newTestModel(t, &fakeData{}) // loaded Overview at 120x40
	before := m.View().Content
	if !strings.Contains(before, "● live") {
		t.Fatalf("loaded frame missing the live chip:\n%s", before)
	}
	if !strings.Contains(before, "TREND") {
		t.Fatal("loaded frame missing the Overview body")
	}

	tm, cmd := m.Update(keyMsg("r")) // force refresh: flight in flight
	m = tm.(Model)
	mid := m.View().Content
	if !strings.Contains(mid, "◐ sync") {
		t.Fatalf("in-flight frame missing the sync chip:\n%s", mid)
	}
	if strings.Contains(mid, "● live") {
		t.Fatal("in-flight frame still shows the live chip")
	}
	// The old picture is held behind the chip — no blanking, no spinner body.
	if !strings.Contains(mid, "TREND") || !strings.Contains(mid, "codex") {
		t.Fatal("in-flight frame dropped the prior body content")
	}
	if strings.Contains(mid, "loading usage…") {
		t.Fatal("in-flight frame regressed to the cold loading screen")
	}

	m = send(m, cmd())
	after := m.View().Content
	if !strings.Contains(after, "● live") {
		t.Fatal("applied frame missing the live chip")
	}
	if strings.Contains(after, "◐ sync") {
		t.Fatal("applied frame still shows the sync chip")
	}
}

// TestFailedLoadHoldsLastFrame is the L-cut hold: a failed background load
// keeps the last good body on screen and routes the error to the chip — it
// must NOT blank four healthy panels behind a full-body error panel.
func TestFailedLoadHoldsLastFrame(t *testing.T) {
	src := &flakySource{}
	m := newTestModel(t, src)
	src.fail.Store(true)

	tm, cmd := m.Update(keyMsg("r")) // invalidates the cache; flight will fail
	m = send(tm.(Model), cmd())

	if m.fresh != FreshStale {
		t.Fatalf("failed load freshness = %v, want stale", m.fresh)
	}
	out := m.View().Content
	if !strings.Contains(out, "◔ stale") {
		t.Fatalf("failed load frame missing the stale chip:\n%s", out)
	}
	if !strings.Contains(out, "TREND") || !strings.Contains(out, "codex") {
		t.Fatal("failed load blanked the held body")
	}
	if strings.Contains(out, "press r to retry") {
		t.Fatal("full-body error panel rendered despite a held picture")
	}
}

// TestColdFailureShowsErrorPanel: with no prior good frame there is nothing to
// hold, so a cold failure renders the full-body error panel (the only place it
// exists) with the retry affordance.
func TestColdFailureShowsErrorPanel(t *testing.T) {
	src := &flakySource{}
	src.fail.Store(true)
	m := NewModel(src, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = loadOnce(tm.(Model))

	if m.fresh != FreshCold {
		t.Fatalf("cold failure freshness = %v, want cold", m.fresh)
	}
	out := m.View().Content
	if !strings.Contains(out, "✕ query failed") || !strings.Contains(out, "press r to retry") {
		t.Fatalf("cold failure missing the error panel:\n%s", out)
	}
	if strings.Contains(out, "TREND") {
		t.Fatal("cold failure rendered a body it never had")
	}
	if strings.Contains(out, "loading usage…") {
		t.Fatal("cold failure still shows the loading screen")
	}
}

// TestHeartbeatReducedMotion: under reduced motion the heartbeat renders a
// static glyph plus the observed ingest age instead of a pulse frame; in
// animated mode the frame advances only with the beat counter (an observed
// db write), never with wall time.
func TestHeartbeatReducedMotion(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m.data.now = func() time.Time { return fixed }
	m.ingestMTime = fixed.Add(-2 * time.Minute)

	m.reducedMotion = true
	cell := m.heartbeatCell()
	if !strings.Contains(cell, "⣿") {
		t.Fatalf("reduced-motion heartbeat missing the static glyph: %q", cell)
	}
	if !strings.Contains(cell, "2m") {
		t.Fatalf("reduced-motion heartbeat missing the age text: %q", cell)
	}

	m.reducedMotion = false
	m.beat = 0
	f0 := m.heartbeatCell()
	if m.heartbeatCell() != f0 {
		t.Fatal("heartbeat moved without an observed ingest (decorative motion)")
	}
	m.beat = 1
	if m.heartbeatCell() == f0 {
		t.Fatal("observed ingest did not advance the heartbeat frame")
	}
}

// TestObserveIngestBeatsOnlyOnAdvance: the pulse is driven by db-mtime
// advances alone — repeated or older mtimes must not beat.
func TestObserveIngestBeatsOnlyOnAdvance(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	t0 := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	base := m.beat
	m.observeIngest(t0)
	if m.beat != base+1 {
		t.Fatalf("first observation beat = %d, want %d", m.beat, base+1)
	}
	m.observeIngest(t0) // unchanged: no beat
	m.observeIngest(t0.Add(-time.Minute))
	m.observeIngest(time.Time{})
	if m.beat != base+1 {
		t.Fatalf("non-advancing observations beat = %d, want %d", m.beat, base+1)
	}
	m.observeIngest(t0.Add(time.Minute))
	if m.beat != base+2 {
		t.Fatalf("advance beat = %d, want %d", m.beat, base+2)
	}
}

// TestDeadCollectorBanner: ingest lag beyond 3x the collection interval
// escalates to the one-line banner; fresh ingest renders none.
func TestDeadCollectorBanner(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m.data.now = func() time.Time { return fixed }

	m.ingestMTime = fixed.Add(-time.Minute) // one interval fresh: no banner
	if out := m.View().Content; strings.Contains(out, "no ingest for") {
		t.Fatal("banner shown with fresh ingest")
	}

	m.ingestMTime = fixed.Add(-16 * time.Minute) // > 3x the 5m default
	out := m.View().Content
	if !strings.Contains(out, "no ingest for 16m") {
		t.Fatalf("stalled collector did not raise the banner:\n%s", out)
	}
}

// TestFreshnessChipClickForcesRefresh: the chip is a zone and a left-press on
// it dispatches a force refresh (Invalidate + new generation), same contract
// as the r key.
func TestFreshnessChipClickForcesRefresh(t *testing.T) {
	f := &fakeData{}
	m := newTestModel(t, f)
	gen := m.loadGen

	m2, found := click(t, m, views.ZoneFreshness)
	if !found {
		t.Fatal("freshness chip zone not found on screen")
	}
	if m2.loadGen != gen+1 {
		t.Fatalf("chip click loadGen = %d, want %d", m2.loadGen, gen+1)
	}
	if m2.fresh != FreshCutIn {
		t.Fatalf("chip click freshness = %v, want cutIn", m2.fresh)
	}
}

// TestApplySideReloadUsesCacheOnlySyncTwins locks the issue #4 residual: a
// selection moved while a load is in flight must NOT run a synchronous detail
// query when the flight applies — the apply's detail leg is the cache-only
// sync twin, and the miss converts to a debounced background dispatch.
func TestApplySideReloadUsesCacheOnlySyncTwins(t *testing.T) {
	f := &fakeData{}
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m := newPinnedModel(t, f, fixed)
	m = step(t, m, keyMsg("2")) // By-Tool, selection 0, detail warm

	tm, cmd := m.Update(keyMsg("r")) // invalidate + flight for selection 0
	m = tm.(Model)
	msg := cmd() // flight warms base + selection-0 detail only

	m = send(m, keyMsg("down")) // selection moves to 1 mid-flight

	var applyCmd tea.Cmd
	n := queriesDuring(f, func() {
		tm, c := m.Update(msg)
		m, applyCmd = tm.(Model), c
	})
	if n != 0 {
		t.Fatalf("apply with a moved selection ran %d UI-thread queries, want 0", n)
	}
	if applyCmd == nil {
		t.Fatal("apply-side detail miss did not arm the debounced dispatch")
	}
	if m.fresh != FreshLive {
		t.Fatalf("apply freshness = %v, want live", m.fresh)
	}
}
