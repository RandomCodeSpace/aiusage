package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// TestMetaContractShape pins the fields the page reads at boot. A missing key
// here is not a cosmetic regression: the page decodes this before it draws
// anything.
func TestMetaContractShape(t *testing.T) {
	srv, path := newTestServer(t, defaultEvents())
	srv.opt.Daemon = func() DaemonInfo {
		return DaemonInfo{Running: true, PID: 4242, Uptime: 90 * time.Second, Interval: 5 * time.Minute}
	}
	srv.opt.Resources = func() Resources { return Resources{CPU: 0.25, Memory: 0.5, Disk: 0.75} }

	var meta metaResponse
	body := getJSON(t, srv, "/api/meta", &meta)

	if meta.ContractVersion != ContractVersion {
		t.Errorf("contract_version = %d, want %d", meta.ContractVersion, ContractVersion)
	}
	if meta.ServerVersion != "v0.0.0-test" {
		t.Errorf("server_version = %q, want the injected identity", meta.ServerVersion)
	}
	if meta.NowUnix != seedTime.Add(4*time.Hour).Unix() {
		t.Errorf("now_unix = %d, want the server clock", meta.NowUnix)
	}
	// The watermark is the newest row's OBSERVED time, which the seed sets one
	// minute after the event time of the last row.
	if want := seedTime.Add(2*time.Hour + time.Minute).Unix(); meta.Watermark != want {
		t.Errorf("watermark = %d, want %d (newest observed_time)", meta.Watermark, want)
	}
	if !meta.Daemon.Running || meta.Daemon.PID != 4242 || meta.Daemon.UptimeSeconds != 90 || meta.Daemon.IntervalSeconds != 300 {
		t.Errorf("daemon = %+v, want the injected probe values", meta.Daemon)
	}
	if meta.Daemon.LastCycleUnix != newestMTime(path).Unix() {
		t.Errorf("last_cycle_unix = %d, want the database write time", meta.Daemon.LastCycleUnix)
	}
	if meta.Database.Events != 4 || meta.Database.SchemaVersion != store.SchemaVersion {
		t.Errorf("database = %+v, want 4 events at schema v%d", meta.Database, store.SchemaVersion)
	}
	if meta.Database.EarliestEventUnix != seedTime.Unix() ||
		meta.Database.LatestEventUnix != seedTime.Add(2*time.Hour).Unix() {
		t.Errorf("database range = [%d,%d], want the seeded span",
			meta.Database.EarliestEventUnix, meta.Database.LatestEventUnix)
	}
	if meta.Resources.CPU != 0.25 || meta.Resources.Memory != 0.5 || meta.Resources.Disk != 0.75 {
		t.Errorf("resources = %+v, want the injected gauges", meta.Resources)
	}
	if len(meta.Tools) != 2 {
		t.Errorf("tools = %v, want both seeded tools", meta.Tools)
	}
	if meta.Capabilities.EmbeddedUI != HasEmbeddedUI() {
		t.Errorf("capabilities.embedded_ui = %v, want %v", meta.Capabilities.EmbeddedUI, HasEmbeddedUI())
	}
	assertNoRaw(t, body)
}

// TestMetaOnAnEmptyLedger: nothing collected yet is an ordinary 200 with honest
// zeros, not an error and not a made-up range.
func TestMetaOnAnEmptyLedger(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	var meta metaResponse
	getJSON(t, srv, "/api/meta", &meta)

	if meta.Database.Events != 0 {
		t.Errorf("events = %d, want 0", meta.Database.Events)
	}
	if meta.Watermark != 0 {
		t.Errorf("watermark = %d, want 0 for an empty ledger", meta.Watermark)
	}
	if meta.Database.EarliestEventUnix != 0 || meta.Database.LatestEventUnix != 0 {
		t.Errorf("range = [%d,%d], want zeros", meta.Database.EarliestEventUnix, meta.Database.LatestEventUnix)
	}
	if meta.Tools == nil {
		t.Error("tools is null; an empty ledger must serialise [] so the page can iterate it")
	}
	if meta.Daemon.Running || meta.Daemon.PID != 0 {
		t.Errorf("daemon = %+v, want a stopped daemon when no probe is wired", meta.Daemon)
	}
}

