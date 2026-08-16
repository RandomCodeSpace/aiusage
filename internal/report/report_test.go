package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

func sampleSummary() *store.Summary {
	return &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{
				Keys:          map[string]string{"tool": "claude-code"},
				OrderedKeys:   []string{"tool"},
				Events:        1200,
				Input:         2_000_000,
				Output:        912_345,
				CacheCreation: 50_000,
				CacheRead:     150_000,
				Total:         3_112_345,
			},
			{
				Keys:        map[string]string{"tool": "codex"},
				OrderedKeys: []string{"tool"},
				Events:      42,
				Input:       500,
				Output:      300,
				Total:       800,
			},
		},
		Totals: store.Bucket{
			Events:        1242,
			Input:         2_000_500,
			Output:        912_645,
			CacheCreation: 50_000,
			CacheRead:     150_000,
			Total:         3_113_145,
		},
	}
}

func TestRenderTableHasTotalsAndHeaders(t *testing.T) {
	out := RenderTable(sampleSummary(), Opt{})

	for _, want := range []string{"tool", colEvents, colInput, colOutput, colCache, colTotal, totalsLabel} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
	// The grouping value should appear.
	if !strings.Contains(out, "claude-code") {
		t.Errorf("table missing grouping value\n%s", out)
	}
}

func TestRenderTableHumanisesNumbers(t *testing.T) {
	out := RenderTable(sampleSummary(), Opt{})

	for _, want := range []string{"2.0M", "912.3K"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing humanised %q\n%s", want, out)
		}
	}
}

func TestRenderTableColorDoesNotPanicAndContainsData(t *testing.T) {
	out := RenderTable(sampleSummary(), Opt{Color: true})
	if !strings.Contains(out, "claude-code") {
		t.Errorf("colored table missing data\n%s", out)
	}
	if !strings.Contains(out, "2.0M") {
		t.Errorf("colored table missing humanised total\n%s", out)
	}
}

func TestRenderTableNoGrouping(t *testing.T) {
	sum := &store.Summary{
		Buckets: []store.Bucket{{
			Events: 5,
			Input:  100,
			Output: 200,
			Total:  300,
		}},
		Totals: store.Bucket{Events: 5, Input: 100, Output: 200, Total: 300},
	}
	out := RenderTable(sum, Opt{})
	if !strings.Contains(out, totalsLabel) {
		t.Errorf("ungrouped table missing TOTAL row\n%s", out)
	}
	if !strings.Contains(out, colTotal) {
		t.Errorf("ungrouped table missing Total header\n%s", out)
	}
}

func TestRenderTableNilSafe(t *testing.T) {
	if got := RenderTable(nil, Opt{}); got != "" {
		t.Errorf("expected empty string for nil summary, got %q", got)
	}
}

func TestHumanize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0K"},
		{912_345, "912.3K"},
		{2_000_000, "2.0M"},
		{1_500_000_000, "1.5G"},
		{-2_000_000, "-2.0M"},
	}
	for _, c := range cases {
		if got := humanize(c.in); got != c.want {
			t.Errorf("humanize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteSummaryJSONRoundTrips(t *testing.T) {
	sum := sampleSummary()
	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sum, nil); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("\n  ")) {
		t.Errorf("expected indented JSON, got:\n%s", buf.String())
	}
	var got store.Summary
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*sum, got) {
		t.Errorf("summary round-trip mismatch:\nwant %+v\ngot  %+v", *sum, got)
	}
}

