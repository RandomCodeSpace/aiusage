package tui

import (
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// Theme holds the intentional palette and the reusable lipgloss styles for the
// whole TUI. Colors are AdaptiveColor so the UI reads well in both light and
// dark terminals while keeping WCAG-AA contrast on both floors.
//
// Direction: a graphite "trading desk" dashboard. Exactly one cold-cyan
// interaction accent (also the focus ring), a warm amber "now"/scrub readout,
// and the per-component (input/output/cache) token series colored from the
// ANSI palette in buildCtx and rendered in every chart, bar and split.
type Theme struct {
	// Core palette.
	Bg          compat.AdaptiveColor
	Surface     compat.AdaptiveColor
	SurfaceHi   compat.AdaptiveColor // focused pane / hovered tile floor (+1 elevation)
	Border      compat.AdaptiveColor
	BorderFocus compat.AdaptiveColor // cyan focus ring ("you are here")
	Text        compat.AdaptiveColor
	Muted       compat.AdaptiveColor
	Faint       compat.AdaptiveColor // gridlines, ghosted/dimmed series, disabled

	// Semantic palette.
	Accent compat.AdaptiveColor // the ONE interaction accent (cold cyan)
	Now    compat.AdaptiveColor // live/today/scrub readout (warm amber)

	// The three token series (input, output, cache) are colored from the ANSI
	// palette in buildCtx so they adapt to the user's terminal theme; they are
	// not stored here. cache combines the DB read+creation sub-types on screen.

	Positive compat.AdaptiveColor // down-spend vs prior period (good)
	Good     compat.AdaptiveColor // alias of Positive
	Warn     compat.AdaptiveColor // up-spend / anomaly / error (red)

	// Reusable styles.
	Title       lipgloss.Style
	Subtle      lipgloss.Style
	Crumb       lipgloss.Style
	CrumbActive lipgloss.Style
	Panel       lipgloss.Style
	PanelTitle  lipgloss.Style
	Stat        lipgloss.Style
	StatLabel   lipgloss.Style
	HeaderBar   lipgloss.Style
	FooterBar   lipgloss.Style
	TabActive   lipgloss.Style
	TabInactive lipgloss.Style
	Number      lipgloss.Style
}

// adaptive builds a compat.AdaptiveColor from light/dark hex strings, keeping
// the palette literals as readable as the v1 AdaptiveColor{Light, Dark} form.
func adaptive(light, dark string) compat.AdaptiveColor {
	return compat.AdaptiveColor{Light: lipgloss.Color(light), Dark: lipgloss.Color(dark)}
}

// NewTheme builds the default theme.
func NewTheme() Theme {
	t := Theme{
		Bg:          adaptive("#FBFCFE", "#0B0E14"),
		Surface:     adaptive("#F1F4F9", "#11161F"),
		SurfaceHi:   adaptive("#E7ECF4", "#161D29"),
		Border:      adaptive("#D2DAE6", "#232B38"),
		BorderFocus: adaptive("#0E8C97", "#3DD6E0"),
		Text:        adaptive("#10151D", "#E8EEF6"),
		Muted:       adaptive("#5A6B82", "#7C8DA6"),
		Faint:       adaptive("#9AA3AE", "#4A535F"),

		Accent: adaptive("#0E8C97", "#3DD6E0"),
		Now:    adaptive("#B5780A", "#F2B441"),

		Positive: adaptive("#1A7F37", "#56D364"),
		Good:     adaptive("#1A7F37", "#56D364"),
		Warn:     adaptive("#C0362C", "#E5534B"),
	}

	t.Title = lipgloss.NewStyle().Bold(true).Foreground(t.Text)
	t.Subtle = lipgloss.NewStyle().Foreground(t.Muted)
	t.Crumb = lipgloss.NewStyle().Foreground(t.Muted)
	t.CrumbActive = lipgloss.NewStyle().Bold(true).Foreground(t.Accent)

	t.Panel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Padding(0, 1)
	t.PanelTitle = lipgloss.NewStyle().Bold(true).Foreground(t.Accent)

	t.Stat = lipgloss.NewStyle().Bold(true).Foreground(t.Text)
	t.StatLabel = lipgloss.NewStyle().Foreground(t.Muted)

	t.HeaderBar = lipgloss.NewStyle().Foreground(t.Text).Padding(0, 1)
	t.FooterBar = lipgloss.NewStyle().Foreground(t.Muted).Padding(0, 1)

	t.TabActive = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Bg).
		Background(t.Accent).
		Padding(0, 1)
	t.TabInactive = lipgloss.NewStyle().Foreground(t.Muted).Padding(0, 1)

	t.Number = lipgloss.NewStyle().Foreground(t.Text)

	return t
}

