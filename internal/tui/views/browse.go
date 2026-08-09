package views

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// Browse owns a bubbles table that lists the current grouping dimension as a
// borderless list, with a side preview pane (sparkline + stats) for the
// selected entity. The root model feeds it buckets via SetData on every
// range/sort/drill change and forwards navigation keys via Update.
//
// Browse owns the scroll window outright — cursor and top are the source of
// truth and the table is handed ONLY the rows that fit on screen. That is a
// deliberate inversion of the widget's default: bubbles v2 scrolls internally
// (it renders rows start..end into a viewport at its own YOffset, neither of
// which it exports), so with the full row set the i-th rendered line stops
// being row i the moment the list is taller than the pane — and the per-row
// click zones markedRows lays down are keyed by line. Keeping the window here
// makes "rendered line i is row top+i" an invariant of this file instead of a
// coincidence of the widget's scroll state.
type Browse struct {
	table      table.Model
	ctx        Ctx
	dim        string
	rows       []store.Bucket
	grand      int64
	preview    []store.Bucket // selected row's daily trend
	previewErr bool           // the preview trend query failed (distinct from "no rows")
	cols       []table.Column // current columns (for per-cell right-alignment)
	lay        Layout         // central responsive layout (drives widths + preview)
	width      int
	height     int
	compact    bool
	focused    int  // PaneBrowse* — which pane wears the ring
	drillable  bool // rows still descend a level (chevron affordance)
	cursor     int  // selected row, indexing rows (NOT the table's window)
	top        int  // first row of the on-screen window, indexing rows
}

// Browse view panes (pane 0 = rail).
const (
	PaneBrowseTable = iota
	PaneBrowsePreview
)

// NewBrowse builds an empty Browse view. The Ctx is wired at construction, not
// only via SetData: with async loads the view can be rendered before its first
// data arrives, and the empty-state frame must not draw with zero-value styles
// (a zero compat.AdaptiveColor panics inside lipgloss).
func NewBrowse(c Ctx) Browse {
	t := table.New(table.WithFocused(true), table.WithHeight(10))
	return Browse{table: t, ctx: c}
}

// Dim is the dimension currently displayed (tool/model/project/session).
func (b Browse) Dim() string { return b.dim }

// SelectedValue returns the grouping value of the highlighted row, or "" when
// there are no rows.
func (b Browse) SelectedValue() (string, bool) {
	sb, ok := b.SelectedBucket()
	if !ok {
		return "", false
	}
	return sb.Keys[b.dim], true
}

// SelectedBucket returns the highlighted bucket.
func (b Browse) SelectedBucket() (store.Bucket, bool) {
	if b.cursor < 0 || b.cursor >= len(b.rows) {
		return store.Bucket{}, false
	}
	return b.rows[b.cursor], true
}

// Cursor returns the current row index.
func (b Browse) Cursor() int { return b.cursor }

// RowCount returns the number of rows currently displayed.
func (b Browse) RowCount() int { return len(b.rows) }

// WindowTop returns the index of the first row currently on screen. The click
// zones are keyed by absolute row index, so this is only exported for the tests
// that pin the window contract.
func (b Browse) WindowTop() int { return b.top }

// SetCursor sets the current row index (clamped) and scrolls the window when
// the selection would leave it.
func (b *Browse) SetCursor(i int) {
	if len(b.rows) == 0 {
		return
	}
	prev := b.top
	b.cursor = i
	b.reframe()
	if b.top != prev {
		// The window moved: the table is holding the wrong slice of rows.
		b.applyRows()
		return
	}
	b.table.SetCursor(b.cursor - b.top)
}

// windowH is the number of data rows on screen: the embedded table's viewport
// height, which its View() always pads to exactly.
func (b Browse) windowH() int {
	if h := b.table.Height(); h > 0 {
		return h
	}
	return 1
}

// reframe clamps the cursor into range and slides the window so the cursor sits
// inside it. The window is never longer than the viewport, which is what keeps
// the table from scrolling itself (see the type comment).
func (b *Browse) reframe() {
	n := len(b.rows)
	if n == 0 {
		b.cursor, b.top = 0, 0
		return
	}
	if b.cursor < 0 {
		b.cursor = 0
	}
	if b.cursor >= n {
		b.cursor = n - 1
	}
	h := b.windowH()
	maxTop := n - h
	if maxTop < 0 {
		maxTop = 0
	}
	if b.top > maxTop {
		b.top = maxTop
	}
	if b.top < 0 {
		b.top = 0
	}
	if b.cursor < b.top {
		b.top = b.cursor
	}
	if b.cursor > b.top+h-1 {
		b.top = b.cursor - h + 1
	}
}

