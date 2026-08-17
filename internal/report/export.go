package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/model"
)

// timeLayout is the export timestamp format: RFC3339 in UTC, machine-stable and
// human-readable.
const timeLayout = "2006-01-02T15:04:05Z07:00"

// csvHeader is the stable column order for CSV event exports. Callers and
// downstream tooling depend on this order; do not reorder.
var csvHeader = []string{
	"tool",
	"model",
	"session",
	"project",
	"event_time",
	"observed_time",
	"input",
	"output",
	"cache_creation",
	"cache_read",
	"reasoning",
	"total",
	"request_id",
	"message_id",
	"source_path",
	"kind",
	// v3 columns, appended so existing consumers keep their positions.
	// provider is the raw stored value and stays empty when the source never
	// named one: only the rendered table labels that "unknown", and the JSON
	// summary carries the label in its own provider_label field. A machine
	// surface does not relabel the ledger.
	// cost_micro_usd is the exact stored integer (millionths of USD) and
	// cost_usd the same value as a decimal string; BOTH are empty for an
	// unpriced event, never "0" — export mirrors the ledger, so it does not
	// substitute a display-time price.
	"provider",
	"service_tier",
	"cost_micro_usd",
	"cost_usd",
	"price_source",
}

// summaryJSON is the --json payload. It carries the store summary's own fields
// unchanged (existing consumers keep every key they had) plus the resolved
// display cost, so --json and the rendered table answer the SAME question about
// the same query: the stamped-only figure is a floor whenever the range holds
// rows collected before pricing existed, and a surface that emits it alone with
// no marker reports a different, quieter number than the table beside it.
type summaryJSON struct {
	GroupBy []string
	Buckets []bucketJSON
	Totals  bucketJSON
}

// bucketJSON is one summary bucket plus its resolved cost. The embedded bucket
// keeps CostMicroUSD as the exact stamped sum and UnpricedEvents as the count
// behind it; the three added cost keys say what the table's Cost column says.
type bucketJSON struct {
	store.Bucket
	// ProviderLabel is the human string the table prints in the provider
	// column: the stored value, or "unknown" when the ledger holds the empty
	// string. Keys stays exactly what the ledger holds - a consumer must be
	// able to tell a provider literally named "unknown" from an absent one -
	// so the label lives here instead of overwriting the value. Omitted when
	// the summary does not group by provider: there is nothing to label.
	ProviderLabel string `json:"provider_label,omitempty"`
	// DisplayCostMicroUSD is CostMicroUSD plus a valuation, at the CURRENT
	// price table, of the rows that carry no stamped cost — the number the
	// table renders.
	DisplayCostMicroUSD int64
	// CostApproximate is true when DisplayCostMicroUSD contains any
	// display-priced row, or when rows no table could value are missing from
	// it. It is the tilde in the table; a consumer treating the figure as
	// billed must read it.
	CostApproximate bool
	// CostKnown is false when nothing in the bucket could be priced at all, in
	// which case DisplayCostMicroUSD is 0 because the cost is UNKNOWN, not
	// because the usage was free. The table renders this as "-".
	CostKnown bool
}

// WriteSummaryJSON writes a summary as indented JSON, including the resolved
// display costs. costs comes from ResolveCosts (the same value the table
// renders); nil, or one that does not line up with the summary, degrades to the
// stamped figures — a floor, correctly reported as such by CostApproximate.
func WriteSummaryJSON(w io.Writer, sum *store.Summary, costs *Costs) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(summaryPayload(sum, costs)); err != nil {
		return fmt.Errorf("encode summary json: %w", err)
	}
	return nil
}

// summaryPayload folds a summary and its resolved costs into the JSON shape. A
// nil summary stays a JSON null, as it was before costs existed.
func summaryPayload(sum *store.Summary, costs *Costs) *summaryJSON {
	if sum == nil {
		return nil
	}
	out := &summaryJSON{
		GroupBy: sum.GroupBy,
		Buckets: make([]bucketJSON, len(sum.Buckets)),
	}
	for i, b := range sum.Buckets {
		out.Buckets[i] = bucketPayload(b, bucketCost(costs, i, b))
	}
	totals := stampedCost(sum.Totals)
	if costs != nil {
		totals = costs.Totals
	}
	out.Totals = bucketPayload(sum.Totals, totals)
	return out
}

