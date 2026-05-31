package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/store"
)

// Browse owns a bubbles table that lists the current grouping dimension as a
// borderless list, with a side preview pane (sparkline + stats) for the
// selected entity. The root model feeds it buckets via SetData on every
// range/sort/drill change and forwards navigation keys via Update.
type Browse struct {
	table   table.Model
	ctx     Ctx
	dim     string
	rows    []store.Bucket
	grand   int64
	preview []store.Bucket // selected row's daily trend
	cols    []table.Column // current columns (for per-cell right-alignment)
	lay     Layout         // central responsive layout (drives widths + preview)
	width   int
	height  int
	compact bool
	focused int // PaneBrowse* — which pane wears the ring
}

// Browse view panes (pane 0 = rail).
const (
	PaneBrowseTable = iota
	PaneBrowsePreview
)

// NewBrowse builds an empty Browse view.
func NewBrowse() Browse {
	t := table.New(table.WithFocused(true), table.WithHeight(10))
	return Browse{table: t}
}

// Dim is the dimension currently displayed (tool/model/project/session).
func (b Browse) Dim() string { return b.dim }

// SelectedValue returns the grouping value of the highlighted row, or "" when
// there are no rows.
func (b Browse) SelectedValue() (string, bool) {
	if len(b.rows) == 0 {
		return "", false
	}
	idx := b.table.Cursor()
	if idx < 0 || idx >= len(b.rows) {
		return "", false
	}
	return b.rows[idx].Keys[b.dim], true
}

// SelectedBucket returns the highlighted bucket.
func (b Browse) SelectedBucket() (store.Bucket, bool) {
	if len(b.rows) == 0 {
		return store.Bucket{}, false
	}
	idx := b.table.Cursor()
	if idx < 0 || idx >= len(b.rows) {
		return store.Bucket{}, false
	}
	return b.rows[idx], true
}

// Cursor returns the current row index.
func (b Browse) Cursor() int { return b.table.Cursor() }

// RowCount returns the number of rows currently displayed.
func (b Browse) RowCount() int { return len(b.rows) }

// SetCursor sets the current row index (clamped).
func (b *Browse) SetCursor(i int) {
	if i < 0 {
		i = 0
	}
	if i >= len(b.rows) {
		i = len(b.rows) - 1
	}
	if i >= 0 {
		b.table.SetCursor(i)
	}
}

// SetFocusedPane records which pane within Browse wears the ring.
func (b *Browse) SetFocusedPane(p int) { b.focused = p }

// SetPreview sets the selected entity's trend buckets for the preview pane.
func (b *Browse) SetPreview(trend []store.Bucket) { b.preview = trend }

// SetLayout updates the render area + columns from the central responsive
// layout. The preview pane shows only when the layout grants a side panel; the
// table panel takes the primary column (or the whole body otherwise).
func (b *Browse) SetLayout(lay Layout) {
	b.lay = lay
	b.width = lay.BodyW
	b.height = lay.BodyH
	b.compact = !lay.SidePanel
	// The table lives inside a panel whose total on-screen width is tablePanelW.
	// The panel style (Idle/Focused) adds a 1-cell border AND Padding(0,1) on
	// each side, so the usable text area is tablePanelW - 4 (border 2 + pad 2).
	// Sizing the table to only -2 made it overflow the text area by 2 cells, and
	// lipgloss word-wrapped the trailing "total" column onto its own line.
	b.table.SetWidth(b.tablePanelW() - 4)
	// Panel = title(1) + table(h) + border(2); fit table to bodyH so the panel
	// never exceeds the body region.
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
	b.applyColumns()
	b.applyRows()
	if b.table.Cursor() >= len(rows) {
		b.table.SetCursor(0)
	}
}

// ApplyStyles wires the table styles from the injected context once at startup.
func (b *Browse) ApplyStyles(header, cell, selected lipgloss.Style) {
	b.table.SetStyles(table.Styles{Header: header, Cell: cell, Selected: selected})
}

// Update forwards a tea.Msg (navigation keys) to the embedded table.
func (b Browse) Update(msg tea.Msg) (Browse, tea.Cmd) {
	var cmd tea.Cmd
	b.table, cmd = b.table.Update(msg)
	return b, cmd
}

