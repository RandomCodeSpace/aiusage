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
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// usageMarker fast-skips lines that cannot carry usage data.
const usageMarker = `"usage":{`

// usageMarkerBytes lets the scanner loop probe with bytes.Contains instead of
// copying every line to a string first.
var usageMarkerBytes = []byte(usageMarker)

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
		// The no-message-id dedup fallback embeds transcript paths, so resolve
		// symlinks: a re-pointed root would otherwise mint new keys for those
		// lines. A genuinely moved root still re-adds them once — irreducible
		// without state migration.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
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

// fileStamp is one manifest entry: the size+mtime of a transcript file at the
// last completed read.
type fileStamp struct {
	Size    int64 `json:"size"`
	MTimeNS int64 `json:"mtime"`
}

// Collect walks <root>/projects/**/*.jsonl, parses every usage line, applies
// in-cycle dedup, and returns the surviving events. Per-file errors are skipped
// so one bad transcript never fails the cycle.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the whole root on a per-file size+mtime manifest:
// when no transcript under projects/ changed, the walk is stats only and no
// file is opened. Any change re-parses EVERY file — the deduper's sidechain
// consolidation spans all files of a root in one pass, so per-file tail reads
// would break cross-file dedup. Correctness of the full re-parse is carried by
// the persisted dedup keys (INSERT OR IGNORE collapses re-derived events).
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	projDir := filepath.Join(src.Path, "projects")

	type entry struct {
		path    string
		segment string
	}
	var (
		files    []entry
		manifest = make(map[string]fileStamp)
		statErr  bool
	)
	walkErr := filepath.WalkDir(projDir, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees, keep walking
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			return nil
		}
		if !adapter.WalkEntryIsFile(de, path) {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			// A file we cannot stat cannot be gated; parse unconditionally and
			// withhold the checkpoint so the next cycle re-examines it.
			statErr = true
		} else {
			manifest[path] = fileStamp{Size: fi.Size(), MTimeNS: fi.ModTime().UnixNano()}
		}
		files = append(files, entry{path: path, segment: projectSegment(projDir, path)})
		return nil
	})
	if walkErr != nil {
		return adapter.Observation{}, walkErr // only ctx cancellation escapes the walk
	}

	if cp != nil && !statErr && manifestUnchanged(cp.State, manifest) {
		return adapter.Observation{}, nil // nothing changed: no parse, keep stored checkpoint
	}

	d := newDeduper()
	parseErr := false
	for _, e := range files {
		if ctx.Err() != nil {
			return adapter.Observation{Events: d.events()}, ctx.Err()
		}
		if err := parseFile(e.path, e.segment, d); err != nil {
			// A failed or partial parse must not land in the manifest: the gate
			// would skip the unread content until some file under the root next
			// changes. Withhold the checkpoint so the next cycle re-reads.
			parseErr = true
		}
	}

	obs := adapter.Observation{Events: d.events()}
	if statErr || parseErr {
		return obs, nil
	}
	stateJSON, err := json.Marshal(manifest)
	if err != nil {
		return obs, nil
	}
	obs.Checkpoint = &model.SourceCheckpoint{
		Tool: model.ToolClaudeCode, SourcePath: src.Path, State: string(stateJSON),
	}
	return obs, nil
}

// manifestUnchanged reports whether the stored manifest JSON matches the
// current file set exactly (same paths, sizes, mtimes — additions, removals
// and edits all break equality).
func manifestUnchanged(stored string, current map[string]fileStamp) bool {
	if stored == "" {
		return false
	}
	var prev map[string]fileStamp
	if err := json.Unmarshal([]byte(stored), &prev); err != nil {
		return false
	}
	if len(prev) != len(current) {
		return false
	}
	for path, st := range current {
		if p, ok := prev[path]; !ok || p != st {
			return false
		}
	}
	return true
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
// into the deduper. Malformed lines are skipped. A non-nil error (open failure
// or scanner error) means the read did not complete; the caller must not
// checkpoint the root.
func parseFile(path, segment string, d *deduper) error {
	f, err := os.Open(path) // O_RDONLY
	if err != nil {
		return err
	}
	defer f.Close()

	sessionFromName := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long JSONL lines

	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, usageMarkerBytes) {
			continue
		}
		cand, ok := parseLine(line, path, segment, sessionFromName)
		if ok {
			d.add(cand)
		}
	}
	return sc.Err()
}

// nullable wraps an optional JSON scalar, distinguishing an absent key (zero
// value), an explicit JSON null (Null), and a decoded value — all recorded
// during the single typed decode pass, replacing the former generic re-parse
// of every line just to find guarded nulls. A wrong-typed value fails the
// enclosing Unmarshal, matching the previous pointer-field behaviour.
type nullable[T any] struct {
	Present bool
	Null    bool
	Value   T
}

