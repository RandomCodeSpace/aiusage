// Package claudecode implements the event-level adapter for the Claude Code
// CLI. It reads append-only JSONL transcripts under each config root's
// projects/ tree, maps Anthropic-style usage (cache tokens additive), and
// deduplicates message replays (including sidechain replays) within a cycle.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY; nothing under the
// agent's directories is created, locked, or modified.
package claudecode

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
)

// usageMarker fast-skips lines that cannot carry usage data.
const usageMarker = `"usage":{`

// syntheticModel is dropped per the parsing spec.
const syntheticModel = "<synthetic>"

// Adapter reads Claude Code usage transcripts.
type Adapter struct{}

// New returns a Claude Code adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolClaudeCode }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Claude Code" }

// Discover locates Claude Code config roots that contain a projects/ tree.
//
// Resolution order:
//  1. An explicit override (DiscoverConfig.Overrides[claude-code]), normalised.
//  2. env CLAUDE_CONFIG_DIR (comma list), each entry normalised.
//  3. BOTH <home>/.config/claude and <home>/.claude.
//
// A path ending in /projects is normalised to its parent. A root is accepted
// only if <root>/projects/ exists as a directory. Results are path-deduped.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	var candidates []string

	if cfg.Overrides != nil {
		if v := strings.TrimSpace(cfg.Overrides[model.ToolClaudeCode]); v != "" {
			candidates = append(candidates, splitRoots(v)...)
		}
	}
	if len(candidates) == 0 {
		if env := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); env != "" {
			candidates = append(candidates, splitRoots(env)...)
		}
	}
	if len(candidates) == 0 && cfg.Home != "" {
		candidates = append(candidates,
			filepath.Join(cfg.Home, ".config", "claude"),
			filepath.Join(cfg.Home, ".claude"),
		)
	}

	seen := make(map[string]struct{})
	var sources []adapter.Source
	for _, c := range candidates {
		root := normaliseRoot(c)
		if root == "" {
			continue
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		if _, dup := seen[root]; dup {
			continue
		}
		projDir := filepath.Join(root, "projects")
		if fi, err := os.Stat(projDir); err != nil || !fi.IsDir() {
			continue
		}
		seen[root] = struct{}{}
		sources = append(sources, adapter.Source{
			Tool:  model.ToolClaudeCode,
			Class: model.EventLevel,
			Path:  root,
			Label: "Claude Code: " + root,
			Meta:  map[string]string{"projects": projDir},
		})
	}
	return sources, nil
}

// splitRoots splits a comma-separated env/override value into trimmed entries.
func splitRoots(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normaliseRoot collapses a trailing /projects segment to the parent root.
func normaliseRoot(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	if filepath.Base(clean) == "projects" {
		return filepath.Dir(clean)
	}
	return clean
}

// Collect walks <root>/projects/**/*.jsonl, parses every usage line, applies
// in-cycle dedup, and returns the surviving events. Per-file errors are skipped
// so one bad transcript never fails the cycle.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	projDir := filepath.Join(src.Path, "projects")
	d := newDeduper()

	_ = filepath.WalkDir(projDir, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees, keep walking
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			return nil
		}
		segment := projectSegment(projDir, path)
		parseFile(path, segment, d)
		return nil
	})

	return adapter.Observation{Events: d.events()}, nil
}

// projectSegment returns the immediate projects/<segment> directory name used
// as the fallback project when a line carries no cwd.
func projectSegment(projDir, file string) string {
	rel, err := filepath.Rel(projDir, file)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// parseFile reads one JSONL transcript line-by-line, feeding parsed candidates
// into the deduper. Malformed lines are skipped.
func parseFile(path, segment string, d *deduper) {
	f, err := os.Open(path) // O_RDONLY
	if err != nil {
		return
	}
	defer f.Close()

	sessionFromName := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long JSONL lines

	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), usageMarker) {
			continue
		}
		cand, ok := parseLine(line, path, segment, sessionFromName)
		if ok {
			d.add(cand)
		}
	}
}

// rawLine models both serde-untagged shapes: Direct fields at the top level and
// the AgentProgress wrapper under data. Pointer fields distinguish JSON null
// (present, set) from absent (nil).
type rawLine struct {
	Timestamp         *string  `json:"timestamp"`
	CWD               *string  `json:"cwd"`
	SessionID         *string  `json:"sessionId"`
	RequestID         *string  `json:"requestId"`
	IsSidechain       *bool    `json:"isSidechain"`
	IsAPIErrorMessage *bool    `json:"isApiErrorMessage"`
	Version           *string  `json:"version"`
	Message           *message `json:"message"`
	Data              *struct {
		Message *message `json:"message"`
	} `json:"data"`
}

type message struct {
	ID    *string `json:"id"`
	Model *string `json:"model"`
	Usage *usage  `json:"usage"`
}

type usage struct {
	InputTokens         *int64  `json:"input_tokens"`
	OutputTokens        *int64  `json:"output_tokens"`
	CacheCreationTokens *int64  `json:"cache_creation_input_tokens"`
	CacheReadTokens     *int64  `json:"cache_read_input_tokens"`
	Speed               *string `json:"speed"`
	CostUSD             *float64
}

// candidate is a parsed usage record awaiting dedup.
type candidate struct {
	event       model.UsageEvent
	messageID   string
	requestID   string
	isSidechain bool
	hasSpeed    bool
	cost        float64
	total       int64
}