// TestSummaryMatchesTheStore compares the served buckets against the store's own
// answer over the same filter. Both sides go through a store query, so the
// bucket keys are produced by SQLite on both - the only way two surfaces reading
// the same database can be compared at all (CLAUDE.md, time buckets).
func TestSummaryMatchesTheStore(t *testing.T) {
	events := defaultEvents()
	srv, _ := newTestServer(t, events)
	since, until := seedTime, seedTime.Add(3*time.Hour)

	var got summaryResponse
	body := getJSON(t, srv, "/api/summary?"+rangeQuery(since, until)+"&group_by=day&group_by=tool", &got)

	want, err := srv.reader.SummarizeRollup(context.Background(), store.Filter{
		Since: since, Until: until, GroupBy: []string{"day", "tool"},
	})
	if err != nil {
		t.Fatalf("store summarize: %v", err)
	}
	if len(got.Buckets) != len(want.Buckets) {
		t.Fatalf("buckets = %d, want %d", len(got.Buckets), len(want.Buckets))
	}
	for i, b := range want.Buckets {
		if got.Buckets[i].Total != b.Total || got.Buckets[i].Events != b.Events {
			t.Errorf("bucket %d = %+v, want total=%d events=%d", i, got.Buckets[i], b.Total, b.Events)
		}
		if got.Buckets[i].Keys["tool"] != b.Keys["tool"] || got.Buckets[i].Keys["day"] != b.Keys["day"] {
			t.Errorf("bucket %d keys = %v, want %v", i, got.Buckets[i].Keys, b.Keys)
		}
	}
	if got.Totals.Total != want.Totals.Total {
		t.Errorf("totals.total = %d, want %d", got.Totals.Total, want.Totals.Total)
	}
	if got.Totals.Events != 4 {
		t.Errorf("totals.events = %d, want 4", got.Totals.Events)
	}
	// Cost carries its precision: three priced rows sum, the unpriced one is
	// counted rather than valued at zero.
	if got.Totals.CostMicroUSD != 8_000 {
		t.Errorf("totals.cost_micro_usd = %d, want 8000 (the stamped rows only)", got.Totals.CostMicroUSD)
	}
	if got.Totals.UnpricedEvents != 1 {
		t.Errorf("totals.unpriced_events = %d, want 1", got.Totals.UnpricedEvents)
	}
	if got.Since != want.Since.Unix() || got.Until != want.Until.Unix() {
		t.Errorf("range echoed as [%d,%d], want the snapped [%d,%d]",
			got.Since, got.Until, want.Since.Unix(), want.Until.Unix())
	}
	assertNoRaw(t, body)
}

// TestSummaryOfAnUnpricedRangeIsNotAnError: a range where nothing could be
// priced still answers 200 with real token counts and a zero cost that says why.
func TestSummaryOfAnUnpricedRangeIsNotAnError(t *testing.T) {
	events := []model.UsageEvent{
		seedEvent("u1", model.ToolClaudeCode, "sonnet-4", "/w/beta", model.ProviderAnthropic, "s", seedTime, nil),
		seedEvent("u2", model.ToolClaudeCode, "sonnet-4", "/w/beta", model.ProviderAnthropic, "s", seedTime, nil),
	}
	srv, _ := newTestServer(t, events)

	var got summaryResponse
	getJSON(t, srv, "/api/summary?"+rangeQuery(seedTime, seedTime.Add(time.Hour)), &got)

	if got.Totals.Events != 2 {
		t.Fatalf("events = %d, want 2", got.Totals.Events)
	}
	if got.Totals.CostMicroUSD != 0 || got.Totals.UnpricedEvents != 2 {
		t.Errorf("cost = %d with %d unpriced, want 0 cost and 2 unpriced",
			got.Totals.CostMicroUSD, got.Totals.UnpricedEvents)
	}
	if got.Totals.Total == 0 {
		t.Error("tokens are zero; unpriced must not mean uncounted")
	}
}

