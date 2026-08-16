package buildinfo

import (
	"strings"
	"testing"
)

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
	want := "v9.9.9"
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
	if want := "v1.2.3"; fromGoReleaser != want {
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
		// Stamps left by an older binary carried a capability suffix. This
		// build emits none, so one of those must compare UNEQUAL to the same
		// version without it - that daemon is a different build and has to be
		// restarted.
		{"legacy capability differs", "v1.2.3", "v1.2.3+webui", false},
		{"legacy capability matches", "1.2.3+webui", "v1.2.3+webui", true},
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
// stamp) independent of any capability suffix an older binary stamped: a dev
// stamp carrying one must not become something cmd.ensureDaemon auto-restarts.
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

// TestIdentityDeclaresNoCapabilities: this binary has no optional halves left,
// so an identity is a bare version. A suffix appearing here again would be a
// capability nobody declared.
func TestIdentityDeclaresNoCapabilities(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v1.2.3"

	got := Identity()
	if strings.Contains(got, "+") {
		t.Fatalf("Identity() = %q; this build declares no capabilities", got)
	}
	if BaseVersion(got) != "v1.2.3" {
		t.Fatalf("BaseVersion(%q) = %q, want v1.2.3", got, BaseVersion(got))
	}
}
