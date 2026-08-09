// Package config resolves aiusage runtime configuration from XDG paths, an
// optional JSON config file, and environment overrides.
//
// Resolution order for Load(path):
//  1. Default() — XDG-derived paths, IntervalSeconds=300.
//  2. JSON file at path merged over the defaults (a missing file is not an
//     error; it simply leaves the defaults in place). Unknown keys are rejected.
//     A relative path written in the file is resolved against the directory
//     holding that file, never the process working directory.
//  3. Environment overrides (AIUSAGE_DB, AIUSAGE_INTERVAL, AIUSAGE_HOME).
//  4. An overridden Home re-derives DBPath/PIDPath/LogPath (see SetHome);
//     paths explicitly set by file or env stay put.
//  5. IntervalSeconds clamped to [minInterval, maxInterval].
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

const (
	// defaultInterval is the collection cadence in seconds (plan D8).
	defaultInterval = 300
	// minInterval / maxInterval bound IntervalSeconds (plan D8).
	minInterval = 60
	maxInterval = 1800

	appName = "aiusage"
	dbFile  = "usage.db"
	pidFile = "aiusage.pid"
	logFile = "aiusage.log"
	cfgFile = "config.json"
)

// ModelRates overrides the published per-token rates for one model, in USD per
// single token (LiteLLM's unit, e.g. 3e-06 for $3 per million input tokens). An
// override beats every price table, so only the fields the user sets are used:
// a zero field falls back the same way a table entry's missing field does.
type ModelRates struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	CacheWrite   float64 `json:"cache_write,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	InputBatch   float64 `json:"input_batch,omitempty"`
	OutputBatch  float64 `json:"output_batch,omitempty"`
}

// Pricing configures how usage is valued. The refresh URL and interval are
// hardcoded on purpose (see pricing.RefreshURL): the off switch plus overrides
// already cover the air-gapped and the disagrees-with-the-table cases.
type Pricing struct {
	// Refresh enables the daily runtime refresh of the LiteLLM price table.
	// Default true; set false for a strict air gap, which pins pricing to the
	// embedded snapshot plus any overrides.
	Refresh bool `json:"refresh"`
	// Overrides maps a model id to the rates that must be used for it.
	Overrides map[string]ModelRates `json:"overrides,omitempty"`
}

// Privacy configures what an install persists beyond the token counters.
type Privacy struct {
	// NoRaw drops the per-record audit payload entirely: no adapter's raw JSON
	// is stored, in usage_events or in aggregate_state. Default false, which
	// stores the usage-object-only payload (counters, model, message/request
	// ids, timestamp, service tier, cache-creation split) — never message
	// content. Existing rows are unaffected either way: usage_events is
	// append-only, while aggregate_state rows shrink on their next snapshot.
	NoRaw bool `json:"no_raw"`
}

// Config holds resolved runtime settings.
//
// Path rule: db_path, pid_path, log_path, home and every source_roots value may
// be written relative in a config file, and a relative value is resolved
// against the directory holding that config file — never the process working
// directory, which differs between the CLI (wherever the user stands) and the
// daemon it spawns (the CWD of the shell that started it). Every field below is
// absolute once Load returns.
type Config struct {
	// DBPath, PIDPath and LogPath are absolute; a relative value in a config
	// file is anchored at that file's directory (see the path rule above).
	DBPath  string `json:"db_path,omitempty"`
	PIDPath string `json:"pid_path,omitempty"`
	LogPath string `json:"log_path,omitempty"`
	// Home is the discovery root, absolute under the same path rule.
	Home            string `json:"home,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	// SourceRoots overrides an adapter's discovery root; values follow the
	// same path rule.
	SourceRoots map[string]string `json:"source_roots,omitempty"`
	Pricing     Pricing           `json:"pricing,omitzero"`
	Privacy     Privacy           `json:"privacy,omitzero"`

	// derived* track which path fields still hold values derived from Home
	// rather than explicit overrides (config file, env, or flag). Only derived
	// paths move when SetHome re-targets Home — an explicit override always
	// wins over derivation.
	derivedDB, derivedPID, derivedLog bool
}

// Default returns the baseline configuration derived from XDG base dirs and the
// user's home directory. It never returns an error: if the home directory
// cannot be determined, paths fall back to relative XDG-style locations.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DBPath:          filepath.Join(dataHome(home), appName, dbFile),
		PIDPath:         filepath.Join(stateHome(home), appName, pidFile),
		LogPath:         filepath.Join(stateHome(home), appName, logFile),
		Home:            home,
		IntervalSeconds: defaultInterval,
		SourceRoots:     map[string]string{},
		// Pricing refresh is on by default: an install that can reach the
		// network should price with current rates, and every failure of the
		// refresh degrades silently to the embedded snapshot.
		Pricing:    Pricing{Refresh: true},
		derivedDB:  true,
		derivedPID: true,
		derivedLog: true,
	}
}

// SetHome sets the discovery home and moves every still-derived path
// (DBPath/PIDPath/LogPath) with it, using the same XDG layout as Default().
// Paths the user overrode explicitly (flag, env, or config file) stay put, as
// do absolute XDG_*_HOME env dirs — both outrank derivation. Without this,
// --home/AIUSAGE_HOME sandboxing would share the production DB and daemon lock.
func (c *Config) SetHome(home string) {
	c.Home = home
	if c.derivedDB {
		c.DBPath = filepath.Join(dataHome(home), appName, dbFile)
	}
	if c.derivedPID {
		c.PIDPath = filepath.Join(stateHome(home), appName, pidFile)
	}
	if c.derivedLog {
		c.LogPath = filepath.Join(stateHome(home), appName, logFile)
	}
}

