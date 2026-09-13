package tui

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	zone "github.com/lrstanley/bubblezone/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

func TestElapsedComparisonUsesSameCalendarSnapshot(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range []*time.Location{time.UTC, loc} {
		for _, now := range []time.Time{
			time.Date(2026, 3, 8, 0, 0, 0, 0, zone),
			time.Date(2026, 3, 8, 12, 34, 56, 0, zone),
			time.Date(2026, 11, 1, 23, 59, 59, 0, zone),
			time.Date(2026, 11, 2, 0, 0, 0, 0, zone),
		} {
			for _, r := range []Range{RangeToday, Range7d, Range30d, RangeAll} {
				t.Run(fmt.Sprintf("%s/%s/%s", zone, now.Format("01-02T15:04:05"), r.Label()), func(t *testing.T) {
					m := newPinnedModel(t, &fakeData{}, now)
					m.rng = r
					f := m.data.filterFor(context.Background(), now, m.span(), nil, nil)
					ps, pu, ok := m.prevWindow()
					if r == RangeAll {
						if ok || !f.Since.IsZero() || !f.Until.IsZero() {
							t.Fatal("All gained a bounded comparison")
						}
						return
					}
					if !ok || !f.Until.Equal(now) || !ps.Equal(f.Since.AddDate(0, 0, -r.spanDays())) || !pu.Equal(now.AddDate(0, 0, -r.spanDays())) {
						t.Fatalf("current [%v,%v), prior [%v,%v)", f.Since, f.Until, ps, pu)
					}
					m.step = -1
					ps, pu, ok = m.prevWindow()
					wantS, wantU := (Span{R: r, Step: -2}).Window(now)
					if !ok || !ps.Equal(wantS) || !pu.Equal(wantU) {
						t.Fatal("stepped comparison lost its complete prior span")
					}
				})
			}
		}
	}
}

func TestOverviewComparisonCapsExistingTimelineQuery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local)
	for i, at := range []time.Time{now.Add(-time.Minute), now.Add(time.Minute), now.AddDate(0, 0, -7).Add(-time.Minute), now.AddDate(0, 0, -7).Add(time.Minute)} {
		_, err = st.InsertEvents(context.Background(), []model.UsageEvent{{Tool: model.ToolCodex, Model: "m", SessionID: fmt.Sprint(i), EventTime: at, InputTokens: 100, TotalTokens: 100, Kind: model.KindUsage, DedupKey: fmt.Sprint(i)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	m := classicOverview(newPinnedModel(t, st, now))
	if m.err != nil || m.overview.Totals.Total != 100 || m.overview.Prev.Total != 100 {
		t.Fatalf("snapshot comparison = %d / %d, err %v", m.overview.Totals.Total, m.overview.Prev.Total, m.err)
	}
	m.data.Invalidate()
	m.loadNow = now.Add(2 * time.Minute)
	m.loadOverview()
	if m.overview.Totals.Total != 200 || m.overview.Prev.Total != 200 {
		t.Fatalf("refresh froze the cap: %d/%d", m.overview.Totals.Total, m.overview.Prev.Total)
	}
}

func TestOverviewDetailApplyNeverQueries(t *testing.T) {
	for _, state := range []string{"warm", "invalidated", "failed", "cancelled", "stale"} {
		t.Run(state, func(t *testing.T) {
			f := &fakeData{}
			m := newPinnedModel(t, f, windowClock)
			before := m.overview
			msg := detailLoadedMsg{gen: m.loadGen, seq: m.detailSeq}
			if state != "warm" {
				m.data.Invalidate()
			}
			if state == "failed" {
				msg.failed = true
			}
			if state == "cancelled" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				m.loadCtx = ctx
				msg.failed = true
			}
			if state == "stale" {
				msg.gen--
			}
			var cmd tea.Cmd
			if n := queriesDuring(f, func() {
				tm, c := m.Update(msg)
				m, cmd = tm.(Model), c
			}); n != 0 {
				t.Fatalf("%s apply queried %d times", state, n)
			}
			if state != "warm" && !reflect.DeepEqual(before, m.overview) {
				t.Fatal("cache miss replaced displayed data")
			}
			if state == "invalidated" {
				if cmd == nil {
					t.Fatal("cache miss did not schedule background load")
				}
				m = runPending(t, m, cmd)
				if m.err != nil || m.detailWanted {
					t.Fatal("background retry did not restore cache")
				}
			}
			if state == "failed" || state == "cancelled" {
				if cmd != nil || m.err == nil || m.fresh != FreshStale {
					t.Fatalf("failed load state: cmd=%v err=%v fresh=%v", cmd != nil, m.err, m.fresh)
				}
				m.loadCtx = context.Background()
				recovered := m.detailLoadCmd()()
				m = send(m, recovered)
				if m.err != nil || m.fresh != FreshLive {
					t.Fatalf("successful retry did not restore live state: %v %v", m.err, m.fresh)
				}
			}
		})
	}
}

func TestCurrentFrameZoneWaitDelayedAndAbsent(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = step(t, m, keyMsg("2"))
	id := views.BarZone("codex")
	old := resolveZone(m, id)
	if old == nil {
		t.Fatal("old zone absent")
	}
	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 30})
	start := time.Now()
	_ = m.View()
	end := time.Now()
	if zoneFromFrame(old, start, end) {
		t.Fatal("old frame accepted")
	}
	readyAt := time.Now().Add(40 * time.Millisecond)
	z := waitZone(func() *zone.ZoneInfo {
		if time.Now().Before(readyAt) {
			return old
		}
		return m.zoneMgr.Get(id)
	}, func(z *zone.ZoneInfo) bool { return zoneFromFrame(z, start, end) }, 500*time.Millisecond)
	if z == nil || time.Since(start) < 40*time.Millisecond {
		t.Fatal("wait failed to observe the delayed current frame")
	}
	startWait := time.Now()
	if got := waitZone(func() *zone.ZoneInfo { return old }, func(z *zone.ZoneInfo) bool { return zoneFromFrame(z, start, end) }, 25*time.Millisecond); got != nil {
		t.Fatal("absent current frame accepted stale geometry")
	}
	if elapsed := time.Since(startWait); elapsed < 25*time.Millisecond || elapsed > time.Second {
		t.Fatalf("absent frame wait took %s", elapsed)
	}
	// A real stamp near a second boundary belongs only to the correct interval.
	it := reflect.ValueOf(z).Elem().FieldByName("iteration").Int()
	base := time.Unix(100, 0)
	if !zoneFromFrame(z, base.Add(-time.Nanosecond), base.Add(time.Duration(it))) {
		t.Fatal("second-boundary wrap rejected current frame")
	}
}

