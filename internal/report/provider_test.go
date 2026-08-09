package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// providerSummary is a provider-grouped summary holding both a named provider
// and the bucket of rows whose source never named one (stored as the empty
// string).
func providerSummary() *store.Summary {
	return &store.Summary{
		GroupBy: []string{"provider"},
		Buckets: []store.Bucket{
			{
				Keys:        map[string]string{"provider": ""},
				OrderedKeys: []string{"provider"},
				Events:      2,
				Input:       10,
				Output:      5,
				Total:       15,
			},
			{
				Keys:        map[string]string{"provider": model.ProviderAnthropic},
				OrderedKeys: []string{"provider"},
				Events:      3,
				Input:       100,
				Output:      50,
				Total:       150,
			},
		},
		Totals: store.Bucket{Events: 5, Input: 110, Output: 55, Total: 165},
	}
}

// TestRenderTableLabelsEmptyProviderUnknown is the issue #38 display rule: a
// provider the ledger stores as the empty string renders as "unknown". A
// blank cell in the middle of the grouping column reads as a broken renderer,
// not as an honest gap in what the sources reported.
func TestRenderTableLabelsEmptyProviderUnknown(t *testing.T) {
	out := RenderTable(providerSummary(), Opt{})

	if !strings.Contains(out, unknownLabel) {
		t.Errorf("table does not label the empty provider %q:\n%s", unknownLabel, out)
	}
	if !strings.Contains(out, model.ProviderAnthropic) {
		t.Errorf("table lost the named provider:\n%s", out)
	}
	// The unknown row must still be a row, not a blank-keyed line: its own
	// metrics have to sit beside the label.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, unknownLabel) {
			if !strings.Contains(line, "15") {
				t.Errorf("unknown provider row lost its metrics: %q", line)
			}
			return
		}
	}
	t.Errorf("no row starts with %q:\n%s", unknownLabel, out)
}

// TestSummaryJSONLabelsEmptyProviderUnknown holds --json to the same rule as
// the table beside it, and pins that labelling the copy does not reach back
// into the summary the cost fold-back matches on.
func TestSummaryJSONLabelsEmptyProviderUnknown(t *testing.T) {
	sum := providerSummary()
	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sum, nil); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}

	var got struct {
		Buckets []struct {
			Keys map[string]string
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal summary json: %v\n%s", err, buf.String())
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2\n%s", len(got.Buckets), buf.String())
	}
	if got.Buckets[0].Keys["provider"] != unknownLabel {
		t.Errorf("json empty provider = %q, want %q", got.Buckets[0].Keys["provider"], unknownLabel)
	}
	if got.Buckets[1].Keys["provider"] != model.ProviderAnthropic {
		t.Errorf("json named provider = %q, want %q", got.Buckets[1].Keys["provider"], model.ProviderAnthropic)
	}
	if sum.Buckets[0].Keys["provider"] != "" {
		t.Errorf("rendering rewrote the stored key to %q; the cost fold-back matches on the stored value",
			sum.Buckets[0].Keys["provider"])
	}
}

// TestEventsCSVKeepsEmptyProviderRaw records the deliberate asymmetry: the CSV
// export mirrors the ledger, so an unknown provider stays an empty field there
// while the table and JSON label it. Every other export column already follows
// that rule (an unpriced cost is empty, not "0").
func TestEventsCSVKeepsEmptyProviderRaw(t *testing.T) {
	evs := []model.UsageEvent{{
		Tool:      model.ToolOpenCode,
		Model:     "some-model",
		EventTime: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Kind:      model.KindUsage,
	}}
	var buf bytes.Buffer
	if err := WriteEventsCSV(&buf, evs); err != nil {
		t.Fatalf("WriteEventsCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want header + 1 row", len(records))
	}
	col := -1
	for i, h := range records[0] {
		if h == "provider" {
			col = i
			break
		}
	}
	if col < 0 {
		t.Fatalf("csv header has no provider column: %v", records[0])
	}
	if records[1][col] != "" {
		t.Errorf("csv provider = %q, want the raw empty value (an export mirrors the ledger)", records[1][col])
	}
}
