// Package agy implements an AGGREGATE adapter for the Antigravity CLI.
//
// Antigravity is Gemini-based and is expected to reuse the Gemini CLI telemetry
// shape once it emits usage. This adapter scans the canonical Antigravity data
// directories for usage-bearing *.json / *.jsonl files and parses them via
// internal/adapter/geminishape (cumulative per-id records, max snapshot per id,
// one AggregateSnapshot per (file, id)) tagged tool="agy" so it is never
// confused with the Gemini CLI.
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
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/adapter/geminishape"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// metaProject labels every Antigravity turn (no cwd is recorded here).
const metaProject = "agy"

// shape is the shared Gemini-telemetry parser stamped for this adapter.
var shape = geminishape.Shape{Tool: model.ToolAgy, Provider: model.ProviderGoogle, Project: metaProject}

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

// Discover scans each Antigravity root for *.json / *.jsonl files. Files are
// NOT pre-parsed for token usage here — that would parse every file twice per
// cycle (Discover, then Collect). Content-only blobs from an unauthenticated
// install become sources whose Collect emits nothing: the all-zero filter in
// the shared parser rejects every record without tokens.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, root := range a.roots(cfg) {
		if root == "" || !adapter.IsDir(root) {
			continue
		}
		// Aggregate keys embed absolute file paths, so resolve symlinks: a
		// re-pointed root would otherwise mint new identities and re-add full
		// cumulative totals. A genuinely moved root still re-adds once —
		// irreducible without state migration.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if d.IsDir() {
				return nil
			}
			if !geminishape.HasUsageExt(path) {
				return nil
			}
			if _, dup := seen[path]; dup {
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
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the file on size+mtime: unchanged files are not
// opened at all. Any change re-parses the whole file (cumulative records need
// the max-per-id grouping over every record). A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("agy: stat %s: %w", src.Path, err)
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}

	res, err := shape.ReadFile(src.Path, time.Now().UTC())
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("agy: read %s: %w", src.Path, err)
	}

	obs := adapter.Observation{Snapshots: res.Snapshots}
	if res.ScanErr == nil {
		// The read completed (skipped lines are permanently unparseable, not
		// partial), so the checkpoint may advance. A scan abort withholds it:
		// the unread remainder must be re-read next cycle.
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolAgy, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
		}
	}
	switch {
	case res.ScanErr != nil:
		return obs, fmt.Errorf("agy: partial read of %s: %w", src.Path, res.ScanErr)
	case res.Skipped > 0:
		return obs, fmt.Errorf("agy: skipped %d unparseable record(s) in %s", res.Skipped, src.Path)
	}
	return obs, nil
}
