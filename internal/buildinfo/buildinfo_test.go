package buildinfo

import (
	"strings"
	"testing"
)

// capSuffix is what this build appends to every identity, so a test can state
// the expected identity without hardcoding one build tag's answer.
func capSuffix() string {
	if HasWebUI {
		return "+webui"
	}
	return ""
}

func TestIdentityDevStamp(t *testing.T) {
	// With the default "dev" Version, Identity derives a per-build stamp from the
	// running test binary (size+modtime) - non-empty and prefixed "dev-".
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
	want := "v9.9.9" + capSuffix()
	if got := Identity(); got != want {
		t.Fatalf("Identity() with explicit Version = %q, want %q", got, want)
	}
}

// TestIdentityNormalisesGoReleaserVersion pins the format trap: GoReleaser links
// {{ .Version }}, which has no leading v, while `go install <module>@vX.Y.Z`
// embeds one. Both must produce the same identity or the CLI restarts the daemon
// on every invocation.
func TestIdentityNormalisesGoReleaserVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "1.2.3"
	fromGoReleaser := Identity()
	Version = "v1.2.3"
	fromModule := Identity()

	if fromGoReleaser != fromModule {
		t.Fatalf("identity differs by version spelling: %q vs %q", fromGoReleaser, fromModule)
	}
	if want := "v1.2.3" + capSuffix(); fromGoReleaser != want {
		t.Fatalf("Identity() = %q, want the canonical %q", fromGoReleaser, want)
	}
}

func TestSameIdentity(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "v1.2.3", "v1.2.3", true},
		{"spelling only", "1.2.3", "v1.2.3", true},
		{"surrounding space", " v1.2.3\n", "v1.2.3", true},
		{"different version", "v1.2.3", "v1.2.4", false},
		{"capability differs", "v1.2.3", "v1.2.3+webui", false},
		{"capability matches", "1.2.3+webui", "v1.2.3+webui", true},
		{"dev stamps", "dev-1-2", "dev-1-2", true},
		{"dev vs release", "dev-1-2", "v1.2.3", false},
		{"unrecorded", "", "v1.2.3", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameIdentity(tc.a, tc.b); got != tc.want {
				t.Fatalf("SameIdentity(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestBaseVersionStripsCapabilities keeps build CLASSIFICATION (release vs dev
// stamp) independent of what the build can do: gaining the web UI must not turn
// a dev stamp into something cmd.ensureDaemon auto-restarts.
func TestBaseVersionStripsCapabilities(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"v1.2.3+webui", "v1.2.3"},
		{"1.2.3+webui", "v1.2.3"},
		{"dev-100-200+webui", "dev-100-200"},
		{"dev+webui", "dev"},
		{"v1.2.3", "v1.2.3"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := BaseVersion(tc.in); got != tc.want {
			t.Errorf("BaseVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIdentityCarriesTheBuildCapability is the whole point of issue #61: the
// identity a UI build stamps must differ from the same version without the UI,
// or an upgrade leaves the old collector running.
func TestIdentityCarriesTheBuildCapability(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v1.2.3"

	got := Identity()
	if HasWebUI != strings.HasSuffix(got, "+webui") {
		t.Fatalf("Identity() = %q with HasWebUI=%v; the suffix must track the capability", got, HasWebUI)
	}
	if BaseVersion(got) != "v1.2.3" {
		t.Fatalf("BaseVersion(%q) = %q, want v1.2.3", got, BaseVersion(got))
	}
}
