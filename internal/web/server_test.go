package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

func openLedger(t *testing.T) *store.Ledger {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func event(key, tool, mdl, session string, at time.Time, total int64, micro int64) model.UsageEvent {
	e := model.UsageEvent{
		Tool: tool, Model: mdl, SessionID: session, Project: "/w/" + tool,
		EventTime: at, ObservedTime: at, InputTokens: total, TotalTokens: total,
		DedupKey: key, Kind: model.KindUsage,
	}
	if micro >= 0 {
		e.SetCost(micro, "litellm-2026-09-02")
	}
	return e
}

// fixture seeds one hour-old priced claude-code turn under a subagent, one
// three-hour-old codex turn, one two-day-old unpriced gemini row and a pi row
// from ten days ago, and returns a server pinned to the clock they were placed
// against.
func fixture(t *testing.T) (*Server, *store.Ledger, time.Time) {
	t.Helper()
	st := openLedger(t)
	now := time.Now().Truncate(time.Second)
	ctx := context.Background()
	batch := store.ObservationBatch{
		Events: []model.UsageEvent{
			event("cc-1", model.ToolClaudeCode, "claude-opus-5", "s1", now.Add(-30*time.Minute), 1000, 250_000),
			event("cx-1", model.ToolCodex, "gpt-5", "s2", now.Add(-3*time.Hour), 500, 40_000),
			event("gm-1", "gemini", "gemini-2.5-flash-lite", "s3", now.Add(-48*time.Hour), 300, -1),
			event("pi-1", "pi", "claude-sonnet-5", "s4", now.Add(-10*24*time.Hour), 200, 10_000),
		},
		TurnContexts: []model.TurnContext{
			{UsageDedupKey: "cc-1", Tool: model.ToolClaudeCode, Dimension: model.DimensionAgent, Value: "Explore",
				SessionID: "s1", Project: "/w/claude-code", Model: "claude-opus-5", EventTime: now.Add(-30 * time.Minute)},
		},
	}
	if _, err := st.ApplyBatch(ctx, batch); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := New(st.Reader, Options{
		Now:  func() time.Time { return now },
		Poll: 10 * time.Millisecond,
		Capabilities: map[string]model.ToolCapability{
			model.ToolClaudeCode: {Tool: model.ToolClaudeCode, Cost: "computed", Tier: "live"},
		},
	})
	return srv, st, now
}

func getJSON(t *testing.T, h http.Handler, path string, into any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "localhost:8930"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("decode %s: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec
}

func TestNowSnapshot(t *testing.T) {
	srv, _, _ := fixture(t)
	var n Now
	if rec := getJSON(t, srv.Handler(), "/api/now", &n); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n.LastHour.MicroUSD != 250_000 || n.LastHour.Sessions != 1 || n.LastHour.Harnesses != 1 {
		t.Errorf("last hour = %+v, want 250000 micro-USD, 1 session, 1 harness", n.LastHour)
	}
	if n.LastHour.Computed != 1 || n.LastHour.Unpriced != 0 {
		t.Errorf("last hour provenance = computed %d unpriced %d, want 1 and 0", n.LastHour.Computed, n.LastHour.Unpriced)
	}
	if len(n.Hours) != 24 {
		t.Fatalf("hours = %d, want 24", len(n.Hours))
	}
	var hourly int64
	for _, h := range n.Hours {
		hourly += h.MicroUSD
	}
	if hourly != 290_000 {
		t.Errorf("hourly bars sum to %d, want 290000", hourly)
	}
	if got := toolsOf(n.Harness); strings.Join(got, ",") != "claude-code,codex,gemini" {
		t.Errorf("board = %v, want claude-code,codex,gemini (newest first)", got)
	}
	if got := toolsOf(n.Idle); strings.Join(got, ",") != "pi" {
		t.Errorf("idle = %v, want pi", got)
	}
	if n.Harness[0].CostFrom != "computed" || n.Harness[0].Tier != "live" {
		t.Errorf("claude-code capability = %q/%q, want computed/live", n.Harness[0].CostFrom, n.Harness[0].Tier)
	}
	// Thirty minutes ago is this hour or the previous one, depending on the
	// wall clock the test runs at; either way it is the tail of the spark.
	if sp := n.Harness[0].Spark; sp[22]+sp[23] != 1000 {
		t.Errorf("claude-code spark = %v, want 1000 tokens in the last two hours", sp)
	}
	if n.Unpriced.Events != 1 || strings.Join(n.Unpriced.Models, ",") != "gemini-2.5-flash-lite" {
		t.Errorf("unpriced 7d = %+v, want 1 row on gemini-2.5-flash-lite", n.Unpriced)
	}
	if len(n.Models) != 2 || n.Models[0].Model != "claude-opus-5" {
		t.Errorf("models 24h = %+v, want claude-opus-5 first of 2", n.Models)
	}
	if n.Watermark.IsZero() || n.RollupStale {
		t.Errorf("watermark %v stale %v, want set and false", n.Watermark, n.RollupStale)
	}
}

func toolsOf(hs []Harness) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Tool
	}
	return out
}

