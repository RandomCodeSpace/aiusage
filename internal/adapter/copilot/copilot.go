// Package copilot implements the event-level adapter for GitHub Copilot CLI.
//
// Copilot does not expose billing-grade token usage in its session store or
// events.jsonl (those are context-window gauges). Usage lives only in the
// OpenTelemetry JSONL the CLI writes when the user enables file export. We read
// ~/.copilot/otel/**/*.jsonl (recursively) plus the single file named by
// COPILOT_OTEL_FILE_EXPORTER_PATH. When neither exists the adapter returns no
// sources and no error (the feature is opt-in; doctor surfaces this).
//
// Each OTEL record can describe the same model call from several vantage points
// (chat span, inference log, agent-turn log, agent-summary span). We keep the
// highest-priority record per shared traceId / gen_ai.response.id and suppress
// the rest so a single call is counted once.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY; nothing under the
// agent's directories is created, locked, or modified.
package copilot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/tokenutil"
)

// attrMarker fast-skips JSONL lines that cannot carry usage attributes.
const attrMarker = `"attributes"`

// exporterEnv names the single-file OTEL exporter override.
const exporterEnv = "COPILOT_OTEL_FILE_EXPORTER_PATH"

// recordSource classifies an OTEL record. Lower value = higher priority; a
// higher-priority record for the same trace/response suppresses lower ones.
type recordSource int

const (
	srcChatSpan recordSource = iota
	srcInferenceLog
	srcAgentTurnLog
	srcAgentSummarySpan
)

// model attribute keys, in preference order.
var modelAttrs = []string{"gen_ai.response.model", "gen_ai.request.model"}

// sessionAttr pairs a session attribute key with its priority (higher wins).
type sessionAttr struct {
	key      string
	priority int
}

var sessionAttrs = []sessionAttr{
	{"gen_ai.conversation.id", 3},
	{"copilot_chat.session_id", 3},
	{"copilot_chat.chat_session_id", 3},
	{"session.id", 3},
	{"github.copilot.interaction_id", 2},
	{"gen_ai.response.id", 1},
}

// Adapter reads GitHub Copilot CLI OpenTelemetry usage exports.
type Adapter struct{}

// New returns a Copilot adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolCopilot }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "GitHub Copilot" }

// Discover finds every OTEL JSONL file under <root>/.copilot/otel (recursively)
// and, additively, the single file named by COPILOT_OTEL_FILE_EXPORTER_PATH.
// Each file becomes one Source. When the directory is absent and the env file is
// unset/missing, Discover returns no sources and no error.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	root := cfg.Root(model.ToolCopilot, "")
	seen := make(map[string]struct{})
	var sources []adapter.Source

	add := func(path string) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if _, dup := seen[path]; dup {
			return
		}
		seen[path] = struct{}{}
		sources = append(sources, adapter.Source{
			Tool:  model.ToolCopilot,
			Class: model.EventLevel,
			Path:  path,
			Label: "GitHub Copilot OTEL: " + path,
		})
	}

	if root != "" {
		otelDir := filepath.Join(root, ".copilot", "otel")
		if fi, err := os.Stat(otelDir); err == nil && fi.IsDir() {
			_ = filepath.WalkDir(otelDir, func(p string, de fs.DirEntry, err error) error {
				if err != nil {
					return nil // skip unreadable subtrees, keep walking
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
					return nil
				}
				add(p)
				return nil
			})
		}
	}

	if env := strings.TrimSpace(os.Getenv(exporterEnv)); env != "" {
		if fi, err := os.Stat(env); err == nil && !fi.IsDir() {
			add(env)
		}
	}

	return sources, nil
}