// bucketCost picks the resolved cost for bucket i, falling back to the stamped
// figure when the caller supplied none (or a slice that does not line up).
func bucketCost(costs *Costs, i int, b store.Bucket) Cost {
	if costs == nil || i < 0 || i >= len(costs.Buckets) {
		return stampedCost(b)
	}
	return costs.Buckets[i]
}

func bucketPayload(b store.Bucket, c Cost) bucketJSON {
	// Keys is emitted untouched: JSON is a machine surface and must report what
	// the ledger holds. The human wording travels beside it in ProviderLabel.
	return bucketJSON{
		Bucket:              b,
		ProviderLabel:       providerLabel(b.Keys),
		DisplayCostMicroUSD: c.MicroUSD,
		CostApproximate:     c.Approximate,
		CostKnown:           c.Known,
	}
}

// providerLabel returns the human string for a bucket's provider dimension, or
// "" when the summary does not group by provider (the TOTAL bucket included).
func providerLabel(keys map[string]string) string {
	val, ok := keys["provider"]
	if !ok {
		return ""
	}
	return displayKey("provider", val)
}

// WriteEventsJSON writes a slice of usage events as indented JSON. Raw is
// excluded (json:"-"): it can carry transcript content. WriteEventsJSONWithRaw
// is the explicit opt-in.
//
// An unpriced event emits "CostMicroUSD": null — the JSON spelling of the empty
// cost_micro_usd/cost_usd cells the CSV path writes. Both formats say the same
// thing in their own vocabulary, and neither substitutes 0, which would claim
// the request was free. The key set is pinned by a test, like the CSV header.
func WriteEventsJSON(w io.Writer, evs []model.UsageEvent) error {
	if evs == nil {
		evs = []model.UsageEvent{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(evs); err != nil {
		return fmt.Errorf("encode events json: %w", err)
	}
	return nil
}

// eventWithRaw restores the Raw payload that UsageEvent's json:"-" tag strips.
// The outer field marshals under the pre-tag key "Raw", so --include-raw
// output keeps the historical shape.
type eventWithRaw struct {
	model.UsageEvent
	Raw string
}

// WriteEventsJSONWithRaw is WriteEventsJSON plus the Raw provider payload.
// Only the export --include-raw path may call it: Raw can carry full
// transcript content.
func WriteEventsJSONWithRaw(w io.Writer, evs []model.UsageEvent) error {
	wrapped := make([]eventWithRaw, len(evs))
	for i, e := range evs {
		wrapped[i] = eventWithRaw{UsageEvent: e, Raw: e.Raw}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(wrapped); err != nil {
		return fmt.Errorf("encode events json: %w", err)
	}
	return nil
}

// WriteEventsCSV writes a slice of usage events as CSV with a stable header.
func WriteEventsCSV(w io.Writer, evs []model.UsageEvent) error {
	return writeEventsCSV(w, evs, false)
}

// WriteEventsCSVWithRaw is WriteEventsCSV plus a trailing "raw" column.
// Existing columns keep their position; only the export --include-raw path
// may call it (Raw can carry full transcript content).
func WriteEventsCSVWithRaw(w io.Writer, evs []model.UsageEvent) error {
	return writeEventsCSV(w, evs, true)
}

func writeEventsCSV(w io.Writer, evs []model.UsageEvent, includeRaw bool) error {
	header := csvHeader
	if includeRaw {
		header = append(append([]string{}, csvHeader...), "raw")
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, e := range evs {
		rec := eventRecord(e)
		if includeRaw {
			rec = append(rec, e.Raw)
		}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}

// eventRecord serialises one event into CSV fields matching csvHeader order.
func eventRecord(e model.UsageEvent) []string {
	costMicro, costUSD := "", ""
	if micro, ok := e.Cost(); ok {
		costMicro = itoa(micro)
		costUSD = strconv.FormatFloat(float64(micro)/1e6, 'f', 6, 64)
	}
	return []string{
		e.Tool,
		e.Model,
		e.SessionID,
		e.Project,
		formatTime(e.EventTime),
		formatTime(e.ObservedTime),
		itoa(e.InputTokens),
		itoa(e.OutputTokens),
		itoa(e.CacheCreationTokens),
		itoa(e.CacheReadTokens),
		itoa(e.ReasoningTokens),
		itoa(e.TotalTokens),
		e.RequestID,
		e.MessageID,
		e.SourcePath,
		string(e.Kind),
		e.Provider,
		e.ServiceTier,
		costMicro,
		costUSD,
		e.PriceSource,
	}
}

// formatTime renders a timestamp as UTC RFC3339, or empty for the zero time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
