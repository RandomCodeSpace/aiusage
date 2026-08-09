package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
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

// WriteSummaryJSON writes a summary as indented JSON.
func WriteSummaryJSON(w io.Writer, sum *store.Summary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sum); err != nil {
		return fmt.Errorf("encode summary json: %w", err)
	}
	return nil
}

// WriteEventsJSON writes a slice of usage events as indented JSON. Raw is
// excluded (json:"-"): it can carry transcript content. WriteEventsJSONWithRaw
// is the explicit opt-in.
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
