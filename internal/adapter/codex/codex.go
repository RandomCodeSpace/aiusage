// Package codex implements an event-level adapter for the Codex CLI. Codex
// records usage in JSONL session transcripts under CODEX_HOME (default
// ~/.codex). Each `token_count` event carries either a per-turn delta
// (info.last_token_usage) or a cumulative running total (info.total_token_usage);
// we prefer the per-turn delta and otherwise derive deltas with a saturating
// subtraction against a per-file running previous total.
//
// Token accounting follows OpenAI semantics: cached tokens are a SUBSET of
// input tokens, so we map Input = input-cached and CacheRead = cached so the
// components sum to the provider total without double counting.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

const (
	defaultModel = "gpt-5"
	dirSessions  = "sessions"
	dirArchived  = "archived_sessions"
)

// Adapter reads Codex CLI session transcripts. Read-only.
type Adapter struct{}

// New returns a Codex adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolCodex }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Codex" }

// homes returns the configured Codex home directories. CODEX_HOME may be a
// comma-separated list; otherwise the discovery root (override or ~/.codex).
func (a Adapter) homes(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv("CODEX_HOME")); env != "" {
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
		def = filepath.Join(cfg.Home, ".codex")
	}
	return []string{cfg.Root(model.ToolCodex, def)}
}

// Discover locates *.jsonl session files. For each home it scans <home>/sessions
// (or <home> itself if no sessions dir) plus <home>/archived_sessions if
// present. The Source.Meta["root"] records the sessions root so Collect can
// compute the session id as a path relative to it.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source

	for _, home := range a.homes(cfg) {
		if home == "" {
			continue
		}
		// Primary root: <home>/sessions if it is a dir, else <home>.
		root := filepath.Join(home, dirSessions)
		if !isDir(root) {
			root = home
		}
		a.scanRoot(ctx, root, seen, &srcs)

		// Durability enhancement: also read archived sessions if present.
		archived := filepath.Join(home, dirArchived)
		if isDir(archived) {
			a.scanRoot(ctx, archived, seen, &srcs)
		}
	}
	return srcs, nil
}

// scanRoot recursively collects *.jsonl files under root, tagging each Source
// with the root for relative-session computation.
func (a Adapter) scanRoot(ctx context.Context, root string, seen map[string]struct{}, srcs *[]adapter.Source) {
	if !isDir(root) {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Skip unreadable entries; never fail the whole walk.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
			return nil
		}
		if _, dup := seen[path]; dup {
			return nil
		}
		seen[path] = struct{}{}
		*srcs = append(*srcs, adapter.Source{
			Tool:  model.ToolCodex,
			Class: model.EventLevel,
			Path:  path,
			Label: "codex session " + filepath.Base(path),
			Meta:  map[string]string{"root": root},
		})
		return nil
	})
}

// lineMarkers fast-skip lines that can neither carry usage nor set the model:
// only token_count payloads and turn_context lines matter to this adapter.
// Unquoted so whitespace-padded type strings (" token_count ") still match.
var (
	markerTokenCount  = []byte(`token_count`)
	markerTurnContext = []byte(`turn_context`)
)

// ckptState is the per-file parse state persisted in the checkpoint. The
// cumulative baseline (Prev/HavePrev) is what makes tail reads correct: a
// naive tail read would count the first cumulative total_token_usage record
// after the offset IN FULL instead of as a delta. Model is the turn_context
// carry-forward, which a tail read would otherwise miss.
type ckptState struct {
	Model     string `json:"model,omitempty"`
	HavePrev  bool   `json:"havePrev,omitempty"`
	Input     int64  `json:"input,omitempty"`
	Cached    int64  `json:"cached,omitempty"`
	Output    int64  `json:"output,omitempty"`
	Reasoning int64  `json:"reasoning,omitempty"`
	Total     int64  `json:"total,omitempty"`
}