// Collect parses one OTEL JSONL file, applies cross-record suppression, and
// returns the surviving usage events. Malformed lines are tolerated; a read
// that does not complete returns an error and no checkpoint so the next cycle
// retries.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the file on size+mtime: unchanged files are not
// opened at all. Any change re-parses the whole file — the per-trace fallback
// model/session context spans all records of a file (a later record may
// supply the context an earlier usage record needs), so a tail read is not
// applicable. A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, nil // absent/unreadable: nothing to report (opt-in feature)
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}

	newCp := &model.SourceCheckpoint{
		Tool: model.ToolCopilot, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
	}

	records, err := readRecords(src.Path)
	if err != nil {
		// Failed or partial read: no checkpoint, so the next cycle retries even
		// if size+mtime are unchanged. Partial records are dropped too —
		// cross-record suppression needs the whole file, and an unsuppressed
		// lower-priority record would mint a distinct dedup key (double count).
		return adapter.Observation{}, fmt.Errorf("copilot: read %s: %w", src.Path, err)
	}
	if len(records) == 0 {
		return adapter.Observation{Checkpoint: newCp}, nil
	}

	fallbackTS := fi.ModTime().UTC()
	traceModels, traceSessions := collectTraceContexts(records)

	cands := make([]*candidate, 0, len(records))
	for i, rec := range records {
		if ctx.Err() != nil {
			// Partial parse: no checkpoint, so the next cycle re-reads in full.
			return adapter.Observation{Events: candidatesToEvents(filterEmitted(cands), src.Path)}, ctx.Err()
		}
		if c := toCandidate(rec, i, fallbackTS, traceModels, traceSessions); c != nil {
			cands = append(cands, c)
		}
	}

	return adapter.Observation{
		Events:     candidatesToEvents(filterEmitted(cands), src.Path),
		Checkpoint: newCp,
	}, nil
}

// otelRecord is the typed decode of one OTEL JSONL line. Every field stays raw
// JSON, decoded lazily only where read, so loosely-typed exporters (numeric
// strings, unexpected shapes in unread fields) never fail a line — matching
// the tolerance of the previous generic-map decode at a fraction of the
// allocations.
type otelRecord struct {
	Type         json.RawMessage            `json:"type"`
	Name         json.RawMessage            `json:"name"`
	Kind         json.RawMessage            `json:"kind"`
	TraceID      json.RawMessage            `json:"traceId"`
	SpanID       json.RawMessage            `json:"spanId"`
	SpanContext  *spanContextObj            `json:"spanContext"`
	StartTime    json.RawMessage            `json:"startTime"`
	EndTime      json.RawMessage            `json:"endTime"`
	Duration     json.RawMessage            `json:"duration"`
	HrTime       json.RawMessage            `json:"hrTime"`
	HrTimeAlt    json.RawMessage            `json:"_hrTime"`
	Time         json.RawMessage            `json:"time"`
	Timestamp    json.RawMessage            `json:"timestamp"`
	ObservedTS   json.RawMessage            `json:"observedTimestamp"`
	TimeUnixNano json.RawMessage            `json:"timeUnixNano"`
	Body         json.RawMessage            `json:"body"`
	BodyAlt      json.RawMessage            `json:"_body"`
	Attributes   map[string]json.RawMessage `json:"attributes"`

	// ts/hasTS are resolved once at read time; stamp is the content-derived
	// dedup stamp, computed only for timestamp-less records.
	ts    time.Time
	hasTS bool
	stamp string
}

type spanContextObj struct {
	TraceID json.RawMessage `json:"traceId"`
	SpanID  json.RawMessage `json:"spanId"`
}

