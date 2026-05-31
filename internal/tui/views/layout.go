package views

// layout.go is the single source of truth for responsive layout. ComputeLayout
// turns a raw terminal size (w,h) into a Layout describing which regions render
// and at what size. Every view and the root frame read these derived flags
// instead of comparing against magic widths, so the breakpoint policy lives in
// exactly one place and is exhaustively unit-tested.
//
// The model is fluid/budget-driven, not a fixed tier table: a region appears
// only when the available cells clear its minimum useful size, and columns are
// sized proportionally from what is left. The few discrete switches that remain
// (the three nav modes) exist because a left rail physically cannot fit below a
// certain width — that is geometry, not a magic threshold.

// NavMode is how the view switcher is presented at the current width.
type NavMode int

const (
	// NavRail is the full left navigation rail (widest terminals).
	NavRail NavMode = iota
	// NavTabs is a top tab strip with all six labels (medium width).
	NavTabs
	// NavMini is a compact icon row + active label (phone-width terminals).
	NavMini
)

// ChartMode is how much chart a pane can afford in the current body.
type ChartMode int

const (
	// ChartNone means no graphical chart fits; render numbers only.
	ChartNone ChartMode = iota
	// ChartSpark means only a one-row sparkline fits.
	ChartSpark
	// ChartFull means a full axed line/bar chart fits.
	ChartFull
)

// Layout breakpoint constants, in terminal cells. These are the ONLY layout
// magic numbers in the TUI.
const (
	// MinUsableW / MinUsableH are the absolute floor; below either the UI cannot
	// render usefully and a resize card is shown instead.
	MinUsableW = 40
	MinUsableH = 10

	// minMainW / minSideW are the smallest widths at which the primary column and
	// the side panel are worth rendering. sideGutter is the column between them.
	minMainW   = 46
	minSideW   = 26
	sideGutter = 1

	// maxMainW caps the primary column so charts/tables don't stretch uselessly
	// on ultrawide terminals (the surplus becomes a centering margin).
	maxMainW = 200

	// minTabStripW is the width below which the six-label tab strip no longer
	// fits and the mini icon nav is used instead.
	minTabStripW = 56

	// minChartW / minChartH gate a full axed chart; minSparkW gates the
	// one-row sparkline fallback. Below minChartW a real line chart is too narrow
	// to read axes, and below minChartH too short to plot — both fall back to a
	// sparkline, then to numbers.
	minChartW = 48
	minChartH = 9
	minSparkW = 8

	// minBodyH is the smallest body the layout will hand a view (it may scroll);
	// denseH is the height below which optional rows (blank spacers, secondary
	// stats, the timeline readout) are dropped.
	minBodyH = 6
	denseH   = 16
)

// Layout is the computed responsive frame for one terminal size.
type Layout struct {
	W, H int

	// TooSmall is set when the terminal is below the absolute floor; when true
	// every other field is zero/false and only the resize card should render.
	TooSmall bool

	Nav   NavMode
	RailW int // on-screen rail width (0 unless Nav==NavRail)

	// Chrome rows; each present row costs one terminal line.
	ShowHeader     bool
	ShowBreadcrumb bool
	ShowFooter     bool
	ShowTabStrip   bool // a row above the body when Nav != NavRail

	// Body region handed to the active view (rail + chrome already subtracted).
	BodyW, BodyH int

	// Body sub-layout (budget-driven, continuous).
	SidePanel bool
	MainW     int // primary column width within the body
	SideW     int // side column width (0 unless SidePanel)

	ChartMode  ChartMode
	Sparklines bool // KPI / per-row micro-charts fit
	Readout    bool // timeline docked readout has a spare row
	Dense      bool // height is tight → views drop optional rows
}

// ComputeLayout derives the responsive Layout for a terminal of size w×h. It is
// a pure function: identical inputs always yield identical output.
func ComputeLayout(w, h int) Layout {
	l := Layout{W: w, H: h}
	if w < MinUsableW || h < MinUsableH {
		l.TooSmall = true
		return l
	}

	// Navigation is always a top strip (no left rail): a full tab strip while the
	// labels fit, else a compact icon row on phone widths.
	if w >= minTabStripW {
		l.Nav = NavTabs
	} else {
		l.Nav = NavMini
	}

	// Chrome. Header + footer are always present (both MaxWidth-clamp to one
	// line). A tab strip is an extra row whenever the rail is collapsed.
	l.ShowHeader = true
	l.ShowFooter = true
	l.ShowTabStrip = l.Nav != NavRail

	// Body width: the nav is a top strip (a row, not a column), so the body keeps
	// the full terminal width.
	bodyW := w
	if bodyW < 1 {
		bodyW = 1
	}

	// Body height is the exact residual after chrome — never floored above what
	// is actually available, or the frame would overrun the screen. The
	// breadcrumb row is claimed only when the body would still clear its floor;
	// on very short terminals the body simply gets less room (and scrolls).
	chrome := 2 // header + footer (both always shown)
	if l.ShowTabStrip {
		chrome++
	}
	if h-chrome-1 >= minBodyH {
		l.ShowBreadcrumb = true
		chrome++
	}
	bodyH := h - chrome
	if bodyH < 1 {
		bodyH = 1
	}
	l.BodyW = bodyW
	l.BodyH = bodyH

	// Body sub-layout. A side panel appears only when the primary column AND the
	// side panel both clear their minimums; otherwise the body is a single
	// column. The primary column is capped on ultrawide terminals.
	if bodyW >= minMainW+sideGutter+minSideW {
		side := bodyW * 32 / 100
		if side < minSideW {
			side = minSideW
		}
		if side > 40 {
			side = 40
		}
		main := bodyW - side - sideGutter
		if main > maxMainW {
			// Ultrawide: cap the primary column; let the side panel absorb a bit
			// more, still capped, so neither stretches absurdly.
			main = maxMainW
			side = bodyW - main - sideGutter
			if side > 56 {
				side = 56
			}
		}
		l.SidePanel = true
		l.MainW = main
		l.SideW = side
	} else {
		main := bodyW
		if main > maxMainW {
			main = maxMainW
		}
		l.MainW = main
	}

	l.applyHeightFlags(bodyH)
	return l
}

// applyHeightFlags derives the height-sensitive affordances (chart mode,
// sparklines, readout, dense) from the primary column width and a body height.
// Factored out so WithBodyHeight can recompute them when the body shrinks.
func (l *Layout) applyHeightFlags(bodyH int) {
	switch {
	case l.MainW >= minChartW && bodyH >= minChartH:
		l.ChartMode = ChartFull
	case l.MainW >= minSparkW:
		l.ChartMode = ChartSpark
	default:
		l.ChartMode = ChartNone
	}
	l.Sparklines = l.MainW >= 28 && bodyH >= minBodyH
	l.Readout = bodyH >= minChartH+1
	l.Dense = bodyH < denseH
}

// WithBodyHeight returns a copy of the layout with the body height replaced and
// the height-derived flags recomputed. Used when a transient overlay (the help
// panel) claims rows from the body, so views still fit the reduced region.
func (l Layout) WithBodyHeight(h int) Layout {
	if h < 1 {
		h = 1
	}
	l.BodyH = h
	l.applyHeightFlags(h)
	return l
}