// View renders the table plus (when wide enough) the side preview pane.
func (b Browse) View() string {
	c := b.ctx
	if len(b.rows) == 0 {
		return c.panelStyle(b.focused == PaneBrowseTable).Width(maxInt(b.width-2, 20)).Render(
			c.PanelTitle.Render(strings.ToUpper(title(b.dim))) + "\n" +
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

// tablePanel wraps the table in a focus-aware panel with per-row click zones.
func (b Browse) tablePanel() string {
	c := b.ctx
	body := b.markedRows()
	focused := b.focused == PaneBrowseTable
	style := c.panelStyle(focused).Width(b.tablePanelW() - 2)
	return style.Render(c.titleChip(strings.ToUpper(title(b.dim)), focused) + "\n" + body)
}

// markedRows renders the table view, then wraps each visible row line in a row
// click zone so the mouse can select rows.
func (b Browse) markedRows() string {
	view := b.table.View()
	c := b.ctx
	if c.Zone == nil {
		return view
	}
	lines := strings.Split(view, "\n")
	// The first line is the header; data rows follow in cursor order.
	for i := 1; i < len(lines); i++ {
		rowIdx := i - 1
		if rowIdx < len(b.rows) {
			lines[i] = c.mark(RowZone(rowIdx), lines[i])
		}
	}
	return strings.Join(lines, "\n")
}

// previewPanel renders the selected entity's per-series trend + four-component
// breakdown. Read-only (the table is the interactive surface).
func (b Browse) previewPanel() string {
	c := b.ctx
	prevW := b.previewPanelW()
	pfocus := b.focused == PaneBrowsePreview
	// Fill the box to the body height (border = 2 rows) so the preview matches the
	// table panel's height instead of floating short above empty terminal.
	style := c.panelStyle(pfocus).Width(prevW - 2).Height(maxInt(b.height-2, 1))
	inner := prevW - 4

	sb, ok := b.SelectedBucket()
	if !ok {
		return c.mark(ZonePreview, style.Render(c.titleChip("PREVIEW", pfocus)+"\n"+c.Faint.Render("no selection")))
	}
	name := sb.Keys[b.dim]
	comp := Split(sb)
	sum := comp.Sum()
	lines := []string{
		c.Stat.Render(displayName(c, name, inner)),
		c.Faint.Render(strings.Repeat("─", inner)),
		trendStrip(c, b.preview, inner, len(c.Comp)),
		c.Faint.Render(strings.Repeat("─", inner)),
	}
	for _, s := range c.Comp {
		lines = append(lines, s.Style().Render(c.PadRight(s.Short, 7))+" "+
			c.Number.Render(c.Humanize(s.Pick(comp))+" ("+c.Percent(s.Pick(comp), sum)+")"))
	}
	lines = append(lines,
		c.StatLabel.Render("events ")+c.Number.Render(c.Humanize(sb.Events)),
		c.StatLabel.Render("total  ")+c.Number.Render(c.Humanize(sb.Total)),
	)
	return c.mark(ZonePreview, style.Render(c.titleChip("PREVIEW", pfocus)+"\n"+strings.Join(lines, "\n")))
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
	out := make([]table.Row, 0, len(b.rows))
	for _, r := range b.rows {
		name := r.Keys[b.dim]
		if name == "" {
			name = "—"
		}
		if full {
			comp := Split(r)
			row := table.Row{glyphName(c, b.dim, r, name), rnum(r.Events, colW(1))}
			for i, s := range c.Comp {
				row = append(row, rnum(s.Pick(comp), colW(2+i)))
			}
			row = append(row, rnum(r.Total, colW(2+len(c.Comp))))
			out = append(out, row)
		} else {
			out = append(out, table.Row{
				glyphName(c, b.dim, r, name),
				rnum(r.Events, colW(1)),
				rnum(r.Total, colW(2)),
			})
		}
	}
	b.table.SetRows(out)
}

// glyphName prefixes a tool-dim row with its tool glyph (other dims pass the
// name through). Keeps color out (the table cell style governs that) but the
// glyph survives monochrome.
func glyphName(c Ctx, dim string, r store.Bucket, name string) string {
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