// readRecords streams the file line by line into typed records, keeping only
// lines that contain the "attributes" marker and decode as JSON objects. A
// non-nil error (open failure or scanner error) means the read did not
// complete; the caller must not checkpoint the file.
func readRecords(path string) ([]*otelRecord, error) {
	f, err := os.Open(path) // O_RDONLY
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []*otelRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // OTEL lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(attrMarker)) {
			continue
		}
		rec := &otelRecord{}
		if err := json.Unmarshal(line, rec); err != nil {
			continue
		}
		rec.ts, rec.hasTS = timestampFromRecord(rec)
		if !rec.hasTS {
			rec.stamp = contentStamp(line)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// traceContext accumulates fallback model/session for records sharing a traceId.
type traceContext struct {
	model           string
	sessionID       string
	sessionPriority int
}

// collectTraceContexts builds per-trace fallback model and session, mirroring
// ccusage: first non-empty model wins; highest-priority session wins.
func collectTraceContexts(records []*otelRecord) (map[string]string, map[string]string) {
	ctxs := make(map[string]*traceContext)
	for _, rec := range records {
		trace, ok := traceIDFromRecord(rec)
		if !ok {
			continue
		}
		if rec.Attributes == nil {
			continue
		}
		tc := ctxs[trace]
		if tc == nil {
			tc = &traceContext{}
			ctxs[trace] = tc
		}
		if tc.model == "" {
			tc.model = firstNonEmptyAttr(rec.Attributes, modelAttrs)
		}
		if sid, prio, ok := bestSessionAttr(rec.Attributes); ok && prio > tc.sessionPriority {
			tc.sessionID = sid
			tc.sessionPriority = prio
		}
	}
	models := make(map[string]string, len(ctxs))
	sessions := make(map[string]string, len(ctxs))
	for trace, tc := range ctxs {
		models[trace] = tc.model
		sessions[trace] = tc.sessionID
	}
	return models, sessions
}

// candidate is a parsed usage record awaiting suppression and conversion.
type candidate struct {
	source     recordSource
	traceID    string
	hasTrace   bool
	responseID string
	model      string
	sessionID  string
	eventTime  time.Time

	input, output, cacheCreation, cacheRead, reasoning int64
	total                                              int64

	dedupKey string
	raw      string
	emit     bool
}

// toCandidate classifies a record and maps its token attributes. Records that
// match no known shape, or that carry no tokens, are dropped (nil).
func toCandidate(rec *otelRecord, index int, fallbackTS time.Time, traceModels, traceSessions map[string]string) *candidate {
	attrs := rec.Attributes
	if attrs == nil {
		return nil
	}

	var source recordSource
	switch {
	case isChatSpan(rec, attrs):
		source = srcChatSpan
	case isInferenceLog(rec, attrs):
		source = srcInferenceLog
	case isAgentTurnLog(rec, attrs):
		source = srcAgentTurnLog
	case isAgentSummarySpan(rec, attrs):
		source = srcAgentSummarySpan
	default:
		return nil
	}

	input := attrNumber(attrs, "gen_ai.usage.input_tokens")
	output := attrNumber(attrs, "gen_ai.usage.output_tokens")
	cacheRead := attrNumber(attrs, "gen_ai.usage.cache_read.input_tokens")
	cacheCreation := attrNumberFirst(attrs,
		"gen_ai.usage.cache_write.input_tokens",
		"gen_ai.usage.cache_creation.input_tokens")
	reasoning := attrNumberFirst(attrs,
		"gen_ai.usage.reasoning.output_tokens",
		"gen_ai.usage.reasoning_tokens")
	totalAttr := attrNumberFirst(attrs,
		"gen_ai.usage.total_tokens",
		"gen_ai.usage.total.token_count")

	// Cache-read is a subset of input; never double-count it.
	if cacheRead > input {
		input = 0
	} else {
		input -= cacheRead
	}

	// Reconcile against the provider total. extra == reasoning per spec; the
	// fallback may fill output or grow reasoning, never shrink known parts.
	output, reasoning = tokenutil.ApplyTotalFallback(input, output, cacheCreation, cacheRead, reasoning, totalAttr)

	// Authoritative total: gen_ai.usage.total_tokens when the exporter reported
	// one. Copilot proxies several vendors and for Anthropic-backed models the
	// reasoning attribute is already inside output_tokens, so recomputing the
	// sum would inflate the exporter's own total and bill those tokens twice.
	// The component sum is used only when no total was reported — the same rule
	// codex and gemini already follow.
	//
	// UNVERIFIED: no local Copilot OTEL data exists to check the reasoning rule
	// per backing provider (issue #28). model.ReasoningModeFor now agrees with
	// this handling and treats Copilot reasoning as a subset of output, so the
	// attribute is never billed twice; revisit both together.
	total := totalAttr
	if total <= 0 {
		total = input + output + cacheCreation + cacheRead + reasoning
	}
	if total == 0 {
		return nil
	}

	trace, hasTrace := traceIDFromRecord(rec)
	responseID := attrString(attrs, "gen_ai.response.id")

	mdl := firstNonEmptyAttr(attrs, modelAttrs)
	if mdl == "" && hasTrace {
		mdl = traceModels[trace]
	}
	if mdl == "" {
		mdl = "unknown"
	}

	session := ""
	if sid, _, ok := bestSessionAttr(attrs); ok {
		session = sid
	}
	if session == "" && hasTrace {
		session = traceSessions[trace]
	}
	if session == "" {
		session = trace
	}
	if session == "" {
		session = "unknown-session"
	}

	ts, hasTS := rec.ts, rec.hasTS
	if !hasTS {
		ts = fallbackTS
	}

	dedup := dedupKeyForRecord(source, rec, attrs, trace, hasTrace, session, ts, hasTS, index)

	return &candidate{
		source:        source,
		traceID:       trace,
		hasTrace:      hasTrace,
		responseID:    responseID,
		model:         mdl,
		sessionID:     session,
		eventTime:     ts,
		input:         input,
		output:        output,
		cacheCreation: cacheCreation,
		cacheRead:     cacheRead,
		reasoning:     reasoning,
		total:         total,
		dedupKey:      dedup,
		raw:           rawAttrs(attrs),
	}
}

// filterEmitted marks survivors of cross-record suppression and returns them in
// original order. Suppression follows the priority chain: ChatSpan always wins;
// each lower source is dropped if a higher-priority source shares its trace or
// response id.
func filterEmitted(cands []*candidate) []*candidate {
	chatTraces := traceSet(cands, srcChatSpan)
	infTraces := traceSet(cands, srcInferenceLog)
	turnTraces := traceSet(cands, srcAgentTurnLog)
	chatResp := respSet(cands, srcChatSpan)
	infResp := respSet(cands, srcInferenceLog)
	turnResp := respSet(cands, srcAgentTurnLog)

	traceHit := func(c *candidate, set map[string]struct{}) bool {
		if !c.hasTrace {
			return false
		}
		_, ok := set[c.traceID]
		return ok
	}
	respHit := func(c *candidate, set map[string]struct{}) bool {
		if c.responseID == "" {
			return false
		}
		_, ok := set[c.responseID]
		return ok
	}

	var out []*candidate
	for _, c := range cands {
		switch c.source {
		case srcChatSpan:
			c.emit = true
		case srcInferenceLog:
			c.emit = !traceHit(c, chatTraces) && !respHit(c, chatResp)
		case srcAgentTurnLog:
			c.emit = !traceHit(c, chatTraces) && !traceHit(c, infTraces) &&
				!respHit(c, chatResp) && !respHit(c, infResp)
		case srcAgentSummarySpan:
			c.emit = !traceHit(c, chatTraces) && !traceHit(c, infTraces) && !traceHit(c, turnTraces) &&
				!respHit(c, chatResp) && !respHit(c, infResp) && !respHit(c, turnResp)
		}
		if c.emit {
			out = append(out, c)
		}
	}
	return out
}

func traceSet(cands []*candidate, src recordSource) map[string]struct{} {
	set := make(map[string]struct{})
	for _, c := range cands {
		if c.source == src && c.hasTrace {
			set[c.traceID] = struct{}{}
		}
	}
	return set
}

func respSet(cands []*candidate, src recordSource) map[string]struct{} {
	set := make(map[string]struct{})
	for _, c := range cands {
		if c.source == src && c.responseID != "" {
			set[c.responseID] = struct{}{}
		}
	}
	return set
}

// candidatesToEvents converts emitted candidates to immutable usage events.
func candidatesToEvents(cands []*candidate, path string) []model.UsageEvent {
	if len(cands) == 0 {
		return nil
	}
	evs := make([]model.UsageEvent, 0, len(cands))
	for _, c := range cands {
		evs = append(evs, model.UsageEvent{
			Tool:                model.ToolCopilot,
			Model:               c.model,
			Provider:            model.ProviderGitHub,
			SessionID:           c.sessionID,
			Project:             "",
			EventTime:           c.eventTime,
			InputTokens:         c.input,
			OutputTokens:        c.output,
			CacheCreationTokens: c.cacheCreation,
			CacheReadTokens:     c.cacheRead,
			ReasoningTokens:     c.reasoning,
			TotalTokens:         c.total,
			RequestID:           c.responseID,
			SourcePath:          path,
			DedupKey:            model.ToolCopilot + "|" + c.dedupKey,
			Kind:                model.KindUsage,
			Raw:                 c.raw,
		})
	}
	return evs
}

// dedupKeyForRecord builds the per-record key string (without the tool prefix).
// Timestamp-less records (hasTS false) get a content-derived stamp instead of
// ts: ts is then the file mtime, which advances on every append and would mint
// a fresh key per poll — an unbounded recount.
func dedupKeyForRecord(source recordSource, rec *otelRecord, attrs map[string]json.RawMessage, trace string, hasTrace bool, session string, ts time.Time, hasTS bool, index int) string {
	span, hasSpan := spanIDFromRecord(rec)
	stamp := strconv.FormatInt(ts.UnixMilli(), 10)
	if !hasTS {
		stamp = rec.stamp
	}
	switch source {
	case srcChatSpan, srcAgentSummarySpan:
		if hasTrace && hasSpan {
			return trace + ":" + span
		}
		return "span:" + session + ":" + stamp + ":" + strconv.Itoa(index)
	case srcInferenceLog:
		if hasTrace && hasSpan {
			return "log:" + trace + ":" + span
		}
		return "log:" + session + ":" + stamp + ":" + strconv.Itoa(index)
	default: // srcAgentTurnLog
		turnIdx := ""
		if v, ok := numberValueRaw(attrs["turn.index"]); ok {
			turnIdx = strconv.FormatInt(v, 10)
		} else if v, ok := numberValueRaw(attrs["copilot_chat.turn.index"]); ok {
			turnIdx = strconv.FormatInt(v, 10)
		} else {
			turnIdx = "idx-" + strconv.Itoa(index)
		}
		if hasTrace {
			return "agent-turn:" + trace + ":" + turnIdx
		}
		return "agent-turn:" + session + ":" + turnIdx + ":" + strconv.Itoa(index)
	}
}

// contentStamp hashes the record's own content — never the file mtime — so a
// timestamp-less record keeps a stable dedup key across polls. The line is
// round-tripped through the generic map decode and json.Marshal (sorted keys)
// to stay byte-identical with the stamps of previously stored dedup keys; a
// direct hash of the raw line would re-key (and re-count) history.
func contentStamp(line []byte) string {
	var v map[string]any
	if err := json.Unmarshal(line, &v); err != nil {
		return "unhashable"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "unhashable"
	}
	sum := sha1.Sum(b)
	return fmt.Sprintf("c%x", sum[:8])
}

// --- record-shape detection (mirrors ccusage parser.rs) ---

func isSpanRecord(rec *otelRecord) bool {
	if t, ok := rawString(rec.Type); ok {
		return t == "span"
	}
	if _, ok := rawString(rec.Name); !ok {
		return false
	}
	if _, ok := rawString(rec.SpanID); ok {
		return true
	}
	if _, ok := rawString(rec.TraceID); ok {
		return true
	}
	// Presence checks: a present-but-null field still counts, matching the
	// previous generic-map decode.
	return len(rec.StartTime) > 0 || len(rec.EndTime) > 0 || len(rec.Duration) > 0 || len(rec.Kind) > 0
}

func isChatSpan(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == "chat" {
		return true
	}
	if name, ok := rawString(rec.Name); ok && strings.HasPrefix(name, "chat ") {
		return true
	}
	return false
}

func isAgentSummarySpan(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == "invoke_agent" {
		return true
	}
	if name, ok := rawString(rec.Name); ok && strings.HasPrefix(name, "invoke_agent ") {
		return true
	}
	return false
}

func isInferenceLog(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "event.name") == "gen_ai.client.inference.operation.details" {
		return true
	}
	if body, ok := recordBody(rec); ok && strings.HasPrefix(body, "GenAI inference:") {
		return true
	}
	return false
}

