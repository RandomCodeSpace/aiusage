// Package adapter defines the read-only interface every agent-CLI integration
// implements, plus the registry that wires them together.
//
// CRITICAL: adapters are strictly observational. They MUST only read
// already-produced local files/DBs. They must never write, modify, lock for
// writing, rotate, or otherwise influence agent files. Open SQLite sources
// read-only (immutable=1 / mode=ro) so a poll can never disturb the agent.
package adapter

import (
	"context"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// Source is a concrete usage source discovered by an adapter — typically a
// file, directory, or database belonging to one agent CLI.
type Source struct {
	Tool  string            // owning tool id (model.ToolXxx)
	Class model.SourceClass // EventLevel or Aggregate
	Path  string            // primary path (file or db)
	Label string            // human-friendly label for `sources`
	Meta  map[string]string // adapter-specific extras (e.g. session id, dir)
}

// Observation is the result of reading a Source once. EventLevel adapters fill
// Events; Aggregate adapters fill Snapshots. An adapter may return both.
type Observation struct {
	Events    []model.UsageEvent
	Snapshots []model.AggregateSnapshot
}

// DiscoverConfig carries discovery roots and per-tool path overrides.
type DiscoverConfig struct {
	Home      string            // user home directory
	Overrides map[string]string // tool id -> explicit root path (optional)
}

// Root returns the discovery root for a tool: an explicit override if present,
// otherwise the user's home directory.
func (c DiscoverConfig) Root(tool, def string) string {
	if c.Overrides != nil {
		if v, ok := c.Overrides[tool]; ok && v != "" {
			return v
		}
	}
	if def != "" {
		return def
	}
	return c.Home
}

// Adapter reads one agent CLI's local usage data. Implementations MUST be
// read-only and must tolerate missing/partial/corrupt files without erroring
// the whole collection cycle (return best-effort results + a non-fatal error).
type Adapter interface {
	// ID is the stable tool identifier (model.ToolXxx).
	ID() string
	// DisplayName is the human-friendly name ("Claude Code").
	DisplayName() string
	// Discover locates sources under the configured roots. Read-only.
	Discover(ctx context.Context, cfg DiscoverConfig) ([]Source, error)
	// Collect reads a single source and returns its observations. Read-only.
	Collect(ctx context.Context, src Source) (Observation, error)
}

// Registry holds the set of available adapters.
type Registry struct{ adapters []Adapter }

// NewRegistry builds a registry from the given adapters.
func NewRegistry(as ...Adapter) *Registry { return &Registry{adapters: as} }

// All returns every registered adapter.
func (r *Registry) All() []Adapter { return r.adapters }

// Get returns the adapter for id, if registered.
func (r *Registry) Get(id string) (Adapter, bool) {
	for _, a := range r.adapters {
		if a.ID() == id {
			return a, true
		}
	}
	return nil, false
}
