package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// upstreamJSON is a minimal stand-in for LiteLLM's bare model map.
const upstreamJSON = `{
  "claude-sonnet-4-6": {"input_cost_per_token": 2e-06, "output_cost_per_token": 2e-05,
                        "litellm_provider": "anthropic", "mode": "chat"},
  "not-a-model": "sample_spec"
}`

// TestLadderPrecedence walks every rung: an override beats the refreshed table,
// which beats the embedded snapshot, and a model none of them knows is unpriced
// rather than free.
func TestLadderPrecedence(t *testing.T) {
	const m = "claude-sonnet-4-6"
	charge := Charge{Model: m, Provider: model.ProviderAnthropic, Input: 1_000_000}

	// Rung 3 only: the embedded snapshot.
	e := New(Options{})
	micro, source, ok := e.Price(charge)
	if !ok || micro <= 0 {
		t.Fatalf("embedded rung: %d,%q,%v want a price", micro, source, ok)
	}
	embeddedMicro, embeddedSource := micro, source
	if got := embeddedSource[:len("embedded-")]; got != "embedded-" {
		t.Errorf("source = %q, want an embedded-* stamp", embeddedSource)
	}

	// Rung 2 wins over rung 3.
	e.refreshed = &Table{Source: "litellm-2026-08-09", Models: map[string]Rates{m: {Input: 7e-06}}}
	micro, source, ok = e.Price(charge)
	if !ok || micro != 7_000_000 || source != "litellm-2026-08-09" {
		t.Fatalf("refreshed rung: %d,%q,%v want 7000000,litellm-2026-08-09,true", micro, source, ok)
	}

	// Rung 1 wins over both.
	e = New(Options{Overrides: map[string]Rates{m: {Input: 1e-05}}})
	e.refreshed = &Table{Source: "litellm-2026-08-09", Models: map[string]Rates{m: {Input: 7e-06}}}
	micro, source, ok = e.Price(charge)
	if !ok || micro != 10_000_000 || source != SourceOverride {
		t.Fatalf("override rung: %d,%q,%v want 10000000,override,true", micro, source, ok)
	}

	// Nothing knows this model: unpriced, and emphatically not zero.
	micro, source, ok = e.Price(Charge{Model: "no-such-model-anywhere", Input: 1_000_000})
	if ok || micro != 0 || source != "" {
		t.Fatalf("unknown model: %d,%q,%v want 0,\"\",false", micro, source, ok)
	}

	if embeddedMicro == 7_000_000 {
		t.Errorf("embedded rate coincidentally equals the test's refreshed rate; the test proves nothing")
	}
}

// TestPriceNeverReportsSilentZero is the "$0.00 is a lie" guard: rates that
// value real usage at exactly nothing must report unpriced, so the display
// layer shows a dash instead of a free lunch.
func TestPriceNeverReportsSilentZero(t *testing.T) {
	e := New(Options{})
	// Output-only rates against an input-only charge: a real 0 for real tokens.
	e.refreshed = &Table{Source: "litellm-test", Models: map[string]Rates{
		"weird": {Output: 1e-05},
	}}
	micro, source, ok := e.Price(Charge{Model: "weird", Input: 1000})
	if ok || micro != 0 || source != "" {
		t.Fatalf("zero-valued charge: %d,%q,%v want unpriced", micro, source, ok)
	}

	// A charge with no tokens at all is genuinely free and may be stamped.
	micro, _, ok = e.Price(Charge{Model: "weird"})
	if !ok || micro != 0 {
		t.Fatalf("empty charge: %d,%v want 0,true", micro, ok)
	}
}

// TestPriceEventUsesToolReasoningRule checks the collector-facing entry point
// threads the per-tool reasoning rule through to the bill.
func TestPriceEventUsesToolReasoningRule(t *testing.T) {
	e := New(Options{Overrides: map[string]Rates{"m": {Input: 1e-06, Output: 1e-05}}})

	subset, _, ok := e.PriceEvent(model.UsageEvent{
		Tool: model.ToolClaudeCode, Model: "m", OutputTokens: 100, ReasoningTokens: 50,
	})
	if !ok || subset != 1000 {
		t.Fatalf("subset tool = %d,%v want 1000,true", subset, ok)
	}
	additive, _, ok := e.PriceEvent(model.UsageEvent{
		Tool: model.ToolOpenCode, Model: "m", OutputTokens: 100, ReasoningTokens: 50,
	})
	if !ok || additive != 1500 {
		t.Fatalf("additive tool = %d,%v want 1500,true", additive, ok)
	}
}