// TestSummaryOfAnEmptyRange: a window with nothing in it is a 200 with an empty
// bucket list, not a 404 and not null.
func TestSummaryOfAnEmptyRange(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	far := seedTime.Add(240 * time.Hour)

	body := getJSON(t, srv, "/api/summary?"+rangeQuery(far, far.Add(time.Hour))+"&group_by=day", nil)
	var got summaryResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Buckets == nil {
		t.Error("buckets is null; an empty range must serialise []")
	}
	if len(got.Buckets) != 0 || got.Totals.Events != 0 {
		t.Errorf("got %d buckets / %d events, want an empty answer", len(got.Buckets), got.Totals.Events)
	}
	if got.GroupBy == nil {
		t.Error("group_by is null; it must echo the request as a list")
	}
}

// TestSummaryFallsBackToTheLedgerForSessions: the derived rollup keeps no
// session dimension. Asking for one must be answered by the ledger with real
// numbers, never refused and never answered with zeros.
func TestSummaryFallsBackToTheLedgerForSessions(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	var got summaryResponse
	getJSON(t, srv, "/api/summary?"+rangeQuery(seedTime, seedTime.Add(3*time.Hour))+"&group_by=session", &got)

	if len(got.Buckets) != 3 {
		t.Fatalf("session buckets = %d, want 3 (sess-a, sess-b, sess-c)", len(got.Buckets))
	}
	if got.Totals.Sessions != 3 {
		t.Errorf("totals.sessions = %d, want 3; the ledger path must count them", got.Totals.Sessions)
	}
	// The ledger path echoes the range as asked, not snapped.
	if got.Since != seedTime.Unix() {
		t.Errorf("since = %d, want the requested %d", got.Since, seedTime.Unix())
	}
}

// TestSummaryServesTheProviderDimension is the other rollup gap: provider lives
// only in the ledger.
func TestSummaryServesTheProviderDimension(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	var got summaryResponse
	getJSON(t, srv, "/api/summary?"+rangeQuery(seedTime, seedTime.Add(3*time.Hour))+"&group_by=provider", &got)

	seen := map[string]int64{}
	for _, b := range got.Buckets {
		seen[b.Keys["provider"]] = b.Events
	}
	if seen[model.ProviderOpenAI] != 2 || seen[model.ProviderAnthropic] != 2 {
		t.Errorf("provider buckets = %v, want two events each for openai and anthropic", seen)
	}
}

// TestSummaryRejectsAnUnknownDimension: a bad parameter is a 400 naming the
// parameter, not a 500 quoting a store error.
func TestSummaryRejectsBadParameters(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	for _, target := range []string{
		"/api/summary?group_by=galaxy",
		"/api/summary?since=yesterday",
		"/api/summary?since=-5",
		"/api/summary?since=200&until=100",
		"/api/events?limit=none",
		"/api/events?cursor=abc",
	} {
		rec := get(t, srv, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 (body: %s)", target, rec.Code, rec.Body.String())
		}
	}
}

// TestFacetsAreOrderedHeaviestFirst: the page builds its lanes from this order.
func TestFacetsAreOrderedHeaviestFirst(t *testing.T) {
	events := append(defaultEvents(),
		seedEvent("a3", model.ToolCodex, "gpt-5", "/w/alpha", model.ProviderOpenAI, "sess-a", seedTime, cost(500)),
	)
	srv, _ := newTestServer(t, events)

	var got facetsResponse
	body := getJSON(t, srv, "/api/facets?"+rangeQuery(seedTime, seedTime.Add(3*time.Hour)), &got)

	if len(got.Tools) != 2 {
		t.Fatalf("tools = %+v, want 2", got.Tools)
	}
	if got.Tools[0].Value != model.ToolCodex {
		t.Errorf("tools[0] = %q, want the heaviest (%s)", got.Tools[0].Value, model.ToolCodex)
	}
	if got.Tools[0].Events != 3 {
		t.Errorf("tools[0].events = %d, want 3", got.Tools[0].Events)
	}
	for i := 1; i < len(got.Tools); i++ {
		if got.Tools[i-1].Total < got.Tools[i].Total {
			t.Errorf("tools not ordered by total: %+v", got.Tools)
		}
	}
	if len(got.Models) != 2 || len(got.Projects) != 2 {
		t.Errorf("models=%d projects=%d, want 2 each", len(got.Models), len(got.Projects))
	}
	// providers cannot come from the rollup; this is the ledger path.
	if len(got.Providers) != 2 {
		t.Errorf("providers = %+v, want 2", got.Providers)
	}
	assertNoRaw(t, body)
}

