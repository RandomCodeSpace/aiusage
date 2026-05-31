// Package buildinfo exposes a single build identity used to keep the CLI and the
// background daemon on the same code: when the CLI's identity differs from the
// running daemon's, the CLI restarts the daemon (see cmd.ensureDaemon).
//
// Version is overridable at build time:
//
//	go build -ldflags "-X aiusage/internal/buildinfo.Version=v1.2.3"
//
// When Version is left at its "dev" default, Identity() falls back to a stamp
// derived from the running executable's size + modtime, so a freshly built or
// reinstalled binary always has a distinct identity and the daemon is restarted
// automatically — no manual version bump required.
package buildinfo

import (
	"fmt"
	"os"
)

// Version is the declared build version. Override via -ldflags for a real
// release; otherwise it stays "dev" and Identity() derives a per-build stamp.
var Version = "dev"

// Identity returns a stable identifier for this build. A real (non-"dev")
// Version is returned verbatim; otherwise it is "dev-<size>-<modtimeUnixNano>"
// of the running executable, which changes on every rebuild/reinstall. If the
// executable cannot be stat'd, the bare Version is returned (degrades to "always
// matches", which is safe — it just disables auto-restart).
func Identity() string {
	if Version != "dev" && Version != "" {
		return Version
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