func TestOverviewTotalVisibleAtRequiredGeometries(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, mode := range []string{"normal", "mono", "reduced"} {
			t.Setenv("NO_COLOR", "")
			t.Setenv("AIUSAGE_REDUCED_MOTION", "")
			if mode == "mono" {
				t.Setenv("NO_COLOR", "1")
			}
			if mode == "reduced" {
				t.Setenv("AIUSAGE_REDUCED_MOTION", "1")
			}
			for _, width := range []int{42, 60, 80, 120, 160} {
				for _, height := range []int{24, 40} {
					for _, r := range []Range{RangeToday, Range7d, Range30d, RangeAll} {
						var src DataSource = &fakeData{}
						if empty {
							src = emptySource{}
						}
						m := newTestModelWH(t, src, width, height)
						m.rng = r
						m.reload()
						frame := plainFrame(m)
						want := "7.0K"
						if empty {
							want = "0"
						}
						if !strings.Contains(frame, "total") || !strings.Contains(frame, want) || !strings.Contains(frame, "tokens") {
							t.Fatalf("%dx%d %s %s empty=%v missing labeled total %s:\n%s", width, height, r.Label(), mode, empty, want, frame)
						}
						if width <= 60 && height == 24 && !strings.Contains(frame, "total "+want+" tokens") {
							t.Fatal("compact labeled total missing")
						}
						for _, line := range strings.Split(frame, "\n") {
							if lipgloss.Width(line) > width {
								t.Fatal("total fallback overflowed frame")
							}
						}
					}
				}
			}
		}
	}
}

func TestAllOverviewUsesLedgerWhenRollupIsMissingOrStale(t *testing.T) {
	for _, state := range []string{"empty", "stale"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.db")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			_, err = st.InsertEvents(context.Background(), []model.UsageEvent{{Tool: model.ToolCodex, Model: "m", SessionID: "s", EventTime: windowClock.Add(-time.Hour), InputTokens: 7000, TotalTokens: 7000, Kind: model.KindUsage, DedupKey: "total"}})
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if state == "empty" {
				_, err = db.Exec("DELETE FROM usage_rollup")
			} else {
				_, err = db.Exec("UPDATE schema_meta SET value='0' WHERE key='rollup_watermark'")
			}
			if err != nil {
				t.Fatal(err)
			}
			m := newPinnedModel(t, st, windowClock)
			m.rng = RangeAll
			m.reload()
			m = send(m, tea.WindowSizeMsg{Width: 42, Height: 24})
			if m.err != nil || m.overview.Totals.Total != 7000 || !strings.Contains(plainFrame(m), "total 7.0K tokens") {
				t.Fatalf("All %s rollup lost ledger total: %d %v", state, m.overview.Totals.Total, m.err)
			}
		})
	}
}