// DefaultConfigPath returns the conventional config file location
// (~/.config/aiusage/config.json, honoring XDG_CONFIG_HOME).
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(configHome(home), appName, cfgFile)
}

// Load builds a Config by merging the JSON file at path over Default(), then
// applying environment overrides, then clamping the interval.
//
// A missing config file is not an error — Load returns the env-overridden,
// clamped defaults with a nil error. Any other read or parse failure is
// returned.
func Load(path string) (Config, error) {
	cfg := Default()
	defaultHome := cfg.Home

	if path != "" {
		before := cfg
		if err := mergeFile(&cfg, path); err != nil {
			return Config{}, err
		}
		resolveFilePaths(&cfg, before, path)
		markExplicit(&cfg, before)
	}

	before := cfg
	applyEnv(&cfg)
	markExplicit(&cfg, before)

	// A home overridden by file or env moves the derived paths with it;
	// explicitly overridden paths were pinned by markExplicit above.
	if cfg.Home != defaultHome {
		cfg.SetHome(cfg.Home)
	}

	cfg.IntervalSeconds = clampInterval(cfg.IntervalSeconds)
	if cfg.SourceRoots == nil {
		cfg.SourceRoots = map[string]string{}
	}
	if cfg.Pricing.Overrides == nil {
		cfg.Pricing.Overrides = map[string]ModelRates{}
	}
	return cfg, nil
}

// resolveFilePaths anchors every relative path the config file set to the
// directory holding that file, so the value means the same thing to every
// process that reads it. Used verbatim, a relative path would resolve against
// the process working directory: the CLI would read one database and the
// daemon it spawns (which keeps the CWD of the shell that started it) would
// write another, with no error either way.
//
// Only fields the file actually changed are anchored — the defaults are
// absolute whenever the user's home directory is known, and rebasing the
// unknown-home fallback onto the config directory would move an install nobody
// asked to move. SourceRoots needs no such check: Default() leaves the map
// empty and JSON decoding merges into it, so every entry present came from the
// file.
func resolveFilePaths(cfg *Config, prev Config, path string) {
	dir := filepath.Dir(path)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if cfg.Home != prev.Home {
		cfg.Home = anchor(dir, cfg.Home)
	}
	if cfg.DBPath != prev.DBPath {
		cfg.DBPath = anchor(dir, cfg.DBPath)
	}
	if cfg.PIDPath != prev.PIDPath {
		cfg.PIDPath = anchor(dir, cfg.PIDPath)
	}
	if cfg.LogPath != prev.LogPath {
		cfg.LogPath = anchor(dir, cfg.LogPath)
	}
	for tool, root := range cfg.SourceRoots {
		cfg.SourceRoots[tool] = anchor(dir, root)
	}
}

// anchor resolves p against dir. An empty or already-absolute p is returned
// unchanged.
func anchor(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// markExplicit pins every path field whose value changed between prev and cfg:
// a config-file or env override must not be moved by a later home override.
func markExplicit(cfg *Config, prev Config) {
	if cfg.DBPath != prev.DBPath {
		cfg.derivedDB = false
	}
	if cfg.PIDPath != prev.PIDPath {
		cfg.derivedPID = false
	}
	if cfg.LogPath != prev.LogPath {
		cfg.derivedLog = false
	}
}

// mergeFile decodes the JSON file at path over cfg. Only fields present in the
// file override the corresponding defaults. A non-existent file is ignored.
// Unknown keys are an error: silently ignoring them hides typos (a misspelled
// db_path would leave collection pointed at the default DB).
func mergeFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}

	// Decode into the same struct so unspecified keys retain the defaults
	// already present in cfg.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	if dec.More() {
		return fmt.Errorf("parse config %s: unexpected trailing data", path)
	}
	return nil
}

// applyEnv applies AIUSAGE_* environment overrides in place. An empty or unset
// variable leaves the existing value untouched; a malformed AIUSAGE_INTERVAL is
// ignored (clamping/defaults still apply).
func applyEnv(cfg *Config) {
	// Home first: a relative AIUSAGE_DB resolves against the effective home
	// (not the process cwd) so the CLI and the daemon it spawns agree on the
	// target no matter where each was started.
	if v := os.Getenv("AIUSAGE_HOME"); v != "" {
		cfg.Home = v
	}
	if v := os.Getenv("AIUSAGE_DB"); v != "" {
		if !filepath.IsAbs(v) {
			v = filepath.Join(cfg.Home, v)
		}
		cfg.DBPath = v
	}
	if v := os.Getenv("AIUSAGE_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.IntervalSeconds = n
		}
	}
}

// clampInterval bounds n to [minInterval, maxInterval]. A non-positive value
// (e.g. an absent field decoded as zero) resets to the default.
func clampInterval(n int) int {
	if n <= 0 {
		n = defaultInterval
	}
	if n < minInterval {
		return minInterval
	}
	if n > maxInterval {
		return maxInterval
	}
	return n
}

// --- XDG base directory helpers ---

func dataHome(home string) string {
	return xdgDir("XDG_DATA_HOME", home, ".local", "share")
}

func stateHome(home string) string {
	return xdgDir("XDG_STATE_HOME", home, ".local", "state")
}

func configHome(home string) string {
	return xdgDir("XDG_CONFIG_HOME", home, ".config")
}

// xdgDir returns the value of env (if set to an absolute path) or the XDG
// default built from home and fallback path segments. Per the XDG spec,
// relative env values are ignored in favor of the default.
func xdgDir(env, home string, fallback ...string) string {
	if v := os.Getenv(env); filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(append([]string{home}, fallback...)...)
}