// TestFacetsOnAnEmptyLedger serialises empty lists, never null: the page maps
// over them without a guard.
func TestFacetsOnAnEmptyLedger(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	var got facetsResponse
	getJSON(t, srv, "/api/facets", &got)

	if got.Tools == nil || got.Models == nil || got.Providers == nil || got.Projects == nil {
		t.Fatalf("a facet list serialised as null: %+v", got)
	}
	if len(got.Tools)+len(got.Models)+len(got.Providers)+len(got.Projects) != 0 {
		t.Errorf("facets on an empty ledger = %+v, want all empty", got)
	}
}

// TestEventsProjection pins the row shape, including the two things it must NOT
// carry: the raw payload and a fabricated cost.
func TestEventsProjection(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	var got eventsResponse
	body := getJSON(t, srv, "/api/events?"+rangeQuery(seedTime, seedTime.Add(3*time.Hour)), &got)
	assertNoRaw(t, body)

	if len(got.Rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(got.Rows))
	}
	if got.Truncated || got.NextCursor != nil {
		t.Errorf("truncated=%v cursor=%v, want a complete first page", got.Truncated, got.NextCursor)
	}
	if got.Total != 4 || got.Limit != EventsPageLimit {
		t.Errorf("total=%d limit=%d, want 4 and %d", got.Total, got.Limit, EventsPageLimit)
	}

	var unpriced, priced int
	for _, r := range got.Rows {
		if r.Seq <= 0 {
			t.Errorf("row %+v has no seq; the cursor field must be the ledger id", r)
		}
		if r.CostMicroUSD == nil {
			unpriced++
		} else {
			priced++
		}
		if r.Total != 132 || r.Input != 100 {
			t.Errorf("row %+v lost its token counts", r)
		}
		if r.Kind != string(model.KindUsage) {
			t.Errorf("row kind = %q, want usage", r.Kind)
		}
	}
	if priced != 3 || unpriced != 1 {
		t.Errorf("priced=%d unpriced=%d, want 3 and 1", priced, unpriced)
	}
	// A null cost is the wire's "unpriced". A zero would claim the request was
	// free, so the JSON itself is checked, not just the decoded pointer.
	if !bytes.Contains(body, []byte(`"cost_micro_usd":null`)) {
		t.Error("no null cost in the body; an unpriced row must serialise null")
	}
	// Source paths and dedup keys are not the dashboard's business.
	if bytes.Contains(body, []byte("source_path")) || bytes.Contains(body, []byte("dedup")) {
		t.Errorf("the event projection leaked storage-internal fields: %s", body)
	}
}

