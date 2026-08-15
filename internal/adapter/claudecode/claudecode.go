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
	"strconv"
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

// hookMarkerBytes fast-skips to the hook records. They are type=="system"
// lines that carry no usage block at all, so the usage marker alone would skip
// every one of them.
var hookMarkerBytes = []byte(hookSummarySubtype)

// hookSummarySubtype is the only hook record Claude Code writes to a
// transcript. Verified over 1264 local transcripts: the type=="system" subtypes
// present are stop_hook_summary, turn_duration, local_command,
// scheduled_task_fire, compact_boundary, bridge_status, informational and the
// model-fallback pair — no PreToolUse/PostToolUse/SessionStart record exists in
// this format at all, so Stop is the only hook event observable here.
const hookSummarySubtype = "stop_hook_summary"

// hookEventName is what a hook row is NAMED. The transcript does not carry a
// hook name: each element of hookInfos identifies its hook only by the raw
// shell COMMAND it ran, which is exactly the tool-input content activity
// collection refuses to store. So the recorded name is the hook EVENT, which is
// the part that is a name rather than content, and the per-hook identity is
// deliberately dropped rather than smuggled in as a command string or a hash of
// one (a hash would be an identifier no human could read and no query could
// group by usefully).
const hookEventName = "Stop"

// syntheticModel is dropped per the parsing spec.
const syntheticModel = "<synthetic>"

// ConfigDirEnv names the environment variable that moves the Claude Code
// configuration root, and with it every transcript this adapter reads. It is
// exported because a caller that copies this process's discovery into another
// one - the systemd units internal/service writes - has to know that the
// environment, not the defaults, decided what gets collected.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

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
		if env := strings.TrimSpace(os.Getenv(ConfigDirEnv)); env != "" {
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
			return adapter.Observation{
				Events:        d.events(),
				Activity:      d.activity(),
				SkillContexts: d.skillContexts(),
			}, ctx.Err()
		}
		if err := parseFile(e.path, e.segment, d); err != nil {
			// A failed or partial parse must not land in the manifest: the gate
			// would skip the unread content until some file under the root next
			// changes. Withhold the checkpoint so the next cycle re-reads.
			parseErr = true
		}
	}

	obs := adapter.Observation{
		Events:        d.events(),
		Activity:      d.activity(),
		SkillContexts: d.skillContexts(),
	}
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
		if bytes.Contains(line, usageMarkerBytes) {
			cand, ok := parseLine(line, path, segment, sessionFromName)
			if ok {
				d.add(cand)
			}
			continue
		}
		// Hook records carry no usage block, so they only reach here.
		if bytes.Contains(line, hookMarkerBytes) {
			d.addHooks(parseHookLine(line, path, segment, sessionFromName))
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
	// AttributionSkill names the skill this record was produced INSIDE, when the
	// turn ran under one. It is a plain top-level string — not an object, not a
	// list — so a record cannot carry two, which is what makes per-skill cost a
	// sum with no divisor.
	//
	// It decodes leniently, and that is not cosmetic: it is enrichment, and a
	// field the ledger did without until now must never become a way to LOSE a
	// usage row. A null, a number or an object yields the empty string and the
	// record is stored exactly as it was before this field existed, with no skill
	// context emitted. It is likewise NOT a guarded key — a record without it is
	// the overwhelming majority (8,039 of 209,435 locally carry one).
	AttributionSkill lenientString `json:"attributionSkill"`
	Message          *message      `json:"message"`
	Data             *struct {
		Message *message `json:"message"`
	} `json:"data"`
}

type message struct {
	ID    nullable[string] `json:"id"`
	Model nullable[string] `json:"model"`
	Usage *usage           `json:"usage"`
	// Content carries the assistant's content blocks, decoded ONLY far enough
	// to see which tools were called — see contentBlock.
	Content []contentBlock `json:"content"`
}

// contentBlock is one element of message.content, decoded as an explicit
// ALLOW-LIST of the fields activity collection needs. It is the same discipline
// the audit payload follows, applied to a second fact type.
//
// Input is the part that matters. A tool_use block's input is the tool's
// arguments — the Bash command, the file path, the prompt — and none of it may
// reach the ledger. Decoding it into a struct with a single Skill field means
// encoding/json DISCARDS every other key as it parses: the content is not
// stripped after the fact, it never becomes a value in this process at all. A
// new field appears in a tool's input the day the format grows one, and this
// shape ignores it without being told to.
type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input holds the ONE input field that is a name rather than content: which
	// skill a Skill call invoked.
	Input struct {
		Skill string `json:"skill"`
	} `json:"input"`
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
	// activity is the tool/skill calls this same record contained. It rides the
	// candidate rather than being emitted directly so it is deduplicated by
	// exactly the rule the usage event is: a sidechain replay repeats the whole
	// record, tool_use blocks included, and emitting those separately would
	// count every replayed call a second time.
	activity []model.ActivityEvent
	// skill is the skill this record was produced inside, or "" for none. It
	// rides the candidate for the same reason activity does: a sidechain replay
	// repeats the attribution too, and only the winner's copy may reach the
	// store — though here a duplicate would be absorbed anyway, since the usage
	// dedup key is the context table's primary key.
	skill string
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
		activity:    callActivity(msg.Content, ev, requestID),
		skill:       strings.TrimSpace(string(rl.AttributionSkill)),
	}, true
}

