package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// The wire shapes. They mirror webui/src/api/contract.ts field for field - that
// file is the settled contract (issue #56) and this is the server half of it.
// Names are snake_case because the page reads them; the Go structs are
// unexported because nothing outside this package should be building one.
//
// The one thing to look for here is what is ABSENT: there is no raw field on any
// shape, and adding one would publish transcript content on an unauthenticated
// port. See TestPackageNeverProjectsRaw.

// metaResponse is GET /api/meta.
type metaResponse struct {
	ContractVersion int            `json:"contract_version"`
	ServerVersion   string         `json:"server_version"`
	NowUnix         int64          `json:"now_unix"`
	Watermark       int64          `json:"watermark"`
	Daemon          daemonWire     `json:"daemon"`
	Database        databaseWire   `json:"database"`
	Resources       resourcesWire  `json:"resources"`
	Tools           []string       `json:"tools"`
	Capabilities    capabilityWire `json:"capabilities"`
}

// daemonWire is the collection daemon as the serving process can observe it.
type daemonWire struct {
	// Running is the advisory lock's answer, and it is not the same question as
	// "is pid non-zero": a daemon whose pidfile cannot be read still holds the
	// lock, and reporting pid 0 alone would let the page call that stopped.
	Running       bool  `json:"running"`
	PID           int   `json:"pid"`
	UptimeSeconds int64 `json:"uptime_seconds"`
	// LastCycleUnix is the newest write time of the database file or its WAL -
	// the last cycle that CHANGED anything. A cycle that found nothing new
	// leaves no trace a read-only process could see, so this is the honest
	// answer to "when did data last land", not to "when did the daemon last
	// wake up".
	LastCycleUnix   int64 `json:"last_cycle_unix"`
	IntervalSeconds int64 `json:"interval_seconds"`
}

type databaseWire struct {
	SizeBytes         int64 `json:"size_bytes"`
	Events            int64 `json:"events"`
	SchemaVersion     int   `json:"schema_version"`
	EarliestEventUnix int64 `json:"earliest_event_unix"`
	LatestEventUnix   int64 `json:"latest_event_unix"`
}

type resourcesWire struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
	Disk   float64 `json:"disk"`
}

// capabilityWire reports what this build can do, so the page can say why a
// feature is missing instead of appearing broken (issue #61).
type capabilityWire struct {
	EmbeddedUI bool `json:"embedded_ui"`
}

// bucketWire is store.Bucket on the wire.
type bucketWire struct {
	Keys        map[string]string `json:"keys"`
	OrderedKeys []string          `json:"ordered_keys"`
	Events      int64             `json:"events"`
	// Sessions is the distinct session count, and it is 0 for every bucket the
	// derived rollup answered: the rollup keeps no session dimension, so the
	// count is not merely absent but underivable there. Group by "session" (or
	// filter by one) to force the question onto the ledger, which counts them.
	Sessions      int64 `json:"sessions"`
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
	Reasoning     int64 `json:"reasoning"`
	Total         int64 `json:"total"`
	// CostMicroUSD sums only the costs stamped at collect time. A bucket with
	// UnpricedEvents > 0 is an UNDERSTATEMENT, which is why the count travels
	// with it: cost carries its own precision instead of pretending to exact.
	CostMicroUSD   int64 `json:"cost_micro_usd"`
	UnpricedEvents int64 `json:"unpriced_events"`
}

// summaryResponse is GET /api/summary.
type summaryResponse struct {
	GroupBy []string     `json:"group_by"`
	Buckets []bucketWire `json:"buckets"`
	Totals  bucketWire   `json:"totals"`
	// Since and Until are the range these buckets actually cover. They are the
	// requested bounds snapped OUTWARD to whole UTC 15-minute buckets when the
	// rollup answered, because a bucket is the finest thing it knows; the caller
	// labels what it got rather than what it asked for.
	Since int64 `json:"since"`
	Until int64 `json:"until"`
	// Source is "rollup" or "ledger": which table produced these numbers. Both
	// are correct - the ledger path is the fallback for a question the rollup
	// cannot serve OR for a rollup that has not been filled yet - but they
	// differ in cost and in bound snapping, and a client that cannot tell them
	// apart cannot explain why a page went slow.
	Source string `json:"source"`
}

// facetWire is one distinct value of a dimension within the range.
type facetWire struct {
	Value  string `json:"value"`
	Events int64  `json:"events"`
	Total  int64  `json:"total"`
}

// facetsResponse is GET /api/facets. Each list is ordered heaviest first, which
// is the order the scene lays its lanes out in.
type facetsResponse struct {
	Tools     []facetWire `json:"tools"`
	Models    []facetWire `json:"models"`
	Providers []facetWire `json:"providers"`
	Projects  []facetWire `json:"projects"`
	Since     int64       `json:"since"`
	Until     int64       `json:"until"`
	// Source is "rollup" or "ledger", naming where the lists the rollup CAN
	// serve came from. Providers is always the ledger's - the rollup keeps no
	// provider dimension - so "rollup" here means "the other three were cheap",
	// not "nothing scanned the ledger".
	Source string `json:"source"`
}