// TestRefreshFetchesCachesAndSwaps covers the happy path: the upstream table is
// fetched, becomes the middle rung, and lands in the data dir so the next
// process starts with it.
func TestRefreshFetchesCachesAndSwaps(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(upstreamJSON))
	}))
	defer srv.Close()

	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	micro, source, ok := e.Price(Charge{
		Model: "claude-sonnet-4-6", Provider: model.ProviderAnthropic, Input: 1_000_000,
	})
	if !ok || micro != 2_000_000 || source != "litellm-2026-08-09" {
		t.Fatalf("after refresh: %d,%q,%v want 2000000,litellm-2026-08-09,true", micro, source, ok)
	}

	// A fresh process must pick the cache up without touching the network.
	reloaded := New(Options{DataDir: dir, Refresh: false})
	micro, source, ok = reloaded.Price(Charge{
		Model: "claude-sonnet-4-6", Provider: model.ProviderAnthropic, Input: 1_000_000,
	})
	if !ok || micro != 2_000_000 || source != "litellm-2026-08-09" {
		t.Fatalf("cache reload: %d,%q,%v want the refreshed table", micro, source, ok)
	}
}

// TestRefreshDisabledNeverFetches proves pricing.refresh=false is a real air
// gap: no request leaves the process.
func TestRefreshDisabledNeverFetches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write([]byte(upstreamJSON))
	}))
	defer srv.Close()

	e := New(Options{DataDir: t.TempDir(), Refresh: false})
	e.url = srv.URL
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if hits != 0 {
		t.Errorf("refresh disabled but the URL was fetched %d time(s)", hits)
	}
}

// TestRefreshThrottlesOnCacheAge lets the caller invoke Refresh every cycle: a
// cache younger than the interval skips the network entirely.
func TestRefreshThrottlesOnCacheAge(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write([]byte(upstreamJSON))
	}))
	defer srv.Close()

	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL

	for i := 0; i < 3; i++ {
		if err := e.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("fetches = %d, want 1 (throttled by the cache age)", hits)
	}

	// Age the cache past the interval and the next call fetches again.
	old := nowFn().Add(-2 * refreshInterval)
	if err := os.Chtimes(filepath.Join(dir, cacheFile), old, old); err != nil {
		t.Fatalf("age cache: %v", err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh after ageing: %v", err)
	}
	if hits != 2 {
		t.Errorf("fetches = %d, want 2 after the cache expired", hits)
	}
}

// TestRefreshFailureKeepsPreviousTable is the silence contract: a dead upstream
// must leave the loaded table exactly as it was, and must not poison the cache.
func TestRefreshFailureKeepsPreviousTable(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	e := New(Options{DataDir: dir, Refresh: true})
	e.url = srv.URL
	e.refreshed = &Table{Source: "litellm-old", Models: map[string]Rates{"m": {Input: 1e-06}}}

	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error from a failing upstream")
	}
	micro, source, ok := e.Price(Charge{Model: "m", Input: 1_000_000})
	if !ok || micro != 1_000_000 || source != "litellm-old" {
		t.Errorf("after failed refresh: %d,%q,%v want the previous table intact", micro, source, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheFile)); err == nil {
		t.Errorf("a failed refresh wrote a cache file")
	}
}

// TestRefreshRejectsGarbageUpstream stops a mangled or truncated feed from
// replacing a working table with nothing.
func TestRefreshRejectsGarbageUpstream(t *testing.T) {
	pinNow(t, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"broken":`))
	}))
	defer srv.Close()

	e := New(Options{DataDir: t.TempDir(), Refresh: true})
	e.url = srv.URL
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("expected a parse error for a truncated upstream table")
	}
	if e.refreshed != nil {
		t.Errorf("garbage upstream installed a table: %+v", e.refreshed)
	}
}

// TestDecodeSkipsUnpriceableEntries checks the tolerant decoder: upstream ships
// non-object records and rows with no rates, and neither may cost the user the
// rest of the table.
func TestDecodeSkipsUnpriceableEntries(t *testing.T) {
	raw, err := decodeUpstream([]byte(upstreamJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := decodeModels(raw)
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1 (the string record must be skipped)", len(models))
	}
	if _, ok := models["claude-sonnet-4-6"]; !ok {
		t.Errorf("priceable model missing from %v", models)
	}
}

// pinNow freezes the package clock for the duration of a test.
func pinNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := nowFn
	nowFn = func() time.Time { return at }
	t.Cleanup(func() { nowFn = prev })
}