// TestWriteSummaryJSONMatchesTableCost is the divergence guard: for a range
// holding rows collected before pricing existed, the JSON must report the same
// resolved cost the table renders, and must say that the figure is approximate.
// The stamped sum stays in the payload as the exact floor it is.
func TestWriteSummaryJSONMatchesTableCost(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{Keys: map[string]string{"tool": model.ToolClaudeCode}, Events: 2, CostMicroUSD: 5_000},
			{Keys: map[string]string{"tool": model.ToolCodex}, Events: 3, CostMicroUSD: 1_000, UnpricedEvents: 1},
		},
		Totals: store.Bucket{Events: 5, CostMicroUSD: 6_000, UnpricedEvents: 1},
	}
	groups := []store.UnpricedGroup{{
		Keys:   map[string]string{"tool": model.ToolCodex},
		Tool:   model.ToolCodex,
		Model:  "gpt-5",
		Events: 1,
		Input:  400,
	}}
	costs := ResolveCosts(sum, groups, fixedPricer{})

	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sum, costs); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}
	var got summaryJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The table's own numbers, read off the same resolved costs.
	table := RenderTable(sum, Opt{Costs: costs})
	if !strings.Contains(table, "~$0.0064") {
		t.Fatalf("table total is not the expected ~$0.0064:\n%s", table)
	}

	if got.Totals.DisplayCostMicroUSD != costs.Totals.MicroUSD {
		t.Errorf("json total = %d, table total = %d — the two surfaces disagree",
			got.Totals.DisplayCostMicroUSD, costs.Totals.MicroUSD)
	}
	if !got.Totals.CostApproximate {
		t.Errorf("json total is display-priced but not flagged approximate: %+v", got.Totals)
	}
	if got.Totals.CostMicroUSD != 6_000 {
		t.Errorf("stamped total = %d, want the untouched 6000 floor", got.Totals.CostMicroUSD)
	}
	if got.Buckets[1].DisplayCostMicroUSD != costs.Buckets[1].MicroUSD || !got.Buckets[1].CostApproximate {
		t.Errorf("json bucket = %+v, want the table's %+v", got.Buckets[1], costs.Buckets[1])
	}
	if !got.Buckets[0].CostKnown || got.Buckets[0].CostApproximate {
		t.Errorf("fully stamped bucket = %+v, want known and exact", got.Buckets[0])
	}
}

// TestWriteSummaryJSONUnpricedIsNotZero checks the third state survives the
// JSON surface: a bucket nothing could price reports CostKnown=false, so a
// consumer cannot read its 0 as a free request (the table prints "-").
//
// Asserting only that is worthless, because a payload MISSING the cost keys
// unmarshals into exactly the false/0 the unpriced bucket wants. So the summary
// carries a second, priceable bucket whose keys must come back non-zero and
// known, and the raw document is checked for the key names before anything is
// decoded into a struct that would invent them.
func TestWriteSummaryJSONUnpricedIsNotZero(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{
			{Keys: map[string]string{"tool": model.ToolCopilot}, Events: 4, UnpricedEvents: 4},
			{Keys: map[string]string{"tool": model.ToolCodex}, Events: 1, UnpricedEvents: 1},
		},
		Totals: store.Bucket{Events: 5, UnpricedEvents: 5},
	}
	groups := []store.UnpricedGroup{
		{
			Keys:   map[string]string{"tool": model.ToolCopilot},
			Tool:   model.ToolCopilot,
			Model:  "mystery-model",
			Events: 4,
			Input:  9_999,
		},
		{
			Keys:   map[string]string{"tool": model.ToolCodex},
			Tool:   model.ToolCodex,
			Model:  "gpt-5",
			Events: 1,
			Input:  700,
		},
	}
	costs := ResolveCosts(sum, groups, fixedPricer{miss: map[string]bool{"mystery-model": true}})
	if costs.Buckets[1].MicroUSD == 0 || !costs.Buckets[1].Known {
		t.Fatalf("test setup: the priceable bucket resolved to %+v, which cannot tell an absent key apart", costs.Buckets[1])
	}

	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sum, costs); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}

	// The keys must be in the document, not merely in the struct we decode into.
	var doc struct {
		Buckets []map[string]json.RawMessage
		Totals  map[string]json.RawMessage
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if len(doc.Buckets) != len(sum.Buckets) {
		t.Fatalf("payload has %d buckets, want %d", len(doc.Buckets), len(sum.Buckets))
	}
	for _, key := range []string{"CostKnown", "CostApproximate", "DisplayCostMicroUSD"} {
		if _, ok := doc.Totals[key]; !ok {
			t.Errorf("totals payload omits %s; an absent key decodes to the same zero an unpriced bucket reports", key)
		}
		for i, b := range doc.Buckets {
			if _, ok := b[key]; !ok {
				t.Errorf("bucket %d payload omits %s", i, key)
			}
		}
	}

	var got summaryJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The unpriceable bucket: unknown, not free.
	if got.Buckets[0].CostKnown || got.Buckets[0].DisplayCostMicroUSD != 0 {
		t.Errorf("unpriceable bucket = %+v, want an explicitly unknown cost", got.Buckets[0])
	}
	// The priceable one: the values a missing key could not have produced.
	if !got.Buckets[1].CostKnown || got.Buckets[1].DisplayCostMicroUSD != costs.Buckets[1].MicroUSD {
		t.Errorf("display-priced bucket = %+v, want known and %d", got.Buckets[1], costs.Buckets[1].MicroUSD)
	}
	if !got.Buckets[1].CostApproximate {
		t.Errorf("display-priced bucket = %+v, want the approximate marker", got.Buckets[1])
	}
	// Totals mix the two: known and approximate, and short of the mystery rows.
	if !got.Totals.CostKnown || got.Totals.DisplayCostMicroUSD != costs.Totals.MicroUSD || !got.Totals.CostApproximate {
		t.Errorf("totals = %+v, want known/approximate at %d", got.Totals, costs.Totals.MicroUSD)
	}
}