// TestEventsCapAndCursorWalk is the cap contract: a range holding more rows than
// the cap comes back capped, marked truncated, carrying the TRUE count, and with
// a cursor that walks the rest exactly once each.
func TestEventsCapAndCursorWalk(t *testing.T) {
	const seeded = EventsPageLimit + 234
	events := make([]model.UsageEvent, 0, seeded)
	for i := 0; i < seeded; i++ {
		at := seedTime.Add(time.Duration(i) * time.Second)
		events = append(events, seedEvent(fmt.Sprintf("cap-%04d", i),
			model.ToolCodex, "gpt-5", "/w/alpha", model.ProviderOpenAI, "sess-cap", at, cost(1)))
	}
	srv, _ := newTestServer(t, events)
	window := rangeQuery(seedTime, seedTime.Add(24*time.Hour))

	var first eventsResponse
	getJSON(t, srv, "/api/events?"+window, &first)

	if len(first.Rows) != EventsPageLimit {
		t.Fatalf("first page = %d rows, want the cap %d", len(first.Rows), EventsPageLimit)
	}
	if !first.Truncated {
		t.Error("truncated = false on a capped page; that is a silent slice")
	}
	if first.Total != seeded {
		t.Errorf("total = %d, want the true count %d", first.Total, seeded)
	}
	if first.NextCursor == nil {
		t.Fatal("next_cursor is null on a truncated page; the rest would be unreachable")
	}

	// Walk the remainder and check every row is seen exactly once.
	seen := make(map[int64]bool, seeded)
	for _, r := range first.Rows {
		seen[r.Seq] = true
	}
	cursor := *first.NextCursor
	for pages := 0; cursor != ""; pages++ {
		if pages > 10 {
			t.Fatal("cursor walk did not terminate")
		}
		var page eventsResponse
		getJSON(t, srv, "/api/events?"+window+"&cursor="+cursor, &page)
		if page.Total != seeded {
			t.Errorf("page total = %d, want %d on every page", page.Total, seeded)
		}
		for _, r := range page.Rows {
			if seen[r.Seq] {
				t.Fatalf("row %d returned twice by the walk", r.Seq)
			}
			seen[r.Seq] = true
		}
		if page.NextCursor == nil {
			if page.Truncated {
				t.Error("last page says truncated with no cursor to continue")
			}
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != seeded {
		t.Errorf("walk covered %d rows, want all %d", len(seen), seeded)
	}
}

// TestEventsLimitIsClampedNotRefused: a client that asks for more than the cap
// gets the cap. The cap is a property of the server and a client is allowed not
// to know it yet.
func TestEventsLimitIsClampedNotRefused(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	var got eventsResponse
	getJSON(t, srv, "/api/events?limit=99999", &got)
	if got.Limit != EventsPageLimit {
		t.Errorf("limit = %d, want it clamped to %d", got.Limit, EventsPageLimit)
	}

	var small eventsResponse
	getJSON(t, srv, "/api/events?limit=2", &small)
	if len(small.Rows) != 2 || !small.Truncated || small.Total != 4 {
		t.Errorf("limit=2 page = %d rows truncated=%v total=%d, want 2/true/4",
			len(small.Rows), small.Truncated, small.Total)
	}
}

// TestEventsFilters checks the categorical filters reach the store.
func TestEventsFilters(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	var got eventsResponse
	getJSON(t, srv, "/api/events?tool="+model.ToolCodex, &got)
	if len(got.Rows) != 2 {
		t.Fatalf("tool-filtered rows = %d, want 2", len(got.Rows))
	}
	for _, r := range got.Rows {
		if r.Tool != model.ToolCodex {
			t.Errorf("row tool = %q, want the filtered tool", r.Tool)
		}
	}

	var bySession eventsResponse
	getJSON(t, srv, "/api/events?session=sess-c", &bySession)
	if len(bySession.Rows) != 1 {
		t.Errorf("session-filtered rows = %d, want 1", len(bySession.Rows))
	}
}

// TestTooManyFilterValues bounds the IN list an unauthenticated caller can build.
func TestTooManyFilterValues(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	target := "/api/events?"
	for i := 0; i <= maxFilterValues; i++ {
		target += fmt.Sprintf("tool=t%d&", i)
	}
	if rec := get(t, srv, target); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized filter = %d, want 400", rec.Code)
	}
}

// TestWriteMethodsAreRefused: the API is read-only, and a POST is not a route
// that is missing.
func TestWriteMethodsAreRefused(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	for _, target := range []string{"/api/meta", "/api/summary", "/api/facets", "/api/events"} {
		rec := do(t, srv, http.MethodPost, target, testHost)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", target, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("POST %s: no Allow header", target)
		}
	}
}

// TestUnknownAPIPathIsJSON: an unknown /api path must not fall through to the
// SPA and answer a JSON request with a page.
func TestUnknownAPIPathIsJSON(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	rec := get(t, srv, "/api/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/nope = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want JSON", ct)
	}
}

// TestResponsesAreNotCached: the numbers move every collection cycle, and a
// cached page showing yesterday's total is worse than a slow one.
func TestResponsesAreNotCached(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	for _, target := range []string{"/api/meta", "/api/summary", "/api/facets", "/api/events"} {
		rec := get(t, srv, target)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", target, got)
		}
	}
}