// SetFocusedPane records which pane within Browse wears the ring.
func (b *Browse) SetFocusedPane(p int) { b.focused = p }

// SetDrillable records whether pressing a row still descends a level, which is
// what the per-row chevron announces. It is resolved by the root model at render
// time (the depth it derives from is navigation state, not loaded data) and only
// changes the glyph — the slot itself is reserved at every depth, so the columns
// never reflow as you drill.
func (b *Browse) SetDrillable(v bool) { b.drillable = v }

// SetPreview sets the selected entity's trend buckets for the preview pane.
func (b *Browse) SetPreview(trend []store.Bucket) { b.preview = trend }

// SetPreviewErr marks whether the preview trend query failed, so the pane can
// render the query-failed treatment instead of an ambiguous blank strip.
func (b *Browse) SetPreviewErr(failed bool) { b.previewErr = failed }

// PreviewErr reports whether the preview trend query failed. The detail-flight
// dispatcher reads it off the flight's model copy to carry the failure back to
// the UI thread.
func (b Browse) PreviewErr() bool { return b.previewErr }

// SetLayout updates the render area + columns from the central responsive
// layout. The preview pane shows only when the layout grants a side panel; the
// table panel takes the primary column (or the whole body otherwise).
func (b *Browse) SetLayout(lay Layout) {
	if lay == b.lay {
		// Identical geometry: everything derived below is already in place.
		// Skipping avoids the double column+row build on every data load, whose
		// loadBrowse calls SetData (rows built) and then relayouts unchanged.
		return
	}
	b.lay = lay
	b.width = lay.BodyW
	b.height = lay.BodyH
	b.compact = !lay.SidePanel
	// The table lives inside a card whose total on-screen width is tablePanelW.
	// The card's uniform padding costs 2 columns a side, so the usable text area
	// is tablePanelW - 4. A further rowFocusGutter is reserved for the
	// width-invariant focus bar prefixed to every row (markedRows), which is what
	// carries the cursor in monochrome — the table's Selected background does not
	// — and rowDrillGutter for the chevron that says the row descends.
	b.table.SetWidth(b.tablePanelW() - 4 - rowFocusGutter - rowDrillGutter)
	// Card = title rule(1) + table(h) + padding(2); fit the table to bodyH so the
	// card never exceeds the body region.
	th := lay.BodyH - 3
	if th < 1 {
		th = 1
	}
	b.table.SetHeight(th)
	b.applyColumns()
	b.applyRows()
}

// tablePanelW is the total on-screen width (content + rounded border) of the
// table panel: the full body when there is no side panel, else the primary
// column. Single source of truth so tablePanel/previewPanel agree.
func (b Browse) tablePanelW() int {
	if !b.lay.SidePanel {
		return b.width
	}
	return b.lay.MainW
}

// previewPanelW is the total on-screen width of the preview pane (0 when no side
// panel is granted).
func (b Browse) previewPanelW() int {
	if !b.lay.SidePanel {
		return 0
	}
	return b.lay.SideW
}

// SetData replaces the displayed grouping. cursor is preserved when possible.
func (b *Browse) SetData(c Ctx, dim string, rows []store.Bucket, grand int64) {
	b.dim = dim
	b.rows = rows
	b.grand = grand
	b.ctx = c
	// A cursor left over from a longer grouping is reset rather than clamped:
	// row 7 of the previous dimension has nothing to do with row 7 of this one.
	// (This is also why the cursor lives here and not in the widget — bubbles v2
	// table.SetRows clamps its own cursor to -1 while the table is empty and
	// never restores it when rows arrive.)
	if b.cursor < 0 || b.cursor >= len(rows) {
		b.cursor = 0
	}
	b.applyColumns()
	b.applyRows()
}

// ApplyStyles wires the table styles from the injected context once at startup.
func (b *Browse) ApplyStyles(header, cell, selected lipgloss.Style) {
	b.table.SetStyles(table.Styles{Header: header, Cell: cell, Selected: selected})
}

// Update applies a navigation key to the selection. The key set is the embedded
// table's own KeyMap (line/page/half-page/top/bottom), so the bindings a reader
// learned from the widget still hold — but the movement is applied to Browse's
// cursor rather than handed to table.Update, which would move the widget's
// private scroll offset and break the "line i is row top+i" zone contract.
func (b Browse) Update(msg tea.Msg) (Browse, tea.Cmd) {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return b, nil
	}
	if d, ok := b.navDelta(km); ok {
		b.SetCursor(b.cursor + d)
	}
	return b, nil
}

