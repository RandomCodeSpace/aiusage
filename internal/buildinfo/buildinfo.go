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
// modtime, so a freshly built or reinstalled binary always has a distinct identity
// and the daemon is restarted automatically — no manual version bump required.
package buildinfo

import (
	"fmt"
	"os"
	"runtime/debug"
)

// Version is the declared build version. Override via -ldflags for a real
// release; otherwise it stays "dev" and Identity() derives a per-build stamp.
var Version = "dev"

// Identity returns a stable identifier for this build. A real (non-"dev")
// Version is returned verbatim. Otherwise, if the binary was produced by
// `go install <module>@vX.Y.Z`, the module version embedded by the toolchain is
// returned. Failing that it is "dev-<size>-<modtimeUnixNano>" of the running
// executable, which changes on every rebuild/reinstall. If the executable cannot
// be stat'd, the bare Version is returned (degrades to "always matches", which is
// safe — it just disables auto-restart).
func Identity() string {
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
