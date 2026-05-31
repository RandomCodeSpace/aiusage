package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// uistate.go persists the small "where I left off" slice of UI state across
// launches — the active time range and the active tab. This is incidental state,
// NOT user config: it lives under the XDG state dir (ui-state.json), separate
// from config.json, and every read/write is best-effort (a missing or unwritable
// state file must never break the dashboard).

// UIState is the persisted UI slice.
type UIState struct {
	Range string `json:"range"` // Range key (see rangeFromKey/Range.key)
	Tab   string `json:"tab"`   // View key (see viewFromKey/View.key)
}

// LoadUIState reads the state file, returning a zero UIState when it is missing
// or corrupt (restoring is best-effort — callers apply their own defaults).
func LoadUIState(path string) UIState {
	var s UIState
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s) // corrupt JSON → zero value
	return s
}

// SaveUIState writes the state file, creating the directory if needed. All errors
// are swallowed: a read-only state dir degrades to "don't remember", never a
// crash.
func SaveUIState(path string, s UIState) {
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if data, err := json.Marshal(s); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}
