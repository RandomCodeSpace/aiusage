package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// rawMarker is a payload no honest response may ever contain. It stands in for
// what the raw column really holds on an old ledger: a transcript line, not a
// usage object. Every test that serialises a response looks for it.
const rawMarker = "TRANSCRIPT-CONTENT-THAT-MUST-NEVER-BE-SERVED"

// seedTime is the base instant for seeded events. It is an exact UTC hour so the
// rollup (keyed by 15-minute UTC buckets) and the ledger bucket every event
// identically whatever the machine's timezone is - the trap documented in
// CLAUDE.md.
var seedTime = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

// seedEvent builds one deterministic ledger row. A nil cost means UNPRICED,
// which is a state the API has to carry honestly rather than render as free.
func seedEvent(key, tool, mdl, project, provider, session string, at time.Time, cost *int64) model.UsageEvent {
	e := model.UsageEvent{
		Tool:                tool,
		Model:               mdl,
		Provider:            provider,
		ServiceTier:         "standard",
		SessionID:           session,
		Project:             project,
		EventTime:           at,
		ObservedTime:        at.Add(time.Minute),
		InputTokens:         100,
		OutputTokens:        20,
		CacheCreationTokens: 5,
		CacheReadTokens:     7,
		ReasoningTokens:     3,
		TotalTokens:         132,
		DedupKey:            key,
		Kind:                model.KindUsage,
		SourcePath:          "/home/someone/.config/agent/sessions/" + key + ".jsonl",
		RequestID:           "req-" + key,
		Raw:                 rawMarker,
	}
	if cost != nil {
		e.SetCost(*cost, "litellm-2026-06-01")
	}
	return e
}

// cost is a pointer helper for a stamped cost.
func cost(microUSD int64) *int64 { return &microUSD }

// seedLedger writes events into a fresh database and returns its path. The
// events go in through the real store, so the derived rollup is maintained by
// the same in-transaction delta the collector uses - these tests exercise the
// production read path, not a fixture that agrees with it by construction.
func seedLedger(t *testing.T, events []model.UsageEvent) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if len(events) > 0 {
		if _, err := st.InsertEvents(context.Background(), events); err != nil {
			t.Fatalf("seed events: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return path
}

// defaultEvents is the small mixed ledger most tests read: two tools, two
// projects, two providers, one priced and one unpriced row per tool, spread over
// three hours.
func defaultEvents() []model.UsageEvent {
	return []model.UsageEvent{
		seedEvent("a1", model.ToolCodex, "gpt-5", "/w/alpha", model.ProviderOpenAI, "sess-a", seedTime, cost(1_500)),
		seedEvent("a2", model.ToolCodex, "gpt-5", "/w/alpha", model.ProviderOpenAI, "sess-a", seedTime.Add(time.Hour), cost(2_500)),
		seedEvent("b1", model.ToolClaudeCode, "sonnet-4", "/w/beta", model.ProviderAnthropic, "sess-b", seedTime.Add(time.Hour), nil),
		seedEvent("b2", model.ToolClaudeCode, "sonnet-4", "/w/beta", model.ProviderAnthropic, "sess-c", seedTime.Add(2*time.Hour), cost(4_000)),
	}
}

// openReader opens the seeded database the way `serve` does: read-only.
func openReader(t *testing.T, path string) *store.SQLite {
	t.Helper()
	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newTestServer builds a Server over a seeded ledger with a frozen clock.
func newTestServer(t *testing.T, events []model.UsageEvent) (*Server, string) {
	t.Helper()
	path := seedLedger(t, events)
	srv, err := New(openReader(t, path), Options{
		DBPath:        path,
		ServerVersion: "v0.0.0-test",
		Now:           func() time.Time { return seedTime.Add(4 * time.Hour) },
		PollInterval:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, path
}

// testHost is the Host every handler test sends. It is not decoration:
// httptest.NewRequest defaults to example.com, which the Host allowlist refuses
// with 421 exactly as it would refuse a rebinding attack.
const testHost = "127.0.0.1:37800"

// get issues a request against the handler and returns the recorded response.
func get(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s, http.MethodGet, target, testHost)
}

// do issues one request with an explicit method and Host.
func do(t *testing.T, s *Server, method, target, host string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getJSON issues a request, asserts 200, decodes the body into v and returns the
// raw body so a test can also assert on the bytes that went over the wire.
func getJSON(t *testing.T, s *Server, target string, v any) []byte {
	t.Helper()
	rec := get(t, s, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body: %s)", target, rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			t.Fatalf("decode %s: %v (body: %s)", target, err, body)
		}
	}
	return body
}

// rangeQuery renders the since/until pair the contract uses.
func rangeQuery(since, until time.Time) string {
	return fmt.Sprintf("since=%d&until=%d", since.Unix(), until.Unix())
}