func isAgentTurnLog(rec *otelRecord, attrs map[string]json.RawMessage) bool {
	if isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "event.name") == "copilot_chat.agent.turn" {
		return true
	}
	if body, ok := recordBody(rec); ok && strings.HasPrefix(body, "copilot_chat.agent.turn") {
		return true
	}
	return false
}

// --- field extraction helpers ---

func traceIDFromRecord(rec *otelRecord) (string, bool) {
	if v, ok := rawString(rec.TraceID); ok {
		return v, true
	}
	if rec.SpanContext != nil {
		return rawString(rec.SpanContext.TraceID)
	}
	return "", false
}

func spanIDFromRecord(rec *otelRecord) (string, bool) {
	if v, ok := rawString(rec.SpanID); ok {
		return v, true
	}
	if rec.SpanContext != nil {
		return rawString(rec.SpanContext.SpanID)
	}
	return "", false
}

func recordBody(rec *otelRecord) (string, bool) {
	if v, ok := rawString(rec.Body); ok {
		return v, true
	}
	return rawString(rec.BodyAlt)
}

// rawString decodes a raw JSON value as a trimmed non-empty string, else
// ok=false.
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

// numberValue parses a non-negative integer from a JSON number or numeric
// string. encoding/json decodes numbers as float64; numeric strings are also
// accepted (OTEL exporters sometimes stringify values). Floats at or above
// MaxInt64 are rejected: Go's out-of-range conversion is implementation-
// specific (MinInt64 on amd64).
func numberValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || n < 0 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			if i < 0 {
				return 0, false
			}
			return i, true
		}
		if f, err := n.Float64(); err == nil && f >= 0 && f < math.MaxInt64 {
			return int64(f), true
		}
		return 0, false
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			if i < 0 {
				return 0, false
			}
			return i, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// numberValueRaw decodes a raw JSON value (number or numeric string) through
