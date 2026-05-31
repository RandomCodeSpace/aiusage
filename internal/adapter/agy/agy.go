// Package agy implements an AGGREGATE adapter for the Antigravity CLI.
//
// Antigravity is Gemini-based and is expected to reuse the Gemini CLI telemetry
// shape once it emits usage. This adapter scans the canonical Antigravity data
// directories for usage-bearing *.json / *.jsonl files modelled on the Gemini
// shape, groups records by id within a file, keeps the max (final) cumulative
// snapshot per id, and emits one AggregateSnapshot per (file, id) tagged
// tool="agy" so it is never confused with the Gemini CLI.
//
// TODO(agy): the live Antigravity install is unauthenticated and emits NO token
// usage today — the `.pb` conversation blobs carry content only, no token
// fields. Until a logged-in session runs, Discover finds no usage-bearing files
// and returns an empty source list with no error (the current real state).
// Once Antigravity is authenticated and a session has run, re-inspect
// ~/.gemini/antigravity-cli/ for the real usage file/schema and finalise the
// parser (likely a `.pb`/usage file rather than the Gemini JSON shape probed
// here).
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY and never written,
// locked, or modified.
package agy

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
	// metaProject labels every Antigravity turn (no cwd is recorded here).
	metaProject = "agy"
	// maxLineBytes bounds a single JSONL line so a pathological file cannot
	// exhaust memory.
	maxLineBytes = 8 << 20 // 8 MiB
)

// candidateDirs are the Antigravity data roots, relative to the user's home,
// probed for usage-bearing files.
var candidateDirs = [][]string{
	{".gemini", "antigravity-cli"},
	{".antigravitycli"},
	{".cache", "antigravity"},
}

// Adapter reads Antigravity CLI telemetry files. Read-only.
type Adapter struct{}

// New returns an Antigravity adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolAgy }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Antigravity" }

// roots returns the Antigravity data directories to scan: an explicit override
// when present, otherwise the canonical home-relative candidates.
func (a Adapter) roots(cfg adapter.DiscoverConfig) []string {
	if cfg.Overrides != nil {
		if v := strings.TrimSpace(cfg.Overrides[model.ToolAgy]); v != "" {
			return []string{v}
		}
	}
	if cfg.Home == "" {
		return nil
	}
	out := make([]string, 0, len(candidateDirs))
	for _, parts := range candidateDirs {
		out = append(out, filepath.Join(append([]string{cfg.Home}, parts...)...))
	}
	return out
}

// Discover scans each Antigravity root for *.json / *.jsonl files that actually
// carry token usage. Files without a usable token block are ignored, so an
// unauthenticated install (which emits content-only blobs) yields an empty
// source list and no error.
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
			// Only surface files that genuinely carry token usage. This is what
			// keeps a content-only (unauthenticated) install from producing
			// spurious empty sources.
			if !fileHasUsage(path) {
				return nil
			}
			seen[path] = struct{}{}
			srcs = append(srcs, adapter.Source{
				Tool:  model.ToolAgy,
				Class: model.Aggregate,
				Path:  path,
				Label: "Antigravity turns: " + path,
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
		return adapter.Observation{}, fmt.Errorf("agy: read %s: %w", src.Path, err)
	}

	now := time.Now().UTC()
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
			// Non-usage records (headers, user turns, $set mutations) are dropped
			// silently; only genuinely unparseable lines (counted in readRecords)
			// are reported below.
			continue
		}
		snaps = append(snaps, snap)
	}

	if skipped > 0 {
		return adapter.Observation{Snapshots: snaps}, fmt.Errorf("agy: skipped %d unparseable record(s) in %s", skipped, src.Path)
	}
	return adapter.Observation{Snapshots: snaps}, nil
}

// tokenBlock is the per-turn token breakdown (Gemini-shaped).
type tokenBlock struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

// rawRecord is a single decoded Antigravity telemetry record (Gemini-shaped).
type rawRecord struct {
	ID        string     `json:"id"`
	Model     string     `json:"model"`
	Type      string     `json:"type"`
	SessionID string     `json:"sessionId"`
	Timestamp string     `json:"timestamp"`
	Tokens    tokenBlock `json:"tokens"`
	raw       string
}

// total returns the record's reported provider total when present, else the
// derived total (input+output+thoughts).
func (r rawRecord) total() int64 {
	if r.Tokens.Total > 0 {
		return r.Tokens.Total
	}
	return nonNeg(r.Tokens.Input) + nonNeg(r.Tokens.Output) + nonNeg(r.Tokens.Thoughts)
}

// hasTokens reports whether a record carries any non-zero token field.
func (r rawRecord) hasTokens() bool {
	t := r.Tokens
	return t.Input != 0 || t.Output != 0 || t.Cached != 0 || t.Thoughts != 0 || t.Tool != 0 || t.Total != 0
}

type setWrapper struct {
	Set json.RawMessage `json:"$set"`
}

type messagesBlob struct {
	Messages []rawRecord `json:"messages"`
}

// fileHasUsage reports whether a file contains at least one record with a
// non-zero token field. Used by Discover to ignore content-only blobs.
func fileHasUsage(path string) bool {
	recs, _, err := readRecords(path)
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.hasTokens() {
			return true
		}
	}
	return false
}

// readRecords parses a *.json or *.jsonl file into decoded records.
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
// $set wrapper) into zero or more records.
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
// wrapper, or a {messages:[...]} blob.
func decodeObject(data []byte) []rawRecord {
	var w setWrapper
	if err := json.Unmarshal(data, &w); err == nil && len(w.Set) > 0 {
		if rs, ok := decodeValue(w.Set); ok {
			return rs
		}
	}

	var mb messagesBlob
	if err := json.Unmarshal(data, &mb); err == nil && len(mb.Messages) > 0 {
		var out []rawRecord
		for _, m := range mb.Messages {
			m.raw = ""
			out = append(out, m)
		}
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

// toSnapshot maps a decoded record onto an AggregateSnapshot tagged tool="agy".
// Returns ok=false for all-zero (no usage) records, which are dropped.
func toSnapshot(r rawRecord, sourcePath string, now time.Time) (model.AggregateSnapshot, bool) {
	in := nonNeg(r.Tokens.Input)
	out := nonNeg(r.Tokens.Output)
	cached := nonNeg(r.Tokens.Cached)
	thoughts := nonNeg(r.Tokens.Thoughts)
	toolTok := nonNeg(r.Tokens.Tool)
	reported := nonNeg(r.Tokens.Total)

	inputAdj := in + toolTok - cached
	if inputAdj < 0 {
		inputAdj = 0
	}

	total := reported
	if total == 0 {
		total = in + out + thoughts
	}

	if inputAdj == 0 && out == 0 && cached == 0 && thoughts == 0 && total == 0 {
		return model.AggregateSnapshot{}, false
	}

	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = "turn"
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
		Tool:                model.ToolAgy,
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

// parseTime tries RFC3339 (with and without nanoseconds).
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
