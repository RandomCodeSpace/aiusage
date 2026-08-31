package report

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
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
	stream, err := NewEventsJSONWriter(w, false)
	if err != nil {
		return err
	}
	if err := stream.WritePage(evs); err != nil {
		return err
	}
	return stream.Close()
}

// WriteEventsJSONWithRaw is WriteEventsJSON plus the Raw provider payload.
// Only the export --include-raw path may call it: Raw can carry full
// transcript content.
func WriteEventsJSONWithRaw(w io.Writer, evs []model.UsageEvent) error {
	stream, err := NewEventsJSONWriter(w, true)
	if err != nil {
		return err
	}
	if err := stream.WritePage(evs); err != nil {
		return err
	}
	return stream.Close()
}

// EventsJSONWriter emits one indented JSON array across any number of pages.
// It writes the opening bracket immediately, retains no prior event, and closes
// the array exactly once. The byte shape matches WriteEventsJSON and its raw
// variant, including PascalCase field names and the empty [] result.
type EventsJSONWriter struct {
	w          *bufio.Writer
	includeRaw bool
	wroteEvent bool
	closed     bool
	row        []byte
}

func NewEventsJSONWriter(w io.Writer, includeRaw bool) (*EventsJSONWriter, error) {
	buffered := bufio.NewWriterSize(w, 256<<10)
	stream := &EventsJSONWriter{w: buffered, includeRaw: includeRaw}
	if err := writeExportString(buffered, "["); err != nil {
		return nil, fmt.Errorf("open events json array: %w", err)
	}
	// The performance contract measures first byte separately from completion.
	// Flush the opening bracket immediately, then retain only this bounded buffer
	// while the million-row body streams.
	if err := buffered.Flush(); err != nil {
		return nil, fmt.Errorf("flush events json opening: %w", err)
	}
	return stream, nil
}

func (w *EventsJSONWriter) WritePage(evs []model.UsageEvent) error {
	if w.closed {
		return fmt.Errorf("write events json page: writer is closed")
	}
	for _, e := range evs {
		encoded, err := appendEventJSON(w.row[:0], e, w.includeRaw)
		if err != nil {
			return fmt.Errorf("encode events json row: %w", err)
		}
		w.row = encoded
		separator := "\n  "
		if w.wroteEvent {
			separator = ",\n  "
		}
		if err := writeExportString(w.w, separator); err != nil {
			return fmt.Errorf("write events json separator: %w", err)
		}
		n, err := w.w.Write(encoded)
		if err == nil && n != len(encoded) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("write events json row: %w", err)
		}
		w.wroteEvent = true
	}
	return nil
}

// appendEventJSON is the fixed v1 event encoder. encoding/json reflection plus
// indentation consumed most of the one-million-row export budget; this direct
// flat-struct encoding preserves the exact pinned bytes while reusing one row
// buffer. String escaping intentionally matches encoding/json's HTML-safe
// behavior because paths and opaque provider fields are part of the machine
// contract.
func appendEventJSON(dst []byte, e model.UsageEvent, includeRaw bool) ([]byte, error) {
	dst = append(dst, "{\n    \"Tool\": "...)
	dst = appendJSONString(dst, e.Tool)
	dst = appendJSONFieldString(dst, "Model", e.Model)
	dst = appendJSONFieldString(dst, "Provider", e.Provider)
	dst = appendJSONFieldString(dst, "ServiceTier", e.ServiceTier)
	dst = appendJSONFieldString(dst, "SessionID", e.SessionID)
	dst = appendJSONFieldString(dst, "Project", e.Project)
	var err error
	dst, err = appendJSONFieldTime(dst, "EventTime", e.EventTime)
	if err != nil {
		return nil, err
	}
	dst, err = appendJSONFieldTime(dst, "ObservedTime", e.ObservedTime)
	if err != nil {
		return nil, err
	}
	dst = appendJSONFieldInt(dst, "InputTokens", e.InputTokens)
	dst = appendJSONFieldInt(dst, "OutputTokens", e.OutputTokens)
	dst = appendJSONFieldInt(dst, "CacheCreationTokens", e.CacheCreationTokens)
	dst = appendJSONFieldInt(dst, "CacheReadTokens", e.CacheReadTokens)
	dst = appendJSONFieldInt(dst, "ReasoningTokens", e.ReasoningTokens)
	dst = appendJSONFieldInt(dst, "TotalTokens", e.TotalTokens)
	dst = append(dst, ",\n    \"CostMicroUSD\": "...)
	if e.CostMicroUSD == nil {
		dst = append(dst, "null"...)
	} else {
		dst = strconv.AppendInt(dst, *e.CostMicroUSD, 10)
	}
	dst = appendJSONFieldString(dst, "PriceSource", e.PriceSource)
	dst = appendJSONFieldString(dst, "RequestID", e.RequestID)
	dst = appendJSONFieldString(dst, "MessageID", e.MessageID)
	dst = appendJSONFieldString(dst, "SourcePath", e.SourcePath)
	dst = appendJSONFieldString(dst, "DedupKey", e.DedupKey)
	dst = appendJSONFieldString(dst, "Kind", string(e.Kind))
	if includeRaw {
		dst = appendJSONFieldString(dst, "Raw", e.Raw)
	}
	dst = append(dst, "\n  }"...)
	return dst, nil
}

