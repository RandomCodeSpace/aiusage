package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
)

// rawPayload stands in for what the column really holds on an old ledger: a
// transcript line, not a usage object.
const rawPayload = `{"usage":{"input_tokens":11},"content":"a whole transcript line"}`

func seedWithRaw(t *testing.T, st *SQLite) {
	t.Helper()
	at := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	evs := []model.UsageEvent{
		rollupEvent("proj-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 10),
		rollupEvent("proj-2", model.ToolCodex, "gpt-5", "/w/alpha", at.Add(time.Hour), -1),
	}
	for i := range evs {
		evs[i].Raw = rawPayload
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestListEventsExcludesRawByDefault pins the projection at the SQL level: the
// default event listing must not so much as ASK for the raw column. Reading it
// and dropping it afterwards would still drag the ledger's bulk (tens of MB of
// transcript-bearing rows) through the driver on every call.
func TestListEventsExcludesRawByDefault(t *testing.T) {
	st := openCounting(t)
	seedWithRaw(t, st)
	ctx := context.Background()

	var evs []model.UsageEvent
	queries := queriesDuring(func() {
		var err error
		evs, err = st.ListEvents(ctx, Filter{})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
	})
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	for _, e := range evs {
		if e.Raw != "" {
			t.Errorf("event %s carried raw without an opt-in", e.DedupKey)
		}
	}
	for _, q := range queries {
		if strings.Contains(strings.ToLower(q), "raw") {
			t.Errorf("default ListEvents projection names the raw column:\n%s", q)
		}
	}
	if len(queries) == 0 {
		t.Fatalf("no statement was recorded; the projection guard is not watching anything")
	}
}

// TestListEventsWithRawIsTheOnlyWayIn: export --include-raw still works, and it
// is the only caller that gets the payload.
func TestListEventsWithRawIsTheOnlyWayIn(t *testing.T) {
	st := openTemp(t)
	seedWithRaw(t, st)
	ctx := context.Background()

	evs, err := st.ListEvents(ctx, Filter{}, WithRaw())
	if err != nil {
		t.Fatalf("list events with raw: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	for _, e := range evs {
		if e.Raw != rawPayload {
			t.Errorf("event %s raw = %q, want the stored payload", e.DedupKey, e.Raw)
		}
	}
}

// TestListEventsProjectsRowID pins the id that keyset pagination will page on:
// present, matching the ledger's own ids, and ordered with the listing.
func TestListEventsProjectsRowID(t *testing.T) {
	st := openTemp(t)
	seedWithRaw(t, st)
	ctx := context.Background()

	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	byKey := map[string]int64{}
	for i, e := range evs {
		if e.ID <= 0 {
			t.Fatalf("event %s has id %d; the listing must project the row id", e.DedupKey, e.ID)
		}
		if i > 0 && e.ID <= evs[i-1].ID {
			t.Errorf("ids are not ascending with the listing order: %d after %d", e.ID, evs[i-1].ID)
		}
		byKey[e.DedupKey] = e.ID
	}
	for key, id := range byKey {
		var stored int64
		if err := st.db.QueryRowContext(ctx,
			`SELECT id FROM usage_events WHERE dedup_key=?`, key).Scan(&stored); err != nil {
			t.Fatalf("read stored id for %s: %v", key, err)
		}
		if stored != id {
			t.Errorf("event %s projected id %d, ledger holds %d", key, id, stored)
		}
	}
}