// Collect reads one JSONL session file in full and returns its usage events.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental reads only what is new since cp: an unchanged size+mtime
// skips the file entirely; growth tail-reads from the stored offset with the
// persisted cumulative baseline; any shrink or same-size rewrite re-reads from
// zero (re-derived dedup keys collapse in the store). A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	f, err := os.Open(src.Path) // read-only
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("codex: open %s: %w", src.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("codex: stat %s: %w", src.Path, err)
	}
	size := fi.Size()
	mtimeNS := fi.ModTime().UnixNano()

	var (
		start int64
		state ckptState
	)
	if cp != nil {
		if cp.Size == size && cp.MTimeNS == mtimeNS {
			return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
		}
		// Tail-read only on pure growth. A shrink or same-size change means
		// unknown history: restart from zero with a fresh baseline.
		if size > cp.Size && cp.Offset >= 0 && cp.Offset <= size {
			start = cp.Offset
			if cp.State != "" {
				if err := json.Unmarshal([]byte(cp.State), &state); err != nil {
					start, state = 0, ckptState{}
				}
			}
		}
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return adapter.Observation{}, fmt.Errorf("codex: seek %s: %w", src.Path, err)
		}
	}

	mtime := fi.ModTime().UTC()
	session := a.sessionID(src)

	var (
		events    []model.UsageEvent
		curModel  = state.Model
		prevTotal = rawTokens{
			input: state.Input, cached: state.Cached, output: state.Output,
			reasoning: state.Reasoning, total: state.Total,
		}
		havePrev = state.HavePrev
		consumed = start
		r        = bufio.NewReaderSize(f, 64*1024)
	)

	for {
		if ctx.Err() != nil {
			return adapter.Observation{Events: events}, ctx.Err()
		}
		raw, rerr := r.ReadBytes('\n')
		terminated := rerr == nil
		if rerr != nil && rerr != io.EOF {
			break // unreadable remainder: keep what we have, checkpoint stops here
		}
		if len(bytes.TrimSpace(raw)) > 0 &&
			(bytes.Contains(raw, markerTokenCount) || bytes.Contains(raw, markerTurnContext)) {
			var line map[string]json.RawMessage
			if err := json.Unmarshal(raw, &line); err != nil {
				// Malformed line: skip it and keep parsing — one bad line must
				// not silently drop the rest of the file. But an unterminated
				// malformed tail is likely a write in progress: leave the
				// offset before it so the completed line is read next cycle.
				if !terminated {
					break
				}
			} else if ev, ok := parseUsageLine(line, mtime, session, src.Path,
				&curModel, &prevTotal, &havePrev); ok {
				events = append(events, ev)
			}
		}
		if terminated {
			consumed += int64(len(raw))
			continue
		}
		// Complete-but-unterminated final line: its events were emitted, but
		// the offset stays before it — if it was mid-append, the next cycle
		// re-reads the full line and the store collapses the dedup keys.
		break
	}

	newState, err := json.Marshal(ckptState{
		Model: curModel, HavePrev: havePrev,
		Input: prevTotal.input, Cached: prevTotal.cached, Output: prevTotal.output,
		Reasoning: prevTotal.reasoning, Total: prevTotal.total,
	})
	if err != nil {
		return adapter.Observation{Events: events}, nil // no checkpoint; next cycle re-reads
	}
	return adapter.Observation{
		Events: events,
		Checkpoint: &model.SourceCheckpoint{
			Tool: model.ToolCodex, SourcePath: src.Path,
			Size: size, MTimeNS: mtimeNS, Offset: consumed, State: string(newState),
		},
	}, nil
}

// parseUsageLine handles one decoded JSONL line, updating the model
// carry-forward and cumulative baseline in place. Returns a usage event when
// the line is a non-zero token_count record.
func parseUsageLine(line map[string]json.RawMessage, mtime time.Time, session, path string,
	curModel *string, prevTotal *rawTokens, havePrev *bool) (model.UsageEvent, bool) {

	typ := typeOf(line["type"])

	// turn_context lines set the current model (carried forward).
	if typ == "turn_context" {
		if m := modelFrom(line["payload"]); m != "" {
			*curModel = m
		}
		return model.UsageEvent{}, false
	}
	if typ != "event_msg" {
		return model.UsageEvent{}, false
	}

	payload := objOf(line["payload"])
	if payload == nil || typeOf(payload["type"]) != "token_count" {
		return model.UsageEvent{}, false
	}
	info := objOf(payload["info"])
	if info == nil {
		return model.UsageEvent{}, false
	}

	// Model may also be present on the payload/info; carry forward otherwise.
	if m := firstModel(payload, info); m != "" {
		*curModel = m
	}
	mdl := *curModel
	if mdl == "" {
		mdl = defaultModel
	}

	var (
		tok    rawTokens
		usable bool
	)
	if last := objOf(info["last_token_usage"]); last != nil {
		tok = readRaw(last)
		usable = true
	} else if cum := objOf(info["total_token_usage"]); cum != nil {
		cur := readRaw(cum)
		if *havePrev {
			tok = cur.satSub(*prevTotal)
		} else {
			tok = cur
		}
		*prevTotal = cur
		*havePrev = true
		usable = true
	}
	if !usable {
		return model.UsageEvent{}, false
	}

	return buildEvent(tok, mdl, session, path, ts(line, mtime))
}