// navDelta maps a key press to a cursor step via the table's KeyMap. The
// jump-to-end deltas are whole-list sized; SetCursor clamps.
func (b Browse) navDelta(msg tea.KeyPressMsg) (int, bool) {
	page := b.windowH()
	km := b.table.KeyMap
	switch {
	case key.Matches(msg, km.LineUp):
		return -1, true
	case key.Matches(msg, km.LineDown):
		return +1, true
	case key.Matches(msg, km.PageUp):
		return -page, true
	case key.Matches(msg, km.PageDown):
		return +page, true
	case key.Matches(msg, km.HalfPageUp):
		return -page / 2, true
	case key.Matches(msg, km.HalfPageDown):
		return +page / 2, true
	case key.Matches(msg, km.GotoTop):
		return -len(b.rows), true
	case key.Matches(msg, km.GotoBottom):
		return +len(b.rows), true
	}
	return 0, false
}

// rowFocusGutter is the cell cost of the per-row focus bar slot (bar + space).
// It is invariant across widths and across the cursor moving.
const rowFocusGutter = 2

// rowDrillGutter is the cell cost of the per-row drill slot (chevron + space).
// It is reserved at EVERY drill depth, including the deepest one where no row
// descends, so the columns do not shift under the reader as they drill.
const rowDrillGutter = 2

// View renders the table plus (when wide enough) the side preview pane.
func (b Browse) View() string {
	c := b.ctx
	if len(b.rows) == 0 {
		focused := b.focused == PaneBrowseTable
		elev := paneElev(focused)
		c = c.On(elev)
		w := maxInt(b.width, 22)
		return c.Block(elev).Width(w).Render(
			c.titleRule(strings.ToUpper(title(b.dim)), w-4, focused) + "\n" +
				emptyChartFrame(c, maxInt(b.width-4, 16), maxInt(b.height-3, 4)),
		)
	}
	tableStr := c.mark(ZoneTable, b.tablePanel())
	if b.compact {
		return tableStr
	}
	preview := b.previewPanel()
	return lipgloss.JoinHorizontal(lipgloss.Top, tableStr, " ", preview)
}

// tablePanel wraps the table in a focus-aware painted card with per-row click
// zones.
func (b Browse) tablePanel() string {
	focused := b.focused == PaneBrowseTable
	elev := paneElev(focused)
	c := b.ctx.On(elev)
	body := b.markedRows(c)
	style := c.Block(elev).Width(b.tablePanelW())
	return style.Render(c.titleRule(strings.ToUpper(title(b.dim)), b.tablePanelW()-4, focused) + "\n" + body)
}

// markedRows renders the table view, prefixes every line with the row focus
// slot (the bar on the cursor row, a blank cell elsewhere — the monochrome
// channel for "which row is selected", which the Selected style's background
// cannot carry) and the drill slot (the chevron that says pressing the row
// descends), and wraps each data row in a click zone.
//
// The zone spans the WHOLE composed line, gutters included: the chevron is the
// affordance, so it has to be inside the target it advertises.
//
// Data line i carries row top+i because Browse hands the table only the rows
// that fit (see the type comment); the zone id is that absolute row index, so a
// press acts on the entity the reader pointed at no matter how far the list has
// been scrolled.
func (b Browse) markedRows(c Ctx) string {
	lines := strings.Split(b.table.View(), "\n")
	for i := range lines {
		if i == 0 { // header band
			lines[i] = c.pad(rowFocusGutter+rowDrillGutter) + lines[i]
			continue
		}
		rowIdx := b.top + i - 1
		onScreen := rowIdx < len(b.rows) // the viewport pads short windows with blanks
		lines[i] = c.FocusMark(onScreen && rowIdx == b.cursor) + c.pad(1) +
			c.DrillMark(b.drillable && onScreen) + c.pad(1) + lines[i]
		if onScreen {
			lines[i] = c.mark(RowZone(rowIdx), lines[i])
		}
	}
	return strings.Join(lines, "\n")
}

// previewPanel renders the selected entity's per-series trend + four-component
// breakdown. Read-only (the table is the interactive surface).
func (b Browse) previewPanel() string {
	prevW := b.previewPanelW()
	pfocus := b.focused == PaneBrowsePreview
	elev := paneElev(pfocus)
	c := b.ctx.On(elev)
	// Fill the card to the body height so the preview matches the table panel's
	// height instead of floating short above empty terminal.
	style := c.Block(elev).Width(prevW).Height(maxInt(b.height, 3))
	inner := prevW - 4

	sb, ok := b.SelectedBucket()
	if !ok {
		return c.mark(ZonePreview, style.Render(c.titleRule("PREVIEW", inner, pfocus)+"\n"+c.Faint.Render("no selection")))
	}
	name := sb.Keys[b.dim]
	comp := Split(sb)
	sum := comp.Sum()
	trend := trendStrip(c, b.preview, inner, len(c.Comp))
	if b.previewErr {
		trend = EmptyState(c, EmptyQueryFailed, inner)
	}
	lines := []string{
		c.Stat.Render(displayName(c, name, inner)),
		c.Rule(c.StatLabel.Render("TREND"), inner),
		trend,
		c.Faint.Render(strings.Repeat("─", inner)),
	}
	for _, s := range c.Comp {
		lines = append(lines, c.compStyle(s).Render(c.PadRight(s.Short, 7))+c.pad(1)+
			c.Number.Render(c.Humanize(s.Pick(comp))+" ("+c.Percent(s.Pick(comp), sum)+")"))
	}
	lines = append(lines,
		c.StatLabel.Render("events ")+c.Number.Render(c.Humanize(sb.Events)),
		c.StatLabel.Render("total  ")+c.Number.Render(c.Humanize(sb.Total)),
	)
	return c.mark(ZonePreview, style.Render(c.titleRule("PREVIEW", inner, pfocus)+"\n"+strings.Join(lines, "\n")))
}

