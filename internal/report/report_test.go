package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"aiusage/internal/model"
	"aiusage/internal/store"
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
	if err := WriteSummaryJSON(&buf, sum); err != nil {
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

func TestWriteEventsCSVHeaderStable(t *testing.T) {
	wantHeader := []string{
		"tool", "model", "session", "project", "event_time", "observed_time",
		"input", "output", "cache_creation", "cache_read", "reasoning", "total",
		"request_id", "message_id", "source_path", "kind",
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
