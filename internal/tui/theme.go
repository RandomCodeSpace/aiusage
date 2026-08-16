package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// Theme holds the intentional palette and the reusable lipgloss styles for the
// whole TUI. Colors are AdaptiveColor so the UI reads well in both light and
// dark terminals while keeping WCAG-AA contrast on both floors.
//
// Direction: a graphite "trading desk" dashboard. Exactly one cold-cyan
// interaction accent (the focus bar and the active chip), a warm amber
// "now"/scrub readout,
// and the per-component (input/output/cache) token series colored from the
// ANSI palette in buildCtx and rendered in every chart, bar and split.
type Theme struct {
	// Core palette. Bg/Surface/SurfaceHi/SurfaceTop are the 4-step elevation
	// ladder (views.ElevGround..views.ElevChip); Elev() hands them to the views
	// as an indexable ladder so no view ever names a color.
	Bg         compat.AdaptiveColor
	Surface    compat.AdaptiveColor
	SurfaceHi  compat.AdaptiveColor // focused pane / selected row floor (L2)
	SurfaceTop compat.AdaptiveColor // chips, active tab, table header band (L3)
	Border     compat.AdaptiveColor // the outer app frame — the ONE border
	Text       compat.AdaptiveColor
	Muted      compat.AdaptiveColor
	Faint      compat.AdaptiveColor // gridlines, rules, ghosted series, disabled

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
	Wordmark    lipgloss.Style
	Subtle      lipgloss.Style
	Crumb       lipgloss.Style
	CrumbActive lipgloss.Style
	PanelTitle  lipgloss.Style
	Stat        lipgloss.Style
	StatLabel   lipgloss.Style
	HeaderBar   lipgloss.Style
	FooterBar   lipgloss.Style
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
		Bg:         adaptive("#FBFCFE", "#0B0E14"),
		Surface:    adaptive("#F1F4F9", "#11161F"),
		SurfaceHi:  adaptive("#E7ECF4", "#161D29"),
		SurfaceTop: adaptive("#DCE4F0", "#1E2735"),
		Border:     adaptive("#D2DAE6", "#232B38"),
		Text:       adaptive("#10151D", "#E8EEF6"),
		Muted:      adaptive("#5A6B82", "#7C8DA6"),
		Faint:      adaptive("#9AA3AE", "#4A535F"),

		Accent: adaptive("#0E8C97", "#3DD6E0"),
		Now:    adaptive("#B5780A", "#F2B441"),

		Positive: adaptive("#1A7F37", "#56D364"),
		Good:     adaptive("#1A7F37", "#56D364"),
		Warn:     adaptive("#C0362C", "#E5534B"),
	}

	t.Title = lipgloss.NewStyle().Bold(true).Foreground(t.Text)
	t.Wordmark = lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	t.Subtle = lipgloss.NewStyle().Foreground(t.Muted)
	t.Crumb = lipgloss.NewStyle().Foreground(t.Muted)
	t.CrumbActive = lipgloss.NewStyle().Bold(true).Foreground(t.Accent)

	// An unfocused pane title is muted: the accent is the interaction color and
	// belongs to exactly one surface at a time (the focused pane's bar + title).
	t.PanelTitle = lipgloss.NewStyle().Bold(true).Foreground(t.Muted)

	t.Stat = lipgloss.NewStyle().Bold(true).Foreground(t.Text)
	t.StatLabel = lipgloss.NewStyle().Foreground(t.Muted)

	// The chrome bars are painted blocks too: header/breadcrumb/footer sit one
	// step above the app ground so the body's cards read as floating on them.
	t.HeaderBar = lipgloss.NewStyle().Foreground(t.Text).Background(t.SurfaceHi).Padding(0, 1)
	t.FooterBar = lipgloss.NewStyle().Foreground(t.Muted).Background(t.SurfaceHi).Padding(0, 1)

	t.Number = lipgloss.NewStyle().Foreground(t.Text)

	return t
}

// Elev is the 4-step elevation ladder handed to the views, indexed by
// views.ElevGround..views.ElevChip.
func (t Theme) Elev() [4]color.Color {
	return [4]color.Color{t.Bg, t.Surface, t.SurfaceHi, t.SurfaceTop}
}

// blockPad is the uniform card padding. It is deliberately the exact cell cost
// the rounded border + Padding(0,1) used to carry (4 columns, 2 rows) so every
// width/height budget in the views survives the borders being removed —
// lipgloss v2 Width/Height are border- AND padding-inclusive.
const (
	blockPadY = 1
	blockPadX = 2
)

// Idle returns the resting panel style: a painted card at elevation L1, no
// border. Borders retreat to the outer app frame (issue #22); depth is carried
// by the ladder and structure by the titled rule.
func (t Theme) Idle() lipgloss.Style {
	return lipgloss.NewStyle().Background(t.Surface).Padding(blockPadY, blockPadX)
}

// Errored returns the per-pane error card: a resting card whose content carries
// the warn color (the ✕ glyph + word is the mono channel).
func (t Theme) Errored() lipgloss.Style {
	return lipgloss.NewStyle().Background(t.Surface).Padding(blockPadY, blockPadX)
}

// AppFrame is the ink of the ONE border the design language allows: the outer
// app frame. It styles the frame's glyphs, which render.go lays out by hand —
// a bordered lipgloss style re-flows the whole frame and costs a third of the
// render budget. Everything inside the frame is painted, never boxed.
func (t Theme) AppFrame() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Border).Background(t.Bg)
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
	model.ToolGemini:     adaptive("#0F6FC4", "#6BC2FF"), // sky
	model.ToolAgy:        adaptive("#6E7781", "#8B949E"), // grey (no data)
	model.ToolPi:         adaptive("#8F5A00", "#D4A017"), // ochre
	model.ToolOpenClaw:   adaptive("#A14B1F", "#E08B5A"), // rust (pi's sibling)
	model.ToolCrush:      adaptive("#A93B6B", "#E086B0"), // rose
	model.ToolKimiCode:   adaptive("#2E6F4E", "#68C08E"), // jade
	model.ToolReasonix:   adaptive("#5B4BC4", "#9B8CF0"), // indigo
	model.ToolDSH:        adaptive("#1F6F7A", "#5FBDC9"), // teal
	model.ToolQwenCode:   adaptive("#5F7A0E", "#A3C93A"), // chartreuse
	model.ToolGoose:      adaptive("#8B36A9", "#C77BE0"), // orchid
	model.ToolCline:      adaptive("#0B7D6B", "#34D2B4"), // aqua
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
	model.ToolGemini:     "◇",
	model.ToolAgy:        "○",
	model.ToolPi:         "◐",
	model.ToolOpenClaw:   "◑",
	model.ToolCrush:      "▼",
	model.ToolKimiCode:   "◈",
	model.ToolReasonix:   "✧",
	model.ToolDSH:        "▣",
	model.ToolQwenCode:   "◉",
	model.ToolGoose:      "▽",
	model.ToolCline:      "★",
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