// Idle returns the resting panel style: rounded hairline border in Border, no
// elevated fill.
func (t Theme) Idle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Padding(0, 1)
}

// Focused returns the active-pane panel style. The selected pane is made
// unmistakable through THREE redundant channels (so it reads on any terminal,
// including monochrome): a THICK border glyph set (vs the idle rounded hairline
// — geometrically distinct), a bright cyan border foreground, and a one-step
// elevated fill. Both rounded and thick borders are 1 cell wide, so swapping
// does not disturb any pane's width math. Exactly one pane wears this at a time.
func (t Theme) Focused() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(t.BorderFocus).
		Background(t.SurfaceHi).
		Padding(0, 1)
}

// Errored returns a panel style with a red border for per-pane error states.
func (t Theme) Errored() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Warn).
		Padding(0, 1)
}

// toolAccents maps each known tool to a distinct accent color so per-tool bars
// and rows are visually distinguishable. copilot/gemini are nudged off cyan so
// they don't collide with the interaction Accent. Unknown tools fall back to the
// theme accent.
var toolAccents = map[string]compat.AdaptiveColor{
	model.ToolClaudeCode: adaptive("#B25000", "#E8924A"), // amber
	model.ToolCodex:      adaptive("#1A7F37", "#3FB950"), // green
	model.ToolCopilot:    adaptive("#0969DA", "#5C9CE6"), // blue
	model.ToolOpenCode:   adaptive("#7C3AED", "#A78BFA"), // violet
	model.ToolHermes:     adaptive("#BF3989", "#F778BA"), // magenta
	"gemini":             adaptive("#0F6FC4", "#6BC2FF"), // sky
	model.ToolAgy:        adaptive("#6E7781", "#8B949E"), // grey (no data)
}

// toolGlyphs maps each known tool to a stable glyph so legends/bars survive
// monochrome terminals (color is never the only channel). Unknown tools get a
// neutral bullet.
var toolGlyphs = map[string]string{
	model.ToolClaudeCode: "◆",
	model.ToolCodex:      "▲",
	model.ToolCopilot:    "●",
	model.ToolOpenCode:   "■",
	model.ToolHermes:     "✦",
	"gemini":             "◇",
	model.ToolAgy:        "○",
}

// canonicalTools is the canonical 7-tool set (constants + the string-keyed
// "gemini" accent which has no model.Tool constant).
var canonicalTools = []string{
	model.ToolClaudeCode,
	model.ToolCodex,
	model.ToolCopilot,
	model.ToolOpenCode,
	model.ToolHermes,
	"gemini",
	model.ToolAgy,
}

// ToolAccent returns the accent color for a tool, falling back to the theme
// accent when the tool is unknown.
func (t Theme) ToolAccent(tool string) compat.AdaptiveColor {
	if c, ok := toolAccents[tool]; ok {
		return c
	}
	return t.Accent
}

// ToolGlyph returns the stable glyph for a tool, falling back to a neutral
// bullet when the tool is unknown.
func (t Theme) ToolGlyph(tool string) string {
	if g, ok := toolGlyphs[tool]; ok {
		return g
	}
	return "·"
}