// the same range rules as numberValue.
func numberValueRaw(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return numberValue(num)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return numberValue(s)
	}
	return 0, false
}

func attrString(attrs map[string]json.RawMessage, key string) string {
	s, _ := rawString(attrs[key])
	return s
}

// attrNumber returns the attribute's integer value, defaulting to 0.
func attrNumber(attrs map[string]json.RawMessage, key string) int64 {
	if v, ok := numberValueRaw(attrs[key]); ok {
		return v
	}
	return 0
}

// attrNumberFirst returns the first key whose value parses to > 0, else 0.
func attrNumberFirst(attrs map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := numberValueRaw(attrs[k]); ok && v > 0 {
			return v
		}
	}
	return 0
}

func firstNonEmptyAttr(attrs map[string]json.RawMessage, keys []string) string {
	for _, k := range keys {
		if s := attrString(attrs, k); s != "" {
			return s
		}
	}
	return ""
}

// bestSessionAttr returns the highest-priority present session attribute.
func bestSessionAttr(attrs map[string]json.RawMessage) (string, int, bool) {
	best := ""
	bestPrio := -1
	for _, sa := range sessionAttrs {
		if s := attrString(attrs, sa.key); s != "" && sa.priority > bestPrio {
			best = s
			bestPrio = sa.priority
		}
	}
	if bestPrio < 0 {
		return "", 0, false
	}
	return best, bestPrio, true
}

