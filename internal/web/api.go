package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// handleMeta serves GET /api/meta: what this server is, what it is serving from,
// and how far collection has got. The page reads it at boot and every minute
// after, so nothing here may scan the ledger row by row.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	if !allowGET(w, r) {
		return
	}
	ctx := r.Context()

	stats, err := s.reader.Stats(ctx)
	if err != nil {
		s.fail(w, "read database stats", err)
		return
	}
	watermark, err := s.reader.IngestWatermark(ctx)
	if err != nil {
		s.fail(w, "read ingest watermark", err)
		return
	}
	tools, err := s.toolIDs(ctx)
	if err != nil {
		s.fail(w, "list tools", err)
		return
	}

	var daemon DaemonInfo
	if s.opt.Daemon != nil {
		daemon = s.opt.Daemon()
	}
	var res Resources
	if s.opt.Resources != nil {
		res = s.opt.Resources()
	}

	s.writeJSON(w, metaResponse{
		ContractVersion: ContractVersion,
		ServerVersion:   s.opt.ServerVersion,
		NowUnix:         s.now().Unix(),
		Watermark:       unixOrZero(watermark),
		Daemon: daemonWire{
			Running:         daemon.Running,
			PID:             daemon.PID,
			UptimeSeconds:   int64(daemon.Uptime.Seconds()),
			LastCycleUnix:   unixOrZero(newestMTime(s.opt.DBPath)),
			IntervalSeconds: int64(daemon.Interval.Seconds()),
		},
		Database: databaseWire{
			SizeBytes:         stats.SizeBytes,
			Events:            stats.Events,
			SchemaVersion:     stats.SchemaVersion,
			EarliestEventUnix: unixOrZero(stats.EarliestEvent),
			LatestEventUnix:   unixOrZero(stats.LatestEvent),
		},
		// A conversion, not a copy: it compiles only while the wire shape and the
		// injected reading have the same fields, so adding a gauge to one without
		// the other is a build error rather than a silently dropped number.
		Resources:    resourcesWire(res),
		Tools:        tools,
		Capabilities: capabilityWire{EmbeddedUI: hasEmbeddedUI},
	})
}

// toolIDs lists the tool ids present in the ledger, heaviest first. It prefers
// the rollup: one row per bucket/tool/model/project instead of one per event,
// so this stays milliseconds on a ledger that takes half a second to group.
func (s *Server) toolIDs(ctx context.Context) ([]string, error) {
	f := store.Filter{GroupBy: []string{"tool"}}
	facets, err := s.facetValues(ctx, store.Filter{}, "tool", s.rollupUsable(ctx, f))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(facets))
	for _, f := range facets {
		out = append(out, f.Value)
	}
	return out, nil
}

// handleSummary serves GET /api/summary: grouped buckets plus a grand total,
// aggregated in SQL. The wire carries buckets, never events.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if !allowGET(w, r) {
		return
	}
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Route by what the question needs and by what the rollup actually covers,
	// not by what is fastest: the rollup answers the dimensions it keeps while
	// it is in step with the ledger, and everything else goes to the ledger and
	// pays for it. Both paths return the same numbers over the same range.
	if s.rollupUsable(r.Context(), f) {
		rs, err := s.reader.SummarizeRollup(r.Context(), f)
		if err != nil {
			s.fail(w, "summarize", err)
			return
		}
		s.writeJSON(w, summaryResponse{
			GroupBy: orEmpty(rs.GroupBy),
			Buckets: toBucketWires(rs.Buckets),
			Totals:  toBucketWire(rs.Totals),
			Since:   unixOrZero(rs.Since),
			Until:   unixOrZero(rs.Until),
			Source:  sourceRollup,
		})
		return
	}

	sum, err := s.reader.Summarize(r.Context(), f)
	if err != nil {
		s.fail(w, "summarize", err)
		return
	}
	s.writeJSON(w, summaryResponse{
		GroupBy: orEmpty(sum.GroupBy),
		Buckets: toBucketWires(sum.Buckets),
		Totals:  toBucketWire(sum.Totals),
		Since:   unixOrZero(f.Since),
		Until:   unixOrZero(f.Until),
		Source:  sourceLedger,
	})
}