func (b *Browse) applyColumns() {
	w := b.table.Width()
	if w < 20 {
		w = 20
	}
	numW, evW, totW := 8, 7, 9
	ra := func(s string, wd int) string { // right-align a header title over its numbers
		if len(s) >= wd {
			return s
		}
		return strings.Repeat(" ", wd-len(s)) + s
	}
	n := len(b.ctx.Comp)
	// One PaddingRight gutter per column (name + events + n comps + total) + 1 safety.
	reserve := (n + 2) + 1
	fullMinW := 10 + evW + numW*n + totW + reserve
	var cols []table.Column
	// Full per-component breakdown when the table is wide enough; otherwise
	// name/events/total (the side preview carries the breakdown). No trend column.
	if n > 0 && w >= fullMinW {
		nameW := w - evW - numW*n - totW - reserve
		if nameW < 8 {
			nameW = 8
		}
		cols = append(cols,
			table.Column{Title: title(b.dim), Width: nameW},
			table.Column{Title: ra("events", evW), Width: evW},
		)
		for _, s := range b.ctx.Comp {
			cols = append(cols, table.Column{Title: ra(s.Short, numW), Width: numW})
		}
		cols = append(cols, table.Column{Title: ra("total", totW), Width: totW})
	} else {
		// 3 columns each carry a 1-col gutter (reserve 3) + 1 safety.
		nameW := w - evW - totW - 3 - 1
		if nameW < 8 {
			nameW = 8
		}
		cols = []table.Column{
			{Title: title(b.dim), Width: nameW},
			{Title: ra("events", evW), Width: evW},
			{Title: ra("total", totW), Width: totW},
		}
	}
	b.table.SetColumns(cols)
	b.cols = cols
}

func (b *Browse) applyRows() {
	c := b.ctx
	b.reframe()
	end := b.top + b.windowH()
	if end > len(b.rows) {
		end = len(b.rows)
	}
	window := b.rows[b.top:end]
	colW := func(i int) int { // width of column i (0 before columns applied)
		if i < len(b.cols) {
			return b.cols[i].Width
		}
		return 8
	}
	rnum := func(v int64, w int) string {
		s := hl(c, v)
		if c.PadLeft != nil {
			return c.PadLeft(s, w)
		}
		return s
	}
	full := len(b.cols) == len(c.Comp)+3
	out := make([]table.Row, 0, len(window))
	for _, r := range window {
		name := r.Keys[b.dim]
		if name == "" {
			name = "—"
		}
		if full {
			comp := Split(r)
			row := table.Row{glyphName(c, b.dim, name), rnum(r.Events, colW(1))}
			for i, s := range c.Comp {
				row = append(row, rnum(s.Pick(comp), colW(2+i)))
			}
			row = append(row, rnum(r.Total, colW(2+len(c.Comp))))
			out = append(out, row)
		} else {
			out = append(out, table.Row{
				glyphName(c, b.dim, name),
				rnum(r.Events, colW(1)),
				rnum(r.Total, colW(2)),
			})
		}
	}
	b.table.SetRows(out)
	// The widget's cursor is window-relative; Browse's is absolute.
	b.table.SetCursor(b.cursor - b.top)
}

// glyphName prefixes a tool-dim row with its tool glyph (other dims pass the
// name through). Keeps color out (the table cell style governs that) but the
// glyph survives monochrome.
func glyphName(c Ctx, dim, name string) string {
	if dim == "tool" && c.ToolGlyph != nil {
		return c.ToolGlyph(name) + " " + name
	}
	return name
}

func hl(c Ctx, v int64) string {
	if c.Humanize == nil {
		return ""
	}
	return c.Humanize(v)
}

func title(dim string) string {
	if dim == "" {
		return "name"
	}
	return strings.ToUpper(dim[:1]) + dim[1:]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
