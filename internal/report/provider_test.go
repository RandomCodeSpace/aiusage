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

// TestSummaryJSONSplitsRawProviderFromItsLabel is the issue #50 contract: the
// machine surface emits what the LEDGER holds (an unnamed provider stays the
// empty string, exactly as the CSV and the JSON event export write it) and
// carries the human wording in a separate provider_label. Folding the label
// into the value would make a provider literally named "unknown" and an absent
// one indistinguishable, and would put the two JSON surfaces at odds about the
// same fact. It also pins that building the payload does not reach back into
// the summary the cost fold-back matches on.
func TestSummaryJSONSplitsRawProviderFromItsLabel(t *testing.T) {
	sum := providerSummary()
	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sum, nil); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}

	var got struct {
		Buckets []struct {
			Keys          map[string]string
			ProviderLabel string `json:"provider_label"`
		}
		Totals struct {
			ProviderLabel string `json:"provider_label"`
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal summary json: %v\n%s", err, buf.String())
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2\n%s", len(got.Buckets), buf.String())
	}

	// Machine half: the raw stored values, empty stays empty.
	if v, ok := got.Buckets[0].Keys["provider"]; !ok || v != "" {
		t.Errorf("json provider = %q (present=%v), want the raw empty value the ledger holds", v, ok)
	}
	if got.Buckets[1].Keys["provider"] != model.ProviderAnthropic {
		t.Errorf("json named provider = %q, want %q", got.Buckets[1].Keys["provider"], model.ProviderAnthropic)
	}

	// Human half: the label the table prints, in its own field.
	if got.Buckets[0].ProviderLabel != unknownLabel {
		t.Errorf("provider_label = %q, want %q", got.Buckets[0].ProviderLabel, unknownLabel)
	}
	if got.Buckets[1].ProviderLabel != model.ProviderAnthropic {
		t.Errorf("provider_label = %q, want %q", got.Buckets[1].ProviderLabel, model.ProviderAnthropic)
	}
	// The TOTAL bucket spans every provider, so it has nothing to label.
	if got.Totals.ProviderLabel != "" {
		t.Errorf("totals provider_label = %q, want it absent", got.Totals.ProviderLabel)
	}

	if sum.Buckets[0].Keys["provider"] != "" {
		t.Errorf("rendering rewrote the stored key to %q; the cost fold-back matches on the stored value",
			sum.Buckets[0].Keys["provider"])
	}

	// The human table is unchanged by the split: it still says "unknown".
	if out := RenderTable(sum, Opt{}); !strings.Contains(out, unknownLabel) {
		t.Errorf("table stopped labelling the empty provider %q:\n%s", unknownLabel, out)
	}
}

// TestSummaryJSONOmitsProviderLabelWithoutProviderGrouping: the label answers a
// question only a provider-grouped summary asks. A summary grouped by anything
// else must not sprout an empty field that a consumer could read as "the
// provider is blank".
func TestSummaryJSONOmitsProviderLabelWithoutProviderGrouping(t *testing.T) {
	sum := &store.Summary{
		GroupBy: []string{"tool"},
		Buckets: []store.Bucket{{
			Keys:        map[string]string{"tool": model.ToolOpenCode},
			OrderedKeys: []string{"tool"},
			Events:      1,
			Total:       10,
		}},
		Totals: store.Bucket{Events: 1, Total: 10},
	}
	var buf bytes.Buffer
	if err := WriteSummaryJSON(&buf, sum, nil); err != nil {
		t.Fatalf("WriteSummaryJSON: %v", err)
	}
	if strings.Contains(buf.String(), "provider_label") {
		t.Errorf("tool-grouped summary carries provider_label:\n%s", buf.String())
	}
}

// TestEventsCSVKeepsEmptyProviderRaw: the CSV export mirrors the ledger, so an
// unknown provider stays an empty field - the same value the JSON summary now
// emits, and the same rule every other export column already follows (an
// unpriced cost is empty, not "0"). Only the rendered table says "unknown".
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