func (n *nullable[T]) UnmarshalJSON(b []byte) error {
	n.Present = true
	if string(b) == "null" {
		n.Null = true
		return nil
	}
	return json.Unmarshal(b, &n.Value)
}

// rawLine models both serde-untagged shapes: Direct fields at the top level and
// the AgentProgress wrapper under data. Guarded keys use nullable so a JSON
// null is detected in the same pass.
type rawLine struct {
	Timestamp         *string           `json:"timestamp"`
	CWD               nullable[string]  `json:"cwd"`
	SessionID         nullable[string]  `json:"sessionId"`
	RequestID         nullable[string]  `json:"requestId"`
	IsSidechain       *bool             `json:"isSidechain"`
	IsAPIErrorMessage nullable[bool]    `json:"isApiErrorMessage"`
	Version           nullable[string]  `json:"version"`
	CostUSD           nullable[float64] `json:"costUSD"`
	Message           *message          `json:"message"`
	Data              *struct {
		Message *message `json:"message"`
	} `json:"data"`
}

type message struct {
	ID    nullable[string] `json:"id"`
	Model nullable[string] `json:"model"`
	Usage *usage           `json:"usage"`
}

type usage struct {
	InputTokens         *int64           `json:"input_tokens"`
	OutputTokens        *int64           `json:"output_tokens"`
	CacheCreationTokens nullable[int64]  `json:"cache_creation_input_tokens"`
	CacheReadTokens     nullable[int64]  `json:"cache_read_input_tokens"`
	Speed               nullable[string] `json:"speed"`
	// Enrichment-only fields. They are NOT guarded keys: an absent, null or
	// unexpected value must leave the line's stored accounting untouched, so
	// both decode leniently rather than failing the enclosing Unmarshal.
	ServiceTier   lenientString  `json:"service_tier"`
	CacheCreation *cacheCreation `json:"cache_creation"`
}

// lenientString decodes a JSON string and treats every other shape (null,
// number, object) as the empty string. Enrichment must never drop a line that
// parsed before it existed.
type lenientString string

func (s *lenientString) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err == nil {
		*s = lenientString(v)
	}
	return nil
}

// cacheCreation is Anthropic's per-TTL split of cache_creation_input_tokens.
// The two lifetimes are priced differently, so the pricing stamp reads the
// split transiently; the ledger keeps only the combined count.
type cacheCreation struct {
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

// UnmarshalJSON tolerates any shape for the same reason as lenientString.
func (c *cacheCreation) UnmarshalJSON(b []byte) error {
	type plain cacheCreation
	var v plain
	if err := json.Unmarshal(b, &v); err == nil {
		*c = cacheCreation(v)
	}
	return nil
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

	// Reject lines where any guarded key is explicitly JSON null, recorded by
	// the nullable fields during the same decode pass.
	if hasGuardedNull(&rl, msg) {
		return candidate{}, false
	}

	// Model: drop <synthetic>; append -fast for fast speed; empty rejects.
	modelID := strings.TrimSpace(msg.Model.Value)
	if modelID == "" || modelID == syntheticModel {
		return candidate{}, false
	}
	if u.Speed.Value == "fast" {
		modelID += "-fast"
	}

	in := deref(u.InputTokens)
	out := deref(u.OutputTokens)
	cacheC := adapter.NonNeg(u.CacheCreationTokens.Value)
	cacheR := adapter.NonNeg(u.CacheReadTokens.Value)
	total := in + out + cacheC + cacheR // cache additive; no reasoning

	project := segment
	if strings.TrimSpace(rl.CWD.Value) != "" {
		project = rl.CWD.Value
	}

	session := sessionFromName
	if strings.TrimSpace(rl.SessionID.Value) != "" {
		session = rl.SessionID.Value
	}

	var eventTime time.Time
	if rl.Timestamp != nil {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*rl.Timestamp)); err == nil {
			eventTime = t.UTC()
		}
	}

	messageID := msg.ID.Value
	requestID := rl.RequestID.Value
	isSidechain := rl.IsSidechain != nil && *rl.IsSidechain

	dedupKey := persistedKey(messageID, path, line)

	// Cache-write TTL split: transient pricing input, never stored. When the
	// usage block omits cache_creation the split stays zero and the pricing
	// engine falls back to the 5m rate for the whole cache write.
	var ttl model.CacheWriteTTL
	if u.CacheCreation != nil {
		ttl.Ephemeral5m = adapter.NonNeg(u.CacheCreation.Ephemeral5m)
		ttl.Ephemeral1h = adapter.NonNeg(u.CacheCreation.Ephemeral1h)
	}

	ev := model.UsageEvent{
		Tool:                model.ToolClaudeCode,
		Model:               modelID,
		Provider:            model.ProviderAnthropic,
		ServiceTier:         strings.TrimSpace(string(u.ServiceTier)),
		SessionID:           session,
		Project:             project,
		EventTime:           eventTime,
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cacheC,
		CacheReadTokens:     cacheR,
		ReasoningTokens:     0,
		TotalTokens:         total,
		CacheTTL:            ttl,
		RequestID:           requestID,
		MessageID:           messageID,
		SourcePath:          path,
		DedupKey:            dedupKey,
		Kind:                model.KindUsage,
		Raw:                 auditPayload(&rl, msg),
	}

	return candidate{
		event:       ev,
		messageID:   messageID,
		requestID:   requestID,
		isSidechain: isSidechain,
		hasSpeed:    u.Speed.Present,
		cost:        rl.CostUSD.Value,
		total:       total,
	}, true
}

