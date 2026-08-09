package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPricingRefreshDefaultsOn pins the documented default: an install with no
// pricing block still refreshes its price table.
func TestPricingRefreshDefaultsOn(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pricing.Refresh {
		t.Errorf("Pricing.Refresh = false, want true by default")
	}
	if cfg.Pricing.Overrides == nil {
		t.Errorf("Pricing.Overrides = nil, want an empty map")
	}
}

// TestPricingRefreshCanBeDisabled proves the air-gap switch survives the merge
// over the defaults, where a plain false is indistinguishable from an absent
// key unless the decoder writes into the already-defaulted struct.
func TestPricingRefreshCanBeDisabled(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pricing":{"refresh":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pricing.Refresh {
		t.Errorf("Pricing.Refresh = true, want false from the config file")
	}
}

// TestPricingOverridesParse checks the per-model override shape and that
// setting overrides alone leaves the refresh default intact.
func TestPricingOverridesParse(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"pricing":{"overrides":{"claude-sonnet-4-6":{"input":3e-06,"output":1.5e-05,` +
		`"cache_read":3e-07,"cache_write":3.75e-06,"cache_write_1h":6e-06}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pricing.Refresh {
		t.Errorf("Pricing.Refresh = false, want the default to survive an overrides-only block")
	}
	r, ok := cfg.Pricing.Overrides["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("override missing: %+v", cfg.Pricing.Overrides)
	}
	if r.Input != 3e-06 || r.Output != 1.5e-05 || r.CacheRead != 3e-07 ||
		r.CacheWrite != 3.75e-06 || r.CacheWrite1h != 6e-06 {
		t.Errorf("override = %+v, want the file's rates verbatim", r)
	}
}

// TestPricingRejectsUnknownKeys keeps a typo inside the pricing block loud: a
// misspelled rate would otherwise silently bill at the table price.
func TestPricingRejectsUnknownKeys(t *testing.T) {
	clearXDG(t)
	t.Setenv("HOME", t.TempDir())

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pricing":{"refrsh":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(unknown pricing key) error = nil, want non-nil")
	}
}
