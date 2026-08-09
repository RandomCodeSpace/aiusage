// Package gemini implements an AGGREGATE adapter for the Gemini CLI.
//
// Gemini CLI writes per-turn telemetry records under <data-dir>/*.json and
// *.jsonl. The record shape, cumulative-per-id semantics and token mapping are
// documented in internal/adapter/geminishape, which does all the parsing; this
// package keeps only the Gemini-specific discovery policy and the size+mtime
// checkpoint gate. The collector compares each snapshot against the last
// stored state and appends a positive delta as an immutable event, so a turn's
// final total is never under-captured by a mid-stream poll and survives later
// file deletion.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY and never written,
// locked, or modified.
package gemini

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

const (
	// toolID is the stable tool identifier.
	toolID = model.ToolGemini
	// dataDirEnv may hold a comma-separated list of data directories. When set,
	// it REPLACES the default ~/.gemini/tmp root.
	dataDirEnv = "GEMINI_DATA_DIR"
	// metaProject labels every Gemini turn (Gemini CLI records no cwd here).
	metaProject = "gemini"
)

// shape is the shared Gemini-telemetry parser stamped for this adapter.
var shape = geminishape.Shape{Tool: toolID, Project: metaProject}

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
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the file on size+mtime: unchanged files are not
// opened at all. Any change re-parses the whole file — records for one turn id
// are cumulative and the max-per-id grouping needs every record, so a tail
// read is not applicable. A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("gemini: stat %s: %w", src.Path, err)
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}

	res, err := shape.ReadFile(src.Path, time.Now().UTC())
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("gemini: read %s: %w", src.Path, err)
	}

	obs := adapter.Observation{Snapshots: res.Snapshots}
	if res.ScanErr == nil {
		// The read completed (skipped lines are permanently unparseable, not
		// partial), so the checkpoint may advance. A scan abort withholds it:
		// the unread remainder must be re-read next cycle.
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: toolID, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
		}
	}
	switch {
	case res.ScanErr != nil:
		return obs, fmt.Errorf("gemini: partial read of %s: %w", src.Path, res.ScanErr)
	case res.Skipped > 0:
		return obs, fmt.Errorf("gemini: skipped %d unparseable record(s) in %s", res.Skipped, src.Path)
	}
	return obs, nil
}
