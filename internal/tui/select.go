package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

// select.go centralises selection + scrub-sync helpers shared by the keyboard
// and mouse handlers so the two input paths mutate identical state.

// selectionRows returns the rows for the current bar-based view (By-Tool /
// By-Model), or nil.
func (m Model) selectionRows() []store.Bucket {
	switch m.view {
	case ViewByTool:
		return m.byTool.Rows
	case ViewByModel:
		return m.byModel.Rows
	default:
		return nil
	}
}

// selectionCount returns the number of selectable rows in the current view.
// Activity is counted off its own rows: they are activity buckets, a different
// shape from the usage buckets selectionRows deals in, and folding the two into
// one slice type would mean projecting one onto the other somewhere.
func (m Model) selectionCount() int {
	if m.view == ViewActivity {
		return m.activity.RowCount()
	}
	return len(m.selectionRows())
}

// byToolFoldSelected reports whether the By-Tool cursor is on the synthetic
// long-tail row. It is the guard every per-tool operation needs: the row carries
// a real bucket but names no tool, so anything that would query or filter by its
// name has to stop here.
func (m Model) byToolFoldSelected() bool {
	return m.view == ViewByTool && m.byTool.FoldIndex >= 0 && m.byTool.Selected == m.byTool.FoldIndex
}

// currentSelection returns the active bar index for the current view.
func (m Model) currentSelection() int {
	switch m.view {
	case ViewByTool:
		return m.byTool.Selected
	case ViewByModel:
		return m.byModel.Selected
	case ViewActivity:
		return m.activity.Selected
	default:
		return 0
	}
}

// setSelection clamps and applies a bar selection, repricing the selected
// entity's detail from cache only — a miss schedules a debounced background
// load (detail.go), so selection storms never query on the UI thread.
func (m *Model) setSelection(i int) {
	n := m.selectionCount()
	if n == 0 {
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	switch m.view {
	case ViewByTool:
		m.byTool.Selected = i
		m.syncByToolDetail()
	case ViewByModel:
		m.byModel.Selected = i
		m.syncByModelDetail()
	case ViewActivity:
		// No detail leg: the Activity detail card is a pure projection of the
		// selected row, so moving the selection queries nothing, warm or cold.
		m.activity.Selected = i
	}
}

// moveSelection steps the bar selection by dir.
func (m *Model) moveSelection(dir int) { m.setSelection(m.currentSelection() + dir) }

// moveSelectionFromKey maps the bound Up/Down keys to selection steps on bar
// views, so a rebinding moves selection (and help) together.
func (m *Model) moveSelectionFromKey(msg tea.Msg) {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return
	}
	switch {
	case key.Matches(km, m.keys.Up):
		m.moveSelection(-1)
	case key.Matches(km, m.keys.Down):
		m.moveSelection(+1)
	}
}

// selectionDim is the grouping key the current bar view selects on.
func (m Model) selectionDim() string {
	if m.view == ViewByModel {
		return "model"
	}
	return "tool"
}

// selectBar selects the bar matching a clicked name on By-Tool / By-Model. The
// Overview tool bars are read-only (no selection), so a name that isn't found is
// simply a no-op.
func (m *Model) selectBar(name string) {
	dim := m.selectionDim()
	for i, b := range m.selectionRows() {
		if b.Keys[dim] == name {
			m.setSelection(i)
			return
		}
	}
}

// barSelected reports whether name is the bar the current view ALREADY has
// selected — the test behind "a second press on the selected row drills".
func (m Model) barSelected(name string) bool {
	rows := m.selectionRows()
	i := m.currentSelection()
	if i < 0 || i >= len(rows) {
		return false
	}
	return rows[i].Keys[m.selectionDim()] == name
}

// selectedByToolBucket returns the selected tool bucket.
func (m Model) selectedByToolBucket() (store.Bucket, bool) {
	if m.byTool.Selected < 0 || m.byTool.Selected >= len(m.byTool.Rows) {
		return store.Bucket{}, false
	}
	return m.byTool.Rows[m.byTool.Selected], true
}

// selectedByModelBucket returns the selected model bucket.
func (m Model) selectedByModelBucket() (store.Bucket, bool) {
	if m.byModel.Selected < 0 || m.byModel.Selected >= len(m.byModel.Rows) {
		return store.Bucket{}, false
	}
	return m.byModel.Rows[m.byModel.Selected], true
}

// browseRowCount returns the number of visible Browse rows.
func (m Model) browseRowCount() int { return m.browse.RowCount() }
