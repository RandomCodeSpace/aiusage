package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// dimSource records every turn-context dimension a load asks the store for, so
// a test can assert that a pivot queries ITS OWN partition and only that one.
type dimSource struct {
	fakeData
	mu   sync.Mutex
	dims []model.TurnDimension
}

func (s *dimSource) record(d model.TurnDimension) {
	s.mu.Lock()
	s.dims = append(s.dims, d)
	s.mu.Unlock()
}

func (s *dimSource) seen() []model.TurnDimension {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.TurnDimension(nil), s.dims...)
}

func (s *dimSource) SummarizeTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter) (*store.TurnContextSummary, error) {
	s.record(dim)
	return s.fakeData.SummarizeTurnContext(ctx, dim, f)
}

func (s *dimSource) TopTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter, by store.ActivityOrder, limit int) ([]store.TurnContextBucket, error) {
	s.record(dim)
	return s.fakeData.TopTurnContext(ctx, dim, f, by, limit)
}

// The cycle is calls first, then the store's own closed dimension vocabulary in
// its declared order, and it wraps. Building it from model.TurnDimensions rather
// than restating it is what makes a sixth attribution axis arrive on this tab by
// being added there.
func TestPivotCycleCoversEveryDimensionAndWraps(t *testing.T) {
	want := []ActivityPivot{PivotCalls}
	for _, d := range model.TurnDimensions() {
		want = append(want, ActivityPivot(d))
	}

	p := PivotCalls
	for i, w := range want {
		if p != w {
			t.Fatalf("step %d: pivot = %q, want %q", i, p, w)
		}
		p = p.Next()
	}
	if p != PivotCalls {
		t.Errorf("the cycle did not wrap: after %d steps pivot = %q", len(want), p)
	}
}

// Every pivot needs a label a reader can act on; PivotCalls is the only one that
// is not a dimension.
func TestPivotLabelsAndDimensions(t *testing.T) {
	if _, ok := PivotCalls.Dimension(); ok {
		t.Error("PivotCalls reported a turn-context dimension; it reads the other ledger entirely")
	}
	for _, d := range model.TurnDimensions() {
		p := ActivityPivot(d)
		got, ok := p.Dimension()
		if !ok || got != d {
			t.Errorf("%q.Dimension() = %q,%v want %q,true", p, got, ok, d)
		}
		if p.Label() == "" || p.Label() == string(d) && strings.Contains(string(d), "_") {
			t.Errorf("%q has no readable label (got %q)", p, p.Label())
		}
	}
}

// The pivot persists with the range and the tab. An unknown key must NOT resolve
// to a neighbouring dimension: that would answer a reading that does not exist
// with a different partition's numbers.
func TestPivotStateRoundTrips(t *testing.T) {
	for _, p := range activityPivotOrder {
		got, ok := PivotFromKey(p.Key())
		if !ok || got != p {
			t.Errorf("PivotFromKey(%q) = %q,%v want %q,true", p.Key(), got, ok, p)
		}
	}
	if got, ok := PivotFromKey("mcp_tools"); ok {
		t.Errorf("PivotFromKey(unknown) = %q,true; want the calls fallback and ok=false", got)
	}
	if got, ok := PivotFromKey(""); ok {
		t.Errorf("an ABSENT pivot field must not read as a chosen one: got %q,%v", got, ok)
	}
}

// The whole round trip through the state file: a dashboard left on the skills
// pivot comes back on it.
func TestPivotSurvivesARelaunch(t *testing.T) {
	path := t.TempDir() + "/ui-state.json"
	SaveUIState(path, UIState{Range: Range30d.Key(), Tab: ViewActivity.Key(),
		Pivot: ActivityPivot(model.DimensionSkill).Key()})

	m := NewModel(&fakeData{}, Options{StatePath: path})
	if m.pivot != ActivityPivot(model.DimensionSkill) {
		t.Errorf("restored pivot = %q, want the skill dimension", m.pivot)
	}
	if m.view != ViewActivity || m.rng != Range30d {
		t.Errorf("restored view/range = %v/%v, want Activity/30d", m.view, m.rng)
	}
}

// A state file written before this feature has no pivot field at all and must
// land on calls rather than on a zero-valued dimension.
func TestPivotDefaultsToCallsForAnOlderStateFile(t *testing.T) {
	path := t.TempDir() + "/ui-state.json"
	SaveUIState(path, UIState{Range: Range7d.Key(), Tab: ViewActivity.Key()})

	m := NewModel(&fakeData{}, Options{StatePath: path})
	if m.pivot != PivotCalls {
		t.Errorf("pivot = %q for a state file with no pivot field, want calls", m.pivot)
	}
}

// The pivot key cycles the Activity tab and every reading queries exactly its
// own dimension. This is the partition invariant expressed as a UI: no load
// reads two.
func TestPivotKeyQueriesOneDimensionAtATime(t *testing.T) {
	src := &dimSource{}
	m := newTestModelWH(t, src, 160, 44)
	m = step(t, m, keyMsg("5")) // Activity, calls pivot

	if got := src.seen(); len(got) != 0 {
		t.Fatalf("the calls pivot queried the turn-context table: %v", got)
	}

	for _, want := range model.TurnDimensions() {
		before := len(src.seen())
		m = step(t, m, keyMsg("p"))
		if m.pivot != ActivityPivot(want) {
			t.Fatalf("after p the pivot is %q, want %q", m.pivot, want)
		}
		got := src.seen()[before:]
		if len(got) == 0 {
			t.Fatalf("the %q pivot ran no turn-context query", want)
		}
		for _, d := range got {
			if d != want {
				t.Fatalf("the %q pivot queried dimension %q; a load must read exactly one partition",
					want, d)
			}
		}
	}

	// And back to calls, which reads the other ledger.
	before := len(src.seen())
	m = step(t, m, keyMsg("p"))
	if m.pivot != PivotCalls {
		t.Fatalf("the cycle did not return to calls: %q", m.pivot)
	}
	if got := src.seen()[before:]; len(got) != 0 {
		t.Errorf("the calls pivot queried the turn-context table: %v", got)
	}
}

