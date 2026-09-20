package views

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

func TestSelectedChipPreservesContrastOnNativeGround(t *testing.T) {
	accent := compat.AdaptiveColor{Light: lipgloss.Color("#7B268F"), Dark: lipgloss.Color("#D4A2EF")}
	c := Ctx{AccentColor: accent, Elev: [4]color.Color{lipgloss.NoColor{}}}
	style := c.chipStyle(ChipAccent, accent)
	if !style.GetBold() || style.GetForeground() != accent {
		t.Fatal("native selected chip must use bold accent text")
	}
	if _, native := style.GetBackground().(lipgloss.NoColor); !native {
		t.Fatal("native selected chip painted a background with unknown contrast")
	}
	selected := workspaceANSI.ReplaceAllString(c.Chip(ChipAccent, accent, true, true, "Overview"), "")
	resting := c.Chip(ChipCard, accent, true, false, "Overview")
	if !strings.Contains(selected, FocusBar+" Overview") || lipgloss.Width(selected) != lipgloss.Width(resting) {
		t.Fatal("native selected chip changed its selection marker or width")
	}
}

func TestSelectedChipKeepsExplicitGroundInk(t *testing.T) {
	accent := compat.AdaptiveColor{Light: lipgloss.Color("#7B268F"), Dark: lipgloss.Color("#D4A2EF")}
	ground := lipgloss.Color("#15202A")
	c := Ctx{AccentColor: accent, Elev: [4]color.Color{ground}}
	style := c.chipStyle(ChipAccent, accent)
	if style.GetBackground() != accent || style.GetForeground() != ground {
		t.Fatal("explicit selected-chip colors changed")
	}
}
