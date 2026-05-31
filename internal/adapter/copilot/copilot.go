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
	"context"
	"encoding/json"
	"io/fs"
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
// returns the surviving usage events. Read errors and malformed lines are
// tolerated so one bad file never fails the cycle.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	records := readRecords(src.Path)
	if len(records) == 0 {
		return adapter.Observation{}, nil
	}

	fallbackTS := fileMTime(src.Path)
	traceModels, traceSessions := collectTraceContexts(records)

	cands := make([]*candidate, 0, len(records))
	for i, rec := range records {
		if ctx.Err() != nil {
			return adapter.Observation{Events: candidatesToEvents(filterEmitted(cands), src.Path)}, ctx.Err()
		}
		if c := toCandidate(rec, i, fallbackTS, traceModels, traceSessions); c != nil {
			cands = append(cands, c)
		}
	}

	return adapter.Observation{Events: candidatesToEvents(filterEmitted(cands), src.Path)}, nil
}

// readRecords reads the file and returns each parsed JSON object whose raw line
// contained the "attributes" marker. Non-object and unparsable lines are
// skipped.
func readRecords(path string) []map[string]any {
	f, err := os.Open(path) // O_RDONLY
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // OTEL lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), attrMarker) {
			continue
		}
		var v any
		if err := json.Unmarshal(line, &v); err != nil {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// traceContext accumulates fallback model/session for records sharing a traceId.
type traceContext struct {
	model           string
	sessionID       string
	sessionPriority int
}

// collectTraceContexts builds per-trace fallback model and session, mirroring
// ccusage: first non-empty model wins; highest-priority session wins.
func collectTraceContexts(records []map[string]any) (map[string]string, map[string]string) {
	ctxs := make(map[string]*traceContext)
	for _, rec := range records {
		trace, ok := traceIDFromRecord(rec)
		if !ok {
			continue
		}
		attrs, ok := objAttr(rec, "attributes")
		if !ok {
			continue
		}
		tc := ctxs[trace]
		if tc == nil {
			tc = &traceContext{}
			ctxs[trace] = tc
		}
		if tc.model == "" {
			tc.model = firstNonEmptyAttr(attrs, modelAttrs)
		}
		if sid, prio, ok := bestSessionAttr(attrs); ok && prio > tc.sessionPriority {
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
func toCandidate(rec map[string]any, index int, fallbackTS time.Time, traceModels, traceSessions map[string]string) *candidate {
	attrs, ok := objAttr(rec, "attributes")
	if !ok {
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

	// Provider-authoritative total (matches ccusage: components + reasoning).
	total := input + output + cacheCreation + cacheRead + reasoning
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

	ts, ok := timestampFromRecord(rec)
	if !ok {
		ts = fallbackTS
	}

	dedup := dedupKeyForRecord(source, rec, attrs, trace, hasTrace, session, ts, index)

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
func dedupKeyForRecord(source recordSource, rec, attrs map[string]any, trace string, hasTrace bool, session string, ts time.Time, index int) string {
	span, hasSpan := spanIDFromRecord(rec)
	millis := ts.UnixMilli()
	switch source {
	case srcChatSpan, srcAgentSummarySpan:
		if hasTrace && hasSpan {
			return trace + ":" + span
		}
		return "span:" + session + ":" + strconv.FormatInt(millis, 10) + ":" + strconv.Itoa(index)
	case srcInferenceLog:
		if hasTrace && hasSpan {
			return "log:" + trace + ":" + span
		}
		return "log:" + session + ":" + strconv.FormatInt(millis, 10) + ":" + strconv.Itoa(index)
	default: // srcAgentTurnLog
		turnIdx := ""
		if v, ok := numberValue(attrs["turn.index"]); ok {
			turnIdx = strconv.FormatInt(v, 10)
		} else if v, ok := numberValue(attrs["copilot_chat.turn.index"]); ok {
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

// --- record-shape detection (mirrors ccusage parser.rs) ---

func isSpanRecord(rec map[string]any) bool {
	if t, ok := stringField(rec["type"]); ok {
		return t == "span"
	}
	if _, ok := stringField(rec["name"]); !ok {
		return false
	}
	if _, ok := stringField(rec["spanId"]); ok {
		return true
	}
	if _, ok := stringField(rec["traceId"]); ok {
		return true
	}
	_, hasStart := rec["startTime"]
	_, hasEnd := rec["endTime"]
	_, hasDur := rec["duration"]
	_, hasKind := rec["kind"]
	return hasStart || hasEnd || hasDur || hasKind
}

func isChatSpan(rec, attrs map[string]any) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == "chat" {
		return true
	}
	if name, ok := stringField(rec["name"]); ok && strings.HasPrefix(name, "chat ") {
		return true
	}
	return false
}

func isAgentSummarySpan(rec, attrs map[string]any) bool {
	if !isSpanRecord(rec) {
		return false
	}
	if attrString(attrs, "gen_ai.operation.name") == "invoke_agent" {
		return true
	}
	if name, ok := stringField(rec["name"]); ok && strings.HasPrefix(name, "invoke_agent ") {
		return true
	}
	return false
}

func isInferenceLog(rec, attrs map[string]any) bool {
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

func isAgentTurnLog(rec, attrs map[string]any) bool {
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

func traceIDFromRecord(rec map[string]any) (string, bool) {
	if v, ok := stringField(rec["traceId"]); ok {
		return v, true
	}
	return nestedString(rec, "spanContext", "traceId")
}

func spanIDFromRecord(rec map[string]any) (string, bool) {
	if v, ok := stringField(rec["spanId"]); ok {
		return v, true
	}
	return nestedString(rec, "spanContext", "spanId")
}

func nestedString(rec map[string]any, object, key string) (string, bool) {
	obj, ok := objAttr(rec, object)
	if !ok {
		return "", false
	}
	return stringField(obj[key])
}

func recordBody(rec map[string]any) (string, bool) {
	if v, ok := stringField(rec["body"]); ok {
		return v, true
	}
	return stringField(rec["_body"])
}

// objAttr returns rec[key] as a JSON object when it is one.
func objAttr(rec map[string]any, key string) (map[string]any, bool) {
	m, ok := rec[key].(map[string]any)
	return m, ok
}

// stringField returns a trimmed non-empty string value, else ok=false.
func stringField(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
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
// accepted (OTEL exporters sometimes stringify values).
func numberValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
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
		if f, err := n.Float64(); err == nil && f >= 0 {
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

func attrString(attrs map[string]any, key string) string {
	s, _ := stringField(attrs[key])
	return s
}

// attrNumber returns the attribute's integer value, defaulting to 0.
func attrNumber(attrs map[string]any, key string) int64 {
	if v, ok := numberValue(attrs[key]); ok {
		return v
	}
	return 0
}

// attrNumberFirst returns the first key whose value parses to > 0, else 0.
func attrNumberFirst(attrs map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := numberValue(attrs[k]); ok && v > 0 {
			return v
		}
	}
	return 0
}

func firstNonEmptyAttr(attrs map[string]any, keys []string) string {
	for _, k := range keys {
		if s := attrString(attrs, k); s != "" {
			return s
		}
	}
	return ""
}

// bestSessionAttr returns the highest-priority present session attribute.
func bestSessionAttr(attrs map[string]any) (string, int, bool) {
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

func timestampFromRecord(rec map[string]any) (time.Time, bool) {
	for _, key := range []string{"endTime", "startTime", "hrTime", "_hrTime", "time"} {
		if t, ok := timestampFromParts(rec[key]); ok {
			return t, true
		}
	}
	for _, key := range []string{"timestamp", "observedTimestamp"} {
		if t, ok := timestampFromScalar(rec[key]); ok {
			return t, true
		}
	}
	if t, ok := timestampFromUnixNanos(rec["timeUnixNano"]); ok {
		return t, true
	}
	return time.Time{}, false
}

// timestampFromParts reads an OTEL hrTime [seconds, nanos] pair -> ms.
func timestampFromParts(v any) (time.Time, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) < 2 {
		return time.Time{}, false
	}
	sec, ok := numberValue(arr[0])
	if !ok {
		return time.Time{}, false
	}
	nanos, ok := numberValue(arr[1])
	if !ok {
		return time.Time{}, false
	}
	millis := sec*1000 + nanos/1_000_000
	return time.UnixMilli(millis).UTC(), true
}

// timestampFromScalar interprets a single numeric timestamp whose unit is
// inferred from magnitude (ns/us/ms/s), matching ccusage.
func timestampFromScalar(v any) (time.Time, bool) {
	raw, ok := numberValue(v)
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

func timestampFromUnixNanos(v any) (time.Time, bool) {
	raw, ok := numberValue(v)
	if !ok || raw <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(raw / 1_000_000).UTC(), true
}

func fileMTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().UTC()
	}
	return time.Now().UTC()
}

// rawAttrs serialises the attributes object for audit; best-effort.
func rawAttrs(attrs map[string]any) string {
	b, err := json.Marshal(attrs)
	if err != nil {
		return ""
	}
	return string(b)
}
