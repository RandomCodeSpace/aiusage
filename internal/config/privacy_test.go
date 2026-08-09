package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrivacyNoRawDefaultsOff pins the documented default: an install with no
// privacy block still stores the usage-object audit payload.
func TestPrivacyNoRawDefaultsOff(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Privacy.NoRaw {
		t.Errorf("Privacy.NoRaw = true, want false by default")
	}
}

// TestPrivacyNoRawFromFile proves the switch survives the merge over the
// defaults and does not disturb the neighbouring pricing block.
func TestPrivacyNoRawFromFile(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"privacy":{"no_raw":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Privacy.NoRaw {
		t.Errorf("Privacy.NoRaw = false, want true from the config file")
	}
	if !cfg.Pricing.Refresh {
		t.Errorf("Pricing.Refresh = false, want the default to survive a privacy-only block")
	}
}

// TestPrivacyRejectsUnknownKeys keeps a typo loud: a misspelled no_raw would
// otherwise silently leave the payload being stored.
func TestPrivacyRejectsUnknownKeys(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"privacy":{"no_row":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(unknown privacy key) error = nil, want non-nil")
	}
}