// sessionID computes the session as the file path relative to its sessions root,
// extension stripped, path separators normalised to "/".
func (a Adapter) sessionID(src adapter.Source) string {
	root := ""
	if src.Meta != nil {
		root = src.Meta["root"]
	}
	rel := src.Path
	if root != "" {
		if r, err := filepath.Rel(root, src.Path); err == nil {
			rel = r
		}
	} else {
		rel = filepath.Base(src.Path)
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	return filepath.ToSlash(rel)
}

// rawTokens holds the provider-reported token components before mapping.
type rawTokens struct {
	input     int64
	cached    int64
	output    int64
	reasoning int64
	total     int64
}

// satSub returns a per-field saturating subtraction (cur - prev, floored at 0).
func (r rawTokens) satSub(prev rawTokens) rawTokens {
	return rawTokens{
		input:     satSub(r.input, prev.input),
		cached:    satSub(r.cached, prev.cached),
		output:    satSub(r.output, prev.output),
		reasoning: satSub(r.reasoning, prev.reasoning),
		total:     satSub(r.total, prev.total),
	}
}

func satSub(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return 0
}

// clamped floors every field at zero. A negative provider value would violate
// the schema CHECK and poison the insert batch it rides in.
func (r rawTokens) clamped() rawTokens {
	return rawTokens{
		input:     nonNeg(r.input),
		cached:    nonNeg(r.cached),
		output:    nonNeg(r.output),
		reasoning: nonNeg(r.reasoning),
		total:     nonNeg(r.total),
	}
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// buildEvent maps raw tokens (cached ⊆ input) onto a UsageEvent. Negative
// components are clamped to zero first. Returns ok=false for all-zero records.
func buildEvent(t rawTokens, mdl, session, path string, when time.Time) (model.UsageEvent, bool) {
	t = t.clamped()
	cached := t.cached
	if cached > t.input {
		cached = t.input // clamp: cached must be a subset of input
	}
	input := t.input - cached
	output := t.output
	reasoning := t.reasoning

	total := t.total
	if total <= 0 {
		total = t.input + t.output
	}

	if input == 0 && cached == 0 && output == 0 && reasoning == 0 && total == 0 {
		return model.UsageEvent{}, false
	}

	ev := model.UsageEvent{
		Tool:                model.ToolCodex,
		Model:               mdl,
		SessionID:           session,
		Project:             "",
		EventTime:           when,
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: 0,
		CacheReadTokens:     cached,
		ReasoningTokens:     reasoning,
		TotalTokens:         total,
		SourcePath:          path,
		Kind:                model.KindUsage,
	}
	// Dedup key EXCLUDES session id so branch-copied histories count once.
	ev.DedupKey = dedupKey(when, mdl, t.input, cached, output, reasoning, total)
	return ev, true
}

// dedupKey builds the persisted ccusage Stage-A key:
// codex|<ts>|<model>|<input>|<cached>|<output>|<reasoning>|<total> (sha1 hashed).
// Note <input> is the RAW input (pre cached-subtraction) to match the spec tuple.
func dedupKey(when time.Time, mdl string, input, cached, output, reasoning, total int64) string {
	tuple := fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d",
		when.UTC().Format(time.RFC3339Nano), mdl, input, cached, output, reasoning, total)
	sum := sha1.Sum([]byte(tuple))
	return "codex|" + fmt.Sprintf("%x", sum)
}

// ts returns the line's timestamp if parseable, else the file mtime.
//
// TODO(codex,LOW): a token_count line that lacks a timestamp falls back to the
// file mtime, which can change between polls when the agent appends to the
// session file — a timestamp-less record could then be re-counted (an
// OVERcount, never an undercount, so the durability invariant still holds).
// Real codex token_count events carry a timestamp, so this path rarely fires.
// A future fix would use a stable per-line marker that still excludes session
// identity (to preserve cross-branch dedup).
func ts(line map[string]json.RawMessage, mtime time.Time) time.Time {
	if s := strOf(line["timestamp"]); s != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z07:00"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return mtime
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileMTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime().UTC()
}