func appendJSONFieldString(dst []byte, name, value string) []byte {
	dst = append(dst, ",\n    \""...)
	dst = append(dst, name...)
	dst = append(dst, "\": "...)
	return appendJSONString(dst, value)
}

func appendJSONFieldInt(dst []byte, name string, value int64) []byte {
	dst = append(dst, ",\n    \""...)
	dst = append(dst, name...)
	dst = append(dst, "\": "...)
	return strconv.AppendInt(dst, value, 10)
}

func appendJSONFieldTime(dst []byte, name string, value time.Time) ([]byte, error) {
	year := value.Year()
	_, offset := value.Zone()
	if year < 0 || year >= 10000 {
		return nil, fmt.Errorf("Time.MarshalJSON: year outside of range [0,9999]")
	}
	if offset <= -24*60*60 || offset >= 24*60*60 {
		return nil, fmt.Errorf("Time.MarshalJSON: timezone hour outside of range [0,23]")
	}
	dst = append(dst, ",\n    \""...)
	dst = append(dst, name...)
	dst = append(dst, "\": \""...)
	dst = value.AppendFormat(dst, time.RFC3339Nano)
	dst = append(dst, '"')
	return dst, nil
}

func appendJSONString(dst []byte, value string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(value); {
		if b := value[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '\\' && b != '"' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			dst = append(dst, value[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[b>>4], hex[b&0x0f])
			}
			i++
			start = i
			continue
		}

		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, value[start:i]...)
			dst = append(dst, `\ufffd`...)
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, value[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0x0f])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, value[start:]...)
	dst = append(dst, '"')
	return dst
}

func (w *EventsJSONWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	ending := "]\n"
	if w.wroteEvent {
		ending = "\n]\n"
	}
	if err := writeExportString(w.w, ending); err != nil {
		return fmt.Errorf("close events json array: %w", err)
	}
	if err := w.w.Flush(); err != nil {
		return fmt.Errorf("flush events json array: %w", err)
	}
	return nil
}

func writeExportString(w io.Writer, value string) error {
	n, err := io.WriteString(w, value)
	if err == nil && n != len(value) {
		return io.ErrShortWrite
	}
	return err
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
	stream, err := NewEventsCSVWriter(w, includeRaw)
	if err != nil {
		return err
	}
	if err := stream.WritePage(evs); err != nil {
		return err
	}
	return stream.Close()
}

// EventsCSVWriter emits the canonical header once and flushes every page so
// callers never retain the whole export before the first byte is visible.
type EventsCSVWriter struct {
	cw         *csv.Writer
	includeRaw bool
	closed     bool
}

func NewEventsCSVWriter(w io.Writer, includeRaw bool) (*EventsCSVWriter, error) {
	header := csvHeader
	if includeRaw {
		header = append(append([]string{}, csvHeader...), "raw")
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return nil, fmt.Errorf("write csv header: %w", err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, fmt.Errorf("flush csv header: %w", err)
	}
	return &EventsCSVWriter{cw: cw, includeRaw: includeRaw}, nil
}

func (w *EventsCSVWriter) WritePage(evs []model.UsageEvent) error {
	if w.closed {
		return fmt.Errorf("write csv page: writer is closed")
	}
	for _, e := range evs {
		rec := eventRecord(e)
		if w.includeRaw {
			rec = append(rec, e.Raw)
		}
		if err := w.cw.Write(rec); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	w.cw.Flush()
	if err := w.cw.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}

func (w *EventsCSVWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.cw.Flush()
	if err := w.cw.Error(); err != nil {
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