// handleFacets serves GET /api/facets: the distinct values of each
// categorisation dimension within the range, heaviest first, so the page can
// build its lanes and filters out of what is actually there.
//
// The echoed since/until are the range as REQUESTED, because they identify the
// request. Three of the four lists come from the rollup, whose bounds snap
// outward to whole 15-minute buckets, so a request that does not start and end
// on a bucket boundary can count a few minutes the provider list (served by the
// ledger) does not. Every range the page asks for is hour-aligned, and no facet
// is a total anyone spends money against, which is why this is documented
// rather than paid for.
func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	if !allowGET(w, r) {
		return
	}
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// group_by is meaningless here - the endpoint fixes its own dimensions - and
	// honouring it would multiply every list. Drop it rather than half-apply it.
	f.GroupBy = nil

	ctx := r.Context()
	// One freshness verdict for the whole response, so the four lists cannot be
	// answered from different tables within one request. The verdict alone is
	// not the source label: a filter the rollup cannot serve (a session filter)
	// sends every list to the ledger however fresh the rollup is, and the label
	// must say what actually answered.
	probe := f
	probe.GroupBy = []string{"tool"}
	usable := !s.rollupStale(ctx) && rollupServiceable(probe)
	out := facetsResponse{
		Since:  unixOrZero(f.Since),
		Until:  unixOrZero(f.Until),
		Source: sourceLedger,
	}
	if usable {
		out.Source = sourceRollup
	}
	for _, spec := range []struct {
		dim  string
		dest *[]facetWire
	}{
		{"tool", &out.Tools},
		{"model", &out.Models},
		// provider is the one the rollup cannot serve, so this list alone costs
		// a ledger group-by. It is bounded by the range like the others.
		{"provider", &out.Providers},
		{"project", &out.Projects},
	} {
		q := f
		q.GroupBy = []string{spec.dim}
		vals, err := s.facetValues(ctx, f, spec.dim, usable && rollupServiceable(q))
		if err != nil {
			s.fail(w, "facet "+spec.dim, err)
			return
		}
		*spec.dest = vals
	}
	s.writeJSON(w, out)
}

// facetValues aggregates one dimension over the filter and sorts the result
// heaviest first, ties broken by value so the order is stable across calls.
// Values are returned verbatim, INCLUDING the empty string: an empty provider or
// project is a real row group meaning "the source never named one", and labelling
// it is the page's job, not the ledger's.
//
// The caller decides which table to read, because the decision belongs to the
// whole response: a facet list from the rollup next to one from the ledger
// would describe two different states of the same database.
func (s *Server) facetValues(ctx context.Context, f store.Filter, dim string, useRollup bool) ([]facetWire, error) {
	q := f
	q.GroupBy = []string{dim}

	var buckets []store.Bucket
	if useRollup {
		rs, err := s.reader.SummarizeRollup(ctx, q)
		if err != nil {
			return nil, err
		}
		buckets = rs.Buckets
	} else {
		sum, err := s.reader.Summarize(ctx, q)
		if err != nil {
			return nil, err
		}
		buckets = sum.Buckets
	}

	out := make([]facetWire, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, facetWire{Value: b.Keys[dim], Events: b.Events, Total: b.Total})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

// handleEvents serves GET /api/events: one capped page of the ledger, walked by
// keyset cursor on the row id.
//
// The cap is the contract. A range holding more rows than the page returns comes
// back with truncated:true, the true count, and a cursor to continue from - a
// silent slice would let the page draw a confident picture of a third of the
// data.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !allowGET(w, r) {
		return
	}
	q := r.URL.Query()
	f, err := parseFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.GroupBy = nil
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cursor, err := parseCursor(q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	// One row beyond the page: enough to know whether another page exists
	// without a second query, and cheap enough that it costs nothing to say so.
	events, err := s.reader.ListEvents(ctx, f, store.WithKeyset(cursor, limit+1))
	if err != nil {
		s.fail(w, "list events", err)
		return
	}
	more := len(events) > limit
	if more {
		events = events[:limit]
	}

	rows := make([]eventWire, 0, len(events))
	for _, e := range events {
		rows = append(rows, toEventWire(e))
	}

	var next *string
	if more && len(events) > 0 {
		c := strconv.FormatInt(events[len(events)-1].ID, 10)
		next = &c
	}

	// The true count only needs asking when this page is not the whole answer:
	// a first page that fit is its own count, exactly and for free.
	total := int64(len(rows))
	if more || cursor > 0 {
		n, err := s.countEvents(ctx, f)
		if err != nil {
			s.fail(w, "count events", err)
			return
		}
		total = n
	}

	s.writeJSON(w, eventsResponse{
		Rows:       rows,
		NextCursor: next,
		Truncated:  more,
		Limit:      limit,
		Total:      total,
	})
}

// countEvents is the true number of rows the range holds, aggregated in SQL. It
// goes to the ledger even when the rollup could answer: the rollup snaps its
// bounds outward to whole 15-minute buckets, and a count that quietly covers a
// wider range than the rows it explains is worse than no count.
func (s *Server) countEvents(ctx context.Context, f store.Filter) (int64, error) {
	total := f
	total.GroupBy = nil
	sum, err := s.reader.Summarize(ctx, total)
	if err != nil {
		return 0, err
	}
	return sum.Totals.Events, nil
}

// allowGET rejects anything but GET and HEAD. The whole API is read-only, so a
// POST is not a route that is missing - it is a request the server will never
// grow.
func allowGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeError(w, http.StatusMethodNotAllowed, "read-only api: use GET")
	return false
}

// fail logs the real error and returns a generic one. The log is the operator's;
// the response is a stranger's, and store errors quote SQL and file paths.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Printf("web: %s: %v", what, err)
	writeError(w, http.StatusInternalServerError, "could not "+what)
}

// orEmpty normalises a nil string slice to an empty one so the JSON carries []
// rather than null for an ungrouped query.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