func TestHistoryUsageDimension(t *testing.T) {
	srv, _, _ := fixture(t)
	var h History
	if rec := getJSON(t, srv.Handler(), "/api/history?dim=tool&range=7d", &h); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.Days) != 7 || len(h.Rows) != 3 {
		t.Fatalf("days %d rows %d, want 7 and 3", len(h.Days), len(h.Rows))
	}
	if h.Rows[0].Value != "claude-code" || h.Rows[1].Value != "codex" || h.Rows[2].Value != "gemini" {
		t.Errorf("rows = %+v, want claude-code, codex, gemini by cost", h.Rows)
	}
	if h.Total.Cost.MicroUSD != 290_000 || h.Total.Cost.Unpriced != 1 {
		t.Errorf("total = %+v, want 290000 with 1 unpriced", h.Total.Cost)
	}
	if h.Coverage != nil {
		t.Errorf("usage dimension carries coverage %+v, want none", h.Coverage)
	}
	if len(h.Series) != 3 || len(h.Series[0].MicroUSD) != 7 || h.Series[0].MicroUSD[6] != 250_000 {
		t.Errorf("series = %+v, want claude-code's 250000 on the last day", h.Series)
	}
}

func TestHistoryTurnDimension(t *testing.T) {
	srv, _, _ := fixture(t)
	var h History
	if rec := getJSON(t, srv.Handler(), "/api/history?dim=agent&range=7d", &h); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.Rows) != 1 || h.Rows[0].Value != "Explore" || h.Rows[0].Cost.MicroUSD != 250_000 {
		t.Fatalf("rows = %+v, want Explore at 250000", h.Rows)
	}
	if h.Coverage == nil || h.Coverage.Turns != 1 || h.Coverage.Of != 3 {
		t.Errorf("coverage = %+v, want 1 of 3", h.Coverage)
	}
	if len(h.Series) != 1 || h.Series[0].MicroUSD[6] != 250_000 {
		t.Errorf("series = %+v, want Explore's 250000 on the last day", h.Series)
	}
}

func TestHistoryRejectsUnknownInputs(t *testing.T) {
	srv, _, _ := fixture(t)
	for _, path := range []string{"/api/history?dim=kind", "/api/history?dim=tool&range=year"} {
		if rec := getJSON(t, srv.Handler(), path, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, rec.Code)
		}
	}
}

func TestHostGuardRefusesStrangers(t *testing.T) {
	srv, _, _ := fixture(t)
	for host, want := range map[string]int{
		"localhost:8930": http.StatusOK, "127.0.0.1": http.StatusOK, "[::1]:8930": http.StatusOK,
		"evil.example": http.StatusMisdirectedRequest, "": http.StatusMisdirectedRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/now", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: status %d, want %d", host, rec.Code, want)
		}
	}
	srv2 := New(srv.src, Options{AllowedHosts: []string{"Usage.Example.NET"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "usage.example.net:443"
	rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("allowed host: status %d, want 200", rec.Code)
	}
}

func TestPageAndPolicy(t *testing.T) {
	srv, _, _ := fixture(t)
	rec := getJSON(t, srv.Handler(), "/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/static/app.js") {
		t.Fatalf("page: status %d body %q", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("csp = %q, want connect-src 'self'", csp)
	}
	if rec := getJSON(t, srv.Handler(), "/static/app.js", nil); rec.Code != http.StatusOK {
		t.Errorf("app.js: status %d", rec.Code)
	}
}

// TestEventStreamFollowsWatermark runs the real server and checks that the
// stream says the watermark on connect and again when a new row lands.
func TestEventStreamFollowsWatermark(t *testing.T) {
	srv, st, now := fixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, ln) }()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	// The watermark is the LAST row by id, not the latest event: the seed's
	// final row is the ten-day-old pi event.
	rd := bufio.NewReader(resp.Body)
	first := readTick(t, rd)
	if want := now.Add(-10 * 24 * time.Hour); !first.Equal(want) {
		t.Fatalf("first tick %v, want the last seeded row's observed time %v", first, want)
	}

	later := now.Add(time.Minute)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{event("cc-2", model.ToolClaudeCode, "claude-opus-5", "s1", later, 10, 1)}); err != nil {
		t.Fatal(err)
	}
	second := readTick(t, rd)
	if !second.Equal(later) {
		t.Fatalf("second tick %v, want %v", second, later)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after cancel with a stream open")
	}
}

func readTick(t *testing.T, r *bufio.Reader) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg struct {
			Watermark time.Time `json:"watermark"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		return msg.Watermark
	}
	t.Fatal("no tick within 5s")
	return time.Time{}
}