// callActivity turns an assistant record's tool_use blocks into activity rows
// attributed to that record's own usage event.
//
// This is the exact join the whole design rests on: Claude Code puts the
// tool_use blocks and the usage object in the SAME record, so a call's cost is
// a fact about the record rather than a guess about which nearby turn paid for
// it. UsageDedupKey is that event's dedup key and CallsInTurn is the number of
// calls sharing it, which is what lets the read path divide instead of
// multiply. Multi-call turns are rare in practice (9 of 59,622 local records
// carry two blocks, none carry three) but they are the case that would inflate
// every headline if this counted wrong, so the count is taken from the blocks
// rather than assumed to be one.
//
// EventTime is copied from the usage event so both ledgers place the call in
// the same instant, which is what makes a windowed comparison of the two mean
// anything.
//
// Records this adapter rejects as usage — a guarded JSON null, a <synthetic>
// model — produce no activity either. That is deliberate: a call attributed to
// a usage event that was never stored would be a call whose cost is silently
// unattributable, and the honest place to draw the line is the record the
// ledger accepted.
func callActivity(blocks []contentBlock, ev model.UsageEvent, requestID string) []model.ActivityEvent {
	var calls []contentBlock
	for _, b := range blocks {
		if b.Type == "tool_use" && strings.TrimSpace(b.Name) != "" {
			calls = append(calls, b)
		}
	}
	if len(calls) == 0 {
		return nil
	}

	out := make([]model.ActivityEvent, 0, len(calls))
	for i, b := range calls {
		kind := model.ActivityTool
		name := strings.TrimSpace(b.Name)
		// A Skill call names a tool ("Skill") that is never the interesting
		// fact; the skill it invoked is. Record the skill under kind=skill so
		// "which skill" is a group-by rather than a parse at read time. A Skill
		// call with no skill in its input stays a plain tool call rather than
		// becoming a nameless skill row.
		if name == "Skill" {
			if s := strings.TrimSpace(b.Input.Skill); s != "" {
				kind, name = model.ActivitySkill, s
			}
		}
		// The block's own id is the provider's identity for this call and is
		// stable across the sidechain replays that repeat the whole record.
		// Without one, the event's dedup key plus the position in the turn is
		// still stable across re-reads of the same line.
		dedup := ev.DedupKey + "|act|" + strconv.Itoa(i)
		if b.ID != "" {
			dedup = model.ToolClaudeCode + "|call|" + b.ID
		}
		out = append(out, model.ActivityEvent{
			Tool:          model.ToolClaudeCode,
			Kind:          kind,
			Name:          name,
			SessionID:     ev.SessionID,
			Project:       ev.Project,
			Model:         ev.Model,
			EventTime:     ev.EventTime,
			UsageDedupKey: ev.DedupKey,
			MessageID:     ev.MessageID,
			RequestID:     requestID,
			TurnSeq:       i,
			CallsInTurn:   len(calls),
			SourcePath:    ev.SourcePath,
			DedupKey:      dedup,
		})
	}
	return out
}

// hookLine is the ALLOW-LIST decode of a type=="system" hook record.
//
// HookInfos is the point of the shape. Each element identifies its hook only by
// the raw shell command it ran, so it is decoded as a slice of EMPTY structs:
// that yields the NUMBER of hooks that fired while making it impossible for a
// command string to become a value in this process. The count is the fact;
// the command is content.
type hookLine struct {
	Type      string     `json:"type"`
	Subtype   string     `json:"subtype"`
	UUID      string     `json:"uuid"`
	Timestamp string     `json:"timestamp"`
	SessionID string     `json:"sessionId"`
	CWD       string     `json:"cwd"`
	HookCount int        `json:"hookCount"`
	HookInfos []struct{} `json:"hookInfos"`
}

// parseHookLine decodes one hook record into activity rows — one per hook that
// actually fired, so the count reflects hooks rather than summaries.
//
// Hook rows carry no UsageDedupKey: a hook runs outside any assistant turn and
// the record holds no usage block, so there is no cost to attribute and none is
// invented. They are reported as unattributed calls, never as free ones.
func parseHookLine(line []byte, path, segment, sessionFromName string) []model.ActivityEvent {
	var hl hookLine
	if err := json.Unmarshal(line, &hl); err != nil {
		return nil
	}
	if hl.Type != "system" || hl.Subtype != hookSummarySubtype {
		return nil
	}

	n := len(hl.HookInfos)
	if n == 0 {
		n = hl.HookCount
	}
	if n <= 0 {
		return nil // a summary that reports no hook is not a firing
	}

	var eventTime time.Time
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(hl.Timestamp)); err == nil {
		eventTime = t.UTC()
	} else {
		return nil // a row that cannot be windowed is worse than no row
	}

	session := sessionFromName
	if strings.TrimSpace(hl.SessionID) != "" {
		session = hl.SessionID
	}
	project := segment
	if strings.TrimSpace(hl.CWD) != "" {
		project = hl.CWD
	}
	// The record uuid is unique per line, so a re-read of the same transcript
	// re-derives the same keys and inserts nothing.
	base := model.ToolClaudeCode + "|hook|" + hl.UUID
	if hl.UUID == "" {
		sum := sha1.Sum(line)
		base = fmt.Sprintf("%s|hook|%s|%x", model.ToolClaudeCode, path, sum)
	}

	out := make([]model.ActivityEvent, 0, n)
	for i := range n {
		out = append(out, model.ActivityEvent{
			Tool:        model.ToolClaudeCode,
			Kind:        model.ActivityHook,
			Name:        hookEventName,
			SessionID:   session,
			Project:     project,
			EventTime:   eventTime,
			TurnSeq:     i,
			CallsInTurn: n,
			SourcePath:  path,
			DedupKey:    base + "|" + strconv.Itoa(i),
		})
	}
	return out
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