// --- timestamp heuristics (mirrors ccusage parser.rs) ---

func timestampFromRecord(rec *otelRecord) (time.Time, bool) {
	for _, raw := range []json.RawMessage{rec.EndTime, rec.StartTime, rec.HrTime, rec.HrTimeAlt, rec.Time} {
		if t, ok := timestampFromParts(raw); ok {
			return t, true
		}
	}
	for _, raw := range []json.RawMessage{rec.Timestamp, rec.ObservedTS} {
		if t, ok := timestampFromScalar(raw); ok {
			return t, true
		}
	}
	if t, ok := timestampFromUnixNanos(rec.TimeUnixNano); ok {
		return t, true
	}
	return time.Time{}, false
}

// timestampFromParts reads an OTEL hrTime [seconds, nanos] pair -> ms.
func timestampFromParts(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) < 2 {
		return time.Time{}, false
	}
	sec, ok := numberValueRaw(arr[0])
	if !ok {
		return time.Time{}, false
	}
	nanos, ok := numberValueRaw(arr[1])
	if !ok {
		return time.Time{}, false
	}
	millis := sec*1000 + nanos/1_000_000
	return time.UnixMilli(millis).UTC(), true
}

// timestampFromScalar interprets a single numeric timestamp whose unit is
// inferred from magnitude (ns/us/ms/s), matching ccusage.
func timestampFromScalar(rawJSON json.RawMessage) (time.Time, bool) {
	raw, ok := numberValueRaw(rawJSON)
	if !ok {
		return time.Time{}, false
	}
	var millis int64
	switch {
	case raw >= 100_000_000_000_000_000: // nanoseconds
		millis = raw / 1_000_000
	case raw >= 100_000_000_000_000: // microseconds
		millis = raw / 1_000
	case raw >= 100_000_000_000: // milliseconds
		millis = raw
	default: // seconds
		millis = raw * 1_000
	}
	return time.UnixMilli(millis).UTC(), true
}

func timestampFromUnixNanos(rawJSON json.RawMessage) (time.Time, bool) {
	raw, ok := numberValueRaw(rawJSON)
	if !ok || raw <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(raw / 1_000_000).UTC(), true
}

// rawAttrs serialises the attributes object for audit; best-effort. Values
// keep their original encoding (they are raw JSON), keys are sorted.
func rawAttrs(attrs map[string]json.RawMessage) string {
	b, err := json.Marshal(attrs)
	if err != nil {
		return ""
	}
	return string(b)
}