func TestOverviewCostDeltaUsesWiredCurrencyFormatter(t *testing.T) {
	m := classicOverview(newTestModelW(t, &fakeData{}, 260))
	m.overview.Totals.CostMicroUSD = 1_324_620_000
	m.overview.Prev.CostMicroUSD = 2_000_000
	if out := plainFrame(m); !strings.Contains(out, "▲ $1322.62") {
		t.Fatalf("wired cost delta lacks currency:\n%s", out)
	}
}

func TestActivityTurnPivotMouseMatchesKeyboard(t *testing.T) {
	for _, dim := range append([]model.TurnDimension{""}, model.TurnDimensions()...) {
		t.Run(string(dim), func(t *testing.T) {
			m := newTestModel(t, &fakeData{})
			m = step(t, m, keyMsg("5"))
			m.pivot = ActivityPivot(dim)
			m.reload()
			if dim != "" && len(m.activity.CtxRows) < 2 {
				m.activity.CtxRows = []store.TurnContextBucket{
					{Keys: map[string]string{"value": "first", "tool": model.ToolClaudeCode}, Turns: 2, TotalTokens: 200},
					{Keys: map[string]string{"value": "second", "tool": model.ToolClaudeCode}, Turns: 1, TotalTokens: 100},
				}
			}
			keyboard := send(m, keyMsg("down"))
			mouse := mustPress(t, m, views.ActZone(1), tea.MouseLeft)
			if keyboard.activity.Selected != 1 || mouse.activity.Selected != 1 {
				t.Fatalf("selection keyboard=%d mouse=%d", keyboard.activity.Selected, mouse.activity.Selected)
			}
			// Repeating rows forces the viewport to scroll for either ledger.
			for m.activity.RowCount() < 40 {
				if dim == "" {
					m.activity.Rows = append(m.activity.Rows, m.activity.Rows...)
				} else {
					m.activity.CtxRows = append(m.activity.CtxRows, m.activity.CtxRows...)
				}
			}
			for i := 0; i < 30; i++ {
				m = send(m, keyMsg("down"))
			}
			idx := m.activity.Selected
			m = mustPress(t, m, views.ActZone(idx), tea.MouseLeft)
			if m.activity.Selected != idx {
				t.Fatal("scrolled pivot click selected the wrong row")
			}
		})
	}
}

func TestUnknownModelChevronDrillsByMouseAndKeyboard(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local)
	_, err = st.InsertEvents(context.Background(), []model.UsageEvent{
		{Tool: model.ToolCodex, Model: "", SessionID: "unknown", EventTime: now.Add(-time.Minute), InputTokens: 100, TotalTokens: 100, Kind: model.KindUsage, DedupKey: "unknown"},
		{Tool: model.ToolCodex, Model: "named", SessionID: "named", EventTime: now.Add(-time.Minute), InputTokens: 900, TotalTokens: 900, Kind: model.KindUsage, DedupKey: "named"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mouse := range []bool{false, true} {
		m := newPinnedModel(t, st, now)
		m = step(t, m, keyMsg("3")) // warm the unfiltered model grouping
		if len(m.byModel.Rows) != 2 || m.byModel.Grand != 1000 {
			t.Fatalf("By Model did not warm both models: %+v", m.byModel)
		}
		for i, row := range m.byModel.Rows {
			if row.Keys["model"] == "" {
				m.byModel.Selected = i
			}
		}
		z, frame, _ := resolveCurrentZone(m, views.BarZone(""))
		if z == nil {
			t.Fatal("unknown model bar missing")
		}
		line := strings.Split(ansiAct.ReplaceAllString(frame, ""), "\n")[z.StartY]
		if !strings.Contains(line, "›") {
			t.Fatalf("unknown model has no drill affordance: %s", line)
		}
		if mouse {
			m = mustPress(t, m, views.BarZone(""), tea.MouseLeft)
			m = mustPress(t, m, views.BarZone(""), tea.MouseLeft)
		} else {
			m = step(t, m, keyMsg("enter"))
		}
		if m.view != ViewBrowse || len(m.crumbs) != 1 || m.crumbs[0] != (Crumb{Dim: "model", Value: ""}) {
			t.Fatalf("unknown-model drill = %v %v", m.view, m.crumbs)
		}
		row, ok := m.browse.SelectedBucket()
		if !ok || m.browse.RowCount() != 1 || row.Keys["model"] != "" || row.Total != 100 || row.Events != 1 {
			t.Fatalf("mouse=%v unknown-model rows=%d selected=%+v", mouse, m.browse.RowCount(), row)
		}
		totals, ok := m.data.TotalsCached(m.qnow(), m.span(), m.crumbs)
		if !ok || totals.Total != 100 || totals.Events != 1 {
			t.Fatalf("mouse=%v unknown-model totals=%+v cached=%v", mouse, totals, ok)
		}
	}
}