// hasGuardedNull reports whether any guarded key — id, cwd, model, speed,
// costUSD, version, sessionId, requestId, isApiErrorMessage and the cache
// token counts — is explicitly JSON null at its spec position (top level,
// message, usage). Such lines are skipped per the parsing spec. msg and
// msg.Usage are non-nil by the time this runs.
func hasGuardedNull(rl *rawLine, msg *message) bool {
	if rl.CWD.Null || rl.SessionID.Null || rl.RequestID.Null ||
		rl.IsAPIErrorMessage.Null || rl.Version.Null || rl.CostUSD.Null {
		return true
	}
	if msg.ID.Null || msg.Model.Null {
		return true
	}
	u := msg.Usage
	return u.Speed.Null || u.CacheCreationTokens.Null || u.CacheReadTokens.Null
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

// auditLine is the ALLOW-LIST of transcript fields persisted in
// UsageEvent.Raw: the token counters an audit of the stored accounting needs,
// plus the identifiers that tie the row back to its provider request. It
// deliberately mirrors the transcript's own nesting so an auditor can compare
// it against the source line field for field.
//
// Everything else the line carries — message content, cwd, tool results, the
// user's prompt — is never read into this shape, so it cannot reach the ledger
// (issue #17). This is an allow-list on purpose: stripping known-bad keys from
// the whole line would silently start leaking the day the transcript format
// grows a new content field.
type auditLine struct {
	Timestamp string       `json:"timestamp,omitempty"`
	RequestID string       `json:"requestId,omitempty"`
	Message   auditMessage `json:"message"`
}

type auditMessage struct {
	ID    string     `json:"id,omitempty"`
	Model string     `json:"model,omitempty"`
	Usage auditUsage `json:"usage"`
}

// auditUsage records the usage block as the transcript reported it: the counts
// are the provider's, not the clamped values stored in the token columns, so a
// mismatch between the two stays visible.
type auditUsage struct {
	InputTokens         *int64         `json:"input_tokens,omitempty"`
	OutputTokens        *int64         `json:"output_tokens,omitempty"`
	CacheCreationTokens *int64         `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     *int64         `json:"cache_read_input_tokens,omitempty"`
	ServiceTier         string         `json:"service_tier,omitempty"`
	Speed               string         `json:"speed,omitempty"`
	CacheCreation       *cacheCreation `json:"cache_creation,omitempty"`
}

// auditPayload builds the stored audit payload from the decoded line. Values
// are copied out of the typed decode, never sliced out of the original bytes.
// Best-effort: an un-marshalable payload yields an empty raw rather than
// failing the parse. msg and msg.Usage are non-nil by the time this runs.
func auditPayload(rl *rawLine, msg *message) string {
	u := msg.Usage
	a := auditLine{
		RequestID: rl.RequestID.Value,
		Message: auditMessage{
			ID:    msg.ID.Value,
			Model: msg.Model.Value,
			Usage: auditUsage{
				InputTokens:   u.InputTokens,
				OutputTokens:  u.OutputTokens,
				ServiceTier:   string(u.ServiceTier),
				Speed:         u.Speed.Value,
				CacheCreation: u.CacheCreation,
			},
		},
	}
	if rl.Timestamp != nil {
		a.Timestamp = *rl.Timestamp
	}
	if u.CacheCreationTokens.Present {
		v := u.CacheCreationTokens.Value
		a.Message.Usage.CacheCreationTokens = &v
	}
	if u.CacheReadTokens.Present {
		v := u.CacheReadTokens.Value
		a.Message.Usage.CacheReadTokens = &v
	}
	b, err := json.Marshal(a)
	if err != nil {
		return ""
	}
	return string(b)
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return adapter.NonNeg(*p)
}
