package tui

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// TestBucketWindowBracketsLocalDay pins a non-UTC zone and proves the
// store->views time contract round-trips: a day bucket key (a wall-clock
// localtime string, per store/query.go groupExpr) resolved through
// bucketWindow must bracket exactly the events of that local calendar day.
// SQLite's 'localtime' modifier follows the system zone, not a mutated
// time.Local, so the day keys are formatted here exactly as the store contract
// defines them; insertion and the [since,until) windowed queries run against a
// real store. Before the ParseInLocation fix the keys were parsed as UTC,
// shifting every window by the zone offset and misbracketing events near
// local midnight.
func TestBucketWindowBracketsLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	orig := time.Local
	time.Local = loc
	defer func() { time.Local = orig }()

	db := filepath.Join(t.TempDir(), "usage.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Four events straddling two IST midnights, one distinct tool each so a
	// misplaced window changes the tool set, not just the count.
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
	m.tlData.Dim = "day"

	want := map[string][]string{
		"2026-08-08": {"tool-a"},
		"2026-08-09": {"tool-b", "tool-c"},
		"2026-08-10": {"tool-d"},
	}
	for day, tools := range want {
		b := store.Bucket{Keys: map[string]string{"day": day}}
		since, until := m.bucketWindow(b)
		if since.IsZero() {
			t.Fatalf("bucketWindow(%q): key did not parse", day)
		}
		s, err := m.data.GroupByWindow(since, until, nil, "tool", SortName)
		if err != nil {
			t.Fatalf("GroupByWindow(%q): %v", day, err)
		}
		got := make([]string, 0, len(s.Buckets))
		for _, x := range s.Buckets {
			got = append(got, x.Keys["tool"])
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(tools, ",") {
			t.Errorf("day %s window [%s, %s) holds %v, want %v",
				day, since, until, got, tools)
		}
	}
}
