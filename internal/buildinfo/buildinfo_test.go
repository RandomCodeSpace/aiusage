package buildinfo

import (
	"strings"
	"testing"
)

func TestIdentityDevStamp(t *testing.T) {
	// With the default "dev" Version, Identity derives a per-build stamp from the
	// running test binary (size+modtime) — non-empty and prefixed "dev-".
	got := Identity()
	if got == "" {
		t.Fatal("Identity() is empty")
	}
	if Version == "dev" && !strings.HasPrefix(got, "dev-") {
		t.Fatalf("dev Identity() = %q, want a dev- stamp", got)
	}
	// Deterministic within a run (same executable).
	if got2 := Identity(); got2 != got {
		t.Fatalf("Identity() not stable: %q vs %q", got, got2)
	}
}

func TestIdentityExplicitVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v9.9.9"
	if got := Identity(); got != "v9.9.9" {
		t.Fatalf("Identity() with explicit Version = %q, want v9.9.9", got)
	}
}
