package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestThemeInheritsTerminalBackgroundAndText(t *testing.T) {
	theme := NewTheme()
	for name, value := range map[string]any{
		"background": theme.Bg,
		"surface":    theme.Surface,
		"raised":     theme.SurfaceHi,
		"chip":       theme.SurfaceTop,
		"text":       theme.Text,
	} {
		if _, ok := value.(lipgloss.NoColor); !ok {
			t.Errorf("%s uses an application color: %T", name, value)
		}
	}

	base := lipgloss.NewStyle().Foreground(theme.Text).Background(theme.Bg).Render("base")
	if strings.Contains(base, "\x1b[") {
		t.Fatalf("terminal-native base emitted ANSI paint: %q", base)
	}
}
