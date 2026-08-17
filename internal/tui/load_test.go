package tui

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/model"
)

// TestScrubCompositionBracketsLocalDay pins a non-UTC zone and proves the
// store->scrub time contract against a real store: the per-bucket by-tool
// compositions the Overview scrubs through must attribute events to the same
// local calendar day as the timeline buckets themselves. Both sides now come
// from the SAME strftime localtime bucketing (one [day, tool] grouped query
// reduced by key), so a mismatch here would mean the store's grouping is
// inconsistent with itself — the class of timezone bug from issue #1 (events
// near local midnight landing in the neighbouring day) cannot reappear as a
// key mismatch. SQLite's 'localtime' modifier follows the system zone, so the
// expected day keys are written exactly as store/query.go groupExpr defines
// them.
func TestScrubCompositionBracketsLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	db := filepath.Join(t.TempDir(), "usage.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Four events straddling two IST midnights, one distinct tool each so a
	// misattributed event changes a day's tool set, not just its count.
	fixtures := []struct {
		tool string
		at   time.Time
	}{
		{"tool-a", time.Date(2026, 8, 8, 23, 30, 0, 0, loc)},
		{"tool-b", time.Date(2026, 8, 9, 0, 30, 0, 0, loc)},
		{"tool-c", time.Date(2026, 8, 9, 23, 45, 0, 0, loc)},
		{"tool-d", time.Date(2026, 8, 10, 0, 15, 0, 0, loc)},
	}
	evs := make([]model.UsageEvent, 0, len(fixtures))
	for i, f := range fixtures {
		evs = append(evs, model.UsageEvent{
			Tool:        f.tool,
			Model:       "m",
			SessionID:   "s",
			EventTime:   f.at,
			InputTokens: 100,
			TotalTokens: 100,
			DedupKey:    "tz|" + strconv.Itoa(i),
			Kind:        model.KindUsage,
		})
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	m := NewModel(st, Options{DBPath: db})

	// The expected local-day attribution only holds if the test host resolves
	// the same local days as the fixture zone; SQLite's 'localtime' follows the
	// system zone, not a mutated time.Local.
	want := map[string][]string{}
	for _, f := range fixtures {
		day := f.at.In(time.Local).Format("2006-01-02")
		want[day] = append(want[day], f.tool)
	}

	tl, dim, err := m.data.Timeline(m.qctx(), m.qnow(), Span{R: RangeAll}, nil)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	comp, err := m.data.GroupByDims(m.qctx(), m.qnow(), Span{R: RangeAll}, nil, []string{dim, "tool"})
	if err != nil {
		t.Fatalf("GroupByDims: %v", err)
	}
	sc := buildScrubComp(tl.Buckets, comp.Buckets, dim)
	if len(sc) != len(tl.Buckets) {
		t.Fatalf("composition entries = %d, want %d (one per timeline bucket)", len(sc), len(tl.Buckets))
	}

	for i, tb := range tl.Buckets {
		day := tb.Keys[dim]
		got := make([]string, 0, len(sc[i]))
		var total int64
		for _, b := range sc[i] {
			got = append(got, b.Keys["tool"])
			total += b.Total
		}
		sort.Strings(got)
		wantTools := append([]string(nil), want[day]...)
		sort.Strings(wantTools)
		if strings.Join(got, ",") != strings.Join(wantTools, ",") {
			t.Errorf("day %s composition holds %v, want %v", day, got, wantTools)
		}
		if total != tb.Total {
			t.Errorf("day %s composition total = %d, want the timeline bucket's %d", day, total, tb.Total)
		}
	}
}
