// Package config resolves aiusage runtime configuration from XDG paths, an
// optional JSON config file, and environment overrides.
//
// Resolution order for Load(path):
//  1. Default() — XDG-derived paths, IntervalSeconds=300.
//  2. JSON file at path merged over the defaults (a missing file is not an
//     error; it simply leaves the defaults in place).
//  3. Environment overrides (AIUSAGE_DB, AIUSAGE_INTERVAL, AIUSAGE_HOME).
//  4. IntervalSeconds clamped to [minInterval, maxInterval].
package config

import (
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

	if path != "" {
		if err := mergeFile(&cfg, path); err != nil {
			return Config{}, err
		}
	}

	applyEnv(&cfg)
	cfg.IntervalSeconds = clampInterval(cfg.IntervalSeconds)
	if cfg.SourceRoots == nil {
		cfg.SourceRoots = map[string]string{}
	}
	return cfg, nil
}

// mergeFile decodes the JSON file at path over cfg. Only fields present in the
// file override the corresponding defaults. A non-existent file is ignored.
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
	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// applyEnv applies AIUSAGE_* environment overrides in place. An empty or unset
// variable leaves the existing value untouched; a malformed AIUSAGE_INTERVAL is
// ignored (clamping/defaults still apply).
func applyEnv(cfg *Config) {
	if v := os.Getenv("AIUSAGE_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("AIUSAGE_HOME"); v != "" {
		cfg.Home = v
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
