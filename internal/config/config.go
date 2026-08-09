// Package config resolves aiusage runtime configuration from XDG paths, an
// optional JSON config file, and environment overrides.
//
// Resolution order for Load(path):
//  1. Default() — XDG-derived paths, IntervalSeconds=300.
//  2. JSON file at path merged over the defaults (a missing file is not an
//     error; it simply leaves the defaults in place). Unknown keys are rejected.
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

// Config holds resolved runtime settings.
type Config struct {
	DBPath          string            `json:"db_path,omitempty"`
	PIDPath         string            `json:"pid_path,omitempty"`
	LogPath         string            `json:"log_path,omitempty"`
	Home            string            `json:"home,omitempty"`
	IntervalSeconds int               `json:"interval_seconds,omitempty"`
	SourceRoots     map[string]string `json:"source_roots,omitempty"`

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
		derivedDB:       true,
		derivedPID:      true,
		derivedLog:      true,
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
	return cfg, nil
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
