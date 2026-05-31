// Package gemini implements an AGGREGATE adapter for the Gemini CLI.
//
// Gemini CLI writes per-turn telemetry records under <data-dir>/*.json and
// *.jsonl. The records for a single turn are CUMULATIVE: each new record for a
// given id re-emits the turn's growing running totals, so the last record for
// an id (equivalently, the one with the largest total) holds the final figures.
// This adapter is therefore aggregate: within a single file read it groups
// records by id, keeps the max snapshot per id, and emits one AggregateSnapshot
// per (file, id). The collector compares each snapshot against the last stored
// state and appends a positive delta as an immutable event, so a turn's final
// total is never under-captured by a mid-stream poll and survives later file
// deletion.
//
// Token mapping (per plan section 1):
//
//	Input         = (tokens.input + tokens.tool) − cached overlap   (clamped >= 0)
//	Output        = tokens.output
//	CacheRead     = tokens.cached
//	CacheCreation = 0
//	Reasoning     = tokens.thoughts
//	Total         = tokens.total if present, else input+output+thoughts
//	                (cached is EXCLUDED from the authoritative total)
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY and never written,
// locked, or modified.
package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
)

const (
	// toolID is the stable tool identifier. There is no model.ToolGemini const,
	// so the literal "gemini" is used (the integration plan reserves this id).
	toolID = "gemini"
	// dataDirEnv may hold a comma-separated list of data directories. When set,
	// it REPLACES the default ~/.gemini/tmp root.
	dataDirEnv = "GEMINI_DATA_DIR"
	// metaProject labels every Gemini turn (Gemini CLI records no cwd here).
	metaProject = "gemini"
	// maxLineBytes bounds a single JSONL line so a pathological file cannot
	// exhaust memory.
	maxLineBytes = 8 << 20 // 8 MiB
)

// Adapter reads Gemini CLI telemetry files. Read-only.
type Adapter struct{}

// New returns a Gemini adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return toolID }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Gemini CLI" }

// roots returns the configured data directories. GEMINI_DATA_DIR (comma list)
// replaces the default when set; otherwise the discovery root (override or
// ~/.gemini/tmp).
func (a Adapter) roots(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(dataDirEnv)); env != "" {
		var out []string
		for _, p := range strings.Split(env, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	def := ""
	if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".gemini", "tmp")
	}
	return []string{cfg.Root(toolID, def)}
}

// Discover recurses each data directory for *.json and *.jsonl files.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, root := range a.roots(cfg) {
		if root == "" || !isDir(root) {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if d.IsDir() {
				return nil
			}
			if !hasUsageExt(path) {
				return nil
			}
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}
			srcs = append(srcs, adapter.Source{
				Tool:  toolID,
				Class: model.Aggregate,
				Path:  path,
				Label: "Gemini turns: " + path,
				Meta:  map[string]string{"root": root},
			})
			return nil
		})
	}
	return srcs, nil
}

// Collect reads a single file and emits one AggregateSnapshot per (file, id),
// taking the max (final) cumulative snapshot per id. Malformed records are
// skipped; a non-fatal error is returned describing how many were skipped.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	recs, skipped, err := readRecords(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("gemini: read %s: %w", src.Path, err)
	}

	now := time.Now().UTC()
	// Group by id, keeping the record with the largest total per id.
	best := make(map[string]rawRecord)
	var order []string
	for _, r := range recs {
		cur, ok := best[r.ID]
		if !ok {
			order = append(order, r.ID)
		}
		if !ok || r.total() >= cur.total() {
			best[r.ID] = r
		}
	}

	var snaps []model.AggregateSnapshot
	for _, id := range order {
		snap, ok := toSnapshot(best[id], src.Path, now)
		if !ok {
			// Not malformed: session-header records, user turns and $set mutation
			// entries (e.g. {"$set":{"lastUpdated":...}}) carry no token usage and
			// are dropped silently. Only genuinely unparseable lines (counted by
			// readRecords) are reported below.
			continue
		}
		snaps = append(snaps, snap)
	}

	if skipped > 0 {
		return adapter.Observation{Snapshots: snaps}, fmt.Errorf("gemini: skipped %d unparseable record(s) in %s", skipped, src.Path)
	}
	return adapter.Observation{Snapshots: snaps}, nil
}

// tokenBlock is the per-turn token breakdown emitted by Gemini CLI.
type tokenBlock struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

// rawRecord is a single decoded Gemini telemetry record. A $set wrapper (used
// by some Gemini sinks) is unwrapped before decoding into this shape.
type rawRecord struct {
	ID        string     `json:"id"`
	Model     string     `json:"model"`
	Type      string     `json:"type"`
	SessionID string     `json:"sessionId"`
	Timestamp string     `json:"timestamp"`
	Tokens    tokenBlock `json:"tokens"`
	raw       string     // original JSON for audit
}

// total returns the record's reported provider total when present, else the
// derived total (input+output+thoughts). Used to pick the max snapshot per id.
func (r rawRecord) total() int64 {
	if r.Tokens.Total > 0 {
		return r.Tokens.Total
	}
	return nonNeg(r.Tokens.Input) + nonNeg(r.Tokens.Output) + nonNeg(r.Tokens.Thoughts)
}

// $setWrapper captures the optional `{"$set": {...}}` envelope.
type setWrapper struct {
	Set json.RawMessage `json:"$set"`
}

// messagesBlob is the best-effort `messages[]` shape: each message may carry
// its own usage/tokens block. Records here are treated like top-level records.
type messagesBlob struct {
	Messages []rawRecord `json:"messages"`
}

