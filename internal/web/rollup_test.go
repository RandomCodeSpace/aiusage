package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// emptyRollupServer seeds a ledger, empties the derived rollup behind the
// store's back, and returns a read-only server over the result: the exact state
// the v4 migration leaves on a machine where no collection pass has run since.
// The rollup is cleared through a raw SQL handle because the store offers no
// way to damage its own summary, which is the point.
func emptyRollupServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := seedLedger(t, defaultEvents())
	clearRollup(t, path)

	srv, err := New(openReader(t, path), Options{
		DBPath:        path,
		ServerVersion: "v0.0.0-test",
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, path
}

// clearRollup empties the rollup and rewinds its watermark, leaving the ledger
// untouched. It goes around the store on a raw handle because the store offers
// no way to damage its own summary - which is why the damaged state has to be
// staged from outside it.
func clearRollup(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`DELETE FROM usage_rollup`,
		`DELETE FROM schema_meta WHERE key='rollup_watermark'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// rebuildRollup refills the rollup the way a collection pass would.
func rebuildRollup(t *testing.T, path string) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.RebuildRollup(context.Background()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
}

// TestStaleRollupFallsBackToTheLedger is the honesty contract for a read-only
// server on a daemonless machine. The v4 migration creates the rollup empty and
// only a collection pass fills it; a server that trusted it would answer every
// question with zeros over a full ledger, forever, and say nothing.
func TestStaleRollupFallsBackToTheLedger(t *testing.T) {
	srv, path := emptyRollupServer(t)
	window := rangeQuery(seedTime, seedTime.Add(3*time.Hour))

	var stale summaryResponse
	getJSON(t, srv, "/api/summary?"+window+"&group_by=day&group_by=tool", &stale)

	if stale.Source != sourceLedger {
		t.Errorf("source = %q with an empty rollup, want %q", stale.Source, sourceLedger)
	}
	if stale.Totals.Events != 4 || stale.Totals.Total != 4*132 {
		t.Fatalf("totals = events %d / total %d, want 4 / %d from the ledger",
			stale.Totals.Events, stale.Totals.Total, 4*132)
	}
	if len(stale.Buckets) == 0 {
		t.Fatal("no buckets: the fallback served the empty rollup after all")
	}

	// Facets take the same route, so the lanes the page draws are not empty
	// either.
	var facets facetsResponse
	getJSON(t, srv, "/api/facets?"+window, &facets)
	if facets.Source != sourceLedger {
		t.Errorf("facets source = %q, want %q", facets.Source, sourceLedger)
	}
	if len(facets.Tools) != 2 || len(facets.Models) != 2 || len(facets.Projects) != 2 {
		t.Errorf("facets = %d tools / %d models / %d projects, want 2 each", len(facets.Tools), len(facets.Models), len(facets.Projects))
	}

	// And /api/meta, whose tool list is built the same way.
	var meta metaResponse
	getJSON(t, srv, "/api/meta", &meta)
	if len(meta.Tools) != 2 {
		t.Errorf("meta tools = %v, want both seeded tools", meta.Tools)
	}

	// A rebuild is what a collection pass does. The very next request must
	// notice - the fallback is a state, not a latch - and the numbers must be
	// identical to the ones the ledger just gave.
	rebuildRollup(t, path)

	var fresh summaryResponse
	getJSON(t, srv, "/api/summary?"+window+"&group_by=day&group_by=tool", &fresh)
	if fresh.Source != sourceRollup {
		t.Fatalf("source = %q after a rebuild, want %q; the fallback never ended", fresh.Source, sourceRollup)
	}
	if fresh.Totals.Events != stale.Totals.Events || fresh.Totals.Total != stale.Totals.Total ||
		fresh.Totals.CostMicroUSD != stale.Totals.CostMicroUSD ||
		fresh.Totals.UnpricedEvents != stale.Totals.UnpricedEvents {
		t.Errorf("rollup totals %+v differ from the ledger's %+v over the same range",
			fresh.Totals, stale.Totals)
	}
	if len(fresh.Buckets) != len(stale.Buckets) {
		t.Fatalf("rollup returned %d buckets, ledger %d", len(fresh.Buckets), len(stale.Buckets))
	}
	for i := range fresh.Buckets {
		if fresh.Buckets[i].Keys["day"] != stale.Buckets[i].Keys["day"] ||
			fresh.Buckets[i].Keys["tool"] != stale.Buckets[i].Keys["tool"] ||
			fresh.Buckets[i].Total != stale.Buckets[i].Total {
			t.Errorf("bucket %d: rollup %+v vs ledger %+v", i, fresh.Buckets[i], stale.Buckets[i])
		}
	}

	var freshFacets facetsResponse
	getJSON(t, srv, "/api/facets?"+window, &freshFacets)
	if freshFacets.Source != sourceRollup {
		t.Errorf("facets source = %q after a rebuild, want %q", freshFacets.Source, sourceRollup)
	}
}

// TestFreshRollupIsCheckedOnceNotPerRequest: the verdict costs two aggregate
// queries, so it is cached against the database's write time and a short TTL. A
// server that asked per request would put a COUNT(*) over the whole ledger on
// the hot path it exists to avoid.
func TestFreshRollupIsCheckedOnceNotPerRequest(t *testing.T) {
	path := seedLedger(t, defaultEvents())
	counter := &countingReader{Reader: openReader(t, path)}
	srv, err := New(counter, Options{DBPath: path, Now: time.Now})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	for i := 0; i < 5; i++ {
		if rec := get(t, srv, "/api/summary?group_by=day"); rec.Code != http.StatusOK {
			t.Fatalf("summary %d = %d", i, rec.Code)
		}
	}
	if counter.stale != 1 {
		t.Errorf("RollupStale called %d times for 5 requests, want 1", counter.stale)
	}
}

// TestSourceIsAlwaysStated: a client cannot tell a cheap answer from an
// expensive one, or a fallback from a fast path, unless every answer says.
func TestSourceIsAlwaysStated(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	for _, tc := range []struct{ target, want string }{
		{"/api/summary?group_by=day", sourceRollup},
		{"/api/summary?group_by=session", sourceLedger},
		{"/api/summary?group_by=provider", sourceLedger},
		{"/api/facets", sourceRollup},
		// A session filter forces every list to the ledger however fresh the
		// rollup is; the label must follow the data, not the freshness verdict.
		{"/api/facets?session=s1", sourceLedger},
	} {
		body := getJSON(t, srv, tc.target, nil)
		var got struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode %s: %v", tc.target, err)
		}
		if got.Source != tc.want {
			t.Errorf("GET %s source = %q, want %q", tc.target, got.Source, tc.want)
		}
	}
}

// countingReader counts freshness checks. Everything else passes through to the
// real store, so the handlers under test are the production ones.
type countingReader struct {
	Reader
	stale int
}

func (c *countingReader) RollupStale(ctx context.Context) (bool, error) {
	c.stale++
	return c.Reader.RollupStale(ctx)
}
