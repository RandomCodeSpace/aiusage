package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/store"
)

// keyFilter is a filter that exercises every field cacheKey encodes, including
// multi-value drill dimensions.
func keyFilter() store.Filter {
	return store.Filter{
		Since:    time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		GroupBy:  []string{"day", "tool"},
		Tools:    []string{"claude-code", "codex"},
		Models:   []string{"claude-opus"},
		Projects: []string{"/work/a"},
		Sessions: []string{"sess-1", "sess-2"},
	}
}

// joinedKey is the reference encoding cacheKey has always produced. It is spelt
// out here rather than reused so the two cannot drift together: the key is what
// a load generation's warm handoff matches on, and a changed byte would send
// the apply-side reload back to synchronous SQLite on the UI thread with no
// error anywhere.
func joinedKey(f store.Filter) string {
	return strings.Join([]string{
		f.Since.Format(time.RFC3339),
		f.Until.Format(time.RFC3339),
		strings.Join(f.GroupBy, ","),
		strings.Join(f.Tools, ","),
		strings.Join(f.Models, ","),
		strings.Join(f.Projects, ","),
		strings.Join(f.Sessions, ","),
	}, "|")
}

func TestCacheKeyIsByteStable(t *testing.T) {
	for _, f := range []store.Filter{
		{}, // zero times, no dims — the ungrouped Totals key
		{Since: time.Date(2026, 8, 3, 0, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))},
		{GroupBy: []string{"hour"}},
		keyFilter(),
	} {
		if got, want := cacheKey(f), joinedKey(f); got != want {
			t.Errorf("cacheKey = %q, want %q", got, want)
		}
	}

	// Distinct filters must not collide: the separator has to survive empty
	// lists, or a tool filter and a model filter of the same value would share
	// one entry.
	a := cacheKey(store.Filter{Tools: []string{"codex"}})
	b := cacheKey(store.Filter{Models: []string{"codex"}})
	if a == b {
		t.Errorf("a tool filter and a model filter share the key %q", a)
	}
}

// TestCacheKeyAllocatesOnce guards the reclaimed allocations (issue #41).
// cacheKey runs five times per warm reload; assembling through a
// strings.Builder cost two intermediate Format strings plus the builder's
// growth steps every time. Only the returned string may allocate now — a key
// that overflows the stack scratch is allowed one growth, which the long
// filter below deliberately stays inside.
func TestCacheKeyAllocatesOnce(t *testing.T) {
	for name, f := range map[string]store.Filter{
		"totals":  {},
		"grouped": {GroupBy: []string{"day", "tool"}},
		"drilled": keyFilter(),
	} {
		var sink string
		got := testing.AllocsPerRun(200, func() { sink = cacheKey(f) })
		if sink == "" {
			t.Fatalf("%s: cacheKey returned an empty key", name)
		}
		if got > 1 {
			t.Errorf("%s: cacheKey allocated %.0f times per call, want 1 (the key itself)", name, got)
		}
	}
}

// TestSortBucketsIsAllocationFree: the reflect-based sort.SliceStable allocated
// a boxed slice header and a Swapper on every call, and a warm reload sorts
// four times. The generic form is stable in the same way and free.
func TestSortBucketsIsAllocationFree(t *testing.T) {
	rows := fakeRows("tool")
	if len(rows) < 2 {
		t.Fatalf("fixture has %d rows; the sort would be trivially free", len(rows))
	}
	if got := testing.AllocsPerRun(200, func() { sortBuckets(rows, "tool", SortTotal) }); got != 0 {
		t.Errorf("sortBuckets(SortTotal) allocated %.0f times per call, want 0", got)
	}
	if got := testing.AllocsPerRun(200, func() { sortBuckets(rows, "tool", SortEvents) }); got != 0 {
		t.Errorf("sortBuckets(SortEvents) allocated %.0f times per call, want 0", got)
	}
}

// TestSortBucketsOrderUnchanged: swapping the sort implementation must not move
// a single row. Ties keep the store's order in every mode (both sorts are
// stable), which is what keeps a re-sort of the same key from silently
// reordering rows a caller still displays.
func TestSortBucketsOrderUnchanged(t *testing.T) {
	mk := func(name string, total, events int64) store.Bucket {
		return store.Bucket{Keys: map[string]string{"tool": name}, Total: total, Events: events}
	}
	// Deliberate ties on every key so stability is what decides the order.
	base := []store.Bucket{
		mk("delta", 100, 3), mk("alpha", 300, 1), mk("charlie", 100, 3),
		mk("bravo", 300, 5), mk("echo", 200, 1),
	}
	cases := map[Sort][]string{
		SortTotal:  {"alpha", "bravo", "echo", "delta", "charlie"},
		SortEvents: {"bravo", "delta", "charlie", "alpha", "echo"},
		SortName:   {"alpha", "bravo", "charlie", "delta", "echo"},
	}
	for srt, want := range cases {
		rows := append([]store.Bucket(nil), base...)
		sortBuckets(rows, "tool", srt)
		for i, w := range want {
			if got := rows[i].Keys["tool"]; got != w {
				t.Errorf("sort=%v row %d = %s, want %s (order %v)", srt, i, got, w, toolNames(rows))
				break
			}
		}
	}
}

func toolNames(b []store.Bucket) []string {
	out := make([]string, len(b))
	for i, x := range b {
		out[i] = x.Keys["tool"]
	}
	return out
}
