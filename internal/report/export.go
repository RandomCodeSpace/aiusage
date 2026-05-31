package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"aiusage/internal/model"
	"aiusage/internal/store"
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

// WriteEventsJSON writes a slice of usage events as indented JSON.
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

// WriteEventsCSV writes a slice of usage events as CSV with a stable header.
func WriteEventsCSV(w io.Writer, evs []model.UsageEvent) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, e := range evs {
		if err := cw.Write(eventRecord(e)); err != nil {
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
