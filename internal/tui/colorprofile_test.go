package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestStyledRenderEmitsSGR guards the lipgloss v2 rendering contract these
// tests rely on: Style.Render always emits full-fidelity ANSI regardless of
// the output device (downsampling moved to print time), so color assertions
// under `go test` — where stdout is not a TTY — are never vacuous. In v1 this
// needed an explicit SetColorProfile pin; v2 has no profile on the render path.
func TestStyledRenderEmitsSGR(t *testing.T) {
	out := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5f87")).Render("styled")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("no SGR codes in styled render: %q", out)
	}
}