// readRecords parses a *.json or *.jsonl file into decoded records. JSONL is
// parsed line-by-line; .json is parsed as a single value that may be an object,
// an array, a {messages:[...]} blob, or a {$set:{...}} wrapper. Returns the
// records, the count of unparseable lines/entries skipped, and a fatal error
// only when the file itself cannot be opened.
func readRecords(path string) ([]rawRecord, int, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var recs []rawRecord
	var skipped int

	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			rs, ok := decodeValue([]byte(line))
			if !ok {
				skipped++
				continue
			}
			recs = append(recs, rs...)
		}
		// A scan error (e.g. an over-long line) is non-fatal: keep what we have.
		return recs, skipped, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	rs, ok := decodeValue(data)
	if !ok {
		skipped++
		return recs, skipped, nil
	}
	recs = append(recs, rs...)
	return recs, skipped, nil
}

// decodeValue decodes a single JSON value (object, array, messages blob, or
// $set wrapper) into zero or more records. Returns ok=false when the bytes are
// not valid JSON at all.
func decodeValue(data []byte) ([]rawRecord, bool) {
	trimmed := trimLeadingSpace(data)
	if len(trimmed) == 0 {
		return nil, false
	}
	switch trimmed[0] {
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, false
		}
		var out []rawRecord
		for _, e := range arr {
			if rs, ok := decodeValue(e); ok {
				out = append(out, rs...)
			}
		}
		return out, true
	case '{':
		return decodeObject(data), true
	default:
		return nil, false
	}
}

// decodeObject decodes a JSON object that may be a plain record, a $set
// wrapper, or a {messages:[...]} blob. A single object never reports a parse
// failure for the caller's skip accounting (it is best-effort); empty/zero
// records are filtered later in toSnapshot.
func decodeObject(data []byte) []rawRecord {
	// Unwrap a $set envelope first.
	var w setWrapper
	if err := json.Unmarshal(data, &w); err == nil && len(w.Set) > 0 {
		if rs, ok := decodeValue(w.Set); ok {
			return rs
		}
	}

	// Best-effort messages[] blob: collect any messages carrying tokens.
	var mb messagesBlob
	if err := json.Unmarshal(data, &mb); err == nil && len(mb.Messages) > 0 {
		var out []rawRecord
		for _, m := range mb.Messages {
			m.raw = "" // per-message raw is the parent blob; omit to keep small
			out = append(out, m)
		}
		// A messages blob may ALSO carry top-level tokens (a stats summary);
		// include it too so a summary line is not lost.
		var top rawRecord
		if err := json.Unmarshal(data, &top); err == nil && top.Tokens != (tokenBlock{}) {
			top.raw = string(data)
			out = append(out, top)
		}
		return out
	}

	var r rawRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	r.raw = string(data)
	return []rawRecord{r}
}

// toSnapshot maps a decoded record onto an AggregateSnapshot. Returns ok=false
// for all-zero (no usage) records, which are dropped per the spec.
func toSnapshot(r rawRecord, sourcePath string, now time.Time) (model.AggregateSnapshot, bool) {
	in := nonNeg(r.Tokens.Input)
	out := nonNeg(r.Tokens.Output)
	cached := nonNeg(r.Tokens.Cached)
	thoughts := nonNeg(r.Tokens.Thoughts)
	toolTok := nonNeg(r.Tokens.Tool)
	reported := nonNeg(r.Tokens.Total)

	// Input = (input + tool) minus the cached overlap (cached is reported
	// separately and is a subset of the prompt that was served from cache).
	inputAdj := in + toolTok - cached
	if inputAdj < 0 {
		inputAdj = 0
	}

	// Authoritative total: provider total when present, else input+output+
	// thoughts. Cached is EXCLUDED from the total (verified against Gemini's
	// own `total` field, which excludes cached read tokens).
	total := reported
	if total == 0 {
		total = in + out + thoughts
	}

	// Drop all-zero records (no usage to report).
	if inputAdj == 0 && out == 0 && cached == 0 && thoughts == 0 && total == 0 {
		return model.AggregateSnapshot{}, false
	}

	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = "turn" // a record with no id still represents one turn in this file
	}

	session := strings.TrimSpace(r.SessionID)
	if session == "" {
		session = fileStem(sourcePath)
	}

	obs := now
	if ts := parseTime(r.Timestamp); !ts.IsZero() {
		obs = ts
	}

	return model.AggregateSnapshot{
		Tool:                toolID,
		Key:                 sourcePath + "|" + id,
		Model:               strings.TrimSpace(r.Model),
		SessionID:           session,
		Project:             metaProject,
		ObservedTime:        obs,
		InputTokens:         inputAdj,
		OutputTokens:        out,
		CacheCreationTokens: 0,
		CacheReadTokens:     cached,
		ReasoningTokens:     thoughts,
		TotalTokens:         total,
		SourcePath:          sourcePath,
		Raw:                 r.raw,
	}, true
}

// parseTime tries RFC3339 (with and without nanoseconds) and returns the zero
// time when the stamp is empty or unparseable.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// trimLeadingSpace drops leading ASCII whitespace without allocating.
func trimLeadingSpace(b []byte) []byte {
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			b = b[1:]
		default:
			return b
		}
	}
	return b
}

// fileStem returns the file name without directory or extension.
func fileStem(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// hasUsageExt reports whether a path ends in .json or .jsonl.
func hasUsageExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".json" || ext == ".jsonl"
}

// nonNeg clamps a possibly-negative counter to zero.
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