// TestWriteSummaryJSONKeysStable pins the payload's key set: the store summary
// keys existing consumers already read, plus the three cost keys. Removing or
// renaming any of them breaks a consumer silently.
func TestWriteSummaryJSONKeysStable(t *testing.T) {
	// ComputedCostEvents joined the set with the provenance work: it counts the
	// PRICED rows of a bucket that this project valued from a public rate card
	// rather than reading off the harness. Adding a key is additive — no
	// consumer reads a key it does not know about — while removing or renaming
	// one is what this test exists to catch.
	wantBucketKeys := []string{
		"CacheCreation", "CacheRead", "ComputedCostEvents", "CostApproximate", "CostKnown",
		"CostMicroUSD", "DisplayCostMicroUSD", "Events", "Input", "Keys", "OrderedKeys",
		"Output", "Reasoning", "Sessions", "Total", "UnpricedEvents",
	}

	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sampleSummary(), nil); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := sortedKeys(doc); !reflect.DeepEqual(got, []string{"Buckets", "GroupBy", "Totals"}) {
		t.Errorf("top-level keys = %v", got)
	}
	var buckets []map[string]json.RawMessage
	if err := json.Unmarshal(doc["Buckets"], &buckets); err != nil {
		t.Fatalf("unmarshal buckets: %v", err)
	}
	if got := sortedKeys(buckets[0]); !reflect.DeepEqual(got, wantBucketKeys) {
		t.Errorf("bucket keys changed:\nwant %v\ngot  %v", wantBucketKeys, got)
	}
	var totals map[string]json.RawMessage
	if err := json.Unmarshal(doc["Totals"], &totals); err != nil {
		t.Fatalf("unmarshal totals: %v", err)
	}
	if got := sortedKeys(totals); !reflect.DeepEqual(got, wantBucketKeys) {
		t.Errorf("totals keys differ from bucket keys:\nwant %v\ngot  %v", wantBucketKeys, got)
	}
}