// THE STALE-CACHE TRAP. Two pivots of the same window build identical activity
// filters, so a cache key without the dimension would serve one partition's rows
// to the other — the cross-partition confusion the store refuses to express in
// SQL, reintroduced in memory.
func TestTurnContextCacheKeysOnTheDimension(t *testing.T) {
	src := &dimSource{}
	d := NewData(src)
	now := d.now()
	sp := Span{R: Range7d}

	agent, err := d.TurnContextRank(context.Background(), now, sp, nil,
		model.DimensionAgent, turnContextRankDims, store.ActivityByTokens, 200)
	if err != nil {
		t.Fatalf("agent rank: %v", err)
	}
	skill, err := d.TurnContextRank(context.Background(), now, sp, nil,
		model.DimensionSkill, turnContextRankDims, store.ActivityByTokens, 200)
	if err != nil {
		t.Fatalf("skill rank: %v", err)
	}

	if len(agent) == 0 || len(skill) == 0 {
		t.Fatal("the fixture returned no rows for one of the dimensions")
	}
	if agent[0].Keys["value"] == skill[0].Keys["value"] {
		t.Fatalf("both dimensions returned %q; the second read the first's cache entry",
			agent[0].Keys["value"])
	}
	if got := src.seen(); len(got) != 2 || got[0] != model.DimensionAgent || got[1] != model.DimensionSkill {
		t.Errorf("store saw %v, want one query per dimension", got)
	}

	// The same dimension twice is still one query: the cache works, it just
	// cannot cross a partition.
	if _, err := d.TurnContextRank(context.Background(), now, sp, nil,
		model.DimensionAgent, turnContextRankDims, store.ActivityByTokens, 200); err != nil {
		t.Fatalf("agent rank (repeat): %v", err)
	}
	if got := src.seen(); len(got) != 2 {
		t.Errorf("a repeated query hit the store again: %v", got)
	}
}

// Invalidate must drop the turn-context caches too, or `r` would refresh five of
// the tab's six readings and silently keep the sixth.
func TestInvalidateClearsTheTurnContextCaches(t *testing.T) {
	src := &dimSource{}
	d := NewData(src)
	now := d.now()
	sp := Span{R: Range7d}

	for i := 0; i < 2; i++ {
		if _, err := d.TurnContextGroup(context.Background(), now, sp, nil,
			model.DimensionAgent, []string{"tool"}); err != nil {
			t.Fatalf("group: %v", err)
		}
	}
	if got := len(src.seen()); got != 1 {
		t.Fatalf("store saw %d queries before invalidation, want 1", got)
	}
	d.Invalidate()
	if _, err := d.TurnContextGroup(context.Background(), now, sp, nil,
		model.DimensionAgent, []string{"tool"}); err != nil {
		t.Fatalf("group after invalidate: %v", err)
	}
	if got := len(src.seen()); got != 2 {
		t.Errorf("store saw %d queries after invalidation, want 2", got)
	}
}

// Switching pivot resets the selection: row 3 of the agent list has nothing to
// do with row 3 of the skill list.
func TestPivotResetsTheSelection(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("5"))
	m = step(t, m, keyMsg("p")) // agents
	m.activity.Selected = 2
	m = step(t, m, keyMsg("p")) // skills — one row in the fixture
	if m.activity.Selected != 0 {
		t.Errorf("selection = %d after a pivot switch, want 0", m.activity.Selected)
	}
	if n := m.activity.RowCount(); m.activity.Selected >= n && n > 0 {
		t.Errorf("selection %d is past the %d rows the new pivot loaded", m.activity.Selected, n)
	}
}

// The pivot binding must be advertised where it acts and nowhere else.
func TestPivotBindingIsAdvertisedOnActivity(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)

	m = step(t, m, keyMsg("5"))
	foot := ansiFold.ReplaceAllString(m.renderFooter(), "")
	if !strings.Contains(foot, "pivot") {
		t.Errorf("the Activity footer does not advertise the pivot key:\n%s", foot)
	}
	if !strings.Contains(foot, m.pivot.Next().Label()) {
		t.Errorf("the Activity footer does not name where the pivot key lands (%q):\n%s",
			m.pivot.Next().Label(), foot)
	}

	m = step(t, m, keyMsg("4")) // Sessions has no pivot
	if foot := ansiFold.ReplaceAllString(m.renderFooter(), ""); strings.Contains(foot, "pivot") {
		t.Errorf("the Sessions footer advertises a key that does nothing there:\n%s", foot)
	}
}

// The Overview hero pivot is untouched by the Activity one: it stays pure
// presentation and dispatches nothing.
func TestOverviewHeroPivotStillTogglesLocally(t *testing.T) {
	src := &fakeData{}
	m := newTestModelWH(t, src, 160, 44)
	if m.heroPivot {
		t.Fatal("the hero starts on the trend reading")
	}
	if n := queriesDuring(src, func() { m = step(t, m, keyMsg("p")) }); n != 0 {
		t.Errorf("the hero pivot ran %d queries, want 0 — it re-reads applied data", n)
	}
	if !m.heroPivot {
		t.Error("p did not flip the hero reading on Overview")
	}
	if m.pivot != PivotCalls {
		t.Errorf("the Overview pivot key moved the Activity pivot to %q", m.pivot)
	}
}
