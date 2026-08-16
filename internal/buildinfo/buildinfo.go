// Package buildinfo exposes a single build identity used to keep the CLI and the
// background daemon on the same code: when the CLI's identity differs from the
// running daemon's, the CLI restarts the daemon (see cmd.ensureDaemon).
//
// Version is overridable at build time:
//
//	go build -ldflags "-X github.com/RandomCodeSpace/aiusage/internal/buildinfo.Version=v1.2.3"
//
// When Version is left at its "dev" default, Identity() first consults the module
// version embedded by the Go toolchain (set for `go install <module>@vX.Y.Z`), and
// otherwise falls back to a stamp derived from the running executable's size +
// modtime, so every rebuild/reinstall gets a distinct identity. Dev stamps are
// informational only: cmd.ensureDaemon deliberately does not auto-restart on
// dev-stamp mismatches (`go run` is a fresh temp binary each time — acting on
// those would flap the daemon on every invocation).
//
// This build declares no capabilities, so an identity is a version and nothing
// else. The capability SUFFIX ("<version>+<capability>") is still understood on
// the way in: a daemon stamped by an older binary carries one, and Normalize /
// BaseVersion have to read those stamps for the comparison to mean anything.
// Nothing produces a suffix any more.
//
// Version FORMATS differ by install path and must be normalised before two
// identities are compared: GoReleaser interpolates {{ .Version }}, which has the
// leading v stripped ("1.2.3"), while the module version the toolchain embeds for
// go install keeps it ("v1.2.3"). Comparing those verbatim would restart the
// daemon forever, so Identity emits the canonical leading-v form and SameIdentity
// normalises both sides before comparing.
package buildinfo

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// Version is the declared build version. Override via -ldflags for a real
// release; otherwise it stays "dev" and Identity() derives a per-build stamp.
var Version = "dev"

// capabilitySep separates the version from the capability suffix in an identity.
// It is "+" because that is semver's build-metadata separator and it survives a
// filename, a pidfile stamp and a JSON string without escaping. This build emits
// no suffix; the separator is kept because stamps written by older binaries do.
const capabilitySep = "+"

// Identity returns a stable identifier for this build: the normalised version,
// e.g. "v1.2.3".
//
// The version part is a real (non-"dev") Version when one was linked in.
// Otherwise, if the binary was produced by `go install <module>@vX.Y.Z`, it is
// the module version embedded by the toolchain. Failing that it is
// "dev-<size>-<modtimeUnixNano>" of the running executable, which changes on
// every rebuild/reinstall. If the executable cannot be stat'd, the bare Version
// is used (degrades to "always matches", which is safe - it just disables
// auto-restart).
func Identity() string {
	return Normalize(version())
}

// Normalize canonicalises an identity for comparison: it trims surrounding
// space and restores the leading v on a bare numeric version, so the GoReleaser
// form ("1.2.3") and the module form ("v1.2.3") of one release compare equal.
// A capability suffix left by an older binary is preserved - it was a real
// difference between builds, not a spelling difference, and a daemon still
// running one has to compare unequal to this build.
func Normalize(id string) string {
	id = strings.TrimSpace(id)
	base, caps, hasCaps := strings.Cut(id, capabilitySep)
	if base != "" && base[0] >= '0' && base[0] <= '9' {
		base = "v" + base
	}
	if !hasCaps {
		return base
	}
	return base + capabilitySep + caps
}

// BaseVersion returns the normalised version part of an identity, without any
// capability suffix an older binary stamped. Callers classifying a build
// (release vs dev stamp) want this: a capability never made a dev stamp a
// release.
func BaseVersion(id string) string {
	base, _, _ := strings.Cut(Normalize(id), capabilitySep)
	return base
}

// SameIdentity reports whether two identities name the same build, comparing
// them in normalised form so a version-format difference is not mistaken for a
// version difference. A daemon stamped before normalisation landed differs once,
// is restarted once, and records the canonical form on the way back up.
func SameIdentity(a, b string) bool {
	return Normalize(a) == Normalize(b)
}

// version resolves the version part of the identity (see Identity).
func version() string {
	if Version != "dev" && Version != "" {
		return Version
	}
	// Binaries installed via `go install <module>@version` carry the module
	// version in their build info even without ldflags; prefer it over a stamp.
	// A working-tree build reports "(devel)" (or ""), which we skip.
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return Version
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return Version
	}
	return fmt.Sprintf("dev-%d-%d", fi.Size(), fi.ModTime().UnixNano())
}