// TestWriteSummaryJSONNilSummary keeps the pre-cost behaviour for an absent
// summary: a JSON null, not a panic.
func TestWriteSummaryJSONNilSummary(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, nil, nil); err != nil {
		t.Fatalf("WriteSummaryJSON(nil): %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "null" {
		t.Errorf("nil summary = %q, want null", got)
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sampleEvents() []model.UsageEvent {
	et := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	ot := time.Date(2026, 5, 29, 12, 5, 0, 0, time.UTC)
	return []model.UsageEvent{
		{
			Tool:                model.ToolClaudeCode,
			Model:               "claude-3",
			SessionID:           "sess-1",
			Project:             "/home/dev/projects/aiusage",
			EventTime:           et,
			ObservedTime:        ot,
			InputTokens:         100,
			OutputTokens:        50,
			CacheCreationTokens: 10,
			CacheReadTokens:     20,
			ReasoningTokens:     5,
			TotalTokens:         180,
			RequestID:           "req-1",
			MessageID:           "msg-1",
			SourcePath:          "/tmp/a.jsonl",
			DedupKey:            "claude-code|msg-1",
			Kind:                model.KindUsage,
		},
		{
			Tool:         model.ToolCodex,
			Model:        "gpt-5",
			EventTime:    et,
			ObservedTime: ot,
			InputTokens:  7,
			OutputTokens: 3,
			TotalTokens:  10,
			Kind:         model.KindUsage,
		},
	}
}

func TestWriteEventsJSONRoundTrips(t *testing.T) {
	evs := sampleEvents()
	var buf bytes.Buffer
	if err := WriteEventsJSON(&buf, evs); err != nil {
		t.Fatalf("WriteEventsJSON: %v", err)
	}
	var got []model.UsageEvent
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(evs, got) {
		t.Errorf("events round-trip mismatch:\nwant %+v\ngot  %+v", evs, got)
	}
}

func TestWriteEventsJSONNilIsEmptyArray(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEventsJSON(&buf, nil); err != nil {
		t.Fatalf("WriteEventsJSON(nil): %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Errorf("expected empty array, got %q", got)
	}
}

// TestWriteEventsJSONKeysStable pins the JSON event export's key set, the
// counterpart of the CSV header pin: consumers read these names, and the v3
// cost columns landed without a guard. It also fixes how "unpriced" looks in
// JSON — a null CostMicroUSD, the spelling of the CSV path's empty cell, never
// a 0 that would read as free.
func TestWriteEventsJSONKeysStable(t *testing.T) {
	wantKeys := []string{
		"CacheCreationTokens", "CacheReadTokens", "CostMicroUSD", "DedupKey",
		"EventTime", "InputTokens", "Kind", "MessageID", "Model", "ObservedTime",
		"OutputTokens", "PriceSource", "Project", "Provider", "ReasoningTokens",
		"RequestID", "ServiceTier", "SessionID", "SourcePath", "Tool",
		"TotalTokens",
	}

	evs := sampleEvents()
	evs[0].SetCost(1234, "embedded-2026-08-09")
	var buf bytes.Buffer
	if err := WriteEventsJSON(&buf, evs); err != nil {
		t.Fatalf("WriteEventsJSON: %v", err)
	}
	var got []map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, ev := range got {
		if keys := sortedKeys(ev); !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("event %d key set changed:\nwant %v\ngot  %v", i, wantKeys, keys)
		}
	}
	if string(got[0]["CostMicroUSD"]) != "1234" {
		t.Errorf("priced event CostMicroUSD = %s, want 1234", got[0]["CostMicroUSD"])
	}
	if string(got[1]["CostMicroUSD"]) != "null" {
		t.Errorf("unpriced event CostMicroUSD = %s, want null (never 0)", got[1]["CostMicroUSD"])
	}

	// --include-raw adds exactly one key to the same set.
	buf.Reset()
	if err := WriteEventsJSONWithRaw(&buf, evs); err != nil {
		t.Fatalf("WriteEventsJSONWithRaw: %v", err)
	}
	got = nil
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal with raw: %v", err)
	}
	wantWithRaw := append(append([]string{}, wantKeys...), "Raw")
	sort.Strings(wantWithRaw)
	if keys := sortedKeys(got[0]); !reflect.DeepEqual(keys, wantWithRaw) {
		t.Errorf("--include-raw key set = %v, want the base set plus Raw", keys)
	}
}

func TestWriteEventsCSVHeaderStable(t *testing.T) {
	wantHeader := []string{
		"tool", "model", "session", "project", "event_time", "observed_time",
		"input", "output", "cache_creation", "cache_read", "reasoning", "total",
		"request_id", "message_id", "source_path", "kind",
		"provider", "service_tier", "cost_micro_usd", "cost_usd", "price_source",
	}

	var buf bytes.Buffer
	if err := WriteEventsCSV(&buf, sampleEvents()); err != nil {
		t.Fatalf("WriteEventsCSV: %v", err)
	}
	r := csv.NewReader(&buf)
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(records) != 3 { // header + 2 events
		t.Fatalf("expected 3 records (header+2), got %d", len(records))
	}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Errorf("csv header mismatch:\nwant %v\ngot  %v", wantHeader, records[0])
	}
	// Spot-check a value row maps correctly.
	row := records[1]
	if row[0] != model.ToolClaudeCode {
		t.Errorf("row tool = %q, want %q", row[0], model.ToolClaudeCode)
	}
	if row[11] != "180" {
		t.Errorf("row total = %q, want 180", row[11])
	}
	if row[4] != "2026-05-29T12:00:00Z" {
		t.Errorf("row event_time = %q, want 2026-05-29T12:00:00Z", row[4])
	}
	if row[15] != string(model.KindUsage) {
		t.Errorf("row kind = %q, want %q", row[15], model.KindUsage)
	}
}

func TestWriteEventsCSVEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEventsCSV(&buf, nil); err != nil {
		t.Fatalf("WriteEventsCSV(nil): %v", err)
	}
	r := csv.NewReader(&buf)
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected header-only, got %d records", len(records))
	}
}

// TestWriteEventsJSONOmitsRaw guards the json:"-" privacy contract: the default
// event export must never carry the raw provider payload.
func TestWriteEventsJSONOmitsRaw(t *testing.T) {
	evs := sampleEvents()
	evs[0].Raw = `{"input_tokens":100}`
	var buf bytes.Buffer
	if err := WriteEventsJSON(&buf, evs); err != nil {
		t.Fatalf("WriteEventsJSON: %v", err)
	}
	if strings.Contains(buf.String(), "Raw") || strings.Contains(buf.String(), "input_tokens") {
		t.Errorf("default export leaked the raw payload:\n%s", buf.String())
	}
}

// TestWriteEventsJSONWithRawRestoresRaw checks the explicit opt-in path keeps
// the historical "Raw" key alongside the untouched event fields.
func TestWriteEventsJSONWithRawRestoresRaw(t *testing.T) {
	evs := sampleEvents()
	evs[0].Raw = `{"input_tokens":100}`
	var buf bytes.Buffer
	if err := WriteEventsJSONWithRaw(&buf, evs); err != nil {
		t.Fatalf("WriteEventsJSONWithRaw: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0]["Raw"] != evs[0].Raw {
		t.Errorf("Raw = %v, want %q", got[0]["Raw"], evs[0].Raw)
	}
	if got[0]["Tool"] != model.ToolClaudeCode {
		t.Errorf("Tool = %v, want %q", got[0]["Tool"], model.ToolClaudeCode)
	}
	if raw, ok := got[1]["Raw"]; !ok || raw != "" {
		t.Errorf("event without payload: Raw = %v (present=%v), want empty string", raw, ok)
	}
}

// TestWriteEventsCSVWithRawAppendsColumn checks --include-raw appends a "raw"
// column without disturbing the stable header order.
func TestWriteEventsCSVWithRawAppendsColumn(t *testing.T) {
	evs := sampleEvents()
	evs[0].Raw = `{"input_tokens":100}`
	var buf bytes.Buffer
	if err := WriteEventsCSVWithRaw(&buf, evs); err != nil {
		t.Fatalf("WriteEventsCSVWithRaw: %v", err)
	}
	r := csv.NewReader(&buf)
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	header := records[0]
	if !reflect.DeepEqual(header[:len(header)-1], csvHeader) {
		t.Errorf("existing columns changed:\nwant %v\ngot  %v", csvHeader, header[:len(header)-1])
	}
	if header[len(header)-1] != "raw" {
		t.Errorf("last column = %q, want raw", header[len(header)-1])
	}
	if got := records[1][len(header)-1]; got != evs[0].Raw {
		t.Errorf("raw cell = %q, want %q", got, evs[0].Raw)
	}
	if got := records[2][len(header)-1]; got != "" {
		t.Errorf("raw cell for payload-less event = %q, want empty", got)
	}
}