// eventWire is one ledger row, explicitly projected. It is model.UsageEvent
// minus Raw, minus the transient CacheTTL, minus DedupKey/RequestID/MessageID/
// SourcePath: an unauthenticated surface publishes what the dashboard draws and
// nothing else, and source_path alone would leak the user's directory layout.
type eventWire struct {
	// Seq is the ledger row id, and the keyset cursor field: ids are
	// AUTOINCREMENT, so they never repeat and their order is insertion order.
	Seq              int64  `json:"seq"`
	Tool             string `json:"tool"`
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	ServiceTier      string `json:"service_tier"`
	SessionID        string `json:"session_id"`
	Project          string `json:"project"`
	EventTimeUnix    int64  `json:"event_time_unix"`
	ObservedTimeUnix int64  `json:"observed_time_unix"`
	Input            int64  `json:"input"`
	Output           int64  `json:"output"`
	CacheCreation    int64  `json:"cache_creation"`
	CacheRead        int64  `json:"cache_read"`
	Reasoning        int64  `json:"reasoning"`
	Total            int64  `json:"total"`
	// CostMicroUSD is null when the row is UNPRICED. A stamped 0 would claim the
	// request was free, so the pointer is the precision.
	CostMicroUSD *int64 `json:"cost_micro_usd"`
	PriceSource  string `json:"price_source"`
	Kind         string `json:"kind"`
}

// eventsResponse is GET /api/events.
type eventsResponse struct {
	Rows []eventWire `json:"rows"`
	// NextCursor is null when this page is the last one. It is the last row id
	// of this page; the client passes it back verbatim.
	NextCursor *string `json:"next_cursor"`
	// Truncated is true when the range holds more rows than this page returned.
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit"`
	// Total is the true number of rows the range holds. It is what makes the cap
	// honest: a capped page says how much it is not showing instead of being a
	// silent slice.
	Total int64 `json:"total"`
}

// liveFrame is one WS /api/ws message. It carries a NOTIFICATION, never data:
// the server cannot answer "what changed since X" cheaply, so a frame is a
// query invalidation and the client refetches only the view it is on.
type liveFrame struct {
	Watermark int64 `json:"watermark"`
	CycleAt   int64 `json:"cycle_at"`
}

// errorResponse is the body of every non-2xx JSON reply.
type errorResponse struct {
	Error string `json:"error"`
}

// encodeFrame renders one live frame as the bytes of a WebSocket text message.
func encodeFrame(f liveFrame) ([]byte, error) {
	return json.Marshal(f)
}

// unixOrZero renders a time as unix seconds, mapping the zero time to 0 - the
// wire's "no such instant". An open range bound and an empty ledger both land
// here, and both are ordinary answers.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// toBucketWire converts one store bucket. Keys is normalised to a non-nil map so
// the JSON carries {} instead of null for an ungrouped bucket.
func toBucketWire(b store.Bucket) bucketWire {
	keys := b.Keys
	if keys == nil {
		keys = map[string]string{}
	}
	ordered := b.OrderedKeys
	if ordered == nil {
		ordered = []string{}
	}
	return bucketWire{
		Keys:           keys,
		OrderedKeys:    ordered,
		Events:         b.Events,
		Sessions:       b.Sessions,
		Input:          b.Input,
		Output:         b.Output,
		CacheCreation:  b.CacheCreation,
		CacheRead:      b.CacheRead,
		Reasoning:      b.Reasoning,
		Total:          b.Total,
		CostMicroUSD:   b.CostMicroUSD,
		UnpricedEvents: b.UnpricedEvents,
	}
}

// toBucketWires converts a bucket slice, always returning a non-nil slice: an
// empty range is an ordinary 200 with [], not null and not an error.
func toBucketWires(bs []store.Bucket) []bucketWire {
	out := make([]bucketWire, 0, len(bs))
	for _, b := range bs {
		out = append(out, toBucketWire(b))
	}
	return out
}

// toEventWire projects one ledger row onto the wire. Every field is named; there
// is no struct embedding and no marshalling of model.UsageEvent itself, so a
// field added to the model cannot appear here by accident.
func toEventWire(e model.UsageEvent) eventWire {
	var cost *int64
	if c, ok := e.Cost(); ok {
		v := c
		cost = &v
	}
	return eventWire{
		Seq:              e.ID,
		Tool:             e.Tool,
		Model:            e.Model,
		Provider:         e.Provider,
		ServiceTier:      e.ServiceTier,
		SessionID:        e.SessionID,
		Project:          e.Project,
		EventTimeUnix:    unixOrZero(e.EventTime),
		ObservedTimeUnix: unixOrZero(e.ObservedTime),
		Input:            e.InputTokens,
		Output:           e.OutputTokens,
		CacheCreation:    e.CacheCreationTokens,
		CacheRead:        e.CacheReadTokens,
		Reasoning:        e.ReasoningTokens,
		Total:            e.TotalTokens,
		CostMicroUSD:     cost,
		PriceSource:      e.PriceSource,
		Kind:             string(e.Kind),
	}
}

// writeJSON sends v as the body of a 200-class response. Every API reply is
// no-store: the numbers change on every collection cycle and a cached page
// showing yesterday's total is worse than a slow one.
func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; all that is left is to say so.
		s.log.Printf("web: encode response: %v", err)
	}
}

// writeError sends a JSON error body. Messages are short, lowercase and free of
// filesystem paths: this port is unauthenticated, so an error is not a place to
// describe the machine.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