// parseLine decodes one transcript line into a candidate. It returns ok=false
// to skip the line (malformed JSON, null in a guarded key, missing usage, or an
// empty/synthetic model).
func parseLine(line []byte, path, segment, sessionFromName string) (candidate, bool) {
	var rl rawLine
	if err := json.Unmarshal(line, &rl); err != nil {
		return candidate{}, false
	}

	// Flatten AgentProgress (data.message) onto the Direct shape.
	msg := rl.Message
	if msg == nil && rl.Data != nil {
		msg = rl.Data.Message
	}
	if msg == nil || msg.Usage == nil {
		return candidate{}, false
	}
	u := msg.Usage

	// Reject lines where any guarded key is explicitly JSON null. costUSD is
	// guarded as a top-level field; decode it separately to detect null.
	if hasNullGuardedKey(line) {
		return candidate{}, false
	}
	// id / model / sessionId / requestId / isApiErrorMessage null guards via
	// the decoded structure (a present-but-null pointer is nil; absence is also
	// nil, so we additionally consult the raw-null scan above for those keys).

	// Model: drop <synthetic>; append -fast for fast speed; empty rejects.
	modelID := ""
	if msg.Model != nil {
		modelID = strings.TrimSpace(*msg.Model)
	}
	if modelID == "" || modelID == syntheticModel {
		return candidate{}, false
	}
	if u.Speed != nil && *u.Speed == "fast" {
		modelID += "-fast"
	}

	in := deref(u.InputTokens)
	out := deref(u.OutputTokens)
	cacheC := deref(u.CacheCreationTokens)
	cacheR := deref(u.CacheReadTokens)
	total := in + out + cacheC + cacheR // cache additive; no reasoning

	project := segment
	if rl.CWD != nil && strings.TrimSpace(*rl.CWD) != "" {
		project = *rl.CWD
	}

	session := sessionFromName
	if rl.SessionID != nil && strings.TrimSpace(*rl.SessionID) != "" {
		session = *rl.SessionID
	}

	var eventTime time.Time
	if rl.Timestamp != nil {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*rl.Timestamp)); err == nil {
			eventTime = t.UTC()
		}
	}

	messageID := ""
	if msg.ID != nil {
		messageID = *msg.ID
	}
	requestID := ""
	if rl.RequestID != nil {
		requestID = *rl.RequestID
	}
	isSidechain := rl.IsSidechain != nil && *rl.IsSidechain

	dedupKey := persistedKey(messageID, path, line)

	ev := model.UsageEvent{
		Tool:                model.ToolClaudeCode,
		Model:               modelID,
		SessionID:           session,
		Project:             project,
		EventTime:           eventTime,
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cacheC,
		CacheReadTokens:     cacheR,
		ReasoningTokens:     0,
		TotalTokens:         total,
		RequestID:           requestID,
		MessageID:           messageID,
		SourcePath:          path,
		DedupKey:            dedupKey,
		Kind:                model.KindUsage,
		Raw:                 rawUsage(line),
	}

	return candidate{
		event:       ev,
		messageID:   messageID,
		requestID:   requestID,
		isSidechain: isSidechain,
		hasSpeed:    u.Speed != nil,
		cost:        cost(line),
		total:       total,
	}, true
}

// guardedNullKeys are the keys whose explicit JSON null causes the line to be
// skipped per the parsing spec.
var guardedNullKeys = []string{
	"id", "cwd", "model", "speed", "costUSD", "version",
	"sessionId", "requestId", "isApiErrorMessage",
	"cache_read_input_tokens", "cache_creation_input_tokens",
}

// hasNullGuardedKey reports whether the raw line sets any guarded key to JSON
// null. It walks the decoded generic structure so nested occurrences (e.g. keys
// inside message / usage / data) are also caught.
func hasNullGuardedKey(line []byte) bool {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		return false
	}
	guard := make(map[string]struct{}, len(guardedNullKeys))
	for _, k := range guardedNullKeys {
		guard[k] = struct{}{}
	}
	return walkForNull(v, guard)
}

// walkForNull recursively reports whether any guarded key maps to a nil value.
func walkForNull(v any, guard map[string]struct{}) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if _, ok := guard[k]; ok && child == nil {
				return true
			}
			if walkForNull(child, guard) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if walkForNull(child, guard) {
				return true
			}
		}
	}
	return false
}

// persistedKey returns the stable cross-poll dedup key. With a message id it is
// "claude-code|<id>"; otherwise "claude-code|<sourcePath>|<sha1(line)>" which is
// effectively never deduped (matches ccusage).
func persistedKey(messageID, path string, line []byte) string {
	if messageID != "" {
		return model.ToolClaudeCode + "|" + messageID
	}
	sum := sha1.Sum(line)
	return fmt.Sprintf("%s|%s|%x", model.ToolClaudeCode, path, sum)
}

// rawUsage extracts the usage object substring for audit, falling back to the
// whole line. Best-effort; never fails the parse.
func rawUsage(line []byte) string {
	s := string(line)
	if i := strings.Index(s, usageMarker); i >= 0 {
		return s
	}
	return s
}

// cost decodes the optional top-level costUSD for tie-breaking. Missing/null
// yields 0.
func cost(line []byte) float64 {
	var probe struct {
		CostUSD *float64 `json:"costUSD"`
	}
	if err := json.Unmarshal(line, &probe); err == nil && probe.CostUSD != nil {
		return *probe.CostUSD
	}
	return 0
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	if *p < 0 {
		return 0
	}
	return *p
}
